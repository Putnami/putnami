package contract

import (
	"strings"
	"testing"
	"time"
)

// TestMarkerCatalogIsTheMethodV0Set pins the catalog: the twelve markers of
// methods 0.1 to 0.3, and recover.contained-changes, which method 0.4 reads
// in place of recover.small-changes.
func TestMarkerCatalogIsTheMethodV0Set(t *testing.T) {
	if len(MarkerCatalog) != 13 {
		t.Fatalf("the catalog lists %d markers, want 13", len(MarkerCatalog))
	}
	seen := map[string]bool{}
	for _, definition := range MarkerCatalog {
		if seen[definition.ID] {
			t.Fatalf("marker %q is defined twice", definition.ID)
		}
		seen[definition.ID] = true
		if !strings.HasPrefix(definition.ID, string(definition.Step)+".") {
			t.Fatalf("marker %q is filed under step %q", definition.ID, definition.Step)
		}
	}
	if _, ok := LookupMarker("verify.tests"); !ok {
		t.Fatal("LookupMarker(verify.tests) found nothing")
	}
	if _, ok := LookupMarker("verify.unknown"); ok {
		t.Fatal("LookupMarker(verify.unknown) found a definition")
	}
}

func cleanPayload() Payload {
	value := 0.4
	return Payload{
		Meta: PayloadMeta{
			CollectorVersion: "0.1.0",
			MethodVersion:    MethodVersion,
			CollectedAt:      time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC),
			RepoFingerprint:  strings.Repeat("a", 64),
			HeadCommit:       strings.Repeat("b", 40),
		},
		Inventory: Inventory{
			InstructionFiles: []InstructionFile{{Path: "AGENTS.md", Bytes: 10, AgeDays: 3}},
		},
		Areas: []Area{{Name: "checkout", Path: "services/checkout", Source: AreaFromManifest, Authors90d: []string{"0123456789abcdef"}}},
		Markers: []Marker{{
			ID: "bound.cross-area-changes", Area: "checkout", State: MarkerExists, Value: &value,
			Evidence: Evidence{
				Command: `rg -l "from '@acme/checkout/src/" --glob '!services/checkout/**'`,
				Sample:  []string{"apps/storefront/src/cart.tsx:3"},
			},
		}},
	}
}

func TestPrivacyViolationsAcceptsCountsAndRelativePaths(t *testing.T) {
	violations, err := PrivacyViolations(cleanPayload())
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("clean payload reported %v", violations)
	}
}

func TestPrivacyViolationsRefusesContentsIdentitiesAndAbsolutePaths(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Payload)
		want   string
	}{
		{"source line in a sample", func(p *Payload) { p.Markers[0].Evidence.Sample = []string{"const total = sum(items);"} }, "evidence.sample[0]"},
		{"multi-line command", func(p *Payload) { p.Markers[0].Evidence.Command = "git log\nrm -rf ." }, "multi-line text"},
		{"email in a command", func(p *Payload) { p.Markers[0].Evidence.Command = "git log --author=dev@acme.example" }, "email address"},
		{"absolute area path", func(p *Payload) { p.Areas[0].Path = "/Users/dev/acme/services/checkout" }, "absolute path"},
		{"home path in a command", func(p *Payload) { p.Markers[0].Evidence.Command = "rg foo ~/acme" }, "absolute path"},
		{"windows path", func(p *Payload) { p.Inventory.InstructionFiles[0].Path = `C:\acme\AGENTS.md` }, "absolute path"},
		{"path outside the repository", func(p *Payload) { p.Areas[0].Path = "../other/repo" }, "outside the repository"},
		{"code block", func(p *Payload) { p.Areas[0].Name = "func main() { run() }" }, "source-like text"},
		{"file-sized string", func(p *Payload) { p.Inventory.Frameworks = []Tool{{Name: strings.Repeat("x", 600)}} }, "longer than"},
		{"too many locations", func(p *Payload) { p.Markers[0].Evidence.Sample = []string{"a", "b", "c", "d", "e", "f"} }, "more than 5 locations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := cleanPayload()
			tc.mutate(&payload)
			violations, err := PrivacyViolations(payload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(violations, "\n"), tc.want) {
				t.Fatalf("violations %v do not mention %q", violations, tc.want)
			}
		})
	}
}
