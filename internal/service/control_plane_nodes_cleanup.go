package service

import (
	"sort"
	"strings"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/topology"
)

type NodeCleanupRequest struct {
	NodeHashes   []string `json:"node_hashes"`
	Mode         string   `json:"mode"`
	MaxLatencyMS int      `json:"max_latency_ms"`
}

type NodeCleanupResult struct {
	CleanedCount      int      `json:"cleaned_count"`
	SkippedCount      int      `json:"skipped_count"`
	CleanedNodeHashes []string `json:"cleaned_node_hashes"`
}

// CleanupNodes evicts only selected nodes that still satisfy the cleanup rule.
// All hashes are validated before any mutation; missing nodes are skipped.
func (s *ControlPlaneService) CleanupNodes(req NodeCleanupRequest) (*NodeCleanupResult, error) {
	if len(req.NodeHashes) == 0 {
		return nil, invalidArg("node_hashes: must not be empty")
	}
	if req.Mode != "unhealthy" && req.Mode != "high_latency" {
		return nil, invalidArg("mode: must be unhealthy or high_latency")
	}
	if req.Mode == "high_latency" && (req.MaxLatencyMS < 1 || req.MaxLatencyMS > 2147483647) {
		return nil, invalidArg("max_latency_ms: must be between 1 and 2147483647")
	}
	selected := make(map[node.Hash]struct{}, len(req.NodeHashes))
	for _, raw := range req.NodeHashes {
		h, err := node.ParseHex(strings.TrimSpace(raw))
		if err != nil {
			return nil, invalidArg("node_hashes: invalid node hash")
		}
		selected[h] = struct{}{}
	}
	cfg := config.NewDefaultRuntimeConfig()
	if s.RuntimeCfg != nil {
		if current := s.RuntimeCfg.Load(); current != nil {
			cfg = current
		}
	}
	shouldRemove := func(entry *node.NodeEntry) bool {
		if _, ok := selected[entry.Hash]; !ok {
			return false
		}
		if req.Mode == "unhealthy" {
			return node.IsFailedForCleanup(entry)
		}
		return node.ExceedsReferenceLatency(entry, cfg.LatencyAuthorities, req.MaxLatencyMS)
	}
	subIDs := make(map[string]struct{})
	for h := range selected {
		if entry, ok := s.Pool.GetEntry(h); ok {
			for _, id := range entry.SubscriptionIDs() {
				subIDs[id] = struct{}{}
			}
		}
	}
	cleaned := make(map[node.Hash]struct{})
	for id := range subIDs {
		sub := s.SubMgr.Lookup(id)
		if sub == nil {
			continue
		}
		var evicted []node.Hash
		sub.WithOpLock(func() {
			if s.SubMgr.Lookup(id) != sub {
				return
			}
			_, evicted = topology.CleanupSubscriptionNodesWithConfirmNoLock(sub, s.Pool, shouldRemove, nil)
		})
		for _, h := range evicted {
			cleaned[h] = struct{}{}
			if s.Engine != nil {
				s.Engine.MarkSubscriptionNode(id, h.Hex())
			}
		}
		s.maybeDeleteEmptySubscriptionAfterCleanup(id)
	}
	result := &NodeCleanupResult{
		CleanedCount: len(cleaned), SkippedCount: len(selected) - len(cleaned),
		CleanedNodeHashes: make([]string, 0, len(cleaned)),
	}
	for h := range cleaned {
		result.CleanedNodeHashes = append(result.CleanedNodeHashes, h.Hex())
	}
	sort.Strings(result.CleanedNodeHashes)
	return result, nil
}
