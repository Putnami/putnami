package agentcontext

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden regenerates the canonical fixtures pinned by
// TestSerialization_CanonicalByteForm. Run `go test -run CanonicalByteForm
// -update` after intentionally changing a sample builder; commit the result.
var updateGolden = flag.Bool("update", false, "regenerate canonical golden fixtures")

// sampleDocument builds a representative document exercising every section and
// type: a full identity/graph, both composition-root kinds, a reference in each
// aggregation section (one infra ref flagged sensitive), two representative
// source ranges, a populated tests section, checked and unchecked docs, a config
// section with operational hints, and full provenance. It is the shared input
// for the byte-stability and canonical-form guards.
func sampleDocument() *Document {
	return &Document{
		Schema:          "https://putnami.dev/schemas/putnami-agent-context.json",
		ProtocolVersion: ProtocolVersion,
		Identity: Identity{
			ID:           "/go/samples/task-api",
			Name:         "go.putnami.dev/examples/task-api",
			Path:         "go/samples/task-api",
			Type:         "",
			Tags:         []string{"go", "e2e"},
			Languages:    []string{"go"},
			Dependencies: []string{"/go/framework/app", "/go/framework/http"},
			Dependents:   []string{"/go/samples/application"},
		},
		CompositionRoots: []CompositionRoot{
			{Kind: RootKindApplicationMain, Path: "go/samples/task-api/main.go", Provenance: "type=application"},
			{Kind: RootKindDescribeEntrypoint, Path: "go/samples/task-api/describe.go", Provenance: "describe-hook"},
		},
		Capabilities: []ArtifactRef{
			{Path: "go/samples/task-api/schema/capabilities.json", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		},
		Contracts: []ArtifactRef{
			{Path: "go/samples/task-api/schema/contracts.json", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		},
		Infra: []ArtifactRef{
			{Path: "go/samples/task-api/infra/requirements.json", Digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333", Kind: "secret", Sensitive: true},
		},
		Migrations: []ArtifactRef{
			{Path: "go/samples/task-api/migrations/bundle.json", Digest: "sha256:4444444444444444444444444444444444444444444444444444444444444444"},
		},
		RepresentativeSources: []SourceRange{
			{
				Path:      "go/samples/task-api/main.go",
				StartLine: 1,
				EndLine:   40,
				Why:       SourceReasonMain,
				Tokens:    TokenEstimate{Estimated: 260, Method: TokenMethodBytesDiv4},
			},
			{
				Path:      "go/samples/task-api/tasks.go",
				StartLine: 12,
				EndLine:   58,
				Why:       SourceReasonCapabilityEvidence,
				Tokens:    TokenEstimate{Estimated: 410, Method: TokenMethodBytesDiv4},
			},
		},
		Tests: &TestsSection{
			Policy: TestPolicyRequire,
			Packs: []PackRef{
				{ID: "putnami.transaction.conformance", CapabilityKinds: []string{"datasource"}, Languages: []string{"go", "typescript"}},
			},
			FixtureDigests: []ArtifactRef{
				{Path: "go/samples/task-api/testdata/fixtures.json", Digest: "sha256:5555555555555555555555555555555555555555555555555555555555555555"},
			},
		},
		Docs: []DocRef{
			{Path: "go/samples/task-api/README.md", Relationship: DocRelationshipChecked},
			{Path: "docs/task-api.md", Relationship: DocRelationshipUnchecked},
		},
		Config: &ConfigSection{
			SchemaRef:   &ArtifactRef{Path: "go/samples/task-api/schema/config.json", Digest: "sha256:6666666666666666666666666666666666666666666666666666666666666666"},
			Operational: &OperationalSurface{PlatformEndpoints: true},
		},
		Provenance: Provenance{
			WorkspaceRevision: "0123456789abcdef0123456789abcdef01234567",
			Generator:         Generator{Name: "putnami", Version: "0.1.0"},
			AggregationMethod: AggregationMethodByReference,
		},
	}
}

// sampleOverrides builds a representative overrides file exercising every field:
// an added source range, removed sources and docs, an added doc, and a
// sensitivity override list.
func sampleOverrides() *OverridesFile {
	return &OverridesFile{
		Schema:          "https://putnami.dev/schemas/putnami-agent-context.json",
		ProtocolVersion: ProtocolVersion,
		AddSources: []SourceRange{
			{
				Path:      "go/samples/task-api/handlers.go",
				StartLine: 5,
				EndLine:   30,
				Why:       SourceReasonOverride,
				Tokens:    TokenEstimate{Estimated: 180, Method: TokenMethodBytesDiv4},
			},
		},
		RemoveSources: []string{"go/samples/task-api/tasks.go"},
		AddDocs: []DocRef{
			{Path: "docs/task-api-internals.md", Relationship: DocRelationshipChecked},
		},
		RemoveDocs: []string{"docs/task-api.md"},
		Sensitive:  []string{"go/samples/task-api/secrets.env"},
	}
}

// canonical returns the canonical serialization of v: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form
// every agent-context producer must reproduce.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestSerialization_Stable100 verifies the canonical serialization of the same
// document and overrides file is byte-identical across 100 marshals — no map
// iteration or other nondeterminism leaks into the wire form.
func TestSerialization_Stable100(t *testing.T) {
	cases := map[string]any{
		"document":  sampleDocument(),
		"overrides": sampleOverrides(),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			first := canonical(t, v)
			for i := range 100 {
				if got := canonical(t, v); !bytes.Equal(got, first) {
					t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
				}
			}
		})
	}
}

// TestSerialization_CanonicalByteForm pins the exact canonical bytes: the
// committed fixtures must equal the Go serialization of the sample builders.
// This is the contract every producer of an agent-context document or overrides
// file must reproduce byte-for-byte.
func TestSerialization_CanonicalByteForm(t *testing.T) {
	cases := []struct {
		path string
		v    any
	}{
		{filepath.Join("fixtures", "valid", "full.json"), sampleDocument()},
		{filepath.Join("fixtures", "overrides", "valid", "full.json"), sampleOverrides()},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := canonical(t, tc.v)
			if *updateGolden {
				if err := os.WriteFile(tc.path, got, 0o644); err != nil {
					t.Fatalf("update golden %s: %v", tc.path, err)
				}
				return
			}
			want, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical serialization does not match %s.\n--- got ---\n%s\n--- want ---\n%s", tc.path, got, want)
			}
		})
	}
}

