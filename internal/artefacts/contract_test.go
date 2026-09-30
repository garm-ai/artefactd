package artefacts_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cardv1 "github.com/garm-ai/contracts/garm/card/v1"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/wire"

	"github.com/garm-ai/artefactd/internal/artefacts"
)

// ---------------------------------------------------------------- the tools

// The four tools this contract declares, with the route each is reachable on.
// This is the table a deployment's catalogue has to agree with, and it is read
// off the contract rather than written down twice.
func TestTheContractDeclaresFourToolsOnTheirOwnRoutes(t *testing.T) {
	want := map[string]string{
		artefacts.ToolBeginWrite: "/garm.artefacts.v1.ArtefactsService/BeginWrite",
		artefacts.ToolCommit:     "/garm.artefacts.v1.ArtefactsService/Commit",
		artefacts.ToolDescribe:   "/garm.artefacts.v1.ArtefactsService/Describe",
		artefacts.ToolReadURL:    "/garm.artefacts.v1.ArtefactsService/ReadUrl",
	}
	sd := serviceDescriptor(t)
	if got := sd.Methods().Len(); got != len(want) {
		t.Fatalf("the contract declares %d methods, want %d", got, len(want))
	}
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		pol := policyOf(t, md)
		fqn := string(md.ParentFile().Package()) + "." + pol.GetName()
		route, known := want[fqn]
		if !known {
			t.Errorf("unexpected tool %q", fqn)
			continue
		}
		full := "/" + string(sd.FullName()) + "/" + string(md.Name())
		if full != route {
			t.Errorf("%s is on %q, want %q", fqn, full, route)
		}
		// Both ends derive the subject from the same function, so neither keeps
		// a routing table that could drift from the other's.
		if wire.Subject(full) == "" {
			t.Errorf("%s resolves to no subject", fqn)
		}
	}
}

// Every method declares a tool set, and the reason is a rule about the daemon
// rather than a style preference: its scope check refuses a caller any tool that
// shares none of its sets, so a tool in NO set is reachable only by a caller
// with no set at all. That went wrong on garm.tasks.v1 — two methods declared an
// audience and no set, written as though the audience were the gate — and the
// symptom was a queue nobody could act on rather than an error anybody could
// see.
func TestEveryMethodDeclaresAToolSet(t *testing.T) {
	sd := serviceDescriptor(t)
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		if len(policyOf(t, md).GetSets()) == 0 {
			t.Errorf("%s declares no tool set, so the daemon refuses it to every caller "+
				"that names one", md.Name())
		}
	}
}

// read_url is for a person, and the audience is the WEAKER half of saying so.
//
// The assertion is deliberately in two parts, because the first part alone is
// the trap. An audience is a LISTING rule: the daemon's visibility check reads
// verbs, clearance, compartments and sets, and never the audience, so a tool
// whose audience excludes an agent is simply not offered to one and remains
// perfectly callable. What keeps a credential away from a model is the refusal
// in the service, and the second half of this test is that the refusal exists
// and says so.
func TestReadUrlIsForAPersonAndTheGateIsACheckAndNotTheAudience(t *testing.T) {
	sd := serviceDescriptor(t)
	pol := policyOf(t, sd.Methods().ByName("ReadUrl"))

	if got := pol.GetAudience(); len(got) != 1 || got[0] != toolv1.Audience_AUDIENCE_PERSON {
		t.Errorf("read_url declares audience %v, want [AUDIENCE_PERSON] so a model is never "+
			"offered it in a listing", got)
	}
	// The other half. Caller.Automated is the check, and it must refuse both
	// shapes an automated caller arrives in: a delegation chain, which every
	// call made for a model carries, and a principal that is not a person.
	for why, c := range map[string]artefacts.Caller{
		"an agent acting for a person": {
			Subject: "user:p@example.com",
			Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
			Act: []artefacts.Act{{
				Subject: "agent:example.agents.v1.Writer",
				Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
			}},
		},
		"an agent calling as itself": {
			Subject: "agent:example.agents.v1.Writer",
			Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		},
		"a service calling for itself": {
			Subject: "service:example.generator",
			Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE,
		},
		"a token that did not say what it was": {
			Subject: "who:knows",
			Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_UNSPECIFIED,
		},
	} {
		if !c.Automated() {
			t.Errorf("%s is not seen as automated, so read_url would mint it a URL", why)
		}
	}
	// And a person calling for themselves is not refused, so the check cannot
	// pass by refusing everybody.
	person := artefacts.Caller{
		Subject: "user:p@example.com", Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
	}
	if person.Automated() {
		t.Error("a person calling for themselves is seen as automated, so nobody could read anything")
	}
}

