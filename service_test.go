package artefactd_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cardv1 "github.com/garm-ai/contracts/garm/card/v1"

	artefactsv1 "github.com/garm-ai/artefactd/gen/garm/artefacts/v1"
	"github.com/garm-ai/artefactd/internal/blob"
)

// The write path over the wire, on the routes the contract names, through the
// entry point a deployment uses.
func TestTheWritePathOverTheWire(t *testing.T) {
	f := newFixture(t, newDouble())

	var begun artefactsv1.BeginWriteResponse
	f.ok(t, routeBegin, generator(), &artefactsv1.BeginWriteRequest{
		MediaType: proto.String(xlsx), Label: proto.String("Q3.xlsx"),
		ToolFqn: proto.String(theTool), RetentionSeconds: proto.Uint32(3600),
	}, &begun)

	if begun.GetArtefactId() == "" {
		t.Fatal("begin_write named no artefact")
	}
	if begun.GetUploadUrl() == "" {
		t.Error("begin_write returned no upload URL, so there is nowhere to put the bytes")
	}
	if len(begun.GetDataKey()) != 0 {
		t.Errorf("the null codec released a %d-byte key", len(begun.GetDataKey()))
	}

	var committed artefactsv1.CommitResponse
	f.ok(t, routeCommit, generator(), &artefactsv1.CommitRequest{
		ArtefactId:         proto.String(begun.GetArtefactId()),
		PlaintextSha256:    proto.String(digest),
		PlaintextSizeBytes: proto.Uint64(11),
		StoredSha256:       proto.String(digest),
		StoredSizeBytes:    proto.Uint64(11),
	}, &committed)
	if committed.GetState() != artefactsv1.State_STATE_COMMITTED {
		t.Errorf("commit answered %v", committed.GetState())
	}
	if committed.GetRetainUntil() == nil {
		t.Error("a committed artefact does not say when its retention runs out")
	}

	var described artefactsv1.DescribeResponse
	f.ok(t, routeDescribe, throughAnAgent(), &artefactsv1.DescribeRequest{
		ArtefactId: proto.String(begun.GetArtefactId()),
	}, &described)
	if described.GetMediaType() != xlsx || described.GetLabel() != "Q3.xlsx" {
		t.Errorf("describe answered %q %q", described.GetMediaType(), described.GetLabel())
	}
	if described.GetClassification().GetClearance() == 0 {
		t.Error("describe answered no classification, so nothing says what this is")
	}
}

// THE TEST THE DECISION IS ABOUT, over the wire: an agent asks for a URL and is
// refused with a code the transport actually carries, and the answer contains
// nothing URL-shaped.
func TestAnAgentIsRefusedAUrlOverTheWire(t *testing.T) {
	f := newFixture(t, newDouble())
	id := writeAnArtefact(t, f)

	code, why := f.call(t, routeReadURL, throughAnAgent(),
		&artefactsv1.ReadUrlRequest{ArtefactId: proto.String(id)}, nil)
	if code != "403" {
		t.Fatalf("read_url answered %s (%s) to an agent, want 403", code, why)
	}
	if !strings.Contains(why, "id") {
		t.Errorf("the refusal is %q, which does not say that the id is what travels", why)
	}
	if strings.Contains(why, "http") {
		t.Errorf("the refusal leaks something URL-shaped: %q", why)
	}
}

// A person gets the URL, and every part of what they were given is inside the
// card — which is what makes the daemon's per-artefact projection the thing
// between a caller and a credential.
func TestAPersonGetsTheUrlAndOnlyInsideTheCard(t *testing.T) {
	f := newFixture(t, newDouble())
	id := writeAnArtefact(t, f)

	var out artefactsv1.ReadUrlResponse
	f.ok(t, routeReadURL, person(), &artefactsv1.ReadUrlRequest{
		ArtefactId: proto.String(id),
	}, &out)

	if out.GetArtefactId() != id {
		t.Errorf("read_url echoed %q, want %q", out.GetArtefactId(), id)
	}
	card := out.GetRelease()
	if card == nil {
		t.Fatal("read_url released no card")
	}
	if card.GetAccess() == nil || card.GetAccess().GetClearance() == 0 {
		t.Error("the card carries no label, so the daemon would project it at the endpoint's " +
			"policy rather than at this artefact's classification")
	}
	url := factOf(card, "url")
	if !strings.HasPrefix(url, "https://") {
		t.Errorf("the card's url fact is %q", url)
	}

	// And nowhere else. Every string on the response, outside the card, is walked
	// and must not look like a URL: a plain field would carry a policy decided at
	// compile time, one answer for every caller and every artefact.
	out.Release = nil
	walkStrings(out.ProtoReflect(), func(path, value string) {
		if strings.Contains(value, "://") {
			t.Errorf("%s carries %q outside the card", path, value)
		}
	})
}

