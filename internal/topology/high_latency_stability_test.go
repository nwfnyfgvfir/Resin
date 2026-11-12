package topology

import (
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

type latencyStabilityFixture struct {
	policy  node.HighLatencyCleanupPolicy
	now     time.Time
	pool    *GlobalNodePool
	manager *SubscriptionManager
	cleaner *EphemeralCleaner
	sub     *subscription.Subscription
	entry   *node.NodeEntry
}

func newLatencyStabilityFixture(t *testing.T) *latencyStabilityFixture {
	t.Helper()
	f := &latencyStabilityFixture{now: time.Now(), policy: node.HighLatencyCleanupPolicy{
		Enabled: true, ThresholdMS: 1000, Authorities: []string{"example.com"}, Revision: 1,
	}}
	f.manager = NewSubscriptionManager()
	policy := func() node.HighLatencyCleanupPolicy { return f.policy }
	f.pool = NewGlobalNodePool(PoolConfig{
		SubLookup:              f.manager.Lookup,
		MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 3 },
		LatencyAuthorities:       func() []string { return f.policy.Authorities },
		HighLatencyCleanupPolicy: policy,
	})
	f.sub = subscription.NewSubscription("a", "a", "https://example.com/sub", true, false)
	f.manager.Register(f.sub)
	raw := []byte(`{"type":"http","server":"edge.example","server_port":8080}`)
	hash := node.HashFromRawOptions(raw)
	f.pool.AddNodeFromSub(hash, raw, f.sub.ID)
	f.sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{})
	f.entry, _ = f.pool.GetEntry(hash)
	ob := testutil.NewNoopOutbound()
	f.entry.Outbound.Store(&ob)
	f.entry.CircuitOpenSince.Store(0)
	f.cleaner = NewEphemeralCleaner(f.manager, f.pool)
	f.cleaner.SetHighLatencyPolicy(policy)
	return f
}

func (f *latencyStabilityFixture) sample(offset, latency time.Duration) {
	f.pool.recordLatencyAt(f.entry.Hash, "example.com", &latency, f.now.Add(offset))
}

func (f *latencyStabilityFixture) qualify() {
	f.sample(-10*time.Minute, 2*time.Second)
	f.sample(-5*time.Minute, 2*time.Second)
	f.sample(0, 2*time.Second)
}

func TestHighLatencyCleanup_SpikeAndRepeatedSweepsDoNotEvict(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.sample(-10*time.Minute, 2*time.Second)
	for range 10 {
		f.cleaner.sweep()
	}
	if f.pool.Size() != 1 {
		t.Fatal("a single spike was counted as multiple samples")
	}
	// Three rapid samples still cannot satisfy the ten-minute measurement span.
	f = newLatencyStabilityFixture(t)
	f.sample(-20*time.Second, 2*time.Second)
	f.sample(-10*time.Second, 2*time.Second)
	f.sample(0, 2*time.Second)
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now.Add(time.Minute)) {
		t.Fatal("burst samples bypassed the observation window")
	}
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now.Add(10*time.Minute)) {
		t.Fatal("wall time alone turned old samples into sustained evidence")
	}
}

func TestHighLatencyCleanup_RequiresThreeFreshSamplesSpanningWindow(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.sample(-10*time.Minute, 2*time.Second)
	f.sample(-5*time.Minute, 2*time.Second)
	f.cleaner.sweep()
	if f.pool.Size() != 1 {
		t.Fatal("removed a node with only two samples")
	}
	f.sample(0, 2*time.Second)
	f.cleaner.sweep()
	if f.pool.Size() != 0 {
		t.Fatal("persistent high latency was not cleaned")
	}
	managed, ok := f.sub.ManagedNodes().LoadNode(f.entry.Hash)
	if !ok || !managed.Evicted {
		t.Fatal("persistent cleanup must preserve the eviction marker")
	}
}

func TestHighLatencyCleanup_RawRecoveryResetsEvenWhileEWMALags(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.sample(-20*time.Minute, 2*time.Second)
	f.sample(-15*time.Minute, 2*time.Second)
	f.sample(-10*time.Minute, 100*time.Millisecond)
	if !node.ExceedsReferenceLatency(f.entry, f.policy.Authorities, 1000) {
		t.Fatal("fixture must retain a high EWMA after the fast raw sample")
	}
	f.sample(-5*time.Minute, 2*time.Second)
	f.sample(0, 2*time.Second)
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
		t.Fatal("low raw latency did not interrupt the streak")
	}
	f.sample(5*time.Minute, 2*time.Second)
	if !f.entry.ReadyForHighLatencyCleanup(f.policy, f.now.Add(5*time.Minute)) {
		t.Fatal("new sustained high latency run was not recognized")
	}
}

