package state

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestPostgresDDLMatchesSQLiteSchema guards against schema drift between the
// SQLite migration chain (source of truth) and the hand-written PostgreSQL
// DDL. Every table/column produced by the SQLite migrations must exist in the
// PostgreSQL DDL (or in the idempotent column additions).
//
// Regression: the PostgreSQL DDL once lagged behind SQLite migrations 5-8,
// so PostgreSQL deployments failed at startup with
//   pq: column "incremental_alive_nodes" does not exist
func TestPostgresDDLMatchesSQLiteSchema(t *testing.T) {
	sqliteSchema := map[string]map[string]bool{}

	stateDir := t.TempDir()
	stateDB, err := OpenSQLiteDB(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer stateDB.Close()
	if err := MigrateStateDBWithDialect(DialectSQLite, stateDB); err != nil {
		t.Fatalf("migrate state.db: %v", err)
	}
	mergeSchema(sqliteSchema, readSQLiteSchema(t, stateDB))

	cacheDir := t.TempDir()
	cacheDB, err := OpenSQLiteDB(filepath.Join(cacheDir, "cache.db"))
	if err != nil {
		t.Fatalf("open cache.db: %v", err)
	}
	defer cacheDB.Close()
	if err := MigrateCacheDBWithDialect(DialectSQLite, cacheDB); err != nil {
		t.Fatalf("migrate cache.db: %v", err)
	}
	mergeSchema(sqliteSchema, readSQLiteSchema(t, cacheDB))

	pgSchema := parsePostgresSchema(postgresStateDDL)
	mergeSchema(pgSchema, parsePostgresSchema(postgresCacheDDL))
	for _, add := range append(append([]postgresColumnAddition{}, postgresStateColumnAdditions...), postgresCacheColumnAdditions...) {
		applyColumnAddition(pgSchema, add)
	}

	for table, columns := range sqliteSchema {
		pgColumns, ok := pgSchema[table]
		if !ok {
			t.Errorf("postgres DDL is missing table %q (present in SQLite migrations)", table)
			continue
		}
		for column := range columns {
			if !pgColumns[column] {
				t.Errorf("postgres DDL is missing column %s.%s (present in SQLite migrations)", table, column)
			}
		}
	}
}

// readSQLiteSchema returns table -> set(column) for all user tables.
func readSQLiteSchema(t *testing.T, db *sql.DB) map[string]map[string]bool {
	t.Helper()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatalf("list sqlite tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan sqlite table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite tables: %v", err)
	}
	rows.Close()

	schema := make(map[string]map[string]bool, len(tables))
	for _, table := range tables {
		if isMigrationBookkeepingTable(table) {
			continue
		}
		columns, err := readSQLiteColumns(t, db, table)
		if err != nil {
			t.Fatalf("read columns for %s: %v", table, err)
		}
		schema[table] = columns
	}
	return schema
}

func isMigrationBookkeepingTable(table string) bool {
	return table == migrateDefaultTable || strings.HasPrefix(table, "sqlite_")
}

func readSQLiteColumns(t *testing.T, db *sql.DB, table string) (map[string]bool, error) {
	t.Helper()

	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			defaultV  sql.NullString
			primaryID int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultV, &primaryID); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// parsePostgresSchema extracts table -> set(column) from CREATE TABLE blocks.
func parsePostgresSchema(ddl string) map[string]map[string]bool {
	schema := map[string]map[string]bool{}

	const marker = "CREATE TABLE IF NOT EXISTS"
	for _, block := range strings.Split(ddl, marker)[1:] {
		block = strings.TrimLeft(block, " \t\r\n")
		nameEnd := strings.IndexAny(block, " \t\r\n(")
		if nameEnd <= 0 {
			continue
		}
		table := block[:nameEnd]

		bodyStart := strings.Index(block, "(")
		bodyEnd := strings.LastIndex(block, ")")
		if bodyStart < 0 || bodyEnd <= bodyStart {
			continue
		}
		body := block[bodyStart+1 : bodyEnd]

		columns := map[string]bool{}
		for _, rawLine := range strings.Split(body, "\n") {
			line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rawLine), ","))
			if line == "" {
				continue
			}
			if isPostgresTableConstraint(line) {
				continue
			}
			if fields := strings.Fields(line); len(fields) > 0 {
				columns[fields[0]] = true
			}
		}
		schema[table] = columns
	}
	return schema
}

func isPostgresTableConstraint(line string) bool {
	upper := strings.ToUpper(line)
	for _, prefix := range []string{"PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK", "CONSTRAINT", "EXCLUDE"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func applyColumnAddition(schema map[string]map[string]bool, add postgresColumnAddition) {
	columns, ok := schema[add.table]
	if !ok {
		columns = map[string]bool{}
		schema[add.table] = columns
	}
	if fields := strings.Fields(add.columnDDL); len(fields) > 0 {
		columns[fields[0]] = true
	}
}

func mergeSchema(dst, src map[string]map[string]bool) {
	for table, columns := range src {
		target, ok := dst[table]
		if !ok {
			target = map[string]bool{}
			dst[table] = target
		}
		for column := range columns {
			target[column] = true
		}
	}
}
