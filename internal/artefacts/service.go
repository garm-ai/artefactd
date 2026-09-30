package artefacts

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	"github.com/garm-ai/artefactd/internal/blob"
	"github.com/garm-ai/artefactd/internal/codec"
	"github.com/garm-ai/artefactd/internal/store"
	"github.com/garm-ai/artefactd/internal/ulid"
)

// Service is the artefact store.
type Service struct {
	DB    *store.DB
	Blob  blob.Store
	Codec codec.Codec
	Log   *slog.Logger

	// Classification is what every artefact this deployment holds is stamped
	// at. It is NOT a request field and there is nowhere in the contract to put
	// one: the classification of a generated document is a governance decision
	// and a caller-set one would be the model choosing its own clearance.
	//
	// It is one value for the deployment because nothing in the platform can
	// yet tell this service what an INDIVIDUAL artefact should be classified
	// at — that needs a declaration on the creating tool's own ToolPolicy,
	// which is the same contracts release ruling 5's retention field needs.
	// Until then this is the floor, and it is a floor rather than a guess: a
	// deployment sets it to the classification it is willing to hold generated
	// documents at. See KNOWN-GAPS.md.
	Classification Classification

	// UploadTTL is how long a presigned PUT is good for. It bounds how long an
	// orphan can sit before the sweeper reaches it.
	UploadTTL time.Duration
	// ReadTTL is how long a presigned GET is good for. Minutes, because its
	// lifetime IS the window in which a leaked link is a live credential — and
	// the other side of that, which the decision does not name, is that a short
	// expiry makes this service a hard dependency of every read: while it is
	// down nothing new can be read, and URLs already minted keep working until
	// they lapse. Neither is free. It is configurable per deployment and logged
	// at startup for that reason, and README.md says both halves plainly.
	ReadTTL time.Duration

	// Now and NewID are replaced by a test and by nothing else.
	Now   func() time.Time
	NewID func() string
}

// Classification is what an artefact is labelled at.
type Classification struct {
	Clearance    toolv1.Clearance
	Compartments []string
}

// The defaults, each with the reason it is the number it is.
const (
	// DefaultUploadTTL is how long a generator has to write its bytes. Long
	// enough for a large spreadsheet over a slow link; short enough that an
	// abandoned write is swept within the hour.
	DefaultUploadTTL = 15 * time.Minute
	// DefaultReadTTL is the presigned read window. Five minutes: long enough
	// for a person to click a link a client has just rendered, short enough
	// that a URL copied into a chat log is dead before anybody reads the log.
	DefaultReadTTL = 5 * time.Minute
	// DefaultClearance is what an artefact is stamped at when a deployment
	// names nothing. INTERNAL and not PUBLIC: a generated document is made of
	// whatever the run was working on, and the absence of a decision is not a
	// decision to publish.
	DefaultClearance = toolv1.Clearance_CLEARANCE_INTERNAL
)

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) newID() string {
	if s.NewID == nil {
		return ulid.NewNow()
	}
	return s.NewID()
}

func (s *Service) uploadTTL() time.Duration {
	if s.UploadTTL <= 0 {
		return DefaultUploadTTL
	}
	return s.UploadTTL
}

// ReadWindow is the effective presigned read lifetime, which Serve logs at
// startup. Exported because a deployment reading its own log line should see
// the number in force and not the flag it did or did not pass.
func (s *Service) ReadWindow() time.Duration {
	if s.ReadTTL <= 0 {
		return DefaultReadTTL
	}
	return s.ReadTTL
}

func (s *Service) classification() Classification {
	c := s.Classification
	if c.Clearance == toolv1.Clearance_CLEARANCE_UNSPECIFIED {
		c.Clearance = DefaultClearance
	}
	return c
}

// ---------------------------------------------------------------- begin_write

// BeginInput is what a generator asks for.
type BeginInput struct {
	MediaType string
	Label     string
	ToolFQN   string
	Retention time.Duration
}

// Begun is an artefact that has been named and has somewhere to go.
type Begun struct {
	Artefact  store.Artefact
	UploadURL string
	ExpiresAt time.Time
	// DataKey is the clear key the writer seals with. Empty under the null
	// codec. It is never stored and never logged.
	DataKey []byte
}

