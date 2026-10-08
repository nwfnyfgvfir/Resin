package state

import (
	"database/sql"
	"fmt"
	"strings"
)

// postgresStateDDL is the current PostgreSQL state schema.
//
// It must stay in sync with the final shape produced by the SQLite migrations
// under migrations/state/. TestPostgresDDLMatchesSQLiteSchema guards this.
//
// IMPORTANT: this DDL is applied with CREATE TABLE IF NOT EXISTS, so it can
// only ever create missing tables. Columns added to an already-deployed
// database must also be listed in postgresStateColumnAdditions below,
// otherwise existing installs will keep running with the old schema.
const postgresStateDDL = `
CREATE TABLE IF NOT EXISTS system_config (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	config_json   TEXT    NOT NULL,
	version       INTEGER NOT NULL,
	updated_at_ns BIGINT  NOT NULL
);

CREATE TABLE IF NOT EXISTS platforms (
	id                                  TEXT PRIMARY KEY,
	name                                TEXT NOT NULL UNIQUE,
	sticky_ttl_ns                       BIGINT NOT NULL,
	regex_filters_json                  TEXT NOT NULL DEFAULT '[]',
	region_filters_json                 TEXT NOT NULL DEFAULT '[]',
	reverse_proxy_miss_action           TEXT NOT NULL DEFAULT 'TREAT_AS_EMPTY',
	reverse_proxy_empty_account_behavior TEXT NOT NULL DEFAULT 'RANDOM',
	reverse_proxy_fixed_account_header  TEXT NOT NULL DEFAULT '',
	allocation_policy                   TEXT NOT NULL DEFAULT 'BALANCED',
	passive_circuit_breaker_disabled    INTEGER NOT NULL DEFAULT 0,
	updated_at_ns                       BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS subscriptions (
	id                             TEXT PRIMARY KEY,
	name                           TEXT NOT NULL,
	source_type                    TEXT NOT NULL DEFAULT 'remote',
	url                            TEXT NOT NULL,
	content                        TEXT NOT NULL DEFAULT '',
	update_interval_ns             BIGINT NOT NULL,
	enabled                        BOOLEAN NOT NULL DEFAULT TRUE,
	ephemeral                      BOOLEAN NOT NULL DEFAULT FALSE,
	incremental_alive_nodes        BOOLEAN NOT NULL DEFAULT FALSE,
	ephemeral_node_evict_delay_ns  BIGINT NOT NULL,
	created_at_ns                  BIGINT NOT NULL,
	updated_at_ns                  BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS endpoints (
	id                      TEXT PRIMARY KEY,
	port                    INTEGER NOT NULL UNIQUE CHECK (port BETWEEN 1 AND 65535),
	enabled                 INTEGER NOT NULL DEFAULT 1,
	allow_management        INTEGER NOT NULL,
	allow_proxy             INTEGER NOT NULL,
	require_proxy_auth_info INTEGER NOT NULL DEFAULT 0,
	allow_http_forward      INTEGER NOT NULL,
	allow_http_reverse      INTEGER NOT NULL,
	allow_socks5            INTEGER NOT NULL,
	created_at_ns           BIGINT NOT NULL,
	updated_at_ns           BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_header_rules (
	url_prefix    TEXT PRIMARY KEY,
	headers_json  TEXT NOT NULL,
	updated_at_ns BIGINT NOT NULL
);
`

// postgresStateColumnAdditions lists columns that were introduced after the
// initial PostgreSQL schema shipped. Each statement is applied with
// ADD COLUMN IF NOT EXISTS so pre-existing databases converge on the current
// schema while fresh databases (already covered by postgresStateDDL) treat it
// as a no-op.
var postgresStateColumnAdditions = []postgresColumnAddition{
	{table: "subscriptions", columnDDL: "incremental_alive_nodes BOOLEAN NOT NULL DEFAULT FALSE"},
	{table: "platforms", columnDDL: "passive_circuit_breaker_disabled INTEGER NOT NULL DEFAULT 0"},
}

