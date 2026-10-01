package artefacts_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/garm-ai/contracts/cards"
	cardv1 "github.com/garm-ai/contracts/garm/card/v1"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/tool-go/toolbind"

	"github.com/garm-ai/artefactd/internal/artefacts"
	"github.com/garm-ai/artefactd/internal/blob"
	"github.com/garm-ai/artefactd/internal/codec"
	"github.com/garm-ai/artefactd/internal/store"
)

// The cast. Generic subjects and an example.com shape: this repository is
// public and names no deployment.
const (
	theTenant    = "example"
	otherTenant  = "other"
	theRun       = "run_01hq"
	theGenerator = "service:example.tools.v1.workbooks"
	thePerson    = "user:reader@example.com"
	theAgent     = "agent:example.agents.v1.Analyst"
	theTool      = "example.tools.v1.create_workbook"
	xlsx         = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// ---------------------------------------------------------------- the write path

func TestTheWritePathNamesAnArtefactAndKeysItUnderItsTenant(t *testing.T) {
	svc, objects := newService(t)
	begun := mustBegin(t, svc, runner(theGenerator, theRun))

	if begun.Artefact.Tenant != theTenant || begun.Artefact.RunID != theRun {
		t.Fatalf("the row says tenant %q run %q", begun.Artefact.Tenant, begun.Artefact.RunID)
	}
	want := theTenant + "/" + theRun + "/" + begun.Artefact.ID
	if begun.Artefact.ObjectKey != want {
		t.Errorf("the key is %q, want %q", begun.Artefact.ObjectKey, want)
	}
	if begun.UploadURL == "" {
		t.Error("begin_write returned no upload URL, so there is nowhere to put the bytes")
	}
	if begun.Artefact.State != store.StatePending {
		t.Errorf("a named artefact is %q, want PENDING", begun.Artefact.State)
	}
	if objects.signedPut != want {
		t.Errorf("the PUT was signed for %q, want %q", objects.signedPut, want)
	}
}

// The classification is the store's, and there is nowhere for a caller to put
// one — which the descriptor test asserts. This is the other half: whatever a
// caller sends, the row carries the deployment's stamp.
func TestTheClassificationIsStampedAndNotSupplied(t *testing.T) {
	svc, _ := newService(t)
	svc.Classification = artefacts.Classification{
		Clearance:    toolv1.Clearance_CLEARANCE_CONFIDENTIAL,
		Compartments: []string{"payments"},
	}
	// A caller cannot send a classification — the request message has no field
	// for one — so what is exercised here is that BeginInput, the widest thing a
	// handler can construct from a request, has no way to influence it either.
	begun, err := svc.Begin(context.Background(), runner(theGenerator, theRun), artefacts.BeginInput{
		MediaType: xlsx, Label: "CLEARANCE_PUBLIC", ToolFQN: "CLEARANCE_PUBLIC",
		Retention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if begun.Artefact.Clearance != "CLEARANCE_CONFIDENTIAL" {
		t.Errorf("the row is classified %q, want the deployment's CLEARANCE_CONFIDENTIAL",
			begun.Artefact.Clearance)
	}
	if len(begun.Artefact.Compartments) != 1 || begun.Artefact.Compartments[0] != "payments" {
		t.Errorf("the row's compartments are %v, want the deployment's", begun.Artefact.Compartments)
	}
}

func TestBeginRefusesACallWithNoRun(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Begin(context.Background(), runner(theGenerator, ""), artefacts.BeginInput{
		MediaType: xlsx, ToolFQN: theTool, Retention: time.Hour,
	})
	assertCode(t, err, artefacts.CodeRefused)
}

func TestCommitRefusesWhenThereAreNoBytes(t *testing.T) {
	svc, objects := newService(t)
	begun := mustBegin(t, svc, runner(theGenerator, theRun))
	objects.missing = true

	_, err := svc.Commit(context.Background(), runner(theGenerator, theRun), artefacts.CommitInput{
		ID: begun.Artefact.ID, PlaintextSHA256: digestA, PlaintextSizeBytes: 11,
		StoredSHA256: digestA, StoredSizeBytes: 11,
	})
	assertCode(t, err, artefacts.CodeRefused)
}

func TestCommitRefusesASizeTheObjectStoreDisagreesWith(t *testing.T) {
	svc, objects := newService(t)
	begun := mustBegin(t, svc, runner(theGenerator, theRun))
	objects.size = 11

	_, err := svc.Commit(context.Background(), runner(theGenerator, theRun), artefacts.CommitInput{
		ID: begun.Artefact.ID, PlaintextSHA256: digestA, PlaintextSizeBytes: 99,
		StoredSHA256: digestA, StoredSizeBytes: 99,
	})
	assertCode(t, err, artefacts.CodeRefused)
}

// A generator that retried after a timeout it never saw the answer to must not
// be told its own artefact is a conflict; two different payloads claiming one id
// must be.
func TestCommitIsIdempotentOnTheSameBytesAndARefusalOnDifferentOnes(t *testing.T) {
	svc, _ := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	again, err := svc.Commit(context.Background(), c, artefacts.CommitInput{
		ID: a.ID, PlaintextSHA256: digestA, PlaintextSizeBytes: 11,
		StoredSHA256: digestA, StoredSizeBytes: 11,
	})
	if err != nil {
		t.Fatalf("a repeated commit of the same bytes was refused: %v", err)
	}
	if again.ID != a.ID || again.State != store.StateCommitted {
		t.Errorf("a repeated commit answered %q %q", again.ID, again.State)
	}

	_, err = svc.Commit(context.Background(), c, artefacts.CommitInput{
		ID: a.ID, PlaintextSHA256: digestB, PlaintextSizeBytes: 11,
		StoredSHA256: digestB, StoredSizeBytes: 11,
	})
	assertCode(t, err, artefacts.CodeConflict)
}

// ---------------------------------------------------------------- the read path

// THE TEST THE DECISION IS ABOUT. An agent asks for a URL and does not get one,
// and the refusal is this service's own check rather than the audience.
func TestNoUrlIsMintedForAnAgentOrThroughOne(t *testing.T) {
	svc, objects := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	for why, caller := range map[string]artefacts.Caller{
		"a person's own agent": {
			Subject: thePerson, Tenant: theTenant, RunID: theRun,
			Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
			Act: []artefacts.Act{{
				Subject: theAgent, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
			}},
		},
		"an agent as itself": {
			Subject: theAgent, Tenant: theTenant,
			Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		},
		"a generator service": runner(theGenerator, theRun),
	} {
		signed := objects.signedGets()
		out, err := svc.ReadURL(context.Background(), caller, a.ID)
		assertCode(t, err, artefacts.CodeDenied)
		if out.URL != "" {
			t.Errorf("%s was given the URL %q", why, out.URL)
		}
		if objects.signedGets() != signed {
			t.Errorf("%s caused a read to be signed", why)
		}
		// And the refusal says why, because the person reading it needs to know
		// that handing the id on is the answer and retrying is not.
		var coded toolbind.CodedError
		if errors.As(err, &coded) && !strings.Contains(coded.Message, "id") {
			t.Errorf("%s was refused with %q, which does not say what to do instead",
				why, coded.Message)
		}
	}
}

// A NO-URL-SHAPED-FIELD assertion at the level of the released value rather than
// the descriptor: everything a reader is given is inside the card, so the
// daemon's per-artefact projection is the only thing between a caller and a
// credential.
func TestTheUrlAndTheKeyAreOnlyEverInsideTheCard(t *testing.T) {
	svc, _ := newService(t)
	svc.Classification = artefacts.Classification{
		Clearance:    toolv1.Clearance_CLEARANCE_CONFIDENTIAL,
		Compartments: []string{"payments"},
	}
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	released, err := svc.ReadURL(context.Background(), person(thePerson), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.URL == "" {
		t.Fatal("a person asked for a URL and got none")
	}

	pol, err := artefacts.PolicyOf("ReadUrl")
	if err != nil {
		t.Fatal(err)
	}
	card := artefacts.ReleaseCard(pol, released)

	// The card's own label is the artefact's classification, joined with the
	// endpoint's floor. That label is what the daemon compares the viewer
	// against, so if it were lower than the artefact's classification the whole
	// mechanism would be decorative.
	want := cards.Join(cards.EndpointLabel(pol),
		cards.Label(toolv1.Clearance_CLEARANCE_CONFIDENTIAL, []string{"payments"}))
	if got := card.GetAccess(); got.GetClearance() != want.GetClearance() {
		t.Errorf("the card is labelled %v, want %v", got.GetClearance(), want.GetClearance())
	}
	if got := card.GetAccess().GetCompartments(); len(got) != 1 || got[0] != "payments" {
		t.Errorf("the card is labelled with compartments %v, want [payments]", got)
	}

	// Every element and every fact carries a label at least as tight as the
	// card's. An element labelled BELOW the endpoint fails the whole card, and an
	// unlabelled one would take the endpoint's policy — which is looser than the
	// artefact's whenever the artefact is classified above it.
	for i, el := range card.GetBody() {
		assertAtLeast(t, want, el.GetAccess(), "element %d", i)
		for j, f := range el.GetFacts().GetFacts() {
			assertAtLeast(t, want, f.GetAccess(), "element %d fact %d", i, j)
		}
	}

	// And the URL is in the card and nowhere else a caller could reach.
	if findFact(card, artefacts.FactURL) != released.URL {
		t.Error("the URL is not a fact of the card, so the daemon's walk would not govern it")
	}
}

// THE ROW COMES BEFORE THE CREDENTIAL. A disclosure that cannot be recorded is a
// URL that is not minted, and that is the whole reason the read path is a
// governed call rather than a library function.
//
// The disclosures table is dropped out from under the service, which is the most
// honest available stand-in for "the store would not take the row".
func TestNoDisclosureRowMeansNoUrl(t *testing.T) {
	svc, objects := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	if _, err := svc.DB.Pool().Exec(context.Background(), "DROP TABLE disclosures"); err != nil {
		t.Fatal(err)
	}
	before := objects.signedGets()
	out, err := svc.ReadURL(context.Background(), person(thePerson), a.ID)
	assertCode(t, err, artefacts.CodeBroke)
	if out.URL != "" {
		t.Errorf("a URL was minted with no disclosure row: %q", out.URL)
	}
	if objects.signedGets() != before {
		t.Error("a read was signed even though the disclosure could not be recorded")
	}
}

func TestEveryMintIsRecordedWithWhoGotItAndWhen(t *testing.T) {
	svc, _ := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	released, err := svc.ReadURL(context.Background(), person(thePerson), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.DB.Disclosures(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("one mint produced %d disclosure rows", len(rows))
	}
	got := rows[0]
	if got.Subject != thePerson {
		t.Errorf("the row names %q, want %q", got.Subject, thePerson)
	}
	if got.Tenant != theTenant {
		t.Errorf("the row names tenant %q", got.Tenant)
	}
	// The label the card was served at, so an auditor joining this row to the
	// daemon's own row for the same call can see what was offered as well as
	// whether the daemon withheld it.
	if got.Clearance != a.Clearance {
		t.Errorf("the row records clearance %q, want the artefact's %q", got.Clearance, a.Clearance)
	}
	if got.CallID == "" {
		t.Error("the row records no call id, so it cannot be joined to the ledger")
	}
	// Compared at the resolution the COLUMN has, not the one Go hands out.
	// Postgres `timestamptz` keeps microseconds, so the expiry comes back
	// truncated while the one the mint returned still carries whatever
	// precision time.Now() gave it. Asserting the two instants are exactly
	// equal asserts a precision the store never promised.
	//
	// THIS IS WHY IT WAS GREEN ON A LAPTOP AND RED IN CI, which is the part
	// worth remembering: time.Now() is microsecond-granular on darwin and
	// nanosecond-granular on linux. The exact comparison therefore passed every
	// time on macOS — there was no remainder to lose — and failed on CI's
	// ubuntu runner on any mint whose nanosecond remainder was non-zero.
	const stored = time.Microsecond
	if !got.URLExpiresAt.Truncate(stored).Equal(released.ExpiresAt.UTC().Truncate(stored)) {
		t.Errorf("the row says the URL dies at %s and the URL dies at %s",
			got.URLExpiresAt, released.ExpiresAt)
	}
}

// The read window is a deployment's decision, and the signature and the row must
// both use the number in force rather than one of them using a constant.
func TestTheReadWindowIsTheOneConfigured(t *testing.T) {
	svc, _ := newService(t)
	svc.ReadTTL = 90 * time.Second
	if got := svc.ReadWindow(); got != 90*time.Second {
		t.Fatalf("ReadWindow is %s, want the configured 90s", got)
	}
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	released, err := svc.ReadURL(context.Background(), person(thePerson), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(released.ExpiresAt); d > 95*time.Second || d < 60*time.Second {
		t.Errorf("the URL expires in %s, want about 90s", d)
	}
	// And zero takes the default, which is minutes rather than hours.
	svc.ReadTTL = 0
	if got := svc.ReadWindow(); got != artefacts.DefaultReadTTL || got > 15*time.Minute {
		t.Errorf("the default read window is %s", got)
	}
}

func TestAnUncommittedArtefactCannotBeRead(t *testing.T) {
	svc, _ := newService(t)
	c := runner(theGenerator, theRun)
	begun := mustBegin(t, svc, c)
	_, err := svc.ReadURL(context.Background(), person(thePerson), begun.Artefact.ID)
	assertCode(t, err, artefacts.CodeRefused)
}

// ---------------------------------------------------------------- tenancy

// An artefact of another tenant is NOT FOUND, and it is the same answer an id
// that never existed gets — so the difference cannot be used to map out another
// tenant's ids.
func TestAnotherTenantsArtefactIsIndistinguishableFromOneThatNeverExisted(t *testing.T) {
	svc, _ := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))

	intruder := artefacts.Caller{
		Subject: "user:someone@other.example", Tenant: otherTenant,
		Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
	}
	_, theirs := svc.ReadURL(context.Background(), intruder, a.ID)
	_, nobodys := svc.ReadURL(context.Background(), intruder, "01k6m0p0w5vr5t3d0zj7k9zzzz")

	assertCode(t, theirs, artefacts.CodeNoSuchArtefact)
	assertCode(t, nobodys, artefacts.CodeNoSuchArtefact)
	if theirs.Error() != nobodys.Error() {
		t.Errorf("another tenant's artefact answers %q and a missing one answers %q; the "+
			"difference is a way to enumerate ids", theirs, nobodys)
	}
	// Describe is the same answer, because it is the cheaper probe.
	_, err := svc.Get(context.Background(), intruder, a.ID)
	assertCode(t, err, artefacts.CodeNoSuchArtefact)
}

// ---------------------------------------------------------------- the sweeper

func TestAnUncommittedWriteIsSweptAndItsObjectDeleted(t *testing.T) {
	svc, objects := newService(t)
	c := runner(theGenerator, theRun)
	begun := mustBegin(t, svc, c)

	// Past the upload window this row was created with.
	svc.Now = func() time.Time { return time.Now().UTC().Add(2 * artefacts.DefaultUploadTTL) }
	out, err := svc.Sweep(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if out.Orphans != 1 {
		t.Fatalf("the sweeper removed %d orphans, want 1", out.Orphans)
	}
	if objects.deleted[begun.Artefact.ObjectKey] != 1 {
		t.Errorf("the orphan's object was not deleted; deletes were %v", objects.deleted)
	}
	if _, err := svc.Get(context.Background(), c, begun.Artefact.ID); !isCode(err, artefacts.CodeNoSuchArtefact) {
		t.Errorf("the orphan's row survived: %v", err)
	}
}

// RETENTION IS ENFORCED BY SHREDDING, which is ruling 6's erasure used as ruling
// 5's lifetime: the key is destroyed, and the record that the artefact existed —
// and of who was given access to it — survives.
//
// And the read path then answers GONE, which is a third answer distinct from
// "never existed" and from "you may not see it". Conflating them either leaks or
// misleads.
func TestAnExpiredArtefactIsShreddedAndItsRecordAndDisclosuresSurvive(t *testing.T) {
	svc, objects := newService(t)
	c := runner(theGenerator, theRun)
	a := mustCommit(t, svc, c, mustBegin(t, svc, c))
	if _, err := svc.ReadURL(context.Background(), person(thePerson), a.ID); err != nil {
		t.Fatal(err)
	}

	svc.Now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	out, err := svc.Sweep(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if out.Shredded != 1 {
		t.Fatalf("the sweeper shredded %d artefacts, want 1", out.Shredded)
	}

	after, err := svc.Get(context.Background(), c, a.ID)
	if err != nil {
		t.Fatalf("the shredded artefact's row is gone, and the whole point is that it is "+
			"not: %v", err)
	}
	if after.State != store.StateShredded {
		t.Errorf("the row says %q, want SHREDDED", after.State)
	}
	if after.WrappedDEK != nil {
		t.Error("the row still holds a wrapped key after shredding")
	}
	if after.ShreddedAt == nil {
		t.Error("the row does not say when it was shredded")
	}
	rows, err := svc.DB.Disclosures(context.Background(), a.ID)
	if err != nil || len(rows) != 1 {
		t.Errorf("the disclosures against a shredded artefact are %d, %v; an audit trail "+
			"keeps the fact and loses the content", len(rows), err)
	}
	// Under the null codec there is no key to destroy, so the object must go as
	// well or the content survives.
	if objects.deleted[a.ObjectKey] != 1 {
		t.Errorf("a null-codec artefact was shredded and its object remains; deletes were %v",
			objects.deleted)
	}

	_, err = svc.ReadURL(context.Background(), person(thePerson), a.ID)
	assertCode(t, err, artefacts.CodeGone)
	if !strings.Contains(err.Error(), "record") {
		t.Errorf("a shredded artefact answers %q, which does not distinguish it from one "+
			"that never existed", err)
	}
}

// ---------------------------------------------------------------- fixtures

// digestA and digestB are two sha256 values, so a test can commit the same
// artefact twice and mean it, or mean something else.
const (
	digestA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	digestB = "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"
)

func newService(t *testing.T) (*artefacts.Service, *fakeObjects) {
	t.Helper()
	db := store.TestDB(t)
	objects := &fakeObjects{deleted: map[string]int{}, size: 11}
	return &artefacts.Service{
		DB: db, Blob: objects, Codec: codec.Null{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, objects
}

// runner is a generator tool executing a run: a service principal with the run
// on its attribution, which is what the write path requires.
func runner(subject, run string) artefacts.Caller {
	return artefacts.Caller{
		Subject: subject, Tenant: theTenant, RunID: run,
		Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE, CallID: "ev_call_1",
	}
}

// person is a person calling for themselves: no delegation chain, which is what
// read_url requires.
func person(subject string) artefacts.Caller {
	return artefacts.Caller{
		Subject: subject, Tenant: theTenant,
		Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER, CallID: "ev_call_2",
	}
}

func mustBegin(t *testing.T, svc *artefacts.Service, c artefacts.Caller) artefacts.Begun {
	t.Helper()
	out, err := svc.Begin(context.Background(), c, artefacts.BeginInput{
		MediaType: xlsx, Label: "Q3.xlsx", ToolFQN: theTool, Retention: time.Hour,
	})
	if err != nil {
		t.Fatalf("begin_write: %v", err)
	}
	return out
}

func mustCommit(t *testing.T, svc *artefacts.Service, c artefacts.Caller, begun artefacts.Begun) store.Artefact {
	t.Helper()
	a, err := svc.Commit(context.Background(), c, artefacts.CommitInput{
		ID: begun.Artefact.ID, PlaintextSHA256: digestA, PlaintextSizeBytes: 11,
		StoredSHA256: digestA, StoredSizeBytes: 11,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return a
}

// fakeObjects is the object store as these tests need it: it records what was
// signed and deleted, and it can be told there are no bytes.
//
// A double rather than a real bucket, for the cases that are about the service's
// own decisions. The round trip against a real S3 is its own test, because a
// double cannot tell you whether a signature the SDK produced is one the store
// actually honours.
type fakeObjects struct {
	mu        sync.Mutex
	signedPut string
	gets      int
	deleted   map[string]int
	size      int64
	missing   bool
}

func (f *fakeObjects) PresignPut(_ context.Context, key, _ string, ttl time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := blob.TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	f.signedPut = key
	return "https://objects.example.com/" + key + "?put", time.Now().UTC().Add(ttl), nil
}

func (f *fakeObjects) PresignGet(_ context.Context, key, _, _ string, ttl time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := blob.TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	f.gets++
	return "https://objects.example.com/" + key + "?get", time.Now().UTC().Add(ttl), nil
}

func (f *fakeObjects) Head(_ context.Context, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing {
		return 0, blob.ErrNoObject
	}
	return f.size, nil
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[key]++
	return nil
}

func (f *fakeObjects) signedGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", want)
	}
	var coded toolbind.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("error %v is not coded, so the transport would answer 500", err)
	}
	if coded.Code != want {
		t.Fatalf("answered %s (%s), want %s", coded.Code, coded.Message, want)
	}
}

func isCode(err error, want string) bool {
	var coded toolbind.CodedError
	return errors.As(err, &coded) && coded.Code == want
}

func assertAtLeast(t *testing.T, floor, got *cardv1.Label, what string, args ...any) {
	t.Helper()
	where := what
	if len(args) > 0 {
		where = fmt.Sprintf(what, args...)
	}
	if got == nil {
		t.Errorf("%s carries no label; an unlabelled element takes the ENDPOINT's policy, "+
			"which is looser than this artefact's classification", where)
		return
	}
	if got.GetClearance() < floor.GetClearance() {
		t.Errorf("%s is labelled %v, below the artefact's %v", where,
			got.GetClearance(), floor.GetClearance())
	}
	for _, need := range floor.GetCompartments() {
		found := false
		for _, have := range got.GetCompartments() {
			if have == need {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not require the %q compartment the artefact is labelled with",
				where, need)
		}
	}
}

func findFact(card *cardv1.Card, field string) string {
	for _, el := range card.GetBody() {
		for _, f := range el.GetFacts().GetFacts() {
			if f.GetField() == field {
				return f.GetValue()
			}
		}
	}
	return ""
}
