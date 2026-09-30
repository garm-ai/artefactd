// Package ulid mints artefact ids.
//
// A ULID is 26 characters of Crockford base32: 48 bits of millisecond
// timestamp and 80 bits of randomness. It sorts by time as a string, which
// is why the queue's index is the primary key and there is no separate
// created_at ordering, and it carries no counter that two processes would
// have to agree on.
//
// Lowercase, because the id appears in an object key, in a URL and in a log
// line, and one spelling is easier to search for than two. It is also the last
// component of the key, which is why internal/blob's allowed character set is a
// superset of this alphabet and not the other way round.
package ulid

import (
	"crypto/rand"
	"strings"
	"time"
)

// crockford is base32 without I, L, O and U, so a transcribed id cannot be
// confused with a digit.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// New mints an id for an instant.
func New(at time.Time) string {
	ms := uint64(at.UTC().UnixMilli())
	var b [16]byte
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand does not fail on any platform this runs on, and an
		// artefact id from a broken source of randomness is worse than a stop:
		// two artefacts sharing an id share an object.
		panic("ulid: " + err.Error())
	}
	return encode(b)
}

// NewNow mints an id for now.
func NewNow() string { return New(time.Now()) }

// Valid reports whether a string has the shape New produces. It is what an id
// arriving from a caller is checked against, so a malformed one is "no such
// artefact" rather than a database round trip — and, because the id is a key
// component, so that nothing shaped like a path reaches internal/blob at all.
func Valid(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(crockford, s[i]) < 0 {
			return false
		}
	}
	return true
}

// encode writes 128 bits as 26 base32 characters, most significant first.
// The first character carries only two bits, which is why a ULID's leading
// character is never above '7'.
func encode(b [16]byte) string {
	out := make([]byte, 26)
	var acc uint64
	bits := 0
	pos := 26
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			pos--
			out[pos] = crockford[acc&31]
			acc >>= 5
			bits -= 5
		}
	}
	if pos > 0 {
		pos--
		out[pos] = crockford[acc&31]
	}
	return string(out)
}
