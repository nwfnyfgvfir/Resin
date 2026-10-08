package state

import "testing"

func TestRebindQueryPostgres(t *testing.T) {
	got := rebindQuery(DialectPostgres, "SELECT * FROM platforms WHERE id = ? AND name = ?")
	want := "SELECT * FROM platforms WHERE id = $1 AND name = $2"
	if got != want {
		t.Fatalf("rebindQuery postgres: got %q want %q", got, want)
	}
}

func TestRebindQuerySQLiteNoop(t *testing.T) {
	query := "INSERT INTO leases (platform_id, account) VALUES (?, ?)"
	if got := rebindQuery(DialectSQLite, query); got != query {
		t.Fatalf("rebindQuery sqlite: got %q want %q", got, query)
	}
}

func TestPostgresColumnName(t *testing.T) {
	cases := []struct {
		ddl  string
		want string
	}{
		{"incremental_alive_nodes BOOLEAN NOT NULL DEFAULT FALSE", "incremental_alive_nodes"},
		{"passive_circuit_breaker_disabled INTEGER NOT NULL DEFAULT 0", "passive_circuit_breaker_disabled"},
		{"  enabled INTEGER NOT NULL DEFAULT 1  ", "enabled"},
	}
	for _, tc := range cases {
		if got := postgresColumnName(tc.ddl); got != tc.want {
			t.Errorf("postgresColumnName(%q): got %q want %q", tc.ddl, got, tc.want)
		}
	}
}

// TestPostgresColumnAdditionsCoverKnownRegressions pins the columns that were
// missing from the PostgreSQL schema and broke startup with
// `pq: column "incremental_alive_nodes" does not exist`.
func TestPostgresColumnAdditionsCoverKnownRegressions(t *testing.T) {
	required := map[string]string{
		"subscriptions": "incremental_alive_nodes",
		"platforms":     "passive_circuit_breaker_disabled",
	}
	for table, column := range required {
		found := false
		for _, add := range postgresStateColumnAdditions {
			if add.table == table && postgresColumnName(add.columnDDL) == column {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("postgresStateColumnAdditions is missing %s.%s", table, column)
		}
	}
}
