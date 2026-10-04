package state

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// persistenceCloser holds DB handles for cleanup. Implements io.Closer.
type persistenceCloser struct {
	stateDB *sql.DB
	cacheDB *sql.DB
}

func (c *persistenceCloser) Close() error {
	if c.stateDB == nil && c.cacheDB == nil {
		return nil
	}
	if c.stateDB != nil && c.cacheDB != nil && c.stateDB == c.cacheDB {
		return c.stateDB.Close()
	}
	var errs []error
	if c.stateDB != nil {
		errs = append(errs, c.stateDB.Close())
	}
	if c.cacheDB != nil {
		errs = append(errs, c.cacheDB.Close())
	}
	return errors.Join(errs...)
}

// PersistenceBootstrap initializes both databases, runs consistency repair,
// and returns a ready-to-use StateEngine plus an io.Closer for the DB handles.
//
// SQLite path mode:
//  1. Open/create state.db and cache.db with recommended pragmas.
//  2. Run schema migrations on both databases.
//  3. Run consistency repair (cross-db orphan cleanup).
//  4. Construct and return StateEngine.
func PersistenceBootstrap(stateDir, cacheDir string) (engine *StateEngine, closer io.Closer, err error) {
	return PersistenceBootstrapWithOptions(BootstrapOptions{
		Dialect:  DialectSQLite,
		StateDir: stateDir,
		CacheDir: cacheDir,
	})
}

// BootstrapOptions controls persistence bootstrap for sqlite or external DB backends.
type BootstrapOptions struct {
	Dialect      Dialect
	StateDir     string
	CacheDir     string
	DatabaseURL  string
	MaxOpenConns int
	MaxIdleConns int
}

func PersistenceBootstrapWithOptions(opts BootstrapOptions) (engine *StateEngine, closer io.Closer, err error) {
	dialect := opts.Dialect
	if dialect == "" {
		dialect = DialectSQLite
	}

	switch dialect {
	case DialectPostgres:
		return persistenceBootstrapPostgres(opts)
	case DialectSQLite:
		return persistenceBootstrapSQLite(opts.StateDir, opts.CacheDir)
	default:
		return nil, nil, fmt.Errorf("persistence bootstrap: unsupported dialect %q", dialect)
	}
}

func persistenceBootstrapSQLite(stateDir, cacheDir string) (engine *StateEngine, closer io.Closer, err error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create cache dir %s: %w", cacheDir, err)
	}

	stateDBPath := filepath.Join(stateDir, "state.db")
	cacheDBPath := filepath.Join(cacheDir, "cache.db")

	stateDB, err := OpenSQLiteDB(stateDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open state.db: %w", err)
	}

	cacheDB, err := OpenSQLiteDB(cacheDBPath)
	if err != nil {
		stateDB.Close()
		return nil, nil, fmt.Errorf("open cache.db: %w", err)
	}

	if err := MigrateStateDBWithDialect(DialectSQLite, stateDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate state.db: %w", err)
	}

	if err := MigrateCacheDBWithDialect(DialectSQLite, cacheDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate cache.db: %w", err)
	}

	if err := RepairConsistencyWithDialect(DialectSQLite, stateDBPath, stateDB, cacheDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("repair consistency: %w", err)
	}

	stateRepo := newStateRepoWithDialect(stateDB, DialectSQLite)
	cacheRepo := newCacheRepoWithDialect(cacheDB, DialectSQLite)
	engine = newStateEngine(stateRepo, cacheRepo)

	return engine, &persistenceCloser{stateDB: stateDB, cacheDB: cacheDB}, nil
}

func persistenceBootstrapPostgres(opts BootstrapOptions) (engine *StateEngine, closer io.Closer, err error) {
	if opts.DatabaseURL == "" {
		return nil, nil, fmt.Errorf("open postgres db: missing database url")
	}
	db, err := openPostgresDB(opts.DatabaseURL, opts.MaxOpenConns, opts.MaxIdleConns)
	if err != nil {
		return nil, nil, err
	}
	if err := MigrateStateDBWithDialect(DialectPostgres, db); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrate state.db: %w", err)
	}
	if err := MigrateCacheDBWithDialect(DialectPostgres, db); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrate cache.db: %w", err)
	}
	if err := RepairConsistencyWithDialect(DialectPostgres, "", db, db); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("repair consistency: %w", err)
	}
	stateRepo := newStateRepoWithDialect(db, DialectPostgres)
	cacheRepo := newCacheRepoWithDialect(db, DialectPostgres)
	engine = newStateEngine(stateRepo, cacheRepo)
	return engine, &persistenceCloser{stateDB: db, cacheDB: db}, nil
}
