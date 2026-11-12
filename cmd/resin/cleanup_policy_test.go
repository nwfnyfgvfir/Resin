package main

import (
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/config"
)

func TestHighLatencyCleanupPolicy_ConfigChangesInvalidateEvidence(t *testing.T) {
	var runtime atomic.Pointer[config.RuntimeConfig]
	cfg := config.NewDefaultRuntimeConfig()
	cfg.AutoRemoveHighLatencyNodesEnabled = true
	runtime.Store(cfg)
	provider := newHighLatencyCleanupPolicyProvider(&runtime)
	first := provider()
	if provider().Revision != first.Revision {
		t.Fatal("unchanged settings invalidated the evidence")
	}
	// Disable and re-enable before another probe or cleaner sweep runs.
	disabled := *cfg
	disabled.AutoRemoveHighLatencyNodesEnabled = false
	runtime.Store(&disabled)
	reenabled := *cfg
	runtime.Store(&reenabled)
	next := provider()
	if !next.Enabled || next.Revision == first.Revision {
		t.Fatal("re-enabling reused old cleanup evidence")
	}
	changed := reenabled
	changed.AutoRemoveHighLatencyNodesThresholdMS = 1500
	runtime.Store(&changed)
	last := provider()
	if last.ThresholdMS != 1500 || last.Revision == next.Revision {
		t.Fatal("new threshold reused old evidence")
	}
}
