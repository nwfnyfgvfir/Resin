package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/subscription"
)

// TestCreateSubscription_IncrementalAliveNodesDefaultsToTrue pins the default
// for the incremental alive-node mode: a subscription created without an
// explicit value keeps its already-known healthy nodes across refreshes.
func TestCreateSubscription_IncrementalAliveNodesDefaultsToTrue(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	name := "default-incremental"
	url := "https://example.com/sub"
	interval := "10m"

	sub, err := cp.CreateSubscription(CreateSubscriptionRequest{
		Name:           &name,
		URL:            &url,
		UpdateInterval: &interval,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if !sub.IncrementalAliveNodes {
		t.Fatal("incremental_alive_nodes must default to true when omitted")
	}
}

// TestCreateSubscription_IncrementalAliveNodesExplicitFalseWins makes sure the
// new default does not make the option un-settable.
func TestCreateSubscription_IncrementalAliveNodesExplicitFalseWins(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	name := "explicit-off"
	url := "https://example.com/sub"
	interval := "10m"
	off := false

	sub, err := cp.CreateSubscription(CreateSubscriptionRequest{
		Name:                  &name,
		URL:                   &url,
		UpdateInterval:        &interval,
		IncrementalAliveNodes: &off,
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.IncrementalAliveNodes {
		t.Fatal("explicit incremental_alive_nodes=false must be honored")
	}
}

// TestSubscriptionBackupItem_PreservesIncrementalAliveNodes covers a data-loss
// bug: the field was absent from SubscriptionBackupItem, so exporting and
// re-importing a backup silently reset it to the default.
func TestSubscriptionBackupItem_PreservesIncrementalAliveNodes(t *testing.T) {
	for _, want := range []bool{true, false} {
		response := SubscriptionResponse{
			Name:                  "round-trip",
			SourceType:            subscription.SourceTypeRemote,
			URL:                   "https://example.com/sub",
			UpdateInterval:        "10m",
			Enabled:               true,
			IncrementalAliveNodes: want,
		}

		item := backupItemFromResponse(response)
		if item.IncrementalAliveNodes == nil {
			t.Fatalf("backup item lost incremental_alive_nodes (want %v)", want)
		}
		if *item.IncrementalAliveNodes != want {
			t.Fatalf("backup item incremental_alive_nodes: got %v want %v", *item.IncrementalAliveNodes, want)
		}

		req, verr := createSubscriptionRequestFromBackupItem(item)
		if verr != nil {
			t.Fatalf("createSubscriptionRequestFromBackupItem: %v", verr)
		}
		if req.IncrementalAliveNodes == nil {
			t.Fatalf("restored request lost incremental_alive_nodes (want %v)", want)
		}
		if *req.IncrementalAliveNodes != want {
			t.Fatalf("restored incremental_alive_nodes: got %v want %v", *req.IncrementalAliveNodes, want)
		}
	}
}

// TestSubscriptionBackupItem_LegacyBackupUsesDefault verifies backward
// compatibility: backups written before the field existed must leave it nil so
// CreateSubscription applies its current default instead of forcing false.
func TestSubscriptionBackupItem_LegacyBackupUsesDefault(t *testing.T) {
	legacy := SubscriptionBackupItem{
		Name:           "legacy",
		SourceType:     subscription.SourceTypeRemote,
		URL:            "https://example.com/sub",
		UpdateInterval: "10m",
		Enabled:        true,
	}

	req, verr := createSubscriptionRequestFromBackupItem(legacy)
	if verr != nil {
		t.Fatalf("createSubscriptionRequestFromBackupItem: %v", verr)
	}
	if req.IncrementalAliveNodes != nil {
		t.Fatalf("legacy backup must leave the field nil, got %v", *req.IncrementalAliveNodes)
	}
}

// TestSubscriptionBackupItem_JSONKeepsExplicitFalse guards the `omitempty`
// interaction: a pointer to false must still be serialized, otherwise an
// explicit "off" would be indistinguishable from "absent" on re-import.
func TestSubscriptionBackupItem_JSONKeepsExplicitFalse(t *testing.T) {
	off := false
	item := SubscriptionBackupItem{
		Name:                  "explicit-off",
		SourceType:            subscription.SourceTypeRemote,
		URL:                   "https://example.com/sub",
		UpdateInterval:        "10m",
		Enabled:               true,
		IncrementalAliveNodes: &off,
	}

	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal backup item: %v", err)
	}
	if !strings.Contains(string(raw), `"incremental_alive_nodes":false`) {
		t.Fatalf("explicit false must survive serialization, got: %s", raw)
	}

	// A legacy document without the key must decode to nil.
	var decoded SubscriptionBackupItem
	if err := json.Unmarshal([]byte(`{"name":"legacy","source_type":"remote","url":"https://example.com/sub","update_interval":"10m","enabled":true}`), &decoded); err != nil {
		t.Fatalf("unmarshal legacy backup item: %v", err)
	}
	if decoded.IncrementalAliveNodes != nil {
		t.Fatalf("missing key must decode to nil, got %v", *decoded.IncrementalAliveNodes)
	}
}