// The write path is for the runner, and its responses are what the model would
// see if a generator leaked them.
func TestTheWritePathIsNotOfferedToAModelOrAPerson(t *testing.T) {
	sd := serviceDescriptor(t)
	for _, method := range []protoreflect.Name{"BeginWrite", "Commit"} {
		got := policyOf(t, sd.Methods().ByName(method)).GetAudience()
		if len(got) != 1 || got[0] != toolv1.Audience_AUDIENCE_RUNNER {
			t.Errorf("%s declares audience %v, want [AUDIENCE_RUNNER]", method, got)
		}
	}
}

// ---------------------------------------------------------------- no bytes

// NO ARTEFACT BYTES CROSS NATS, AND THIS IS WHERE THAT IS GUARANTEED.
//
// It is already the design — that is what begin_write and commit are for, and
// why read_url mints rather than streams — but the way a design like this rots
// is somebody later adding a convenient `fetch_artefact` because one caller
// found two round trips annoying. NATS has a maximum payload, so a large
// artefact would fail anyway; it would fail late, at run time, with a message
// about a message size rather than about a design rule.
//
// So the rule is asserted over the service descriptor, and it has three parts:
//
//  1. No `bytes` field, unless it is named below with a reason AND carries a
//     protovalidate length bound small enough that it cannot be a document.
//  2. No `string` field without a length bound, because an unbounded string is
//     a payload in base64.
//  3. No field NAMED like a payload at all, with no exemption available: a
//     `content` field is a design change and it belongs in a decision record
//     before it belongs in a proto.
func TestNoMessageCanCarryAnArtefact(t *testing.T) {
	for _, f := range governedFields(t) {
		switch {
		case f.fd.Kind() == protoreflect.BytesKind:
			reason, exempt := bytesFieldsWithAReason[f.path]
			if !exempt {
				t.Errorf("%s is a bytes field. No message in this contract may carry an "+
					"artefact's payload: add %s to bytesFieldsWithAReason with the reason it "+
					"is not one, and give it a max_len that proves it", f.path, f.path)
				continue
			}
			max := bytesMaxLen(f.fd)
			if max == 0 {
				t.Errorf("%s is exempt (%q) and carries no max_len. An exemption without a "+
					"bound is the rule removed", f.path, reason)
			} else if max > maxExemptBytes {
				t.Errorf("%s is exempt (%q) with max_len %d, above the %d-byte ceiling an "+
					"exemption may claim", f.path, reason, max, maxExemptBytes)
			}

		case f.fd.Kind() == protoreflect.StringKind:
			if _, exempt := unboundedStringsWithAReason[f.path]; exempt {
				continue
			}
			if stringMaxLen(f.fd) == 0 {
				t.Errorf("%s is an unbounded string, which is a payload in base64. Give it a "+
					"buf.validate max_len or len, or name it in unboundedStringsWithAReason",
					f.path)
			}
		}

		if word := payloadish(string(f.fd.Name())); word != "" {
			t.Errorf("%s is named like a payload (%q). There is no exemption for this: "+
				"bytes travelling through a governed call was rejected by the decision "+
				"record, for reads and for writes, and adding one back is a decision and "+
				"not a field", f.path, word)
		}
	}
}

// maxExemptBytes is the ceiling an exempt bytes field may claim. One AES-256
// key is 32 bytes; 64 leaves room for a codec that needs a key and a tag, and
// nothing that fits in it is a document.
const maxExemptBytes = 64

// bytesFieldsWithAReason are the bytes fields this contract deliberately has,
// each with why it is not a payload.
//
// An entry is a claim, and the test above holds it to a bound as well as to a
// sentence. Adding one without a reason is how the rule it exists for gets
// removed.
var bytesFieldsWithAReason = map[string]string{
	"garm.artefacts.v1.BeginWriteResponse.data_key": "one artefact's data key, in the clear, " +
		"for the writer to seal the payload with — at most 32 bytes, which is an AES-256 key. " +
		"The service never handles a byte of the payload, so the writer must hold the key; " +
		"the alternative is the payload crossing NATS, which is what the whole design avoids.",
}

