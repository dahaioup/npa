package discovery

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// 这一层是自带的最小 mDNS 查询器，替代 grandcat/zeroconf 的 Browse。
//
// 为什么要自己查：
//   - 库的 Browse 只在**单个报文**里凑齐 PTR+SRV+TXT+A 才产出条目。NPA 节点
//     应答 PTR 查询时只带 PTR（其余记录放在 additional 里，也可能不发），
//     于是一台机器广播了服务，别人也永远拿不到条目（本次 125/158 互相
//     发现不了就是这个原因；同一套代码用 _airplay 能收到，因为 Apple 设备是
//     主动广播的）。
//   - 应答里的 A 记录常带多张网卡（虚拟网卡排在前面），必须按“与本机同网段”
//     挑一个，否则会拿到 172.31.x / 192.168.110.1 这类不可达地址。
//
// 做法：向 224.0.0.251:5353 发**带 QU 位**（期望单播应答）的查询，用临时端口
// 收应答，按 PTR → SRV/TXT → A 三级补齐，凑齐后回调上报。不占 5353，不需要
// 特权，不受防火墙对入站组播的限制（应答是单播回给临时端口的）。
const (
	mdnsServiceFQDN  = "_npa._tcp.local."
	mdnsGroupAddress = "224.0.0.251:5353"
	// mdnsPollInterval 是整轮探测（PTR 枚举 + 逐个补全）的周期。
	mdnsPollInterval = 15 * time.Second
	// mdnsCollectWindow 是每一轮查询等待应答的时间。
	mdnsCollectWindow = 2 * time.Second
	// mdnsEmitInterval 是同一实例两次上报的最小间隔，避免刷库。
	mdnsEmitInterval = 60 * time.Second
	// mdnsRecordTTL 是本地缓存的存活时间：超时没再出现就丢弃。
	mdnsRecordTTL = 5 * time.Minute
)

// mdnsInstance 是从 mDNS 解析出的一个 NPA 实例。
type mdnsInstance struct {
	Instance string
	Host     string
	Port     int
	TXT      []string
	Addrs    []net.IP
	Updated  time.Time
	emitted  time.Time
}

// mdnsPoller 维护“实例 → 记录”表，周期性地主动查询。
type mdnsPoller struct {
	emit func(mdnsInstance)

	mu      sync.Mutex
	records map[string]*mdnsInstance
}

func newMDNSPoller(emit func(mdnsInstance)) *mdnsPoller {
	return &mdnsPoller{emit: emit, records: map[string]*mdnsInstance{}}
}

// start 起一个后台循环，直到 ctx 结束。
func (p *mdnsPoller) start(ctx context.Context) {
	ticker := time.NewTicker(mdnsPollInterval)
	defer ticker.Stop()
	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll 执行一轮：先枚举 PTR，再对缺字段的实例补 SRV/TXT/A。
func (p *mdnsPoller) poll(ctx context.Context) {
	// 必须把组播出口钉在“有默认路由的那张网卡”上：Windows 上组播默认走
	// 虚拟网卡（VirtualBox/Hyper-V 的 172.31.x），从那里发出去的查询到不了
	// 真实局域网，表现就是“本机能被别家发现，自己谁也发现不了”。
	primary := net.ParseIP(PrimaryIP())
	bindAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	if primary != nil && !primary.IsLoopback() {
		bindAddr = &net.UDPAddr{IP: primary, Port: 0}
	}
	conn, err := net.ListenUDP("udp4", bindAddr)
	if err != nil {
		slog.Warn("mdns poller cannot open socket", "error", err)
		return
	}
	defer conn.Close()
	var group *ipv4.PacketConn
	if primary != nil {
		if iface := interfaceByIP(primary); iface != nil {
			group = ipv4.NewPacketConn(conn)
			if err := group.SetMulticastInterface(iface); err != nil {
				slog.Debug("mdns cannot set multicast interface", "interface", iface.Name, "error", err)
			}
		}
	}
	send := func(name string, qtype uint16) { p.send(conn, group, name, qtype) }
	deadline := time.Now().Add(mdnsCollectWindow)

	send(mdnsServiceFQDN, dns.TypePTR)
	p.collect(ctx, conn, deadline)

	// 第二级：对已有 PTR、但还缺 SRV/TXT/地址的实例精确查询。
	for _, name := range p.incomplete() {
		send(name, dns.TypeSRV)
		send(name, dns.TypeTXT)
	}
	p.collect(ctx, conn, time.Now().Add(mdnsCollectWindow))

	// 第三级：SRV 拿到了主机名但还没有地址的，再问 A。
	for _, host := range p.hostsWithoutAddress() {
		send(host, dns.TypeA)
	}
	p.collect(ctx, conn, time.Now().Add(mdnsCollectWindow))

	p.flush()
}

func (p *mdnsPoller) send(conn *net.UDPConn, group *ipv4.PacketConn, name string, qtype uint16) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), qtype)
	msg.RecursionDesired = false
	if len(msg.Question) > 0 {
		// QU 位：告诉应答方“请单播回我”，这样不依赖组播入站是否被放行。
		msg.Question[0].Qclass |= 0x8000
	}
	payload, err := msg.Pack()
	if err != nil {
		return
	}
	dest := mustResolveUDPAddr(mdnsGroupAddress)
	if group != nil {
		if _, err := group.WriteTo(payload, nil, dest); err == nil {
			return
		}
	}
	if _, err := conn.WriteToUDP(payload, dest); err != nil {
		slog.Debug("mdns query send failed", "name", name, "error", err)
	}
}

