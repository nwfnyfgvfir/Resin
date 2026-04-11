package service

import (
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/topology"
)

func newSubscriptionBackupTestService(t *testing.T) *ControlPlaneService {
	t.Helper()

	root := t.TempDir()
	engine, closer, err := state.PersistenceBootstrap(
		filepath.Join(root, "state"),
		filepath.Join(root, "cache"),
	)
	if err != nil {
		t.Fatalf("PersistenceBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})

	return &ControlPlaneService{
		Engine: engine,
		Pool:   pool,
		SubMgr: subMgr,
	}
}

func TestExportSubscriptions_PortableBackupDocument(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	remoteName := "Banana Feed"
	remoteURL := "https://example.com/banana"
	remoteInterval := "10m"
	remoteCreated, err := cp.CreateSubscription(CreateSubscriptionRequest{
		Name:           &remoteName,
		URL:            &remoteURL,
		UpdateInterval: &remoteInterval,
	})
	if err != nil {
		t.Fatalf("CreateSubscription(remote): %v", err)
	}

	localName := "Apple Feed"
	localSourceType := subscription.SourceTypeLocal
	localContent := "vmess://example"
	localEnabled := false
	localEphemeral := true
	localEvictDelay := "6h"
	_, err = cp.CreateSubscription(CreateSubscriptionRequest{
		Name:                    &localName,
		SourceType:              &localSourceType,
		Content:                 &localContent,
		Enabled:                 &localEnabled,
		Ephemeral:               &localEphemeral,
		EphemeralNodeEvictDelay: &localEvictDelay,
	})
	if err != nil {
		t.Fatalf("CreateSubscription(local): %v", err)
	}

	doc, err := cp.ExportSubscriptions()
	if err != nil {
		t.Fatalf("ExportSubscriptions: %v", err)
	}
	if doc.Version != subscriptionBackupVersion {
		t.Fatalf("version = %d, want %d", doc.Version, subscriptionBackupVersion)
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
	if apple.SourceType != subscription.SourceTypeLocal {
		t.Fatalf("apple source_type = %q, want %q", apple.SourceType, subscription.SourceTypeLocal)
	}
	if apple.Content != localContent {
		t.Fatalf("apple content = %q, want %q", apple.Content, localContent)
	}
	if apple.URL != "" {
		t.Fatalf("apple url = %q, want empty", apple.URL)
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
	if apple.UpdateInterval != "5m0s" {
		t.Fatalf("apple update_interval = %q, want %q", apple.UpdateInterval, "5m0s")
	}

	if banana.Name != remoteCreated.Name {
		t.Fatalf("second backup item name = %q, want %q", banana.Name, remoteCreated.Name)
	}
	if banana.SourceType != subscription.SourceTypeRemote {
		t.Fatalf("banana source_type = %q, want %q", banana.SourceType, subscription.SourceTypeRemote)
	}
	if banana.URL != remoteURL {
		t.Fatalf("banana url = %q, want %q", banana.URL, remoteURL)
	}
	if banana.Content != "" {
		t.Fatalf("banana content = %q, want empty", banana.Content)
	}
	if banana.UpdateInterval != "10m0s" {
		t.Fatalf("banana update_interval = %q, want %q", banana.UpdateInterval, "10m0s")
	}
}

func TestImportSubscriptions_ReplacesExistingSubscriptions(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	legacyName := "Legacy Feed"
	legacyURL := "https://example.com/legacy"
	legacy, err := cp.CreateSubscription(CreateSubscriptionRequest{
		Name: &legacyName,
		URL:  &legacyURL,
	})
	if err != nil {
		t.Fatalf("CreateSubscription(legacy): %v", err)
	}

	doc := SubscriptionBackupFile{
		Version:    subscriptionBackupVersion,
		ExportedAt: "2026-04-11T00:00:00Z",
		Subscriptions: []SubscriptionBackupItem{
			{
				Name:                    "Alpha Feed",
				SourceType:              subscription.SourceTypeRemote,
				URL:                     "https://example.com/alpha",
				UpdateInterval:          "15m",
				Enabled:                 true,
				Ephemeral:               false,
				EphemeralNodeEvictDelay: "72h",
			},
			{
				Name:                    "Beta Feed",
				SourceType:              subscription.SourceTypeLocal,
				Content:                 "ss://example",
				UpdateInterval:          "45m",
				Enabled:                 false,
				Ephemeral:               true,
				EphemeralNodeEvictDelay: "2h",
			},
		},
	}

	imported, err := cp.ImportSubscriptions(doc)
	if err != nil {
		t.Fatalf("ImportSubscriptions: %v", err)
	}
	if len(imported.Subscriptions) != 2 {
		t.Fatalf("imported subscriptions len = %d, want 2", len(imported.Subscriptions))
	}

	runtimeSubs, err := cp.ListSubscriptions(nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(runtimeSubs) != 2 {
		t.Fatalf("runtime subscriptions len = %d, want 2", len(runtimeSubs))
	}

	persistedSubs, err := cp.Engine.ListSubscriptions()
	if err != nil {
		t.Fatalf("Engine.ListSubscriptions: %v", err)
	}
	if len(persistedSubs) != 2 {
		t.Fatalf("persisted subscriptions len = %d, want 2", len(persistedSubs))
	}

	runtimeByName := make(map[string]SubscriptionResponse, len(runtimeSubs))
	var runtimeNames []string
	for _, sub := range runtimeSubs {
		runtimeByName[sub.Name] = sub
		runtimeNames = append(runtimeNames, sub.Name)
		if sub.ID == legacy.ID {
			t.Fatalf("imported subscription reused legacy id %q", legacy.ID)
		}
	}
	sort.Strings(runtimeNames)
	if strings.Join(runtimeNames, ",") != "Alpha Feed,Beta Feed" {
		t.Fatalf("runtime names = %q, want %q", strings.Join(runtimeNames, ","), "Alpha Feed,Beta Feed")
	}
	if _, ok := runtimeByName[legacy.Name]; ok {
		t.Fatalf("legacy subscription %q should be removed", legacy.Name)
	}
	if cp.SubMgr.Lookup(legacy.ID) != nil {
		t.Fatalf("legacy subscription id %q should be unregistered", legacy.ID)
	}

	alpha := runtimeByName["Alpha Feed"]
	if alpha.SourceType != subscription.SourceTypeRemote {
		t.Fatalf("alpha source_type = %q, want %q", alpha.SourceType, subscription.SourceTypeRemote)
	}
	if alpha.URL != "https://example.com/alpha" {
		t.Fatalf("alpha url = %q, want %q", alpha.URL, "https://example.com/alpha")
	}
	if alpha.Enabled != true {
		t.Fatalf("alpha enabled = %v, want true", alpha.Enabled)
	}
	if alpha.Ephemeral != false {
		t.Fatalf("alpha ephemeral = %v, want false", alpha.Ephemeral)
	}
	if alpha.UpdateInterval != "15m0s" {
		t.Fatalf("alpha update_interval = %q, want %q", alpha.UpdateInterval, "15m0s")
	}

	beta := runtimeByName["Beta Feed"]
	if beta.SourceType != subscription.SourceTypeLocal {
		t.Fatalf("beta source_type = %q, want %q", beta.SourceType, subscription.SourceTypeLocal)
	}
	if beta.Content != "ss://example" {
		t.Fatalf("beta content = %q, want %q", beta.Content, "ss://example")
	}
	if beta.Enabled != false {
		t.Fatalf("beta enabled = %v, want false", beta.Enabled)
	}
	if beta.Ephemeral != true {
		t.Fatalf("beta ephemeral = %v, want true", beta.Ephemeral)
	}
	if beta.EphemeralNodeEvictDelay != "2h0m0s" {
		t.Fatalf("beta ephemeral_node_evict_delay = %q, want %q", beta.EphemeralNodeEvictDelay, "2h0m0s")
	}

	persistedNames := make([]string, 0, len(persistedSubs))
	for _, sub := range persistedSubs {
		persistedNames = append(persistedNames, sub.Name)
		if sub.ID == legacy.ID {
			t.Fatalf("persisted subscriptions still contain legacy id %q", legacy.ID)
		}
	}
	sort.Strings(persistedNames)
	if strings.Join(persistedNames, ",") != "Alpha Feed,Beta Feed" {
		t.Fatalf("persisted names = %q, want %q", strings.Join(persistedNames, ","), "Alpha Feed,Beta Feed")
	}
}

func TestImportSubscriptions_CreateFailureRollsBackNewSubscriptions(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	legacyName := "Legacy Feed"
	legacyURL := "https://example.com/legacy"
	legacy, err := cp.CreateSubscription(CreateSubscriptionRequest{
		Name: &legacyName,
		URL:  &legacyURL,
	})
	if err != nil {
		t.Fatalf("CreateSubscription(legacy): %v", err)
	}

	_, err = cp.ImportSubscriptions(SubscriptionBackupFile{
		Version:    subscriptionBackupVersion,
		ExportedAt: "2026-04-11T00:00:00Z",
		Subscriptions: []SubscriptionBackupItem{
			{
				Name:                    "Alpha Feed",
				SourceType:              subscription.SourceTypeRemote,
				URL:                     "https://example.com/alpha",
				UpdateInterval:          "10m",
				Enabled:                 true,
				Ephemeral:               false,
				EphemeralNodeEvictDelay: "72h",
			},
			{
				Name:                    "Broken Feed",
				SourceType:              subscription.SourceTypeRemote,
				URL:                     "ftp://example.com/not-http",
				UpdateInterval:          "10m",
				Enabled:                 true,
				Ephemeral:               false,
				EphemeralNodeEvictDelay: "72h",
			},
		},
	})
	if err == nil {
		t.Fatal("expected ImportSubscriptions to fail")
	}
	serviceErr, ok := err.(*ServiceError)
	if !ok {
		t.Fatalf("expected ServiceError, got %T", err)
	}
	if serviceErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("service error code = %q, want %q", serviceErr.Code, "INVALID_ARGUMENT")
	}
	if !strings.Contains(serviceErr.Message, "subscriptions[1]:") {
		t.Fatalf("service error message = %q, want indexed prefix", serviceErr.Message)
	}

	runtimeSubs, err := cp.ListSubscriptions(nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(runtimeSubs) != 1 {
		t.Fatalf("runtime subscriptions len = %d, want 1", len(runtimeSubs))
	}
	if runtimeSubs[0].ID != legacy.ID {
		t.Fatalf("legacy runtime id = %q, want %q", runtimeSubs[0].ID, legacy.ID)
	}
	if runtimeSubs[0].Name != legacy.Name {
		t.Fatalf("legacy runtime name = %q, want %q", runtimeSubs[0].Name, legacy.Name)
	}

	persistedSubs, err := cp.Engine.ListSubscriptions()
	if err != nil {
		t.Fatalf("Engine.ListSubscriptions: %v", err)
	}
	if len(persistedSubs) != 1 {
		t.Fatalf("persisted subscriptions len = %d, want 1", len(persistedSubs))
	}
	if persistedSubs[0].ID != legacy.ID {
		t.Fatalf("persisted legacy id = %q, want %q", persistedSubs[0].ID, legacy.ID)
	}
}

func TestImportSubscriptions_RejectsUnsupportedVersion(t *testing.T) {
	cp := newSubscriptionBackupTestService(t)

	_, err := cp.ImportSubscriptions(SubscriptionBackupFile{
		Version:       999,
		ExportedAt:    "2026-04-11T00:00:00Z",
		Subscriptions: []SubscriptionBackupItem{},
	})
	if err == nil {
		t.Fatal("expected ImportSubscriptions to reject unsupported version")
	}
	serviceErr, ok := err.(*ServiceError)
	if !ok {
		t.Fatalf("expected ServiceError, got %T", err)
	}
	if serviceErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("service error code = %q, want %q", serviceErr.Code, "INVALID_ARGUMENT")
	}
}