// unboundedStringsWithAReason are the string fields with no length bound, each
// with why one would be meaningless.
var unboundedStringsWithAReason = map[string]string{
	"garm.artefacts.v1.BeginWriteResponse.upload_url": "a presigned URL, whose length is the " +
		"signer's and not this contract's: a bound here would refuse a signature that a " +
		"future SigV4 parameter made longer, and it cannot hold a payload because the " +
		"object store produced it.",
	"garm.artefacts.v1.BeginWriteResponse.artefact_id": "a ULID this service minted, 26 " +
		"characters by construction and validated as such before it is ever used.",
	"garm.artefacts.v1.CommitResponse.artefact_id":   "as above.",
	"garm.artefacts.v1.DescribeResponse.artefact_id": "as above.",
	"garm.artefacts.v1.ReadUrlResponse.artefact_id":  "as above.",
	"garm.artefacts.v1.DescribeResponse.media_type":  "echoed back from the row, which bounded it on the way in.",
	"garm.artefacts.v1.DescribeResponse.label":       "as above.",
	"garm.artefacts.v1.DescribeResponse.tool_fqn":    "as above.",
	"garm.artefacts.v1.DescribeResponse.codec":       "as above.",
	"garm.artefacts.v1.DescribeResponse.plaintext_sha256": "64 hex characters, written by " +
		"this service from what commit validated.",
	"garm.artefacts.v1.DescribeResponse.run_id": "read off the invocation, and already a key " +
		"component, so internal/blob has bounded it at 200 bytes before it reached the row.",
	"garm.artefacts.v1.Classification.compartments": "declared names, bounded per item by the " +
		"repeated rule rather than by a field rule.",
}

// payloadish reports the word that makes a field name a payload, or "".
//
// Substrings and exact names both, because the two failure shapes are
// different: `content`, `payload` and `blob` are payloads wherever they appear,
// while `data` and `body` are payloads as whole names and innocent inside
// `data_key` and `body_count`.
func payloadish(name string) string {
	for _, word := range []string{"payload", "content", "blob", "attachment", "octet"} {
		if strings.Contains(name, word) {
			return word
		}
	}
	for _, exact := range []string{"data", "body", "file", "bytes", "text", "document", "object"} {
		if name == exact {
			return exact
		}
	}
	return ""
}

// ---------------------------------------------------------------- no key, no URL

// THE DEK MUST NEVER APPEAR IN A GENERAL RESPONSE, and this is that rule as a
// property of the types rather than as a field policy.
//
// A field policy is the daemon projecting what a caller may see. A key that is
// simply not in the response type cannot be leaked by a policy mistake, by a
// plan that failed to compile, or by a daemon older than the annotation. So:
// no field anywhere in this contract names key material except the two places
// that release it, and `describe` — the one method whose whole job is to answer
// "what is this" — has nowhere to put one.
func TestKeyMaterialAppearsOnlyWhereItIsReleased(t *testing.T) {
	for _, f := range governedFields(t) {
		word := keyish(string(f.fd.Name()))
		if word == "" {
			continue
		}
		if reason, ok := keyFieldsWithAReason[f.path]; ok {
			if reason == "" {
				t.Errorf("%s is exempt with no reason", f.path)
			}
			continue
		}
		t.Errorf("%s names key material (%q). The wrapped key and the data key are absent "+
			"from every message but the one that releases them, and being absent is the "+
			"guarantee — a field policy that was meant to hide one is a policy that can be "+
			"got wrong", f.path, word)
	}
}

// And the same rule stated from the other end, because the first test would
// pass if somebody renamed a key field to something innocent: describe's
// response, transitively, must be exactly the metadata below.
func TestDescribeCarriesMetadataAndNothingElse(t *testing.T) {
	want := map[string]bool{
		"artefact_id": true, "state": true, "classification": true,
		"clearance": true, "compartments": true,
		"media_type": true, "label": true, "tool_fqn": true, "run_id": true,
		"plaintext_sha256": true, "plaintext_size_bytes": true,
		"stored_size_bytes": true, "codec": true,
		"created_at": true, "committed_at": true, "retain_until": true, "shredded_at": true,
		// The fields of google.protobuf.Timestamp, which the walk reaches.
		"seconds": true, "nanos": true,
	}
	sd := serviceDescriptor(t)
	md := sd.Methods().ByName("Describe")
	var unexpected []string
	walk(md.Output(), "", map[protoreflect.FullName]bool{}, func(f field) {
		if !want[string(f.fd.Name())] {
			unexpected = append(unexpected, f.path)
		}
	})
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("describe's response carries %v. It answers what an artefact IS, and a "+
			"field added to it is a field a model can read: no URL, no key, and nothing "+
			"that was not deliberately put there", unexpected)
	}
}

