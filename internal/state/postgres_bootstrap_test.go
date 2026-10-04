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
