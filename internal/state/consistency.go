package state

import (
	"database/sql"
	"fmt"
)

// RepairConsistency runs orphan-cleanup SQL on cache.db, cross-referencing
// state.db via ATTACH. All DELETEs execute in a single transaction to avoid
// half-repaired state on crash.
//
// Cleanup order (by dependency):
//  1. subscription_nodes: remove entries whose subscription_id is missing from
//     state.subscriptions OR (for non-evicted rows) whose node_hash is missing from nodes_static.
//  2. nodes_static: remove entries with no remaining non-evicted reference in subscription_nodes.
//  3. nodes_dynamic: remove entries whose hash is missing from nodes_static.
//  4. node_latency: remove entries whose node_hash is missing from nodes_static.
//  5. leases: remove entries whose platform_id is missing from state.platforms
//     OR whose node_hash is missing from nodes_static.
func RepairConsistency(stateDBPath string, cacheDB *sql.DB) error {
	return RepairConsistencyWithDialect(DialectSQLite, stateDBPath, nil, cacheDB)
}

func RepairConsistencyWithDialect(dialect Dialect, stateDBPath string, stateDB *sql.DB, cacheDB *sql.DB) error {
	switch dialect {
	case DialectPostgres:
		return repairConsistencyPostgres(stateDB, cacheDB)
	case DialectSQLite:
		return repairConsistencySQLite(stateDBPath, cacheDB)
	default:
		return fmt.Errorf("repair consistency: unsupported dialect %q", dialect)
	}
}

func repairConsistencySQLite(stateDBPath string, cacheDB *sql.DB) error {
	attachSQL := fmt.Sprintf("ATTACH DATABASE %q AS state_db", stateDBPath)
	if _, err := cacheDB.Exec(attachSQL); err != nil {
		return fmt.Errorf("attach state_db: %w", err)
	}
	defer cacheDB.Exec("DETACH DATABASE state_db")
	return runConsistencyStatements(cacheDB, []string{
		`DELETE FROM subscription_nodes
		 WHERE subscription_id NOT IN (SELECT id FROM state_db.subscriptions)
		    OR (evicted = 0 AND node_hash NOT IN (SELECT hash FROM nodes_static))`,
		`DELETE FROM nodes_static
		 WHERE hash NOT IN (SELECT node_hash FROM subscription_nodes WHERE evicted = 0)`,
		`DELETE FROM nodes_dynamic
		 WHERE hash NOT IN (SELECT hash FROM nodes_static)`,
		`DELETE FROM node_latency
		 WHERE node_hash NOT IN (SELECT hash FROM nodes_static)`,
		`DELETE FROM leases
		 WHERE platform_id NOT IN (SELECT id FROM state_db.platforms)
		    OR node_hash NOT IN (SELECT hash FROM nodes_static)`,
	})
}

func repairConsistencyPostgres(stateDB, cacheDB *sql.DB) error {
	if stateDB == nil || cacheDB == nil {
		return fmt.Errorf("repair consistency postgres: nil db")
	}
	if stateDB != cacheDB {
		return fmt.Errorf("repair consistency postgres: state and cache DB must share the same connection")
	}
	return runConsistencyStatements(cacheDB, []string{
		`DELETE FROM subscription_nodes
		 WHERE subscription_id NOT IN (SELECT id FROM subscriptions)
		    OR (evicted = FALSE AND node_hash NOT IN (SELECT hash FROM nodes_static))`,
		`DELETE FROM nodes_static
		 WHERE hash NOT IN (SELECT node_hash FROM subscription_nodes WHERE evicted = FALSE)`,
		`DELETE FROM nodes_dynamic
		 WHERE hash NOT IN (SELECT hash FROM nodes_static)`,
		`DELETE FROM node_latency
		 WHERE node_hash NOT IN (SELECT hash FROM nodes_static)`,
		`DELETE FROM leases
		 WHERE platform_id NOT IN (SELECT id FROM platforms)
		    OR node_hash NOT IN (SELECT hash FROM nodes_static)`,
	})
}

func runConsistencyStatements(db *sql.DB, stmts []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin repair tx: %w", err)
	}
	defer tx.Rollback()
	for i, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("repair step %d: %w", i+1, err)
		}
	}
	return tx.Commit()
}
