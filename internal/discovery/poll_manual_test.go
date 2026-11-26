//go:build manual

package discovery

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestManualPoll(t *testing.T) {
	p := newMDNSPoller(func(i mdnsInstance) { fmt.Printf("EMIT %s host=%s port=%d addrs=%v txt=%v\n", i.Instance, i.Host, i.Port, i.Addrs, i.TXT) })
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	p.start(ctx)
	p.mu.Lock()
	for name, r := range p.records {
		fmt.Printf("REC %s host=%q port=%d addrs=%v txt=%v\n", name, r.Host, r.Port, r.Addrs, r.TXT)
	}
	p.mu.Unlock()
}