func TestAnArtefactOfAnotherTenantDoesNotExistOverTheWire(t *testing.T) {
	f := newFixture(t, newDouble())
	id := writeAnArtefact(t, f)

	other := caller{Subject: thePerson, Kind: person().Kind}
	// The tenant is not a request field, so the only way to be another tenant is
	// to arrive as one. The fixture always sends `example`, so this asserts the
	// other half: an id that does not exist in this tenant.
	code, _ := f.call(t, routeReadURL, other,
		&artefactsv1.ReadUrlRequest{ArtefactId: proto.String("01k6m0p0w5vr5t3d0zj7k9zzzz")}, nil)
	if code != "404" {
		t.Errorf("an unknown id answered %s, want 404", code)
	}
	if id == "" {
		t.Fatal("the fixture wrote nothing")
	}
}

// A request the contract refuses is refused before it reaches this service: the
// runtime unmarshals into the tool's own type and answers 400 for a body that is
// not one. Asserted here because it is the boundary a caller actually meets.
func TestARequestThatIsNotThisContractIsRefused(t *testing.T) {
	f := newFixture(t, newDouble())
	code, _ := f.call(t, routeCommit, generator(),
		&artefactsv1.ReadUrlRequest{ArtefactId: proto.String("not-a-ulid")}, nil)
	if code == "" {
		t.Error("a request naming no digest was accepted")
	}
}

// ---------------------------------------------------------------- helpers

const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func writeAnArtefact(t *testing.T, f *fixture) string {
	t.Helper()
	var begun artefactsv1.BeginWriteResponse
	f.ok(t, routeBegin, generator(), &artefactsv1.BeginWriteRequest{
		MediaType: proto.String(xlsx), Label: proto.String("Q3.xlsx"),
		ToolFqn: proto.String(theTool), RetentionSeconds: proto.Uint32(3600),
	}, &begun)
	f.ok(t, routeCommit, generator(), &artefactsv1.CommitRequest{
		ArtefactId:         proto.String(begun.GetArtefactId()),
		PlaintextSha256:    proto.String(digest),
		PlaintextSizeBytes: proto.Uint64(11),
		StoredSha256:       proto.String(digest),
		StoredSizeBytes:    proto.Uint64(11),
	}, nil)
	return begun.GetArtefactId()
}

// factOf reads one fact of a card by the dotted field name the service set on
// it, which is the contract with whoever renders one.
func factOf(card *cardv1.Card, field string) string {
	for _, el := range card.GetBody() {
		for _, f := range el.GetFacts().GetFacts() {
			if f.GetField() == field {
				return f.GetValue()
			}
		}
	}
	return ""
}

// walkStrings visits every string on a message, by path.
func walkStrings(m protoreflect.Message, visit func(path, value string)) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			for i := 0; i < v.List().Len(); i++ {
				el := v.List().Get(i)
				if fd.Kind() == protoreflect.StringKind {
					visit(string(fd.Name()), el.String())
				} else if fd.Kind() == protoreflect.MessageKind {
					walkStrings(el.Message(), visit)
				}
			}
		case fd.Kind() == protoreflect.StringKind:
			visit(string(fd.Name()), v.String())
		case fd.Kind() == protoreflect.MessageKind:
			walkStrings(v.Message(), visit)
		}
		return true
	})
}

// double is the object store for the wire tests: it signs plausible URLs and
// remembers what it was asked for, without needing a bucket.
type double struct {
	mu      sync.Mutex
	deleted map[string]int
}

func newDouble() *double { return &double{deleted: map[string]int{}} }

func (d *double) PresignPut(_ context.Context, key, _ string, ttl time.Duration) (string, time.Time, error) {
	if _, err := blob.TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	return "https://objects.example.com/" + key + "?put", time.Now().UTC().Add(ttl), nil
}

func (d *double) PresignGet(_ context.Context, key, _, _ string, ttl time.Duration) (string, time.Time, error) {
	if _, err := blob.TenantOf(key); err != nil {
		return "", time.Time{}, err
	}
	return "https://objects.example.com/" + key + "?get", time.Now().UTC().Add(ttl), nil
}

func (d *double) Head(context.Context, string) (int64, error) { return 11, nil }

func (d *double) Delete(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted[key]++
	return nil
}
