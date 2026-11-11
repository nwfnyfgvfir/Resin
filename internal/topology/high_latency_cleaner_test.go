package topology

import (
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestEphemeralCleaner_HighLatencyIsIndependentAndRechecksRecovery(t *testing.T) {
	mgr := NewSubscriptionManager()
	pool := NewGlobalNodePool(PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 }})
	sub := subscription.NewSubscription("regular", "regular", "https://example.com", true, false)
	mgr.Register(sub)
	entries := make([]*node.NodeEntry, 0, 4)
	for i, latency := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 0} {
		raw := []byte(fmt.Sprintf(`{"type":"http","server":"node-%d.example","server_port":8080}`, i))
		h := node.HashFromRawOptions(raw)
		pool.AddNodeFromSub(h, raw, sub.ID)
		sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{})
		entry, _ := pool.GetEntry(h)
		ob := testutil.NewNoopOutbound()
		entry.Outbound.Store(&ob)
		entry.CircuitOpenSince.Store(0)
		if latency > 0 {
			entry.LatencyTable.Update("example.com", latency, time.Minute)
		}
		entries = append(entries, entry)
	}
	// A failed regular node must stay while only latency cleanup is enabled.
	entries[3].CircuitOpenSince.Store(time.Now().Add(-time.Hour).UnixNano())
	entries[3].FailureCount.Store(3)
	enabled := false
	threshold := 1000
	cleaner := NewEphemeralCleaner(mgr, pool)
	cleaner.SetHighLatencyPolicy(func() (bool, int, []string) { return enabled, threshold, []string{"example.com"} })
	evicted := 0
	cleaner.SetOnNodeEvicted(func(id string, h node.Hash) { evicted++ })
	cleaner.sweep()
	if pool.Size() != 4 {
		t.Fatal("disabled policy removed nodes")
	}
	enabled = true
	hookCalled := false
	cleaner.sweepWithHook(func() {
		hookCalled = true
		entries[1].LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{Ewma: 10 * time.Millisecond, LastUpdated: time.Now()})
	})
	if !hookCalled || pool.Size() != 3 || evicted != 1 {
		t.Fatalf("pool=%d evicted=%d hook=%v", pool.Size(), evicted, hookCalled)
	}
	if _, ok := pool.GetEntry(entries[1].Hash); !ok {
		t.Fatal("recovered latency node was removed")
	}
	if _, ok := pool.GetEntry(entries[3].Hash); !ok {
		t.Fatal("latency cleanup incorrectly removed a failed node")
	}
	threshold = 1
	cleaner.sweep()
	if pool.Size() != 1 || evicted != 3 {
		t.Fatal("hot-updated threshold did not take effect")
	}
}

func TestEphemeralCleaner_KeepsUntestedNodes(t *testing.T) {
	mgr := NewSubscriptionManager()
	pool := NewGlobalNodePool(PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 }})
	sub := subscription.NewSubscription("pending", "pending", "https://example.com", true, true)
	sub.SetEphemeralNodeEvictDelayNs(0)
	mgr.Register(sub)
	raw := []byte(`{"type":"http","server":"pending.example","server_port":8080}`)
	h := node.HashFromRawOptions(raw)
	pool.AddNodeFromSub(h, raw, sub.ID)
	sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{})
	entry, _ := pool.GetEntry(h)
	entry.CircuitOpenSince.Store(time.Now().Add(-time.Hour).UnixNano())
	cleaner := NewEphemeralCleaner(mgr, pool)
	cleaner.sweep()
	if pool.Size() != 1 {
		t.Fatal("untested node was removed before its first failure")
	}
	entry.FailureCount.Store(1)
	cleaner.sweep()
	if pool.Size() != 0 {
		t.Fatal("failed node was not removed after the delay")
	}
}