// Begin names an artefact and returns a presigned PUT for it.
//
// The key is <tenant>/<run>/<artefact_id>, built by internal/blob from the
// invocation's own tenant and run — never from anything the caller sent. The
// rejected alternative was the generator computing its own key and registering
// afterwards, which puts the key layout and the tenancy rules in every
// generator binary; and a shared library was rejected for the sharper reason
// that minting a read URL is a disclosure and a library call leaves no row.
//
// The classification is stamped here, from the deployment's own decision. There
// is no argument to this method that sets one.
func (s *Service) Begin(ctx context.Context, c Caller, in BeginInput) (Begun, error) {
	if c.RunID == "" {
		// The run is the middle component of the key. A call with no run has
		// nowhere to put an object, and inventing a component would be
		// inventing a namespace.
		return Begun{}, refuse(CodeRefused,
			"an artefact belongs to a run, and this call names none")
	}
	if in.MediaType == "" {
		return Begun{}, refuse(CodeRefused, "an artefact declares what its bytes are")
	}
	if in.ToolFQN == "" {
		return Begun{}, refuse(CodeRefused, "an artefact names the tool that created it")
	}
	if in.Retention <= 0 {
		return Begun{}, refuse(CodeRefused,
			"an artefact declares how long it is worth keeping")
	}

	id := s.newID()
	if !ulid.Valid(id) {
		// Only reachable through a test's NewID. It is checked anyway, because
		// the id is a key component and a malformed one must never reach the
		// object store.
		return Begun{}, refuse(CodeBroke, "the artefact store could not mint an id")
	}
	key, err := blob.Key(c.Tenant, c.RunID, id)
	if err != nil {
		// The tenant or the run carried something that cannot be a key
		// component. It came off the invocation, so this is a refusal about the
		// caller's identity and not about their request.
		s.log().Error("a key could not be built", "tenant", c.Tenant, "run", c.RunID, "err", err)
		return Begun{}, refuse(CodeRefused,
			"this call's tenant or run cannot be part of an object key")
	}

	clear, wrapped, err := s.Codec.Begin(ctx)
	if err != nil {
		s.log().Error("a data key could not be minted", "artefact", id, "err", err)
		return Begun{}, refuse(CodeBroke, "the artefact store could not seal a payload")
	}

	now := s.now()
	url, expires, err := s.Blob.PresignPut(ctx, key, in.MediaType, s.uploadTTL())
	if err != nil {
		s.log().Error("a write could not be signed", "artefact", id, "err", err)
		return Begun{}, refuse(CodeBroke, "the object store could not be reached")
	}

	cls := s.classification()
	a, err := s.DB.Begin(ctx, store.NewArtefact{
		ID: id, Tenant: c.Tenant, RunID: c.RunID, ObjectKey: key,
		Clearance:    cls.Clearance.String(),
		Compartments: cls.Compartments,
		MediaType:    in.MediaType, Label: in.Label,
		ToolFQN: in.ToolFQN, CreatedBy: c.Subject, BeginCallID: c.CallID,
		Codec: s.Codec.Name(), KEKID: s.Codec.KeyID(), WrappedDEK: wrapped,
		RetentionSeconds: int64(in.Retention / time.Second),
		CreatedAt:        now, UploadExpiresAt: expires,
	})
	if err != nil {
		return Begun{}, s.fromStore(err, "an artefact could not be named")
	}
	s.log().Info("artefact named", "artefact", a.ID, "tenant", c.Tenant, "run", c.RunID,
		"tool", in.ToolFQN, "media_type", in.MediaType, "codec", a.Codec,
		"clearance", a.Clearance, "call_id", c.CallID)
	return Begun{Artefact: a, UploadURL: url, ExpiresAt: expires, DataKey: clear}, nil
}

// ---------------------------------------------------------------- commit

// CommitInput is what a generator reports once the bytes are written.
type CommitInput struct {
	ID                 string
	PlaintextSHA256    string
	PlaintextSizeBytes uint64
	StoredSHA256       string
	StoredSizeBytes    uint64
}

