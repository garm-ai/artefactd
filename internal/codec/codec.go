// Package codec is the seal on an artefact's payload, as an interface with a
// null implementation.
//
// Ruling 6: the service generates a data key per artefact, the payload is
// sealed with it, and the WRAPPED key is what this service stores. `read_url`
// already authorises the caller and already writes a disclosure row, so it
// returns the URL to the ciphertext and the unwrapped data key in the same
// answer, and the reader decrypts locally. One governed act releases exactly
// one artefact's key.
//
// The daemon is not involved, and the reasons are worth keeping in front of
// whoever implements the next codec. Bytes through the daemon is what ruled
// out streaming for both reads and writes. It would give the daemon key
// material, and today it holds none — compromising the daemon lets an attacker
// bypass policy from that moment on, but it does not hand them the archive.
// It contradicts the rule that the daemon holds no state outliving a request,
// which that repository's CI now enforces. And it collapses the presigned read
// path, which works only because the object store serves the bytes with no
// proxy in the middle.
//
// WHAT IS NOT HERE IS THE KMS, deliberately. In production the wrapping key
// belongs in a KMS or an HSM and this service holds only wrapped keys; a
// service database that holds both the ciphertext key and the wrapped key is a
// decryption oracle in every backup. This package is the seam that integration
// plugs into — KeyID names the wrapping key and its version, so a rotation is
// expressible before it is implemented — and it is not the integration. See
// KNOWN-GAPS.md.
package codec

import (
	"context"
	"errors"
)

// Codec generates and wraps an artefact's data key.
//
// It does not encrypt anything, and that is not an omission: this service
// never sees a payload. The WRITER seals with the key Begin hands it, and the
// READER opens with the key read_url releases. What a codec owns is the key
// hierarchy and the name under which a future codec will recognise its work.
type Codec interface {
	// Name identifies this codec on an artefact's row, so a payload sealed
	// today can still be opened by a build that has moved on. It is recorded
	// per artefact and never assumed from configuration.
	Name() string

	// KeyID names the wrapping key and its version, for the row. Empty when
	// there is no wrapping key.
	KeyID() string

	// Begin mints a data key for one artefact and returns it twice: in the
	// clear for the writer to seal with, and wrapped for this service to
	// store. The clear copy is never stored and never logged.
	//
	// A codec that seals nothing returns two empty slices, which is what makes
	// a development plane need no key infrastructure at all.
	Begin(ctx context.Context) (clear, wrapped []byte, err error)

	// Release unwraps a stored key so read_url can hand it to a reader who
	// has been authorised. It is called once per disclosure and never
	// speculatively.
	Release(ctx context.Context, wrapped []byte) ([]byte, error)
}

// ErrShredded is what Release answers for a key that has been destroyed. The
// caller turns it into an answer that is distinct from "no such artefact" and
// from "you may not see it", because those are three different facts and
// conflating them either leaks or misleads.
var ErrShredded = errors.New("codec: the key for this artefact has been destroyed")

// NullName is the codec a development plane runs.
const NullName = "null"

// Null seals nothing.
//
// It exists so the whole plane works with no key infrastructure, which is what
// keeps the seam honest: a design where the null path is the awkward one gets
// a real codec bolted on beside it rather than through it.
//
// It is NOT what a bank deploys, and the difference is visible in the row: an
// artefact whose codec is "null" is protected by the object store's own access
// control and by nothing else, so shredding it means deleting the object
// rather than destroying a key. The sweeper does both and says which.
type Null struct{}

func (Null) Name() string  { return NullName }
func (Null) KeyID() string { return "" }

func (Null) Begin(context.Context) ([]byte, []byte, error) {
	// Zero-length, not nil, and the distinction reaches the database: a NULL
	// wrapped key means the key was DESTROYED, and an empty one means there
	// never was a key. A schema constraint depends on that difference.
	return []byte{}, []byte{}, nil
}

func (Null) Release(_ context.Context, wrapped []byte) ([]byte, error) {
	if wrapped == nil {
		return nil, ErrShredded
	}
	return []byte{}, nil
}

// Compile-time proof that the null implementation is a Codec, so the seam
// cannot drift from its only implementation without a build failure.
var _ Codec = Null{}
