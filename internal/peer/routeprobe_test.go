package peer

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestParseTraceHops(t *testing.T) {
	linux := `traceroute to 223.5.5.5 (223.5.5.5), 6 hops max
 1  192.168.2.1  1.234 ms
 2  192.168.1.1  2.345 ms
 3  * * *
 4  10.8.0.1  5.678 ms
`
	hops := parseTraceHops(linux)
	if len(hops) != 3 {
		t.Fatalf("linux hops = %v, want 3 entries (header must be skipped)", hops)
	}
	if hops[0].String() != "192.168.2.1" || hops[2].String() != "10.8.0.1" {
		t.Fatalf("unexpected hops: %v", hops)
	}
	windows := `  1     1 ms     1 ms     1 ms  192.168.2.1
  2     2 ms     *        *      192.168.1.1
`
	hops = parseTraceHops(windows)
	if len(hops) != 2 || hops[1].String() != "192.168.1.1" {
		t.Fatalf("windows hops = %v", hops)
	}
}

func TestPickUpperSubnet(t *testing.T) {
	hops := parseTraceHops(" 1  192.168.2.1\n 2  192.168.1.1\n 3  10.8.0.1\n 4  223.5.5.5\n")
	self, _ := ParseProbeSubnets("192.168.2.0/24")
	prefix, ok := pickUpperSubnet(hops, self)
	if !ok || prefix.String() != "192.168.1.0/24" {
		t.Fatalf("pickUpperSubnet = %v,%v; want 192.168.1.0/24", prefix, ok)
	}
	subnets := upperSubnetCandidates(hops, self)
	if len(subnets) != 2 || subnets[0].String() != "192.168.1.0/24" || subnets[1].String() != "10.8.0.0/24" {
		t.Fatalf("upperSubnetCandidates = %v", subnets)
	}
	// 全是本机网段（同网段直连）：不产生候选
	sameHops := parseTraceHops(" 1  192.168.2.1\n 2  192.168.2.254\n")
	if _, ok := pickUpperSubnet(sameHops, self); ok {
		t.Fatal("own-subnet-only hops should not produce a candidate")
	}
	// 直接出公网（NAT/直连出口）：不产生候选
	publicHops := parseTraceHops(" 1  192.168.2.1\n 2  100.64.0.1\n 3  223.5.5.5\n")
	if _, ok := pickUpperSubnet(publicHops, self); ok {
		t.Fatal("public-path hops should not produce a candidate")
	}
}

// TestAutoDiscoverUppers 用假 traceroute 输出 + 假探测，验证整条链路：
// trace → 挑域外私网网段 → 受限探测 → 命中即停。
func TestAutoDiscoverUppers(t *testing.T) {
	originalTrace, originalProbe := traceOutput, probeSubnetsFn
	defer func() { traceOutput, probeSubnetsFn = originalTrace, originalProbe }()
	traceOutput = func(context.Context) ([]byte, error) {
		return []byte("traceroute to 223.5.5.5\n 1  192.168.2.1\n 2  192.168.1.1\n"), nil
	}
	probeSubnetsFn = func(_ context.Context, prefixes []netip.Prefix, _ int) ([]Candidate, error) {
		if len(prefixes) == 0 || prefixes[0].String() != "192.168.1.0/24" {
			t.Errorf("probe got prefixes %v, want 192.168.1.0/24", prefixes)
		}
		return []Candidate{{
			Address: net.JoinHostPort("192.168.1.10", strconv.Itoa(DefaultPort)),
			RTT:     2 * time.Millisecond, Source: "probe",
			ID: "node-upper", Name: "A-1",
		}}, nil
	}
	found, err := AutoDiscoverUppers(context.Background(), DefaultPort, []string{"192.168.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %v, want 1 candidate", found)
	}
	if found[0].ID != "node-upper" || found[0].Source != "auto" {
		t.Fatalf("candidate = %+v", found[0])
	}
}

// TestAutoDiscoverUppersNoUpper：探测网段没有 NPA 时安静返回空。
func TestAutoDiscoverUppersNoUpper(t *testing.T) {
	originalTrace, originalProbe := traceOutput, probeSubnetsFn
	defer func() { traceOutput, probeSubnetsFn = originalTrace, originalProbe }()
	traceOutput = func(context.Context) ([]byte, error) {
		return []byte(" 1  192.168.2.1\n 2  192.168.1.1\n"), nil
	}
	probeSubnetsFn = func(context.Context, []netip.Prefix, int) ([]Candidate, error) {
		return []Candidate{}, nil
	}
	found, err := AutoDiscoverUppers(context.Background(), DefaultPort, []string{"192.168.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %v, want empty", found)
	}
}

// TestAutoDiscoverUppersStopsAtFirstSubnet：第一个网段命中后就不再扫下一个。
func TestAutoDiscoverUppersStopsAtFirstSubnet(t *testing.T) {
	originalTrace, originalProbe := traceOutput, probeSubnetsFn
	defer func() { traceOutput, probeSubnetsFn = originalTrace, originalProbe }()
	traceOutput = func(context.Context) ([]byte, error) {
		return []byte(" 1  192.168.2.1\n 2  192.168.1.1\n 3  10.8.0.1\n"), nil
	}
	probed := 0
	probeSubnetsFn = func(_ context.Context, prefixes []netip.Prefix, _ int) ([]Candidate, error) {
		probed++
		if len(prefixes) != 1 || prefixes[0].String() != "192.168.1.0/24" {
			t.Errorf("unexpected probe of %v", prefixes)
		}
		return []Candidate{{Address: "192.168.1.10:" + strconv.Itoa(DefaultPort), Source: "probe"}}, nil
	}
	found, err := AutoDiscoverUppers(context.Background(), DefaultPort, []string{"192.168.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if probed != 1 || len(found) != 1 {
		t.Fatalf("probed=%d found=%v; should stop at first subnet", probed, found)
	}
}