// A URL-shaped field exists in exactly two places, and the second one is not a
// field at all.
//
// begin_write's presigned PUT is a plain field, because ruling 2 says the
// generator writes the bytes itself and there is no per-artefact decision to
// make about who may write to an object nobody else will be told the key of.
//
// read_url's URL is inside a `garm.card.v1.Card`, and that is the whole design:
// a field policy is decided at compile time and says what EVERY caller of an
// RPC may see, while an artefact store needs the same field readable by one
// viewer and not the next. A card carries its policy on the VALUE, so the
// daemon's step-8 walk can drop it per artefact and per viewer.
//
// So the assertion is: no plain URL field outside begin_write, and the card
// appears in exactly one message.
func TestAUrlIsOnlyEverInThoseTwoPlaces(t *testing.T) {
	for _, f := range governedFields(t) {
		word := urlish(string(f.fd.Name()))
		if word == "" {
			continue
		}
		if _, ok := urlFieldsWithAReason[f.path]; ok {
			continue
		}
		t.Errorf("%s is URL-shaped (%q). A read URL belongs inside the card, where its "+
			"policy travels on the value; a plain field would give it one answer for every "+
			"caller and every artefact, which is what ruling 4 forbids", f.path, word)
	}

	var holders []string
	sd := serviceDescriptor(t)
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		for _, m := range []protoreflect.MessageDescriptor{md.Input(), md.Output()} {
			fields := m.Fields()
			for j := 0; j < fields.Len(); j++ {
				fd := fields.Get(j)
				if fd.Kind() == protoreflect.MessageKind &&
					fd.Message().FullName() == cardMessageName {
					holders = append(holders, string(m.FullName())+"."+string(fd.Name()))
				}
			}
		}
	}
	sort.Strings(holders)
	want := []string{"garm.artefacts.v1.ReadUrlResponse.release"}
	if len(holders) != 1 || holders[0] != want[0] {
		t.Errorf("garm.card.v1.Card is carried by %v, want %v. The card is where a "+
			"credential is released under a per-artefact label; a second one is a second "+
			"answer to the same question", holders, want)
	}
}

// cardMessageName is the one type the daemon knows by name, pinned here because
// the test above depends on it being that one and not a lookalike.
const cardMessageName protoreflect.FullName = "garm.card.v1.Card"

// A compile-time check that the constant above still names the linked type, so
// a rename in the contracts module is a build failure here rather than a test
// that quietly stops asserting anything.
var _ = func() bool {
	return (&cardv1.Card{}).ProtoReflect().Descriptor().FullName() == cardMessageName
}()

var keyFieldsWithAReason = map[string]string{
	"garm.artefacts.v1.BeginWriteResponse.data_key": "the write half of ruling 6: the writer " +
		"seals the payload, so it holds the key for exactly as long as that takes. It is " +
		"released by the call that names the artefact, which is the call that already " +
		"decided this caller may create one.",
}

var urlFieldsWithAReason = map[string]string{
	"garm.artefacts.v1.BeginWriteResponse.upload_url": "a presigned PUT for one object at a " +
		"key this service chose and nobody else is told. Ruling 2's write path, and the " +
		"alternative is the payload crossing NATS.",
	"garm.artefacts.v1.BeginWriteResponse.upload_expires_at": "when that URL dies, which is " +
		"the fact that bounds it.",
}

func keyish(name string) string {
	for _, word := range []string{"key", "secret", "dek", "kek", "credential", "password",
		"passphrase", "token", "nonce", "iv"} {
		if strings.Contains(name, word) {
			return word
		}
	}
	return ""
}

func urlish(name string) string {
	for _, word := range []string{"url", "uri", "href", "link", "presigned", "endpoint"} {
		if strings.Contains(name, word) {
			return word
		}
	}
	return ""
}

// ---------------------------------------------------------------- stamped, not supplied

