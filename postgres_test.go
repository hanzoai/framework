package framework

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	luxlog "github.com/luxfi/log"
)

// testDSN names a PostgreSQL server the suite runs against as well as SQLite: a postgres://
// URL whose role may create schemas. Each test gets a schema of its own, dropped after it.
//
//	FRAMEWORK_TEST_POSTGRES_DSN='postgres://me@/db?host=/run/postgresql&sslmode=disable' go test ./...
const testDSN = "FRAMEWORK_TEST_POSTGRES_DSN"

// testDB is where a test's store lives: a file on SQLite, or a schema on the server
// testDSN names. open is nil on SQLite.
func testDB(t *testing.T) (open OpenDBFunc, pg bool) {
	t.Helper()
	dsn := os.Getenv(testDSN)
	if dsn == "" {
		return nil, false
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "fw_" + hex.EncodeToString(b[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return func(string) (*sql.DB, error) {
		db, err := sql.Open("pgx", dsn+"&search_path="+schema)
		if err != nil {
			return nil, err
		}
		return db, nil
	}, true
}

// testConfig is an engine's configuration on the test engine.
func testConfig(t *testing.T) Config {
	t.Helper()
	open, pg := testDB(t)
	return Config{Dir: t.TempDir(), OpenDB: open, Postgres: pg}
}

func withLogger(c Config, l luxlog.Logger) Config { c.Logger = l; return c }