// Commit records that the bytes are there and makes the artefact readable.
//
// What it can check, it checks: the object store is asked what it holds at the
// key, and a size that disagrees with the one claimed is a refusal. What it
// cannot check is the digest, because verifying it would mean reading the
// payload, and this service never reads a payload. So the plaintext digest is
// the writer's claim, recorded for the reader to verify — which is the right
// place for it, since the reader is the one holding the bytes.
func (s *Service) Commit(ctx context.Context, c Caller, in CommitInput) (store.Artefact, error) {
	a, err := s.Get(ctx, c, in.ID)
	if err != nil {
		return store.Artefact{}, err
	}
	switch a.State {
	case store.StateShredded:
		return store.Artefact{}, refuse(CodeGone, GoneMessage)
	case store.StateCommitted:
		// Idempotent on the same bytes. A second commit naming the same digest
		// is the same commit — a retry after a timeout the caller never saw the
		// answer to — and a different digest is two different payloads claiming
		// one id.
		if a.PlaintextSHA256 == in.PlaintextSHA256 && a.StoredSHA256 == in.StoredSHA256 {
			return a, nil
		}
		return store.Artefact{}, refuse(CodeConflict,
			"this artefact is already committed with a different digest")
	}

	size, err := s.Blob.Head(ctx, a.ObjectKey)
	if err != nil {
		if errors.Is(err, blob.ErrNoObject) {
			return store.Artefact{}, refuse(CodeRefused,
				"there are no bytes at this artefact's key: write them with the URL "+
					"begin_write returned, then commit")
		}
		s.log().Error("an object could not be checked", "artefact", a.ID, "err", err)
		return store.Artefact{}, refuse(CodeBroke, "the object store could not be reached")
	}
	if uint64(size) != in.StoredSizeBytes {
		return store.Artefact{}, refusef(CodeRefused,
			"this artefact's object is %d bytes and the commit claims %d", size, in.StoredSizeBytes)
	}

	now := s.now()
	out, err := s.DB.Commit(ctx, c.Tenant, a.ID, store.Committed{
		PlaintextSHA256:    in.PlaintextSHA256,
		PlaintextSizeBytes: int64(in.PlaintextSizeBytes),
		StoredSHA256:       in.StoredSHA256,
		StoredSizeBytes:    int64(in.StoredSizeBytes),
		CommitCallID:       c.CallID,
		At:                 now,
		RetainUntil:        now.Add(time.Duration(a.RetentionSeconds) * time.Second),
	})
	if err != nil {
		return store.Artefact{}, s.fromStore(err, "an artefact could not be committed")
	}
	s.log().Info("artefact committed", "artefact", out.ID, "tenant", c.Tenant,
		"stored_bytes", out.StoredSizeBytes, "retain_until", out.RetainUntil,
		"call_id", c.CallID)
	return out, nil
}

// ---------------------------------------------------------------- describe

// Get reads one artefact of the caller's tenant.
//
// The tenant is part of the query and not a check afterwards, so an artefact of
// another tenant is "no such artefact" — the same answer an id that never
// existed gets.
func (s *Service) Get(ctx context.Context, c Caller, id string) (store.Artefact, error) {
	if !ulid.Valid(id) {
		// Refused before a round trip, and before anything shaped like a path
		// can reach the key builder.
		return store.Artefact{}, refuse(CodeNoSuchArtefact, NotFoundMessage)
	}
	a, err := s.DB.Get(ctx, c.Tenant, id)
	if err != nil {
		return store.Artefact{}, s.fromStore(err, "an artefact could not be read")
	}
	return a, nil
}

// ---------------------------------------------------------------- read_url

// Released is one disclosure: a URL, when it dies, and the key to open what it
// points at.
type Released struct {
	Artefact  store.Artefact
	URL       string
	ExpiresAt time.Time
	// DataKey is the unwrapped key, released by this one governed act for this
	// one artefact. Empty under the null codec.
	DataKey []byte
	// Label is the classification the card is labelled at, which is what the
	// daemon compares the viewer against.
	Label Classification
}