func (p *mdnsPoller) collect(ctx context.Context, conn *net.UDPConn, deadline time.Time) {
	buf := make([]byte, 9000)
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			continue
		}
		p.absorb(&msg)
	}
}

// absorb 把一条应答里的记录并进实例表。记录可以分散在前后几轮里到达。
func (p *mdnsPoller) absorb(msg *dns.Msg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	sections := make([]dns.RR, 0, len(msg.Answer)+len(msg.Ns)+len(msg.Extra))
	sections = append(sections, msg.Answer...)
	sections = append(sections, msg.Ns...)
	sections = append(sections, msg.Extra...)

	known := func(name string) *mdnsInstance {
		rec, ok := p.records[name]
		if !ok {
			rec = &mdnsInstance{Instance: name, Updated: now}
			p.records[name] = rec
		}
		return rec
	}

	for _, rr := range sections {
		switch record := rr.(type) {
		case *dns.PTR:
			if !strings.EqualFold(record.Hdr.Name, mdnsServiceFQDN) {
				continue
			}
			known(strings.TrimSuffix(record.Ptr, "."))
		case *dns.SRV:
			if !strings.HasSuffix(strings.ToLower(record.Hdr.Name), mdnsServiceFQDN) {
				continue
			}
			rec := known(strings.TrimSuffix(record.Hdr.Name, "."))
			rec.Host = strings.TrimSuffix(record.Target, ".")
			rec.Port = int(record.Port)
			rec.Updated = now
		case *dns.TXT:
			if !strings.HasSuffix(strings.ToLower(record.Hdr.Name), mdnsServiceFQDN) {
				continue
			}
			rec := known(strings.TrimSuffix(record.Hdr.Name, "."))
			rec.TXT = append([]string{}, record.Txt...)
			rec.Updated = now
		case *dns.A:
			host := strings.TrimSuffix(record.Hdr.Name, ".")
			for _, rec := range p.records {
				if strings.EqualFold(rec.Host, host) {
					rec.Addrs = appendUniqueIP(rec.Addrs, record.A)
					rec.Updated = now
				}
			}
		case *dns.AAAA:
			host := strings.TrimSuffix(record.Hdr.Name, ".")
			for _, rec := range p.records {
				if strings.EqualFold(rec.Host, host) {
					rec.Addrs = appendUniqueIP(rec.Addrs, record.AAAA)
					rec.Updated = now
				}
			}
		}
	}
}

