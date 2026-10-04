package discovery

import (
	"net"
	"net/netip"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseEntry(t *testing.T) {
	entry := zeroconf.NewServiceEntry("NPA-peer", "_npa._tcp", "local.")
	entry.AddrIPv4 = []net.IP{net.ParseIP("192.168.1.20")}
	entry.Port = 56700
	entry.Text = []string{
		"id=peer-1", "name=Peer", "ver=0.3.0", "api=56700",
		"caps=ssh,relay", "subnets=192.168.2.0/24,10.0.0.0/24",
	}
	node, ok := parseEntry(entry)
	if !ok {
		t.Fatal("entry was not parsed")
	}
	if node.ID != "peer-1" || node.Address != "192.168.1.20" || node.Port != 56700 || !node.Discovered {
		t.Fatalf("unexpected node: %#v", node)
	}
	if len(node.ReachableSubnets) != 2 || node.ReachableSubnets[0] != "192.168.2.0/24" {
		t.Fatalf("unexpected subnets: %#v", node.ReachableSubnets)
	}
}

func TestLocalSubnetsAreValid(t *testing.T) {
	for _, value := range LocalSubnets() {
		if _, err := netip.ParsePrefix(value); err != nil {
			t.Fatalf("invalid local subnet %q: %v", value, err)
		}
	}
}
