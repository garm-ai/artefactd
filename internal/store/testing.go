package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestDB gives a test its own schema in the Postgres POSTGRES_DSN names, and
// skips when there is none.
//
// Its own schema rather than its own database: creating a database needs a
// connection to another one and a moment of waiting, and search_path buys
// the same isolation for the price of a SET. It is dropped on cleanup, so a
// failed run leaves nothing behind unless the process was killed.
//
// A skip rather than a fake. A fake Postgres would test the fake, and these
// tests exist because the interesting failures are constraint violations and
// the exact behaviour of an UPDATE that matches no rows.
func TestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not set; run `mise run pg` and export what it prints")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "artefactd_test_" + hex.EncodeToString(b[:])

	// `?` when the DSN carries no query string yet and `&` when it does.
	// Deciding this rather than trying both matters: a Postgres that is down
	// must fail as unreachable on the first attempt, not as an invalid
	// parameter on a second one made against a doubled separator.
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	if err := createSchema(t, dsn, schema); err != nil {
		t.Fatalf("creating the schema: %v", err)
	}
	db, err := Open(t.Context(), dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if _, err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(),
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		db.Close()
	})
	return db
}

// TestDSN gives a test its own schema and the DSN that reaches it, for a
// test that starts the whole service rather than opening the store itself.
// Same isolation as TestDB, and dropped the same way.
func TestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN is not set; run `mise run pg` and export what it prints")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "artefactd_test_" + hex.EncodeToString(b[:])
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "search_path=" + schema

	// The schema is created over the UNSCOPED dsn, before anything connects
	// to the scoped one. A connection whose search_path names a schema that
	// does not exist yet silently reads the default one instead, so a pool
	// opened first can end up with one connection in the test's schema and
	// another in a schema every test shares.
	if err := createSchema(t, dsn, schema); err != nil {
		t.Fatalf("creating the schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := Open(context.Background(), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.pool.Exec(context.Background(),
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	})
	return scoped
}

// createSchema makes a test's schema over a connection that is not scoped to
// it, so every later connection finds it already there.
func createSchema(t *testing.T, dsn, schema string) error {
	t.Helper()
	db, err := Open(t.Context(), dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.pool.Exec(t.Context(), fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema))
	return err
}