// TestRoundTrip_Idempotent verifies parse → marshal → parse → marshal is
// byte-stable for every valid fixture: re-serializing a parsed document yields
// the same bytes the second time, so the wire form is a fixed point.
func TestRoundTrip_Idempotent(t *testing.T) {
	docFiles, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(docFiles) == 0 {
		t.Fatal("no valid document fixtures found")
	}
	for _, path := range docFiles {
		t.Run("document/"+filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, path, func(b []byte) (any, bool) {
				d, _ := ParseDocument(b)
				return d, d != nil
			})
		})
	}

	overrideFiles, err := filepath.Glob("fixtures/overrides/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(overrideFiles) == 0 {
		t.Fatal("no valid overrides fixtures found")
	}
	for _, path := range overrideFiles {
		t.Run("overrides/"+filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, path, func(b []byte) (any, bool) {
				o, _ := ParseOverrides(b)
				return o, o != nil
			})
		})
	}
}

func assertRoundTrip(t *testing.T, path string, parse func([]byte) (any, bool)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v1, ok := parse(data)
	if !ok {
		t.Fatalf("parse %s produced nil document", path)
	}
	once := canonical(t, v1)
	v2, ok := parse(once)
	if !ok {
		t.Fatalf("re-parse %s produced nil document", path)
	}
	twice := canonical(t, v2)
	if !bytes.Equal(once, twice) {
		t.Fatalf("%s: round-trip not idempotent\nonce:  %s\ntwice: %s", path, once, twice)
	}
}