func TestHighLatencyCleanup_ExactThresholdResetsTheStreak(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.qualify()
	f.sample(time.Minute, time.Second)
	if !node.ExceedsReferenceLatency(f.entry, f.policy.Authorities, 1000) {
		t.Fatal("fixture must keep reference EWMA above the threshold")
	}
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now.Add(time.Minute)) {
		t.Fatal("a raw sample equal to 1000 ms did not reset the streak")
	}
}

func TestHighLatencyCleanup_RecoversDuringSecondConfirmation(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.qualify()
	called := false
	f.cleaner.sweepWithHook(func() {
		called = true
		fast := 100 * time.Millisecond
		f.pool.RecordLatency(f.entry.Hash, "example.com", &fast)
	})
	if !called || f.pool.Size() != 1 {
		t.Fatal("recovered node was removed during second confirmation")
	}
}

func TestHighLatencyCleanup_StaleUnknownAndFailedSamplesAreNotEvidence(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		f := newLatencyStabilityFixture(t)
		f.sample(-30*time.Minute, 2*time.Second)
		f.sample(-20*time.Minute, 2*time.Second)
		f.sample(-10*time.Minute, 2*time.Second)
		f.cleaner.sweep()
		if f.pool.Size() != 1 {
			t.Fatal("stale measurements removed a node")
		}
	})
	t.Run("restored statistics", func(t *testing.T) {
		f := newLatencyStabilityFixture(t)
		f.entry.LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{Ewma: 2 * time.Second, LastUpdated: f.now})
		f.cleaner.sweep()
		if f.pool.Size() != 1 {
			t.Fatal("restored EWMA was treated as new evidence")
		}
	})
	t.Run("failed probe", func(t *testing.T) {
		f := newLatencyStabilityFixture(t)
		f.qualify()
		f.pool.RecordLatency(f.entry.Hash, "example.com", nil)
		if f.entry.ReadyForHighLatencyCleanup(f.policy, time.Now()) {
			t.Fatal("failed probe did not reset the streak")
		}
	})
	t.Run("health failure", func(t *testing.T) {
		f := newLatencyStabilityFixture(t)
		f.qualify()
		f.pool.RecordResult(f.entry.Hash, false)
		if f.entry.ReadyForHighLatencyCleanup(f.policy, time.Now()) {
			t.Fatal("health failure did not reset the streak")
		}
	})
	t.Run("non-reference traffic", func(t *testing.T) {
		f := newLatencyStabilityFixture(t)
		f.sample(-10*time.Minute, 2*time.Second)
		slow := 2 * time.Second
		for _, offset := range []time.Duration{-5 * time.Minute, 0} {
			f.pool.recordLatencyAt(f.entry.Hash, "unrelated.org", &slow, f.now.Add(offset))
		}
		if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
			t.Fatal("non-reference domains added evidence")
		}
	})
}

func TestHighLatencyCleanup_DuplicateSamplesAndSharedSubscriptionsDoNotAddCounts(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	other := subscription.NewSubscription("b", "b", "https://example.com/other", true, false)
	f.manager.Register(other)
	f.pool.AddNodeFromSub(f.entry.Hash, f.entry.RawOptions, other.ID)
	other.ManagedNodes().StoreNode(f.entry.Hash, subscription.ManagedNode{})
	f.sample(-11*time.Minute, 2*time.Second)
	f.sample(-time.Minute, 2*time.Second)
	f.sample(-time.Minute, 2*time.Second)
	f.cleaner.sweep()
	if f.pool.Size() != 1 {
		t.Fatal("duplicate samples or subscription scans added evidence")
	}
	f.sample(0, 2*time.Second)
	if !f.entry.ReadyForHighLatencyCleanup(f.policy, time.Now()) {
		t.Fatal("new distinct sample did not qualify")
	}
	f.cleaner.sweep()
	if f.pool.Size() != 0 {
		t.Fatal("confirmed node was not removed from all subscriptions")
	}
}

func TestHighLatencyCleanup_ConfigChangeRequiresNewEvidence(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.qualify()
	f.policy.Revision++
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
		t.Fatal("old configuration evidence was reused")
	}
	f.policy.ThresholdMS = 500
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
		t.Fatal("new threshold reused old evidence")
	}
	f.policy.Enabled = false
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
		t.Fatal("disabled cleanup is still eligible")
	}
	f.policy.Enabled = true
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now) {
		t.Fatal("re-enabled cleanup reused old evidence")
	}
}

func TestHighLatencyCleanup_LateSamplesCannotReviveAFailedRun(t *testing.T) {
	f := newLatencyStabilityFixture(t)
	f.qualify()
	f.entry.ResetHighLatencyEvidenceAt(f.now.Add(time.Minute))
	// This high callback belongs to before the failure and arrives late.
	f.sample(30*time.Second, 2*time.Second)
	f.sample(5*time.Minute, 2*time.Second)
	f.sample(11*time.Minute, 2*time.Second)
	if f.entry.ReadyForHighLatencyCleanup(f.policy, f.now.Add(11*time.Minute)) {
		t.Fatal("a late pre-failure sample revived an interrupted run")
	}
}
