package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/garm-ai/artefactd/internal/store/sqlcgen"
)

// State is where an artefact is in its life. The strings are the database's
// CHECK constraint and the contract's enum names, which are the same strings,
// so a state cannot exist in one and not the other.
type State string

const (
	StatePending   State = "PENDING"
	StateCommitted State = "COMMITTED"
	StateShredded  State = "SHREDDED"
)

// Artefact is one row.
//
// It holds no payload and no plaintext key. WrappedDEK is nil only when the
// key has been DESTROYED — an empty slice is "there never was a key", which is
// what the null codec stores — and the schema's own constraint keeps the
// difference true.
type Artefact struct {
	ID        string
	Tenant    string
	RunID     string
	ObjectKey string
	State     State

	// Stamped from the creating run's principal, never supplied.
	Clearance    string
	Compartments []string

	MediaType string
	Label     string

	ToolFQN      string
	CreatedBy    string
	BeginCallID  string
	CommitCallID string

	Codec      string
	KEKID      string
	WrappedDEK []byte

	PlaintextSHA256    string
	PlaintextSizeBytes int64
	StoredSHA256       string
	StoredSizeBytes    int64

	RetentionSeconds int64

	CreatedAt       time.Time
	UploadExpiresAt time.Time
	CommittedAt     *time.Time
	RetainUntil     *time.Time
	ShreddedAt      *time.Time
}

// Shredded reports whether this artefact's key has been destroyed. It is a
// distinct answer from "no such artefact" and from "you may not read it".
func (a Artefact) Shredded() bool { return a.State == StateShredded }

// Disclosure is one presigned read URL this service minted, and who got it.
type Disclosure struct {
	Seq          int64
	ArtefactID   string
	At           time.Time
	Tenant       string
	Subject      string
	CallID       string
	Clearance    string
	Compartments []string
	URLExpiresAt time.Time
	KeyReleased  bool
}

// NewArtefact is everything begin_write decided.
type NewArtefact struct {
	ID        string
	Tenant    string
	RunID     string
	ObjectKey string

	Clearance    string
	Compartments []string

	MediaType string
	Label     string

	ToolFQN     string
	CreatedBy   string
	BeginCallID string

	Codec      string
	KEKID      string
	WrappedDEK []byte

	RetentionSeconds int64
	CreatedAt        time.Time
	UploadExpiresAt  time.Time
}

// Begin records a named, uncommitted artefact.
func (d *DB) Begin(ctx context.Context, in NewArtefact) (Artefact, error) {
	row, err := d.q().InsertPending(ctx, sqlcgen.InsertPendingParams{
		ID:               in.ID,
		Tenant:           in.Tenant,
		RunID:            in.RunID,
		ObjectKey:        in.ObjectKey,
		Clearance:        in.Clearance,
		Compartments:     nonNil(in.Compartments),
		MediaType:        in.MediaType,
		Label:            in.Label,
		ToolFqn:          in.ToolFQN,
		CreatedBy:        in.CreatedBy,
		BeginCallID:      in.BeginCallID,
		Codec:            in.Codec,
		KekID:            in.KEKID,
		WrappedDek:       in.WrappedDEK,
		RetentionSeconds: in.RetentionSeconds,
		CreatedAt:        stamp(in.CreatedAt),
		UploadExpiresAt:  stamp(in.UploadExpiresAt),
	})
	if err != nil {
		return Artefact{}, fmt.Errorf("store: naming an artefact: %w", err)
	}
	return artefactFrom(row), nil
}

// Get reads one artefact of one tenant.
//
// The tenant is part of the WHERE clause and not a check after the fact. It is
// the second of the two mechanisms that keep a caller inside its own tenant —
// the first is the key prefix — and it is the one that holds even if the key
// were built wrongly.
func (d *DB) Get(ctx context.Context, tenant, id string) (Artefact, error) {
	row, err := d.q().GetArtefact(ctx, sqlcgen.GetArtefactParams{ID: id, Tenant: tenant})
	if err != nil {
		if isNoRows(err) {
			return Artefact{}, ErrNotFound
		}
		return Artefact{}, fmt.Errorf("store: reading an artefact: %w", err)
	}
	return artefactFrom(row), nil
}

