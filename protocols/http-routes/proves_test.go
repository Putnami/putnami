package httproutes

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const provesRequirement = "a-page-route-carries-what-it-proves"

func TestProves_PageRouteProofValidates(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", provesRequirement, "a-page-route-proof-validates")
	data, err := os.ReadFile("fixtures/valid/page-proves.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("page-proves fixture produced diagnostics: %v", diags)
	}
	byPath := map[string][]Proof{}
	for _, route := range manifest.Routes {
		if route.Proves != nil {
			byPath[route.Path] = route.Proves
		}
	}
	want := map[string][]Proof{
		"/w/{workspace}/changes/{change}/code": {
			{Feature: "acme/change-review", Requirement: "tabs-in-mission-order", At: "/w/acme/changes/pr-24/code"},
			{Feature: "acme/change-review", Requirement: "diff-renders-per-file", At: "/w/acme/changes/pr-24/code"},
		},
		"/":               {{Feature: "acme/home", Requirement: "lists-recent-changes", At: "/"}},
		"/docs/{slug...}": {{Feature: "acme/docs", Requirement: "renders-nested-pages", At: "/docs/guides/start"}},
	}
	if !reflect.DeepEqual(byPath, want) {
		t.Fatalf("decoded proves = %v, want %v", byPath, want)
	}
}

func TestProves_MisplacedOrMalformedProofFails(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", provesRequirement, "a-misplaced-or-malformed-proof-fails")
	expectations := loadExpectations(t)
	seen := 0
	for name, code := range expectations.Invalid {
		if !strings.HasPrefix(name, "proves-") {
			continue
		}
		seen++
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures/invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateManifest(data)
			for _, finding := range diags {
				if finding.Code == code {
					return
				}
			}
			t.Fatalf("diagnostics %v do not include %q", diags, code)
		})
	}
	if seen == 0 {
		t.Fatal("no proves fixture in the invalid corpus")
	}

	// A proves list on a route of any other source kind is misplaced too.
	api := Route{
		Match: MatchExact, Path: "/api/health", Methods: []string{"GET", "HEAD"}, PublicEdge: true,
		Provenance: Provenance{Project: "example/web", SourceKind: SourceTypedAPI},
		Proves:     []Proof{{Feature: "acme/health", Requirement: "reports-ready", At: "/api/health"}},
	}
	if !hasCode(ValidateRoutes([]Route{api}), ErrorCodeInvalidProof) {
		t.Fatal("a typed-api route with proves validated")
	}
}

func TestProves_CanonicalAndDigested(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", provesRequirement, "proofs-are-canonical-and-digested")
	base := Route{
		Match: MatchTemplate, Path: "/w/{workspace}", Methods: []string{"HEAD", "GET"}, PublicEdge: true,
		Provenance: Provenance{Project: "example/web", Package: "@putnami/web", SourceKind: SourceFileRoute, EvidencePath: "src/app/w/[workspace]/page.tsx"},
	}
	first := Proof{Feature: "acme/workspace", Requirement: "lists-changes", At: "/w/acme"}
	second := Proof{Feature: "acme/workspace", Requirement: "lists-changes", At: "/w/beta"}
	third := Proof{Feature: "acme/billing", Requirement: "shows-plan", At: "/w/acme"}

	ordered := base
	ordered.Proves = []Proof{third, first, second}
	shuffled := base
	shuffled.Proves = []Proof{second, third, first}
	left, diags := Canonicalize([]Route{ordered})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	right, diags := Canonicalize([]Route{shuffled})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	if left.Digest != right.Digest {
		t.Fatalf("proves order changed the digest: %s != %s", left.Digest, right.Digest)
	}
	if !reflect.DeepEqual(left.Routes[0].Proves, []Proof{third, first, second}) {
		t.Fatalf("canonical proves order = %v", left.Routes[0].Proves)
	}
	if !reflect.DeepEqual(shuffled.Proves, []Proof{second, third, first}) {
		t.Fatal("canonicalize mutated its input")
	}

	bare, diags := Canonicalize([]Route{base})
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	if bare.Digest == left.Digest {
		t.Fatal("the digest does not cover proves")
	}
	body, diags := CanonicalJSON(bare)
	if diag.HasErrors(diags) {
		t.Fatalf("canonical JSON: %v", diags)
	}
	if strings.Contains(string(body), "proves") {
		t.Fatalf("a route without proves serialized the member:\n%s", body)
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, finding := range diags {
		if finding.Code == code {
			return true
		}
	}
	return false
}
