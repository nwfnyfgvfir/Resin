package api

import (
	"net/http"
	"testing"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
)

func TestHandleCleanupNodes_AuthValidationAndSelectedScope(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	sub := subscription.NewSubscription("cleanup", "cleanup", "https://example.com/sub", true, false)
	cp.SubMgr.Register(sub)
	const selected = `{"type":"http","server":"selected.example","server_port":8080}`
	const unselected = `{"type":"http","server":"unselected.example","server_port":8080}`
	addNodeForNodeListTest(t, cp, sub, selected, "")
	addNodeForNodeListTest(t, cp, sub, unselected, "")
	h := node.HashFromRawOptions([]byte(selected))
	entry, _ := cp.Pool.GetEntry(h)
	entry.FailureCount.Store(3)
	body := map[string]any{"node_hashes": []string{h.Hex()}, "mode": "unhealthy"}
	if rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes:cleanup", body, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cleanup status: %d", rec.Code)
	}
	for _, invalidBody := range []string{
		`{"node_hashes":[],"mode":"unhealthy"}`,
		`{"node_hashes":["invalid"],"mode":"unhealthy"}`,
		`{"node_hashes":["00000000000000000000000000000000"],"mode":"high_latency","max_latency_ms":0}`,
		`{"node_hashes":["00000000000000000000000000000000"],"mode":"high_latency","max_latency_ms":1.5}`,
		`{"node_hashes":["00000000000000000000000000000000"],"mode":"all"}`,
	} {
		rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes:cleanup", invalidBody, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid request status: %d, %s", rec.Code, rec.Body.String())
		}
		if cp.Pool.Size() != 2 {
			t.Fatal("invalid request removed nodes")
		}
	}
	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes:cleanup", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup failed: %d, %s", rec.Code, rec.Body.String())
	}
	if result := decodeJSONMap(t, rec); result["cleaned_count"] != float64(1) || result["skipped_count"] != float64(0) {
		t.Fatalf("unexpected result: %s", rec.Body.String())
	}
	if cp.Pool.Size() != 1 {
		t.Fatal("cleanup removed unselected node")
	}
	managed, ok := sub.ManagedNodes().LoadNode(h)
	if !ok || !managed.Evicted {
		t.Fatal("eviction marker missing")
	}
}

func TestSystemConfig_HighLatencyCleanupValidation(t *testing.T) {
	srv, _, runtimeCfg := newControlPlaneTestServer(t)
	for _, threshold := range []any{0, -1, 2147483648, 1.5, nil} {
		rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/system/config", map[string]any{
			"auto_remove_high_latency_nodes_enabled":      true,
			"auto_remove_high_latency_nodes_threshold_ms": threshold,
		}, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("accepted threshold %v: %d", threshold, rec.Code)
		}
		if runtimeCfg.Load().AutoRemoveHighLatencyNodesEnabled {
			t.Fatal("invalid patch partially applied")
		}
	}
	rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/system/config", map[string]any{
		"auto_remove_high_latency_nodes_enabled":      true,
		"auto_remove_high_latency_nodes_threshold_ms": 750,
	}, true)
	if rec.Code != http.StatusOK || !runtimeCfg.Load().AutoRemoveHighLatencyNodesEnabled || runtimeCfg.Load().AutoRemoveHighLatencyNodesThresholdMS != 750 {
		t.Fatalf("valid latency cleanup patch failed: %d %s", rec.Code, rec.Body.String())
	}
}
