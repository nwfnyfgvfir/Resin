package state

import (
	"testing"

	"github.com/Resinat/Resin/internal/config"
)

func TestPersistenceBootstrapWithOptions_Postgres(t *testing.T) {
	dsn := "postgres://resin:resin@127.0.0.1:55432/resin?sslmode=disable"
	engine, closer, err := PersistenceBootstrapWithOptions(BootstrapOptions{
		Dialect:      DialectPostgres,
		DatabaseURL:  dsn,
		MaxOpenConns: 4,
		MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("PersistenceBootstrapWithOptions(postgres): %v", err)
	}
	defer closer.Close()

	if err := engine.SaveSystemConfig(config.NewDefaultRuntimeConfig(), 1, 123); err != nil {
		t.Fatalf("SaveSystemConfig: %v", err)
	}

	cfg, version, err := engine.GetSystemConfig()
	if err != nil {
		t.Fatalf("GetSystemConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config")
	}
	if version != 1 {
		t.Fatalf("version: got %d want 1", version)
	}
}
