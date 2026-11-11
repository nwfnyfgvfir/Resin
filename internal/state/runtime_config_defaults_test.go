package state

import (
	"encoding/json"
	"testing"

	"github.com/Resinat/Resin/internal/config"
)

func TestStateRepo_LegacyRuntimeConfigGetsNewDefaults(t *testing.T) {
	repo := newTestStateRepo(t)
	cfg := config.NewDefaultRuntimeConfig()
	cfg.RequestLogEnabled = false
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "auto_remove_high_latency_nodes_enabled")
	delete(legacy, "auto_remove_high_latency_nodes_threshold_ms")
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.exec(`INSERT INTO system_config (id, config_json, version, updated_at_ns) VALUES (1, ?, 1, 0)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := repo.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AutoRemoveHighLatencyNodesEnabled || loaded.AutoRemoveHighLatencyNodesThresholdMS != 1000 {
		t.Fatalf("new settings missing defaults: %+v", loaded)
	}
	if loaded.RequestLogEnabled {
		t.Fatal("explicit legacy value was overwritten by defaults")
	}
}