// ReadURL mints a short-lived URL for one artefact and releases its key.
//
// TWO THINGS HOLD HERE AND THEY ARE DIFFERENT THINGS.
//
// The first is that no automated caller gets one. A call arriving with a
// delegation chain is refused, and so is a principal that is not a person: a
// model's tool call always carries the chain, and a service calling for itself
// carries the kind. This is the mirror image of `decide_task`'s refusal — a
// credential travelling towards a model instead of an approval given through
// one — and it is a CHECK because an audience is a listing rule that the
// daemon's visibility gate never reads.
//
// The second is that WHICH artefacts a person may read is not decided here.
// This service cannot know: the invocation carries no clearance and must not.
// What it does is label the card at the artefact's classification, and the
// daemon drops what the viewer does not reach when it walks the card's value.
//
// And the row comes first. The disclosure is written before the URL is signed,
// so a mint that could not be recorded does not happen. That is the whole
// reason this is a governed call rather than a library function, and the order
// of the two statements below is the enforcement.
func (s *Service) ReadURL(ctx context.Context, c Caller, id string) (Released, error) {
	if c.Automated() {
		s.log().Warn("an automated caller asked for an artefact URL",
			"reason", "delegated_reader", "artefact", id, "subject", c.Subject,
			"agent", c.Agent(), "kind", c.Kind.String(), "call_id", c.CallID)
		return Released{}, refuse(CodeDenied, DelegatedMessage)
	}
	a, err := s.Get(ctx, c, id)
	if err != nil {
		return Released{}, err
	}
	switch a.State {
	case store.StatePending:
		return Released{}, refuse(CodeRefused,
			"this artefact has no bytes yet: it was named and never committed")
	case store.StateShredded:
		return Released{}, refuse(CodeGone, GoneMessage)
	}

	key, err := s.Codec.Release(ctx, a.WrappedDEK)
	if err != nil {
		if errors.Is(err, codec.ErrShredded) {
			// The state and the key disagree, which the schema's constraint
			// should prevent. Answered as gone rather than as a server error,
			// because the fact is the same either way: the content is not
			// coming back.
			return Released{}, refuse(CodeGone, GoneMessage)
		}
		s.log().Error("a data key could not be released", "artefact", a.ID, "err", err)
		return Released{}, refuse(CodeBroke, "the artefact store could not open this payload")
	}

	clearance, known := clearanceOf(a.Clearance)
	if !known {
		// A row classified at something this build cannot name cannot be
		// labelled, and an unlabelled card is not tight — an absent or
		// unspecified clearance is the LOWEST value the daemon compares
		// against, so guessing here would publish rather than withhold. The
		// mint is refused instead.
		s.log().Error("an artefact is classified at a level this build cannot name",
			"artefact", a.ID, "clearance", a.Clearance)
		return Released{}, refuse(CodeBroke,
			"this artefact is classified at a level this build does not understand, "+
				"so no URL was minted")
	}
	label := Classification{Clearance: clearance, Compartments: a.Compartments}
	now := s.now()
	expires := now.Add(s.ReadWindow())

	// THE ROW, THEN THE CREDENTIAL. A disclosure that cannot be recorded is a
	// disclosure that does not happen, and the expiry recorded here is the one
	// the signature is then given.
	if _, err := s.DB.Record(ctx, store.NewDisclosure{
		ArtefactID: a.ID, At: now, Tenant: c.Tenant, Subject: c.Subject,
		CallID: c.CallID, Clearance: a.Clearance, Compartments: a.Compartments,
		URLExpiresAt: expires, KeyReleased: len(key) > 0,
	}); err != nil {
		s.log().Error("a disclosure could not be recorded", "artefact", a.ID, "err", err)
		return Released{}, refuse(CodeBroke,
			"this artefact's disclosure could not be recorded, so no URL was minted")
	}

	// The signer's own idea of when this URL dies is discarded, and `expires` —
	// the value the row already carries — is what is reported. They differ by
	// however long the insert above took, and one of the two numbers has to be
	// the answer: a record that disagrees with what a caller was told is a
	// record an auditor cannot use. The signature is the marginally longer of
	// the two, by milliseconds, because it was produced afterwards.
	url, _, err := s.Blob.PresignGet(ctx, a.ObjectKey, a.MediaType, a.Label, s.ReadWindow())
	if err != nil {
		s.log().Error("a read could not be signed", "artefact", a.ID, "err", err)
		return Released{}, refuse(CodeBroke, "the object store could not be reached")
	}
	s.log().Info("artefact disclosed", "artefact", a.ID, "tenant", c.Tenant,
		"subject", c.Subject, "clearance", a.Clearance, "compartments", a.Compartments,
		"key_released", len(key) > 0, "expires_at", expires, "call_id", c.CallID)
	return Released{
		Artefact: a, URL: url, ExpiresAt: expires, DataKey: key, Label: label,
	}, nil
}

