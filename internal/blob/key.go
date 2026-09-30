// Package blob is the object store: the key namespace and the presigned URLs.
//
// One bucket, and tenancy is a prefix this service enforces. A bucket per
// tenant with its own credential is stronger in principle, and it was
// rejected because SeaweedFS's identity configuration is static — onboarding
// a tenant would mean editing the object store — and because the blast radius
// is bounded anyway: a presigned URL is per-object, so there is no bulk-access
// capability a mistake could hand out.
//
// What is left is this file. A path-construction bug crosses tenants, and it
// is the one failure in the whole design that would, so the key is built by
// one function with a property test over it rather than by string
// concatenation at four call sites.
package blob

import (
	"errors"
	"fmt"
	"strings"
)

// Separator is the one character that structures a key, and therefore the one
// character no component may contain.
const Separator = "/"

// Components is how many parts a key has: the tenant, the run, the artefact.
const Components = 3

var (
	// ErrEmptyComponent is a component that is missing or blank.
	ErrEmptyComponent = errors.New("blob: a key component is empty")
	// ErrUnsafeComponent is a component that would change what the key means:
	// a separator, a traversal, a control character, a percent escape.
	ErrUnsafeComponent = errors.New("blob: a key component is not safe in a key")
)

// maxComponent bounds one component. S3 keys may be 1024 bytes; three
// components at this bound plus two separators leaves room and makes a
// runaway tenant id a refusal rather than a truncation somewhere downstream.
const maxComponent = 200

// Key is the object key for one artefact: <tenant>/<run>/<artefact_id>.
//
// It is the only place a key is built. Every component is checked before it is
// joined, and a component that cannot be a component is an error rather than
// something sanitised into a different one — a key quietly rewritten is a key
// that no longer addresses the object somebody else's row points at.
//
// What the checks are for, in the order they matter:
//
//   - A component containing "/" would add a level, so `tenant = "a/b"` and
//     `tenant = "a", run = "b"` would collide. That is the cross-tenant bug.
//   - A component of "." or ".." would let a key resolve above its own
//     prefix. S3 has no path resolution, but SeaweedFS is a filer, an object
//     store may be fronted by one, and a signed URL is eventually a path on
//     something.
//   - A backslash, a control character or a percent sign are the shapes that
//     survive one layer of decoding and become a separator in the next.
//   - Leading or trailing whitespace makes two tenants that look identical.
func Key(tenant, run, artefactID string) (string, error) {
	for name, part := range map[string]string{
		"tenant": tenant, "run": run, "artefact id": artefactID,
	} {
		if err := checkComponent(part); err != nil {
			return "", fmt.Errorf("%w: %s %q", err, name, part)
		}
	}
	return strings.Join([]string{tenant, run, artefactID}, Separator), nil
}

// TenantOf reads the tenant back off a key, and refuses a key this package
// did not build.
//
// It exists so that "the prefix is the tenant" is a claim something can check
// rather than a comment: the service asserts Key and TenantOf round-trip
// before it hands a key to the object store.
func TenantOf(key string) (string, error) {
	parts := strings.Split(key, Separator)
	if len(parts) != Components {
		return "", fmt.Errorf("%w: a key has %d components, this one has %d",
			ErrUnsafeComponent, Components, len(parts))
	}
	for _, p := range parts {
		if err := checkComponent(p); err != nil {
			return "", err
		}
	}
	return parts[0], nil
}

func checkComponent(s string) error {
	if s == "" || strings.TrimSpace(s) == "" {
		return ErrEmptyComponent
	}
	if len(s) > maxComponent {
		return fmt.Errorf("%w: longer than %d bytes", ErrUnsafeComponent, maxComponent)
	}
	if s != strings.TrimSpace(s) {
		return ErrUnsafeComponent
	}
	// Any occurrence of "..", not only a component that IS one. A component
	// containing one cannot traverse without a separator, and refusing it anyway
	// costs nothing — no tenant, run id or ULID contains a double dot — while
	// making the property the test asserts one sentence instead of three.
	if s == "." || strings.Contains(s, "..") {
		return ErrUnsafeComponent
	}
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			return ErrUnsafeComponent
		case r == '%':
			// A percent escape is a separator one decoding away.
			return ErrUnsafeComponent
		case r < 0x20 || r == 0x7f:
			return ErrUnsafeComponent
		}
	}
	// A component is what this platform's identifiers already are: the tenant
	// and the run id come off the invocation, and the artefact id is a ULID.
	// An allowlist rather than a denylist, because the interesting inputs are
	// the ones nobody thought of.
	if strings.IndexFunc(s, notAllowed) >= 0 {
		return ErrUnsafeComponent
	}
	return nil
}

// notAllowed reports whether a rune is outside the set a key component may
// use: ASCII letters and digits, and the four punctuation marks garm's own
// identifiers contain.
func notAllowed(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case r == '-', r == '_', r == '.', r == ':':
		return false
	default:
		return true
	}
}
