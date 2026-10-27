package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Candidate 是下级探测到的一个可能的上级节点。
type Candidate struct {
	Address string        `json:"address"` // host:port
	RTT     time.Duration `json:"-"`
	Source  string        `json:"source"` // seed | probe | auto
	// ID/Name 是候选节点自报的标识（/api/info），供分域选举比较；可能为空。
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

func (c Candidate) Endpoint() string { return c.Address }

// DefaultPort 是节点通信端口的默认值：注册、控制长连接、数据池、
// 协调 RPC 与 MCP 共用 56701（与 state.defaults、cmd/npa/main.go 保持一致）。
const DefaultPort = 56701

// LegacyPort 是端口收敛前专用的反向接入端口。旧版本节点自报的地址与
// 用户种子里写死的 :56710 一律视为指向 56701（两端口在新版是同一个服务）。
const LegacyPort = 56710

// NormalizePort 把历史遗留的 56710 归一为 56701；其余原样返回。
func NormalizePort(port int) int {
	if port == LegacyPort {
		return DefaultPort
	}
	return port
}

// NormalizeAddress 对 host:port 做同样的端口归一；解析失败原样返回。
func NormalizeAddress(address string) string {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return address
	}
	return net.JoinHostPort(host, strconv.Itoa(NormalizePort(port)))
}

// ParseSeeds 解析逗号/空白分隔的种子地址，缺端口时补默认端口。
func ParseSeeds(raw string, port int) []string {
	if port <= 0 {
		port = DefaultPort
	}
	seen := map[string]struct{}{}
	result := []string{}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\t'
	}) {
		field = strings.Trim(strings.TrimSpace(field), "/")
		if field == "" {
			continue
		}
		host, portText, err := net.SplitHostPort(field)
		if err != nil {
			host, portText = field, strconv.Itoa(port)
		}
		if host == "" {
			continue
		}
		if portText == "" {
			portText = strconv.Itoa(port)
		}
		address := net.JoinHostPort(host, portText)
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result
}

// ParseProbeSubnets 解析显式配置的上级网段；非法项返回错误，避免静默全网扫描。
func ParseProbeSubnets(raw string) ([]netip.Prefix, error) {
	result := []netip.Prefix{}
	seen := map[netip.Prefix]struct{}{}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\t'
	}) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !strings.Contains(field, "/") {
			field += "/32"
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, fmt.Errorf("invalid probe subnet %q: %w", field, err)
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	return result, nil
}

// probeConcurrency 限制探测并发，避免对上级网段造成冲击。
const (
	probeConcurrency = 64
	probeTimeout     = 400 * time.Millisecond
	// maxProbeAddresses 是单次探测允许展开的最大地址数，超出部分直接放弃并报错。
	maxProbeAddresses = 4096
)

// ProbeSubnets 对**显式配置**的网段做受限端口探测，只碰 peer 端口并用 /api/info 校验是否 NPA。
// 不配置网段就不会扫描任何东西（遵循 SRS：不得默认做大范围网段扫描）。
func ProbeSubnets(ctx context.Context, prefixes []netip.Prefix, port int) ([]Candidate, error) {
	if len(prefixes) == 0 {
		return []Candidate{}, nil
	}
	if port <= 0 {
		port = DefaultPort
	}
	addresses := []string{}
	for _, prefix := range prefixes {
		if err := enumerate(prefix, func(ip netip.Addr) error {
			if len(addresses) >= maxProbeAddresses {
				return fmt.Errorf("probe range exceeds %d addresses; split it or use seeds", maxProbeAddresses)
			}
			addresses = append(addresses, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
			return nil
		}); err != nil {
			return nil, err
		}
	}

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		sem   = make(chan struct{}, probeConcurrency)
		found []Candidate
	)
	for _, address := range addresses {
		address := address
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			start := time.Now()
			conn, err := net.DialTimeout("tcp", address, probeTimeout)
			if err != nil {
				return
			}
			_ = conn.Close()
			info, err := fetchInfo(ctx, address)
			if err != nil || info.Product != "npa" {
				return
			}
			candidate := Candidate{Address: address, RTT: time.Since(start), Source: "probe"}
			mu.Lock()
			found = append(found, candidate)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return found, nil
}

// enumerate 展开网段地址，跳过 IPv4 网络号；地址数超过 maxProbeAddresses 直接报错。
func enumerate(prefix netip.Prefix, visit func(netip.Addr) error) error {
	if prefix.Addr().Is6() && prefix.Bits() < 120 {
		return fmt.Errorf("ipv6 probe subnet %s is too broad", prefix)
	}
	ip := prefix.Masked().Addr()
	first := ip
	count := 0
	for ; prefix.Contains(ip); ip = ip.Next() {
		count++
		if count > maxProbeAddresses {
			return fmt.Errorf("probe range exceeds %d addresses; split it or use seeds", maxProbeAddresses)
		}
		if ip == first && prefix.Bits() < 31 {
			continue // 跳过 IPv4 网络号
		}
		if err := visit(ip); err != nil {
			return err
		}
	}
	return nil
}

func fetchInfo(ctx context.Context, address string) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/info", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	info := &Info{}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(info); err != nil {
		return nil, err
	}
	return info, nil
}

// Discover 合并种子与受限探测结果，按来源与往返时长排序去重。
func Discover(ctx context.Context, seeds string, probeSubnets string, port int) ([]Candidate, error) {
	result := []Candidate{}
	seen := map[string]struct{}{}
	for _, address := range ParseSeeds(seeds, port) {
		seen[address] = struct{}{}
		start := time.Now()
		candidate := Candidate{Address: address, RTT: time.Since(start), Source: "seed"}
		// 种子无需探测可达性，交给后续注册失败重试；这里只测一次 RTT 用于选主。
		if conn, err := net.DialTimeout("tcp", address, probeTimeout); err == nil {
			_ = conn.Close()
			candidate.RTT = time.Since(start)
		} else {
			candidate.RTT = time.Hour // 不可达的种子排到最后
		}
		result = append(result, candidate)
	}
	prefixes, err := ParseProbeSubnets(probeSubnets)
	if err != nil {
		return result, err
	}
	probed, err := ProbeSubnets(ctx, prefixes, port)
	if err != nil {
		return result, err
	}
	for _, candidate := range probed {
		if _, ok := seen[candidate.Address]; ok {
			continue
		}
		seen[candidate.Address] = struct{}{}
		result = append(result, candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Source != result[j].Source {
			return result[i].Source == "seed" // 种子优先于探测
		}
		return result[i].RTT < result[j].RTT
	})
	return result, nil
}
