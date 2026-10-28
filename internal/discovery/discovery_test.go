package discovery

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// buildResponse 组装一条 mDNS 应答，用来验证轮询器的记录归并。
func buildResponse(t *testing.T, records ...dns.RR) *dns.Msg {
	t.Helper()
	msg := new(dns.Msg)
	msg.Response = true
	for _, record := range records {
		switch record.(type) {
		case *dns.PTR:
			msg.Answer = append(msg.Answer, record)
		default:
			msg.Extra = append(msg.Extra, record)
		}
	}
	return msg
}

func TestMDNSPollerAbsorbsSplitRecords(t *testing.T) {
	var emitted []mdnsInstance
	poller := newMDNSPoller(func(instance mdnsInstance) { emitted = append(emitted, instance) })

	// 第一轮：只有 PTR（这正是 grandcat/zeroconf 丢失条目的场景）。
	poller.absorb(&dns.Msg{Answer: []dns.RR{&dns.PTR{
		Hdr: dns.RR_Header{Name: mdnsServiceFQDN, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 120},
		Ptr: "NPA-58._npa._tcp.local.",
	}}})
	poller.flush()
	if len(emitted) != 0 {
		t.Fatalf("PTR-only record must not be emitted, got %d", len(emitted))
	}

	// 第二轮：SRV + TXT 补齐。
	poller.absorb(&dns.Msg{Extra: []dns.RR{
		&dns.SRV{Hdr: dns.RR_Header{Name: "NPA-58._npa._tcp.local.", Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 120}, Port: 56700, Target: "lianxiangui.local."},
		&dns.TXT{Hdr: dns.RR_Header{Name: "NPA-58._npa._tcp.local.", Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 120}, Txt: []string{"id=DU927FDI", "name=lianxiangui", "peer=56701"}},
	}})
	poller.flush()
	if len(emitted) != 0 {
		t.Fatalf("record without address must not be emitted, got %d", len(emitted))
	}

	// 第三轮：A 记录（含虚拟网卡地址，应挑同网段那个）。
	poller.absorb(&dns.Msg{Extra: []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "lianxiangui.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.ParseIP("172.26.80.1")},
		&dns.A{Hdr: dns.RR_Header{Name: "lianxiangui.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.ParseIP("192.168.0.158")},
	}})
	poller.flush()
	if len(emitted) != 1 {
		t.Fatalf("expected one emitted instance, got %d", len(emitted))
	}
	got := emitted[0]
	if got.Instance != "NPA-58" {
		t.Fatalf("unexpected instance name %q", got.Instance)
	}
	if got.Port != 56700 || got.Host != "lianxiangui.local" {
		t.Fatalf("unexpected SRV data: %#v", got)
	}
	if len(got.Addrs) != 1 || got.Addrs[0].String() != pickMdnsAddress([]net.IP{net.ParseIP("172.26.80.1"), net.ParseIP("192.168.0.158")}) {
		t.Fatalf("unexpected address selection: %v", got.Addrs)
	}
}

func TestMDNSPollerEmitThrottle(t *testing.T) {
	count := 0
	poller := newMDNSPoller(func(mdnsInstance) { count++ })
	poller.records["NPA-x"] = &mdnsInstance{
		Instance: "NPA-x", Host: "x.local", Port: 56700,
		TXT: []string{"id=abc"}, Addrs: []net.IP{net.ParseIP("127.0.0.1")},
		Updated: time.Now(),
	}
	// 回环地址会被挑掉，不上报。
	poller.flush()
	if count != 0 {
		t.Fatalf("loopback address must not be emitted, got %d", count)
	}
	poller.records["NPA-x"].Addrs = []net.IP{net.ParseIP("10.1.2.3")}
	poller.flush()
	poller.flush()
	if count != 1 {
		t.Fatalf("second flush within interval must be throttled, got %d", count)
	}
}

func TestPickMdnsAddressPrefersSameSubnet(t *testing.T) {
	// 本机若有 192.168.0.x 网段，则应跳过虚拟网卡地址。
	got := pickMdnsAddress([]net.IP{
		net.ParseIP("172.31.160.1"),
		net.ParseIP("192.168.0.158"),
		net.ParseIP("fe80::1"),
		net.ParseIP("127.0.0.1"),
	})
	if got == "127.0.0.1" || got == "172.31.160.1" {
		t.Fatalf("picked unusable address %q", got)
	}
	if net.ParseIP(got) == nil {
		t.Fatalf("picked address is not an IP: %q", got)
	}
}

func TestLocalSubnetsAreValid(t *testing.T) {
	for _, value := range LocalSubnets() {
		if _, err := netip.ParsePrefix(value); err != nil {
			t.Fatalf("invalid local subnet %q: %v", value, err)
		}
	}
}