// Committed is what commit records.
type Committed struct {
	PlaintextSHA256    string
	PlaintextSizeBytes int64
	StoredSHA256       string
	StoredSizeBytes    int64
	CommitCallID       string
	At                 time.Time
	RetainUntil        time.Time
}

// Commit moves a pending artefact to committed, conditional on it still being
// pending. A commit that matches no rows is ErrConflict, which the service
// turns into "already committed" or "swept" by reading the row back.
func (d *DB) Commit(ctx context.Context, tenant, id string, c Committed) (Artefact, error) {
	row, err := d.q().CommitArtefact(ctx, sqlcgen.CommitArtefactParams{
		ID:                 id,
		Tenant:             tenant,
		PlaintextSha256:    c.PlaintextSHA256,
		PlaintextSizeBytes: c.PlaintextSizeBytes,
		StoredSha256:       c.StoredSHA256,
		StoredSizeBytes:    c.StoredSizeBytes,
		CommitCallID:       c.CommitCallID,
		CommittedAt:        stamp(c.At),
		RetainUntil:        stamp(c.RetainUntil),
	})
	if err != nil {
		if isNoRows(err) {
			return Artefact{}, ErrConflict
		}
		return Artefact{}, fmt.Errorf("store: committing an artefact: %w", err)
	}
	return artefactFrom(row), nil
}

// Shred destroys the wrapped key and records when. The row stays and so do the
// disclosures against it: an audit trail keeps the fact and loses the content.
func (d *DB) Shred(ctx context.Context, id string, at time.Time) (Artefact, error) {
	row, err := d.q().ShredArtefact(ctx, sqlcgen.ShredArtefactParams{
		ID: id, ShreddedAt: stamp(at),
	})
	if err != nil {
		if isNoRows(err) {
			return Artefact{}, ErrConflict
		}
		return Artefact{}, fmt.Errorf("store: shredding an artefact: %w", err)
	}
	return artefactFrom(row), nil
}

// DropPending removes a row nobody ever committed bytes for. The only delete
// in this service, and there is no fact for an audit trail to keep: an
// artefact that was named and never written is a name.
func (d *DB) DropPending(ctx context.Context, id string) error {
	if err := d.q().DeletePending(ctx, id); err != nil {
		return fmt.Errorf("store: dropping a pending artefact: %w", err)
	}
	return nil
}