// ---------------------------------------------------------------- sweep

// Swept is what one pass of the sweeper did.
type Swept struct {
	// Orphans are begin_writes that lapsed with no commit.
	Orphans int
	// Shredded are artefacts that outlived their declared retention.
	Shredded int
}

// Sweep is the cost ruling 2 accepted and the mechanism ruling 5 needs.
//
// A begin_write with no commit is an orphan: the row is dropped and the object,
// if the writer managed to put one there and then failed to commit, is deleted.
// There is nothing for an audit trail to keep — an artefact that was named and
// never confirmed is a name.
//
// An artefact past its retention is SHREDDED, not deleted: the wrapped key is
// destroyed and the row and its disclosures remain. That is ruling 6's erasure
// used as ruling 5's retention, and it is why a rejected single bucket
// lifecycle rule would have been the wrong answer twice — it gives a scratch
// preview and a document about a payment the same lifetime, and it removes the
// record along with the content.
//
// Under the null codec there is no key to destroy, so the object is deleted as
// well and the log line says which mechanism did the work. A deployment reading
// that line learns whether its erasure depends on a key or on a delete.
func (s *Service) Sweep(ctx context.Context, batch int32) (Swept, error) {
	now := s.now()
	var out Swept

	orphans, err := s.DB.ExpiredPending(ctx, now, batch)
	if err != nil {
		return out, err
	}
	for _, a := range orphans {
		// The object first: a row dropped before its object is an object
		// nothing points at.
		if err := s.Blob.Delete(ctx, a.ObjectKey); err != nil {
			s.log().Warn("an orphaned object could not be deleted",
				"artefact", a.ID, "key", a.ObjectKey, "err", err)
			continue
		}
		if err := s.DB.DropPending(ctx, a.ID); err != nil {
			s.log().Warn("an orphaned artefact could not be dropped", "artefact", a.ID, "err", err)
			continue
		}
		out.Orphans++
	}

	expired, err := s.DB.ExpiredRetention(ctx, now, batch)
	if err != nil {
		return out, err
	}
	for _, a := range expired {
		// The key goes whatever happens to the object: that is what makes the
		// payload unreadable under a real codec, and it is the part that cannot
		// be undone by a restored backup of the bucket.
		shredded, err := s.DB.Shred(ctx, a.ID, now)
		if err != nil {
			s.log().Warn("an artefact could not be shredded", "artefact", a.ID, "err", err)
			continue
		}
		byDelete := a.Codec == codec.NullName
		if byDelete {
			if err := s.Blob.Delete(ctx, a.ObjectKey); err != nil {
				// The row already says shredded, which is honest: the key is
				// gone. Under the null codec there was no key, so the object
				// surviving means the content survives, and that is worth an
				// error rather than a warning.
				s.log().Error("an artefact sealed by the null codec was shredded and its object remains",
					"artefact", a.ID, "key", a.ObjectKey, "err", err)
			}
		}
		s.log().Info("artefact shredded", "artefact", shredded.ID, "tenant", shredded.Tenant,
			"retain_until", a.RetainUntil, "codec", a.Codec, "object_deleted", byDelete)
		out.Shredded++
	}
	return out, nil
}

// clearanceOf reads a clearance written either way: the enum's own name
// (CLEARANCE_INTERNAL), which is how the row spells it, or the short form.
//
// It reports whether it KNEW the name, and the caller refuses rather than
// substituting a value. The reason is a property of the comparison the daemon
// makes: clearance is ordinal and UNSPECIFIED is zero, so a label that fell
// back to it would be reachable by every caller. Failing closed here means
// failing to answer, not answering with a default — a wrong default in this one
// place publishes an artefact a newer build classified above this one's
// vocabulary.
func clearanceOf(s string) (toolv1.Clearance, bool) {
	if s == "" {
		return toolv1.Clearance_CLEARANCE_UNSPECIFIED, false
	}
	name := s
	if !strings.HasPrefix(name, "CLEARANCE_") {
		name = "CLEARANCE_" + name
	}
	v, ok := toolv1.Clearance_value[name]
	if !ok || toolv1.Clearance(v) == toolv1.Clearance_CLEARANCE_UNSPECIFIED {
		return toolv1.Clearance_CLEARANCE_UNSPECIFIED, false
	}
	return toolv1.Clearance(v), true
}