const postgresCacheDDL = `
CREATE TABLE IF NOT EXISTS nodes_static (
	hash             TEXT PRIMARY KEY,
	raw_options_json TEXT NOT NULL,
	created_at_ns    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes_dynamic (
	hash                                 TEXT PRIMARY KEY,
	failure_count                        INTEGER NOT NULL DEFAULT 0,
	circuit_open_since                   BIGINT NOT NULL DEFAULT 0,
	egress_ip                            TEXT NOT NULL DEFAULT '',
	egress_region                        TEXT NOT NULL DEFAULT '',
	egress_updated_at_ns                 BIGINT NOT NULL DEFAULT 0,
	last_latency_probe_attempt_ns        BIGINT NOT NULL DEFAULT 0,
	last_authority_latency_probe_attempt_ns BIGINT NOT NULL DEFAULT 0,
	last_egress_update_attempt_ns        BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS node_latency (
	node_hash       TEXT NOT NULL,
	domain          TEXT NOT NULL,
	ewma_ns         BIGINT NOT NULL,
	last_updated_ns BIGINT NOT NULL,
	PRIMARY KEY (node_hash, domain)
);

CREATE TABLE IF NOT EXISTS leases (
	platform_id      TEXT NOT NULL,
	account          TEXT NOT NULL,
	node_hash        TEXT NOT NULL,
	egress_ip        TEXT NOT NULL DEFAULT '',
	created_at_ns    BIGINT NOT NULL DEFAULT 0,
	expiry_ns        BIGINT NOT NULL,
	last_accessed_ns BIGINT NOT NULL,
	PRIMARY KEY (platform_id, account)
);

CREATE TABLE IF NOT EXISTS subscription_nodes (
	subscription_id TEXT NOT NULL,
	node_hash       TEXT NOT NULL,
	tags_json       TEXT NOT NULL DEFAULT '[]',
	evicted         BOOLEAN NOT NULL DEFAULT FALSE,
	PRIMARY KEY (subscription_id, node_hash)
);
`

// postgresCacheColumnAdditions mirrors postgresStateColumnAdditions for cache.
var postgresCacheColumnAdditions []postgresColumnAddition

// postgresColumnAddition describes one idempotent column reconciliation step.
type postgresColumnAddition struct {
	table     string
	columnDDL string
}

func migratePostgresDB(db *sql.DB, ddl string, additions []postgresColumnAddition) error {
	if db == nil {
		return fmt.Errorf("migrate postgres: nil db")
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate postgres ddl: %w", err)
	}
	for _, add := range additions {
		if err := ensurePostgresColumn(db, add); err != nil {
			return err
		}
	}
	return nil
}

// ensurePostgresColumn adds a column to an existing table when it is missing.
// It consults information_schema (available on every supported PostgreSQL
// version) instead of relying on ALTER TABLE ... ADD COLUMN IF NOT EXISTS, so
// the reconciliation also works on older servers and reports a precise error.
func ensurePostgresColumn(db *sql.DB, add postgresColumnAddition) error {
	column := postgresColumnName(add.columnDDL)

	exists, err := hasPostgresColumn(db, add.table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", add.table, add.columnDDL)
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("migrate postgres add column %s.%s: %w", add.table, column, err)
	}
	return nil
}

func hasPostgresColumn(db *sql.DB, table, column string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = $1
		  AND column_name = $2
	`, table, column).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("inspect postgres column %s.%s: %w", table, column, err)
	}
	return count > 0, nil
}

// postgresColumnName extracts the leading identifier from a column DDL
// fragment, e.g. "incremental_alive_nodes BOOLEAN NOT NULL DEFAULT FALSE"
// yields "incremental_alive_nodes".
func postgresColumnName(columnDDL string) string {
	if fields := strings.Fields(columnDDL); len(fields) > 0 {
		return fields[0]
	}
	return columnDDL
}

func migratePostgresStateDB(db *sql.DB) error {
	return migratePostgresDB(db, postgresStateDDL, postgresStateColumnAdditions)
}

func migratePostgresCacheDB(db *sql.DB) error {
	return migratePostgresDB(db, postgresCacheDDL, postgresCacheColumnAdditions)
}
