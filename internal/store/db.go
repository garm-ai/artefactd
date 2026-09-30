// Package store is artefactd's Postgres: the artefact record and the
// disclosures made against it.
//
// It is this service's own database, under its own role. No other process
// reads these tables and this one reads no others.
//
// THE MIGRATION MECHANISM IS tasksd's, deliberately and to the line: numbered
// .sql files, one transaction each, recorded in a version table from the first
// release, applied by this runner. Consistency in HOW migrations run matters
// more than consistency in how queries are written, and importing a migration
// framework for one repository would be a third pattern in a platform that has
// one.
//
// The QUERIES are not hand-written, and that is this repository's one
// departure: they are `sqlc`-generated from internal/store/queries against the
// schema these migrations produce. CLAUDE.md records why and what the caveat
// is.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound is a row that is not there, or is not this tenant's. The two
	// are one answer on purpose: a caller must not be able to map out another
	// tenant's ids by the difference.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is a row that was not in the state the change required: a
	// commit of an artefact something else already committed or swept.
	ErrConflict = errors.New("store: the row was not in the state this change requires")
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	pool *pgxpool.Pool
}

// Open connects and checks the connection before returning.
//
// Ping rather than a lazy pool: artefactd refuses to start when Postgres is
// unreachable, and a pool that connected on first use would let the process
// advertise itself on NATS and then refuse every call it was sent.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parsing the dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: postgres is unreachable: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Pool() *pgxpool.Pool { return d.pool }
func (d *DB) Close()              { d.pool.Close() }

// Migrate applies every embedded migration that has not been applied, in
// name order, and returns the version it left the schema at.
//
// A version table from the first release rather than an idempotent script run
// at every boot. The two behave the same while there is one file; they stop
// behaving the same the first time a column has to change, and a table added
// then has no record of what came before it.
func (d *DB) Migrate(ctx context.Context) (int, error) {
	if _, err := d.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	files, err := migrationFiles()
	if err != nil {
		return 0, err
	}
	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	at := 0
	for _, f := range files {
		if f.version > at {
			at = f.version
		}
		if applied[f.version] {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + f.name)
		if err != nil {
			return 0, err
		}
		// One transaction per migration: a file that fails halfway leaves
		// neither half of its schema nor a row saying it ran.
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			return 0, fmt.Errorf("store: %w", err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return 0, fmt.Errorf("store: applying %s: %w", f.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1,$2)
			 ON CONFLICT (version) DO NOTHING`, f.version, f.name); err != nil {
			_ = tx.Rollback(ctx)
			return 0, fmt.Errorf("store: recording %s: %w", f.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("store: committing %s: %w", f.name, err)
		}
	}
	return at, nil
}

type migration struct {
	version int
	name    string
}

// migrationFiles reads the embedded directory and orders it by the number
// each file name starts with. A file whose name does not start with one is
// an error rather than a file applied last: "0002_x.sql" and "x.sql" in one
// directory is a schema whose order depends on how it was listed.
func migrationFiles() ([]migration, error) {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %q does not start with a version number", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, name, v)
		}
		seen[v] = name
		out = append(out, migration{version: v, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: reading schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// SchemaVersion is the highest migration this database has recorded.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v *int
	if err := d.pool.QueryRow(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
