// Package db provides the persistence layer: a thin wrapper over database/sql
// supporting SQLite (modernc.org/sqlite, dev/tests) and Postgres (pgx stdlib),
// with a shared query layer (placeholder rebind `?`→`$N`, dual-dialect DDL) and
// inline migrations. Mirrors a sibling Go service's db package shape.
package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver, registered as "pgx"
	_ "modernc.org/sqlite"             // sqlite driver, registered as "sqlite"
)

// DB wraps the SQL handle plus the normalized driver name.
type DB struct {
	*sql.DB
	driver string // "sqlite" or "postgres"
}

func (d *DB) isPostgres() bool { return d.driver == "postgres" }

// Open opens the database for the given driver+dsn and runs migrations.
// driver is "sqlite" (default; dsn is a file path) or "postgres"/"pgx".
func Open(driver, dsn string) (*DB, error) {
	switch driver {
	case "", "sqlite":
		return openSQLite(dsn)
	case "postgres", "pgx":
		return openPostgres(dsn)
	default:
		return nil, fmt.Errorf("db: unsupported driver %q (use \"sqlite\" or \"postgres\")", driver)
	}
}

func openSQLite(dsn string) (*DB, error) {
	if dsn == "" {
		dsn = "auditloop.db"
	}
	// _pragma busy_timeout avoids "database is locked" under the web+worker
	// goroutines sharing one file.
	sqldb, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite writers must be serialized.
	sqldb.SetMaxOpenConns(1)
	d := &DB{DB: sqldb, driver: "sqlite"}
	if err := d.migrate(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

func openPostgres(dsn string) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("db: postgres driver requires a DSN (set DATABASE_URL)")
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("db: postgres ping: %w", err)
	}
	d := &DB{DB: sqldb, driver: "postgres"}
	if err := d.migrate(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

// rebind rewrites `?` placeholders to Postgres `$1,$2,…`. For SQLite it returns
// the query unchanged. It skips `?` inside single-quoted string literals.
func (d *DB) rebind(query string) string {
	if !d.isPostgres() {
		return query
	}
	var b strings.Builder
	n := 0
	inStr := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inStr = !inStr
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inStr {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func (d *DB) exec(query string, args ...any) (sql.Result, error) {
	return d.Exec(d.rebind(query), args...)
}

func (d *DB) query(query string, args ...any) (*sql.Rows, error) {
	return d.Query(d.rebind(query), args...)
}

func (d *DB) queryRow(query string, args ...any) *sql.Row {
	return d.QueryRow(d.rebind(query), args...)
}