// ExpiredPending is the sweeper's first question: which presigned writes have
// lapsed with no commit.
func (d *DB) ExpiredPending(ctx context.Context, now time.Time, limit int32) ([]Artefact, error) {
	rows, err := d.q().ExpiredPending(ctx, sqlcgen.ExpiredPendingParams{
		UploadExpiresAt: stamp(now), Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: reading lapsed writes: %w", err)
	}
	return artefactsFrom(rows), nil
}

// ExpiredRetention is the sweeper's second question: which artefacts have
// outlived the retention their creating tool declared.
func (d *DB) ExpiredRetention(ctx context.Context, now time.Time, limit int32) ([]Artefact, error) {
	rows, err := d.q().ExpiredRetention(ctx, sqlcgen.ExpiredRetentionParams{
		RetainUntil: stamp(now), Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: reading expired retention: %w", err)
	}
	return artefactsFrom(rows), nil
}

// NewDisclosure is what read_url records before the URL exists.
type NewDisclosure struct {
	ArtefactID   string
	At           time.Time
	Tenant       string
	Subject      string
	CallID       string
	Clearance    string
	Compartments []string
	URLExpiresAt time.Time
	KeyReleased  bool
}

// Record writes the disclosure row.
//
// It returns an error the service does not swallow. A mint whose row could not
// be written must not happen: that is the difference between a governed call
// and a library function, and it is the reason the read path is a call at all.
func (d *DB) Record(ctx context.Context, in NewDisclosure) (Disclosure, error) {
	row, err := d.q().InsertDisclosure(ctx, sqlcgen.InsertDisclosureParams{
		ArtefactID:   in.ArtefactID,
		At:           stamp(in.At),
		Tenant:       in.Tenant,
		Subject:      in.Subject,
		CallID:       in.CallID,
		Clearance:    in.Clearance,
		Compartments: nonNil(in.Compartments),
		UrlExpiresAt: stamp(in.URLExpiresAt),
		KeyReleased:  in.KeyReleased,
	})
	if err != nil {
		return Disclosure{}, fmt.Errorf("store: recording a disclosure: %w", err)
	}
	return Disclosure{
		Seq: row.Seq, ArtefactID: row.ArtefactID, At: row.At.Time, Tenant: row.Tenant,
		Subject: row.Subject, CallID: row.CallID, Clearance: row.Clearance,
		Compartments: row.Compartments, URLExpiresAt: row.UrlExpiresAt.Time,
		KeyReleased: row.KeyReleased,
	}, nil
}

// Disclosures is every read URL minted for one artefact, oldest first. It
// survives the key: who was given access to what and when is the half of the
// record that erasure must not touch.
func (d *DB) Disclosures(ctx context.Context, artefactID string) ([]Disclosure, error) {
	rows, err := d.q().DisclosuresOf(ctx, artefactID)
	if err != nil {
		return nil, fmt.Errorf("store: reading disclosures: %w", err)
	}
	out := make([]Disclosure, 0, len(rows))
	for _, r := range rows {
		out = append(out, Disclosure{
			Seq: r.Seq, ArtefactID: r.ArtefactID, At: r.At.Time, Tenant: r.Tenant,
			Subject: r.Subject, CallID: r.CallID, Clearance: r.Clearance,
			Compartments: r.Compartments, URLExpiresAt: r.UrlExpiresAt.Time,
			KeyReleased: r.KeyReleased,
		})
	}
	return out, nil
}

// q is the generated query set over this pool. One per call rather than one
// held on the struct, because sqlc's Queries is a value over a DBTX and the
// pool is what outlives a call.
func (d *DB) q() *sqlcgen.Queries { return sqlcgen.New(d.pool) }

func artefactsFrom(rows []sqlcgen.Artefact) []Artefact {
	out := make([]Artefact, 0, len(rows))
	for _, r := range rows {
		out = append(out, artefactFrom(r))
	}
	return out
}

func artefactFrom(r sqlcgen.Artefact) Artefact {
	return Artefact{
		ID: r.ID, Tenant: r.Tenant, RunID: r.RunID, ObjectKey: r.ObjectKey,
		State: State(r.State), Clearance: r.Clearance, Compartments: r.Compartments,
		MediaType: r.MediaType, Label: r.Label,
		ToolFQN: r.ToolFqn, CreatedBy: r.CreatedBy,
		BeginCallID: r.BeginCallID, CommitCallID: r.CommitCallID,
		Codec: r.Codec, KEKID: r.KekID, WrappedDEK: r.WrappedDek,
		PlaintextSHA256: r.PlaintextSha256, PlaintextSizeBytes: r.PlaintextSizeBytes,
		StoredSHA256: r.StoredSha256, StoredSizeBytes: r.StoredSizeBytes,
		RetentionSeconds: r.RetentionSeconds,
		CreatedAt:        r.CreatedAt.Time,
		UploadExpiresAt:  r.UploadExpiresAt.Time,
		CommittedAt:      when(r.CommittedAt),
		RetainUntil:      when(r.RetainUntil),
		ShreddedAt:       when(r.ShreddedAt),
	}
}

func stamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func when(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	at := t.Time.UTC()
	return &at
}

// nonNil keeps a nil slice out of a NOT NULL text[] column, where the driver
// would send NULL and the constraint would refuse it.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// IsConflict is for a caller that wants to distinguish a row that moved from a
// store that broke, without importing pgx.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }
