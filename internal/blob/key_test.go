package blob_test

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/garm-ai/artefactd/internal/blob"
)

// The property, and it is the one thing in this design that could cross a
// tenant: whatever Key is given, either it refuses, or the key it returns has
// exactly three components whose FIRST is the tenant it was handed, byte for
// byte.
//
// There is no third outcome. A key that resolved above its prefix, a
// component that gained a separator, a tenant that was trimmed into a
// different tenant — each of those is a violation of this one sentence, which
// is why it is asserted as a property rather than as a list of cases somebody
// thought of.
func TestKeyEitherRefusesOrKeepsTheTenantAsItsFirstComponent(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		tenant, run, id := adversarial(r), adversarial(r), adversarial(r)
		key, err := blob.Key(tenant, run, id)
		if err != nil {
			if key != "" {
				t.Fatalf("Key(%q,%q,%q) refused and still returned %q", tenant, run, id, key)
			}
			continue
		}
		parts := strings.Split(key, blob.Separator)
		if len(parts) != blob.Components {
			t.Fatalf("Key(%q,%q,%q) = %q has %d components, want %d",
				tenant, run, id, key, len(parts), blob.Components)
		}
		if parts[0] != tenant {
			t.Fatalf("Key(%q,%q,%q) = %q: first component is %q, want the tenant",
				tenant, run, id, key, parts[0])
		}
		if parts[1] != run || parts[2] != id {
			t.Fatalf("Key(%q,%q,%q) = %q did not preserve its components", tenant, run, id, key)
		}
		// The prefix property stated the way the service uses it: everything
		// under a tenant's prefix, and nothing else, is that tenant's.
		if !strings.HasPrefix(key, tenant+blob.Separator) {
			t.Fatalf("Key(%q,%q,%q) = %q is not under the tenant's prefix", tenant, run, id, key)
		}
		got, err := blob.TenantOf(key)
		if err != nil {
			t.Fatalf("TenantOf(%q) refused a key Key built: %v", key, err)
		}
		if got != tenant {
			t.Fatalf("TenantOf(%q) = %q, want %q", key, got, tenant)
		}
		if strings.Contains(key, "//") || strings.Contains(key, "..") {
			t.Fatalf("Key(%q,%q,%q) = %q contains a traversal", tenant, run, id, key)
		}
	}
}

// The cases somebody did think of, named so a failure says which idea broke
// rather than printing a random string.
func TestKeyRefusesTheShapesThatWouldCrossATenant(t *testing.T) {
	cases := map[string][3]string{
		"a separator in the tenant":        {"a/b", "run", "01hq"},
		"a separator in the run":           {"a", "r/../other", "01hq"},
		"a separator in the id":            {"a", "run", "01hq/../../etc"},
		"a traversal as the whole tenant":  {"..", "run", "01hq"},
		"a traversal as the whole run":     {"a", "..", "01hq"},
		"a single dot":                     {".", "run", "01hq"},
		"a percent escape of a separator":  {"a%2fb", "run", "01hq"},
		"a backslash":                      {"a\\b", "run", "01hq"},
		"an empty tenant":                  {"", "run", "01hq"},
		"a blank tenant":                   {"   ", "run", "01hq"},
		"an empty run":                     {"a", "", "01hq"},
		"an empty id":                      {"a", "run", ""},
		"leading space":                    {" a", "run", "01hq"},
		"trailing space":                   {"a ", "run", "01hq"},
		"a newline":                        {"a\nb", "run", "01hq"},
		"a null byte":                      {"a\x00b", "run", "01hq"},
		"a unicode lookalike separator":    {"a∕b", "run", "01hq"},
		"a right-to-left override":         {"a‮b", "run", "01hq"},
		"a component longer than the cap":  {strings.Repeat("a", 201), "run", "01hq"},
		"a tenant that is only separators": {"//", "run", "01hq"},
	}
	for why, in := range cases {
		if key, err := blob.Key(in[0], in[1], in[2]); err == nil {
			t.Errorf("%s: Key(%q,%q,%q) = %q, want a refusal", why, in[0], in[1], in[2], key)
		}
	}
}

// And the ordinary case, so a refusal-happy implementation cannot pass the
// two tests above by refusing everything.
func TestKeyBuildsTheKeyTheDecisionNames(t *testing.T) {
	key, err := blob.Key("example", "run_01hq", "01k6m0p0w5vr5t3d0zj7k9ab2c")
	if err != nil {
		t.Fatal(err)
	}
	if want := "example/run_01hq/01k6m0p0w5vr5t3d0zj7k9ab2c"; key != want {
		t.Errorf("Key = %q, want %q", key, want)
	}
}

// TenantOf must refuse a key it did not build, because the service uses it as
// an assertion before it hands a key to the object store.
func TestTenantOfRefusesAKeyThisPackageDidNotBuild(t *testing.T) {
	for _, key := range []string{
		"", "example", "example/run", "example/run/id/extra",
		"/example/run/id", "example//id", "example/run/id/",
		"../example/run/id",
	} {
		if tenant, err := blob.TenantOf(key); err == nil {
			t.Errorf("TenantOf(%q) = %q, want a refusal", key, tenant)
		}
	}
}

// adversarial builds the inputs a property test is for: mostly plausible
// identifiers, salted with the fragments that turn a key into a different key.
func adversarial(r *rand.Rand) string {
	fragments := []string{
		"example", "tenant-2", "run_01hq", "01k6m0p0w5vr5t3d0zj7k9ab2c",
		"a", "", " ", ".", "..", "/", "//", "\\", "%2f", "%2F", "..%2f",
		"\n", "\t", "\x00", "\x7f", "∕", "‮", "／",
		strings.Repeat("z", 64), ":", "-", "_", "aB9", "café",
	}
	n := r.IntN(4)
	var b strings.Builder
	for i := 0; i <= n; i++ {
		b.WriteString(fragments[r.IntN(len(fragments))])
	}
	return b.String()
}
