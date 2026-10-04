package state

import (
	"strconv"
	"strings"
)

// Dialect identifies the SQL dialect used by a persistence backend.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

func normalizeDialect(raw string) Dialect {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(DialectSQLite):
		return DialectSQLite
	case string(DialectPostgres), "postgresql":
		return DialectPostgres
	default:
		return Dialect("")
	}
}

func rebindQuery(dialect Dialect, query string) string {
	if dialect != DialectPostgres || !strings.Contains(query, "?") {
		return query
	}

	var builder strings.Builder
	builder.Grow(len(query) + 8)
	argIndex := 1
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			builder.WriteByte(query[i])
			continue
		}
		builder.WriteByte('$')
		builder.WriteString(strconv.Itoa(argIndex))
		argIndex++
	}
	return builder.String()
}
