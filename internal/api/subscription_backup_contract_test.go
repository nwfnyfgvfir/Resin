package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/service"
)

func TestAPIContract_SubscriptionBackupExport(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	remoteRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":            "Banana Feed",
		"url":             "https://example.com/banana",
		"update_interval": "10m",
	}, true)
	if remoteRec.Code != http.StatusCreated {
		t.Fatalf("create remote subscription status: got %d, want %d, body=%s", remoteRec.Code, http.StatusCreated, remoteRec.Body.String())
	}

	localRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":                        "Apple Feed",
		"source_type":                 "local",
		"content":                     "vmess://example",
		"update_interval":             "1h",
		"enabled":                     false,
		"ephemeral":                   true,
		"ephemeral_node_evict_delay":  "6h",
	}, true)
	if localRec.Code != http.StatusCreated {
		t.Fatalf("create local subscription status: got %d, want %d, body=%s", localRec.Code, http.StatusCreated, localRec.Body.String())
	}

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions:export", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("export subscriptions status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="resin-subscriptions-backup.json"` {
		t.Fatalf("content-disposition: got %q, want %q", got, `attachment; filename="resin-subscriptions-backup.json"`)
	}

	var doc service.SubscriptionBackupFile
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal export body: %v body=%s", err, rec.Body.String())
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if _, err := time.Parse(time.RFC3339Nano, doc.ExportedAt); err != nil {
		t.Fatalf("exported_at parse error: %v (value=%q)", err, doc.ExportedAt)
	}
	if len(doc.Subscriptions) != 2 {
		t.Fatalf("subscriptions len = %d, want 2", len(doc.Subscriptions))
	}

	apple := doc.Subscriptions[0]
	banana := doc.Subscriptions[1]
	if apple.Name != "Apple Feed" {
		t.Fatalf("first backup item name = %q, want %q", apple.Name, "Apple Feed")
	}
	if apple.SourceType != "local" {
		t.Fatalf("apple source_type = %q, want %q", apple.SourceType, "local")
	}
	if apple.Content != "vmess://example" {
		t.Fatalf("apple content = %q, want %q", apple.Content, "vmess://example")
	}
	if apple.URL != "" {
		t.Fatalf("apple url = %q, want empty", apple.URL)
	}
	if apple.UpdateInterval != "1h0m0s" {
		t.Fatalf("apple update_interval = %q, want %q", apple.UpdateInterval, "1h0m0s")
	}
	if apple.Enabled != false {
		t.Fatalf("apple enabled = %v, want false", apple.Enabled)
	}
	if apple.Ephemeral != true {
		t.Fatalf("apple ephemeral = %v, want true", apple.Ephemeral)
	}
	if apple.EphemeralNodeEvictDelay != "6h0m0s" {
		t.Fatalf("apple ephemeral_node_evict_delay = %q, want %q", apple.EphemeralNodeEvictDelay, "6h0m0s")
	}

	if banana.Name != "Banana Feed" {
		t.Fatalf("second backup item name = %q, want %q", banana.Name, "Banana Feed")
	}
	if banana.SourceType != "remote" {
		t.Fatalf("banana source_type = %q, want %q", banana.SourceType, "remote")
	}
	if banana.URL != "https://example.com/banana" {
		t.Fatalf("banana url = %q, want %q", banana.URL, "https://example.com/banana")
	}
	if banana.Content != "" {
		t.Fatalf("banana content = %q, want empty", banana.Content)
	}
	if banana.UpdateInterval != "10m0s" {
		t.Fatalf("banana update_interval = %q, want %q", banana.UpdateInterval, "10m0s")
	}
}

func TestAPIContract_SubscriptionBackupImport(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	legacyRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name": "Legacy Feed",
		"url":  "https://example.com/legacy",
	}, true)
	if legacyRec.Code != http.StatusCreated {
		t.Fatalf("create legacy subscription status: got %d, want %d, body=%s", legacyRec.Code, http.StatusCreated, legacyRec.Body.String())
	}
	legacyBody := decodeJSONMap(t, legacyRec)
	legacyID, _ := legacyBody["id"].(string)
	if legacyID == "" {
		t.Fatalf("legacy subscription missing id: body=%s", legacyRec.Body.String())
	}

	importRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions:import", service.SubscriptionBackupFile{
		Version:    1,
		ExportedAt: "2026-04-11T00:00:00Z",
		Subscriptions: []service.SubscriptionBackupItem{
			{
				Name:                    "Alpha Feed",
				SourceType:              "remote",
				URL:                     "https://example.com/alpha",
				UpdateInterval:          "15m",
				Enabled:                 true,
				Ephemeral:               false,
				EphemeralNodeEvictDelay: "72h",
			},
			{
				Name:                    "Beta Feed",
				SourceType:              "local",
				Content:                 "ss://example",
				UpdateInterval:          "45m",
				Enabled:                 false,
				Ephemeral:               true,
				EphemeralNodeEvictDelay: "2h",
			},
		},
	}, true)
	if importRec.Code != http.StatusOK {
		t.Fatalf("import subscriptions status: got %d, want %d, body=%s", importRec.Code, http.StatusOK, importRec.Body.String())
	}

	var imported service.SubscriptionBackupFile
	if err := json.Unmarshal(importRec.Body.Bytes(), &imported); err != nil {
		t.Fatalf("unmarshal import body: %v body=%s", err, importRec.Body.String())
	}
	if len(imported.Subscriptions) != 2 {
		t.Fatalf("imported subscriptions len = %d, want 2", len(imported.Subscriptions))
	}

	legacyGetRec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/"+legacyID, nil, true)
	if legacyGetRec.Code != http.StatusNotFound {
		t.Fatalf("legacy subscription should be removed, got status %d body=%s", legacyGetRec.Code, legacyGetRec.Body.String())
	}
	assertErrorCode(t, legacyGetRec, "NOT_FOUND")

	listRec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions?sort_by=name&sort_order=asc", nil, true)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list subscriptions after import status: got %d, want %d, body=%s", listRec.Code, http.StatusOK, listRec.Body.String())
	}
	listBody := decodeJSONMap(t, listRec)
	items, ok := listBody["items"].([]any)
	if !ok {
		t.Fatalf("items type: got %T", listBody["items"])
	}
	if len(items) != 2 {
		t.Fatalf("imported items len = %d, want 2, body=%s", len(items), listRec.Body.String())
	}

	alpha, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("alpha item type: got %T", items[0])
	}
	beta, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("beta item type: got %T", items[1])
	}
	if alpha["name"] != "Alpha Feed" {
		t.Fatalf("alpha name: got %v, want %q", alpha["name"], "Alpha Feed")
	}
	if alpha["source_type"] != "remote" {
		t.Fatalf("alpha source_type: got %v, want %q", alpha["source_type"], "remote")
	}
	if beta["name"] != "Beta Feed" {
		t.Fatalf("beta name: got %v, want %q", beta["name"], "Beta Feed")
	}
	if beta["source_type"] != "local" {
		t.Fatalf("beta source_type: got %v, want %q", beta["source_type"], "local")
	}
	if beta["enabled"] != false {
		t.Fatalf("beta enabled: got %v, want false", beta["enabled"])
	}
}

func TestAPIContract_SubscriptionBackupImportRejectsInvalidVersion(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions:import", map[string]any{
		"version":       999,
		"exported_at":   "2026-04-11T00:00:00Z",
		"subscriptions": []any{},
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid import status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}
