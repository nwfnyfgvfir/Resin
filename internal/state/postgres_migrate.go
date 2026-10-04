package state

import (
	"database/sql"
	"fmt"
)

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
	ephemeral_node_evict_delay_ns  BIGINT NOT NULL,
	created_at_ns                  BIGINT NOT NULL,
	updated_at_ns                  BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_header_rules (
	url_prefix    TEXT PRIMARY KEY,
	headers_json  TEXT NOT NULL,
	updated_at_ns BIGINT NOT NULL
);
`

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

func migratePostgresDB(db *sql.DB, ddl string) error {
	if db == nil {
		return fmt.Errorf("migrate postgres: nil db")
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate postgres ddl: %w", err)
	}
	return nil
}

func migratePostgresStateDB(db *sql.DB) error {
	return migratePostgresDB(db, postgresStateDDL)
}

func migratePostgresCacheDB(db *sql.DB) error {
	return migratePostgresDB(db, postgresCacheDDL)
}