// flush 上报“凑齐了且到了上报间隔”的实例，并清理过期记录。
func (p *mdnsPoller) flush() {
	p.mu.Lock()
	type pending struct {
		instance mdnsInstance
		address  string
	}
	var ready []pending
	now := time.Now()
	for name, rec := range p.records {
		if now.Sub(rec.Updated) > mdnsRecordTTL {
			delete(p.records, name)
			continue
		}
		address := pickMdnsAddress(rec.Addrs)
		if rec.Host == "" || rec.Port <= 0 || len(rec.TXT) == 0 || address == "" {
			continue
		}
		if !rec.emitted.IsZero() && now.Sub(rec.emitted) < mdnsEmitInterval {
			continue
		}
		rec.emitted = now
		instance := *rec
		instance.Instance = shortInstanceName(rec.Instance)
		instance.Addrs = []net.IP{net.ParseIP(address)}
		ready = append(ready, pending{instance: instance, address: address})
	}
	p.mu.Unlock()

	for _, item := range ready {
		if p.emit != nil {
			p.emit(item.instance)
		}
	}
}

// incomplete 返回已有 PTR、但缺 SRV/TXT 的实例名（FQDN）。
func (p *mdnsPoller) incomplete() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.records))
	for name, rec := range p.records {
		if rec.Host == "" || rec.Port <= 0 || len(rec.TXT) == 0 {
			names = append(names, name+".")
		}
	}
	sort.Strings(names)
	return names
}

// hostsWithoutAddress 返回已解析出主机名、但还没有地址的主机名（FQDN）。
func (p *mdnsPoller) hostsWithoutAddress() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]struct{}{}
	hosts := make([]string, 0, len(p.records))
	for _, rec := range p.records {
		if rec.Host == "" || pickMdnsAddress(rec.Addrs) != "" {
			continue
		}
		key := strings.ToLower(rec.Host)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		hosts = append(hosts, rec.Host+".")
	}
	sort.Strings(hosts)
	return hosts
}

// pickMdnsAddress 在多张网卡的地址里挑一个“从本机大概率可达”的 IPv4：
// 优先与本机某个网段同网段，其次任选一个私网 IPv4；都不满足返回空串。
func pickMdnsAddress(addrs []net.IP) string {
	if len(addrs) == 0 {
		return ""
	}
	local := localPrefixes()
	var fallback string
	for _, addr := range addrs {
		ip4 := addr.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		ip, ok := netip.AddrFromSlice(ip4)
		if !ok {
			continue
		}
		for _, prefix := range local {
			if prefix.Contains(ip) {
				return ip.String()
			}
		}
		if fallback == "" {
			fallback = ip.String()
		}
	}
	return fallback
}

// localPrefixes 返回本机所有已启用网卡上的网段，用于判断“同网段”。
func localPrefixes() []netip.Prefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	result := make([]netip.Prefix, 0, 8)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			if prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast() {
				continue
			}
			result = append(result, prefix.Masked())
		}
	}
	return result
}

// shortInstanceName 把 "NPA-58._npa._tcp.local" 收敛成 "NPA-58"。
func shortInstanceName(name string) string {
	trimmed := strings.TrimSuffix(name, "."+strings.TrimSuffix(mdnsServiceFQDN, "."))
	trimmed = strings.TrimSuffix(trimmed, ".")
	if trimmed == "" {
		return name
	}
	return trimmed
}

// interfaceByIP 找到持有该地址的网卡，用于固定组播出口。
func interfaceByIP(ip net.IP) *net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			if prefix.Addr() == addrToNetip(ip) {
				candidate := iface
				return &candidate
			}
		}
	}
	return nil
}

func addrToNetip(ip net.IP) netip.Addr {
	parsed, ok := netip.AddrFromSlice(ip.To4())
	if !ok {
		parsed, _ = netip.AddrFromSlice(ip)
	}
	return parsed
}

func appendUniqueIP(list []net.IP, ip net.IP) []net.IP {
	for _, existing := range list {
		if existing.Equal(ip) {
			return list
		}
	}
	return append(list, ip)
}

func mustResolveUDPAddr(address string) *net.UDPAddr {
	addr, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	}
	return addr
}