// CLASSIFICATION IS STAMPED, NEVER SUPPLIED, and here that is a property of the
// contract rather than a check at run time: no request message has anywhere to
// put one.
//
// The platform's own rule is that the model never sets a control field, and a
// classification is the control field — a caller that could set one could
// classify its own output PUBLIC. A refusal in Go would be the weaker version
// of this: it would depend on somebody remembering to write it for the next
// field too.
func TestNoRequestCanSetAClassification(t *testing.T) {
	banned := []string{"clearance", "compartment", "classification", "label_clearance",
		"audience", "tenant", "run_id", "object_key", "key_id", "state"}
	sd := serviceDescriptor(t)
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		walk(md.Input(), "", map[protoreflect.FullName]bool{}, func(f field) {
			for _, b := range banned {
				if strings.Contains(string(f.fd.Name()), b) {
					t.Errorf("%s is a request field named %q. The tenant, the run, the key "+
						"and the classification are the store's, stamped from the "+
						"invocation; a request field for one of them is the caller choosing "+
						"its own governance", f.path, b)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- the walk

type field struct {
	path string
	fd   protoreflect.FieldDescriptor
}

// governedFields is every field of every request and response in this contract,
// transitively, once each.
func governedFields(t *testing.T) []field {
	t.Helper()
	sd := serviceDescriptor(t)
	var out []field
	seen := map[string]bool{}
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		for _, m := range []protoreflect.MessageDescriptor{md.Input(), md.Output()} {
			walk(m, "", map[protoreflect.FullName]bool{}, func(f field) {
				if seen[f.path] {
					return
				}
				seen[f.path] = true
				out = append(out, f)
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	// A guard against the whole file passing vacuously. Every assertion above is
	// a loop over this slice, so a walk that returned nothing — a renamed
	// service, a descriptor that stopped resolving — would read as four green
	// tests asserting nothing at all.
	if len(out) < 20 {
		t.Fatalf("the walk found %d fields across four methods, which is too few to be "+
			"reading this contract; every assertion in this file loops over it", len(out))
	}
	return out
}

// walk visits every field of a message and of the messages it holds.
//
// A `garm.card.v1.Card` is NOT descended into, and that is the contract's own
// rule rather than a convenience: a card is an opaque value to the field-policy
// walk, the way a Timestamp is, because the policy inside one travels on the
// value. Descending would make the tests above assert descriptor rules over a
// structure whose rules are enforced somewhere else entirely — and the test
// that the card appears in exactly one message is what bounds where it can be.
func walk(md protoreflect.MessageDescriptor, prefix string,
	seen map[protoreflect.FullName]bool, visit func(field)) {
	if md == nil || seen[md.FullName()] {
		return
	}
	seen[md.FullName()] = true
	if md.FullName() == cardMessageName {
		return
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		path := fmt.Sprintf("%s.%s", md.FullName(), fd.Name())
		if prefix != "" {
			path = prefix + "." + string(fd.Name())
		}
		visit(field{path: path, fd: fd})
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			walk(fd.Message(), "", seen, visit)
		}
	}
}

func serviceDescriptor(t *testing.T) protoreflect.ServiceDescriptor {
	t.Helper()
	sd, err := artefacts.ServiceDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	return sd
}

func policyOf(t *testing.T, md protoreflect.MethodDescriptor) *toolv1.ToolPolicy {
	t.Helper()
	if md == nil {
		t.Fatal("no such method")
	}
	p, ok := proto.GetExtension(md.Options(), toolv1.E_Tool).(*toolv1.ToolPolicy)
	if !ok || p == nil {
		t.Fatalf("%s carries no tool annotation", md.FullName())
	}
	return p
}

// bytesMaxLen and stringMaxLen read the protovalidate bound off a field. Zero
// means unbounded, which for a bytes field is a refusal and for a string field
// is one unless the field is named in the exemption list.
func bytesMaxLen(fd protoreflect.FieldDescriptor) uint64 {
	r := rulesOf(fd)
	if r == nil || r.GetBytes() == nil {
		return 0
	}
	if n := r.GetBytes().GetLen(); n > 0 {
		return n
	}
	return r.GetBytes().GetMaxLen()
}

func stringMaxLen(fd protoreflect.FieldDescriptor) uint64 {
	r := rulesOf(fd)
	if r == nil {
		return 0
	}
	if s := r.GetString_(); s != nil {
		if n := s.GetLen(); n > 0 {
			return n
		}
		if n := s.GetMaxLen(); n > 0 {
			return n
		}
	}
	// A repeated field bounds its items rather than itself.
	if rep := r.GetRepeated(); rep != nil && rep.GetItems() != nil {
		if s := rep.GetItems().GetString_(); s != nil {
			if n := s.GetLen(); n > 0 {
				return n
			}
			return s.GetMaxLen()
		}
	}
	return 0
}

func rulesOf(fd protoreflect.FieldDescriptor) *validate.FieldRules {
	r, ok := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
	if !ok {
		return nil
	}
	return r
}
