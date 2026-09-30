package store_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/garm-ai/artefactd/internal/store"
)

// NO SQL IS BUILT AT RUN TIME IN THIS PACKAGE, and this is the assertion.
//
// The rule that every injection in a "safe" builder comes down to: values can be
// parameterised, identifiers cannot. No dialect binds a column name, a table name
// or a sort direction as a placeholder, so a builder that lets you write
// `OrderBy(req.SortBy)` is not safer than concatenation — it only looks it.
//
// This service avoids the question rather than guarding it. Its reads are by id,
// by tenant and by state: a small closed set, enumerated as named `sqlc` queries
// and checked at build time against the schema the migrations produce. So there
// is no injection surface to guard, and this test is what keeps it that way — a
// query assembled from a caller's token would have to appear as a format string
// or a concatenation here first.
func TestNoQueryIsAssembled(t *testing.T) {
	// The two patterns a built query takes. Sprintf'd SQL and a string
	// concatenated onto a keyword: anything matching either is either a query
	// being assembled or something that reads exactly like one, and both are
	// worth a conversation.
	assembled := regexp.MustCompile(`(?i)(Sprintf|Join|\+\s*")[^"\n]*\b(select|insert|update|delete|from|where|order\s+by|limit)\b`)
	keywordThenPlus := regexp.MustCompile(`(?i)"[^"\n]*\b(select|insert|update|delete|from|where|order\s+by)\b[^"\n]*"\s*\+`)

	for _, path := range goFiles(t, ".") {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			// The test's own patterns, and the schema-and-search-path helpers in
			// testing.go, which build DDL from a schema name this package
			// generated from crypto/rand and never from a request.
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "testing.go") {
				continue
			}
			if assembled.MatchString(line) || keywordThenPlus.MatchString(line) {
				t.Errorf("%s:%d assembles SQL:\n\t%s\nValues are parameterised and "+
					"identifiers come from a closed set in Go. If this is genuinely needed, "+
					"map the caller's token to a constant with a switch that refuses the "+
					"default case, and say so here.", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 3 {
		t.Fatalf("walked %d Go files, which is too few to be reading this package", len(out))
	}
	return out
}

// ---------------------------------------------------------------- the schema

// The constraint that makes crypto-shredding a fact about the row rather than a
// convention in Go: a SHREDDED artefact has no key and says when it lost it.
//
// Asserted against the database because that is where it is enforced. A check in
// Go would be a check one code path could miss.
func TestTheDatabaseRefusesAShreddedRowThatStillHoldsAKey(t *testing.T) {
	db := store.TestDB(t)
	ctx := context.Background()
	a := insert(t, db, "example", "run_1")

	_, err := db.Pool().Exec(ctx,
		`UPDATE artefacts SET state = 'SHREDDED' WHERE id = $1`, a.ID)
	if err == nil {
		t.Error("a row was marked SHREDDED while still holding its wrapped key and a " +
			"NULL shredded_at; the erasure this design promises would be a state change " +
			"and nothing else")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("the refusal was %v, which does not look like the constraint", err)
	}
}

// And the row survives its key, which is the other half of ruling 6: the record
// that the artefact existed keeps the fact and loses the content.
func TestShreddingKeepsTheRowAndItsDisclosures(t *testing.T) {
	db := store.TestDB(t)
	ctx := context.Background()
	a := insert(t, db, "example", "run_1")
	commit(t, db, a)

	if _, err := db.Record(ctx, store.NewDisclosure{
		ArtefactID: a.ID, At: time.Now().UTC(), Tenant: "example",
		Subject: "user:reader@example.com", CallID: "ev_1",
		Clearance: "CLEARANCE_INTERNAL", URLExpiresAt: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	shredded, err := db.Shred(ctx, a.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if shredded.State != store.StateShredded || shredded.WrappedDEK != nil {
		t.Errorf("the row is %q with a %d-byte key", shredded.State, len(shredded.WrappedDEK))
	}
	if shredded.ShreddedAt == nil {
		t.Error("the row does not say when it was shredded")
	}
	// The metadata that makes the record useful survives too: what it was, who
	// made it, and what it was classified at.
	if shredded.MediaType == "" || shredded.ToolFQN == "" || shredded.Clearance == "" {
		t.Error("shredding removed the metadata the record exists to keep")
	}
	rows, err := db.Disclosures(ctx, a.ID)
	if err != nil || len(rows) != 1 {
		t.Errorf("the disclosures against a shredded artefact are %d, %v", len(rows), err)
	}
}

// Two artefacts cannot resolve to one object. It is the shape a path-construction
// bug takes when it does not happen to cross a tenant, and the unique index is
// the database's own opinion about it.
func TestTwoRowsCannotShareAnObjectKey(t *testing.T) {
	db := store.TestDB(t)
	a := insert(t, db, "example", "run_1")
	_, err := db.Begin(context.Background(), store.NewArtefact{
		ID: "01k6m0p0w5vr5t3d0zj7k9zzzz", Tenant: "example", RunID: "run_1",
		ObjectKey: a.ObjectKey, Clearance: "CLEARANCE_INTERNAL",
		MediaType: "text/plain", ToolFQN: "t", CreatedBy: "s", Codec: "null",
		WrappedDEK: []byte{}, RetentionSeconds: 60,
		CreatedAt: time.Now().UTC(), UploadExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err == nil {
		t.Error("two rows share one object key, so one artefact's bytes are another's")
	}
}

// A commit is conditional on the state it requires, so two of them racing cannot
// both win however they interleave.
func TestASecondCommitMatchesNoRows(t *testing.T) {
	db := store.TestDB(t)
	a := insert(t, db, "example", "run_1")
	commit(t, db, a)

	_, err := db.Commit(context.Background(), "example", a.ID, store.Committed{
		PlaintextSHA256: "b", PlaintextSizeBytes: 1, StoredSHA256: "b", StoredSizeBytes: 1,
		At: time.Now().UTC(), RetainUntil: time.Now().UTC().Add(time.Hour),
	})
	if !store.IsConflict(err) {
		t.Errorf("a second commit answered %v, want a conflict", err)
	}
}

// Reading is by id AND tenant, so another tenant's row is not there at all rather
// than there and refused afterwards.
func TestAnotherTenantsRowIsNotFound(t *testing.T) {
	db := store.TestDB(t)
	a := insert(t, db, "example", "run_1")
	if _, err := db.Get(context.Background(), "other", a.ID); err == nil {
		t.Error("another tenant read the row")
	}
}

func insert(t *testing.T, db *store.DB, tenant, run string) store.Artefact {
	t.Helper()
	a, err := db.Begin(context.Background(), store.NewArtefact{
		ID: "01k6m0p0w5vr5t3d0zj7k9ab2c", Tenant: tenant, RunID: run,
		ObjectKey: tenant + "/" + run + "/01k6m0p0w5vr5t3d0zj7k9ab2c",
		Clearance: "CLEARANCE_INTERNAL", Compartments: []string{"payments"},
		MediaType: "text/plain", Label: "notes.txt",
		ToolFQN: "example.tools.v1.write_notes", CreatedBy: "service:example.tools",
		BeginCallID: "ev_0", Codec: "null", WrappedDEK: []byte{},
		RetentionSeconds: 3600,
		CreatedAt:        time.Now().UTC(), UploadExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func commit(t *testing.T, db *store.DB, a store.Artefact) {
	t.Helper()
	if _, err := db.Commit(context.Background(), a.Tenant, a.ID, store.Committed{
		PlaintextSHA256: "a", PlaintextSizeBytes: 11, StoredSHA256: "a", StoredSizeBytes: 11,
		CommitCallID: "ev_1",
		At:           time.Now().UTC(), RetainUntil: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}
