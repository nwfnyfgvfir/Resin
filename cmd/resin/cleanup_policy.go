package main

import (
	"sync"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
)

// The same provider is used for recording evidence and checking eviction.
// Immutable runtime-config identity supplies a process-local revision without
// persisting evidence or adding another setting to the public configuration.
func newHighLatencyCleanupPolicyProvider(runtimeCfg *atomic.Pointer[config.RuntimeConfig]) func() node.HighLatencyCleanupPolicy {
	var mu sync.Mutex
	var previous *config.RuntimeConfig
	var revision uint64
	return func() node.HighLatencyCleanupPolicy {
		mu.Lock()
		defer mu.Unlock()
		cfg := runtimeConfigSnapshot(runtimeCfg)
		if cfg != previous {
			revision++
			previous = cfg
		}
		return node.HighLatencyCleanupPolicy{
			Enabled:     cfg.AutoRemoveHighLatencyNodesEnabled,
			ThresholdMS: cfg.AutoRemoveHighLatencyNodesThresholdMS,
			Authorities: cfg.LatencyAuthorities,
			Revision:    revision,
		}
	}
}
