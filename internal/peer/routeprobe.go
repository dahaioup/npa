package peer

// routeprobe 实现「上级网络的自动发现」：
// 1. traceroute/tracert（只取前 6 跳）拿到出站路径；
// 2. 取路径上第一个不属于本机网段的 RFC1918 私网地址——它是某台上游
//    内网路由面向我们这一侧的接口，所在 /24 就是候选上级网段；
//    （原理：ICMP Time Exceeded 报文填的是「收到包的那一面」的接口地址，
//    所以第 2 跳的 ICMP 由上一台路由的对侧接口地址被下一跳「报」出来。）
// 3. 对候选网段做既有的受限端口探测（ProbeSubnets，只碰 56701 + /api/info
//    校验），命中 NPA 即停。
// 全程只发出站请求，遵循「连接永远由下级发起」的原则。
// NAT/无上游内网的拓扑探测不到东西（数学边界），此时由种子兜底。

import (
	"bytes"
	"context"
	"net/netip"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// probeSubnetsFn 供测试替换 ProbeSubnets（真实实现见 discover.go）。
var probeSubnetsFn = ProbeSubnets

// autoTraceTarget 是 TTL 探测的固定外部地址（公共 DNS，稳定且丢 ICMP 不影响）。
const autoTraceTarget = "223.5.5.5"

// traceMaxHops/traceTimeout 控制 traceroute 的深度与整体预算；
// 只关心前几跳，深了没有意义还拖慢启动。
const (
	traceMaxHops = 6
	traceTimeout = 12 * time.Second
	// maxAutoHops 最多尝试几个「域外私网跳」对应的网段，避免连续扫多个网段。
	maxAutoHops = 2
)

// traceOutput 是可注入的 traceroute 执行器（测试里替换以避免真实外网探测）。
var traceOutput = func(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, traceTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "tracert", "-d", "-h", "6", "-w", "600", autoTraceTarget)
	} else {
		cmd = exec.CommandContext(ctx, "traceroute", "-n", "-q", "1", "-w", "1", "-m", "6", autoTraceTarget)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Output()
}

// parseTraceHops 从 traceroute/tracert -n -d 的输出里按顺序提取跳点地址。
// 兼容 macOS/Linux（" 1  192.168.2.1  1.2 ms"）与 Windows（"  1  1 ms  192.168.2.1"）：
// 只认「行首是跳数」的行（跳过 traceroute 的标题行），行内取第一个能解析成
// IP 的 token，解析不了（如 "* * *"）就跳过该行。
func parseTraceHops(output string) []netip.Addr {
	hops := []netip.Addr{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSuffix(fields[0], ".")); err != nil {
			continue // 不是跳点行（标题/汇总）
		}
		for _, field := range fields[1:] {
			field = strings.Trim(field, "()")
			addr, err := netip.ParseAddr(field)
			if err != nil {
				continue
			}
			hops = append(hops, addr.Unmap())
			break
		}
	}
	return hops
}

// pickUpperSubnet 从跳点里选候选上级网段：第一个 RFC1918 私网、且不属于
// 本机网段的地址。本机网关与同网段节点天然被 selfPrefixes 过滤掉。
// 找不到（路径直接出公网 → NAT/直连出口）返回 false。
func pickUpperSubnet(hops []netip.Addr, selfPrefixes []netip.Prefix) (netip.Prefix, bool) {
	for _, hop := range hops {
		if !hop.IsValid() || !hop.Is4() || !hop.IsPrivate() {
			continue // 只认 IPv4 RFC1918；公网地址说明已出内网
		}
		inSelf := false
		for _, prefix := range selfPrefixes {
			if prefix.Contains(hop) {
				inSelf = true
				break
			}
		}
		if inSelf {
			continue // 自己网段/网关
		}
		prefix, err := hop.Prefix(24) // 归到 /24
		if err != nil {
			continue
		}
		return prefix.Masked(), true
	}
	return netip.Prefix{}, false
}

// upperSubnetCandidates 返回至多 maxAutoHops 个域外私网跳对应的网段。
func upperSubnetCandidates(hops []netip.Addr, selfPrefixes []netip.Prefix) []netip.Prefix {
	result := []netip.Prefix{}
	seen := map[netip.Prefix]bool{}
	for _, hop := range hops {
		if len(result) >= maxAutoHops {
			break
		}
		if !hop.IsValid() || !hop.Is4() || !hop.IsPrivate() {
			continue
		}
		inSelf := false
		for _, prefix := range selfPrefixes {
			if prefix.Contains(hop) {
				inSelf = true
				break
			}
		}
		if inSelf {
			continue
		}
		prefix, err := hop.Prefix(24)
		if err != nil {
			continue
		}
		prefix = prefix.Masked()
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		result = append(result, prefix)
	}
	return result
}

// AutoDiscoverUppers 执行一次完整的上级网络自动发现，返回命中的 NPA 候选
// （Source=auto，ID/Name 来自 /api/info）。任何一步失败都安静返回已得结果。
func AutoDiscoverUppers(ctx context.Context, port int, selfSubnets []string) ([]Candidate, error) {
	if port <= 0 {
		port = DefaultPort
	}
	output, err := traceOutput(ctx)
	if err != nil {
		return nil, err // 无 traceroute 可用或超时：按「未发现」处理
	}
	hops := parseTraceHops(string(output))
	selfPrefixes, _ := ParseProbeSubnets(strings.Join(selfSubnets, ","))
	subnets := upperSubnetCandidates(hops, selfPrefixes)
	found := []Candidate{}
	for _, prefix := range subnets {
		candidates, err := probeSubnetsFn(ctx, []netip.Prefix{prefix}, port)
		if err != nil {
			return found, err
		}
		for _, candidate := range candidates {
			if info, err := fetchInfo(ctx, candidate.Address); err == nil && info != nil {
				candidate.ID = info.ID
				candidate.Name = info.Name
			}
			candidate.Source = "auto"
			found = append(found, candidate)
		}
		if len(found) > 0 {
			break // 找到一个网段就停
		}
	}
	return found, nil
}
