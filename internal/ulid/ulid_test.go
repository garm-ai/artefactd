package ulid_test

import (
	"testing"
	"time"

	"github.com/garm-ai/artefactd/internal/ulid"
)

func TestAnIdIsTwentySixValidCharacters(t *testing.T) {
	id := ulid.NewNow()
	if len(id) != 26 {
		t.Fatalf("%q is %d characters", id, len(id))
	}
	if !ulid.Valid(id) {
		t.Errorf("%q is not valid", id)
	}
}

// The queue's index is the primary key, which only works if a later task
// sorts after an earlier one as a string.
func TestIdsSortByTime(t *testing.T) {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	first := ulid.New(base)
	second := ulid.New(base.Add(time.Millisecond))
	if !(first < second) {
		t.Errorf("%q does not sort before %q", first, second)
	}
}

func TestTwoIdsOfOneInstantDiffer(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if ulid.New(at) == ulid.New(at) {
		t.Error("two ids of the same millisecond are identical")
	}
}

func TestAnIdThatIsNotOneIsRefused(t *testing.T) {
	for _, s := range []string{"", "short", "UPPERCASE0000000000000000",
		"iiiiiiiiiiiiiiiiiiiiiiiiii", "0123456789012345678901234567"} {
		if ulid.Valid(s) {
			t.Errorf("%q was accepted", s)
		}
	}
}
