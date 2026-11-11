package service

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestCleanupNodes_SelectedFailedNodesAcrossSubscriptions(t *testing.T) {
	cp, mgr, pool := newCleanupSubscriptionTestService()
	a := subscription.NewSubscription("a", "a", "https://example.com/a", true, false)
	b := subscription.NewSubscription("b", "b", "https://example.com/b", true, false)
	mgr.Register(a)
	mgr.Register(b)
	add := func(name string, failed bool) node.Hash {
		raw := []byte(fmt.Sprintf(`{"type":"http","server":%q,"server_port":8080}`, name))
		h := node.HashFromRawOptions(raw)
		pool.AddNodeFromSub(h, raw, a.ID)
		a.ManagedNodes().StoreNode(h, subscription.ManagedNode{Tags: []string{name}})
		entry, _ := pool.GetEntry(h)
		if failed {
			entry.FailureCount.Store(3)
		}
		return h
	}
	shared := add("shared.example", true)
	entry, _ := pool.GetEntry(shared)
	pool.AddNodeFromSub(shared, entry.RawOptions, b.ID)
	b.ManagedNodes().StoreNode(shared, subscription.ManagedNode{Tags: []string{"shared"}})
	pending := add("pending.example", false)
	buildFailed := add("build-failed.example", false)
	buildEntry, _ := pool.GetEntry(buildFailed)
	buildEntry.CircuitOpenSince.Store(0)
	buildEntry.SetLastError("outbound build failed")
	unselected := add("unselected.example", true)
	result, err := cp.CleanupNodes(NodeCleanupRequest{
		NodeHashes: []string{shared.Hex(), shared.Hex(), buildFailed.Hex(), pending.Hex(), node.HashFromRawOptions([]byte(`{}`)).Hex()},
		Mode:       "unhealthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CleanedCount != 2 || result.SkippedCount != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, ok := pool.GetEntry(shared); ok {
		t.Fatal("shared node remains in pool")
	}
	for _, sub := range []*subscription.Subscription{a, b} {
		managed, ok := sub.ManagedNodes().LoadNode(shared)
		if !ok || !managed.Evicted {
			t.Fatal("each subscription must retain the eviction marker")
		}
	}
	for _, h := range []node.Hash{pending, unselected} {
		if _, ok := pool.GetEntry(h); !ok {
			t.Fatal("pending or unselected node removed")
		}
	}
}

func TestCleanupNodes_LatencyThresholdAndValidation(t *testing.T) {
	cp, mgr, pool := newCleanupSubscriptionTestService()
	cp.RuntimeCfg = &atomic.Pointer[config.RuntimeConfig]{}
	cfg := config.NewDefaultRuntimeConfig()
	cfg.LatencyAuthorities = []string{"example.com"}
	cp.RuntimeCfg.Store(cfg)
	sub := subscription.NewSubscription("a", "a", "https://example.com/a", true, false)
	mgr.Register(sub)
	var hashes []string
	for index, latency := range []time.Duration{999 * time.Millisecond, time.Second, 1001 * time.Millisecond, 0} {
		raw := []byte(fmt.Sprintf(`{"type":"http","server":"node-%d.example","server_port":8080}`, index))
		h := node.HashFromRawOptions(raw)
		hashes = append(hashes, h.Hex())
		pool.AddNodeFromSub(h, raw, sub.ID)
		sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{})
		entry, _ := pool.GetEntry(h)
		ob := testutil.NewNoopOutbound()
		entry.Outbound.Store(&ob)
		entry.CircuitOpenSince.Store(0)
		if latency > 0 {
			entry.LatencyTable.Update("example.com", latency, time.Minute)
		}
	}
	for _, req := range []NodeCleanupRequest{
		{NodeHashes: hashes, Mode: "high_latency", MaxLatencyMS: 0},
		{NodeHashes: hashes, Mode: "high_latency", MaxLatencyMS: -1},
		{NodeHashes: hashes, Mode: "unknown"},
		{Mode: "unhealthy"},
		{NodeHashes: append(append([]string{}, hashes...), "invalid"), Mode: "high_latency", MaxLatencyMS: 1000},
	} {
		if _, err := cp.CleanupNodes(req); err == nil {
			t.Fatalf("accepted invalid request: %+v", req)
		}
		if pool.Size() != 4 {
			t.Fatal("invalid request mutated the pool")
		}
	}
	result, err := cp.CleanupNodes(NodeCleanupRequest{NodeHashes: hashes, Mode: "high_latency", MaxLatencyMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if result.CleanedCount != 1 || result.SkippedCount != 3 || result.CleanedNodeHashes[0] != hashes[2] {
		t.Fatalf("unexpected threshold result: %+v", result)
	}
	result, err = cp.CleanupNodes(NodeCleanupRequest{NodeHashes: hashes, Mode: "high_latency", MaxLatencyMS: 1000})
	if err != nil || result.CleanedCount != 0 {
		t.Fatalf("cleanup must be idempotent: %+v, %v", result, err)
	}
}
