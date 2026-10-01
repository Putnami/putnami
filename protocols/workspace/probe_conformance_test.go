package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The fixture-provider conformance suite.
//
// A probe provider is conformant when its answers survive the four properties
// core relies on, and those properties are checkable without running the
// provider: replay its recorded answer and assert them. fixtureProvider is the
// reference implementation of that replay — a provider that answers from a
// fixture file — so the suite below is exactly the suite a real provider is
// held to, run against known-good and known-bad payloads.
//
// The four properties:
//
//  1. STRICT. The answer parses under DisallowUnknownFields and validates.
//  2. CANONICAL. Normalization is idempotent and the digest is invariant under
//     re-serialization.
//  3. PATH-KEYED. Every path is repo-relative and inside the workspace, and no
//     project path is reported twice.
//  4. MERGEABLE. The answer merges with itself and with an empty explicit
//     config without producing a conflict — a provider that disagrees with
//     ITSELF is broken regardless of what any other provider says.

// probeProvider is the seam a conformance run drives. A real provider adapter
// (subprocess, in-process extension) implements the same one-method shape.
type probeProvider interface {
	Name() string
	Probe(ProbeRequest) ([]byte, error)
}

// fixtureProvider answers every request with the bytes of one fixture file.
type fixtureProvider struct {
	name    string
	payload []byte
}

func (p fixtureProvider) Name() string { return p.name }

func (p fixtureProvider) Probe(req ProbeRequest) ([]byte, error) {
	if diags := ValidateProbeRequest(&req); diag.HasErrors(diags) {
		return nil, NewProbeFailure(ProbeFailureInvalidResult, p.name, "request rejected: %v", diags)
	}
	return p.payload, nil
}

func loadFixtureProvider(t *testing.T, path string) fixtureProvider {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fixtureProvider{name: filepath.Base(path), payload: data}
}

// runProbeProviderConformance asserts the four properties against one provider.
func runProbeProviderConformance(t *testing.T, provider probeProvider) {
	t.Helper()

	request := ProbeRequest{Version: ProbeProtocolVersion, Extension: provider.Name(), Reason: ProbeReasonLoad}
	payload, err := provider.Probe(request)
	if err != nil {
		t.Fatalf("provider %s refused a well-formed request: %v", provider.Name(), err)
	}

	// 1. Strict.
	result, diags := ParseAndValidateProbeResult(payload)
	if diag.HasErrors(diags) {
		t.Fatalf("provider %s answered with a non-conformant payload: %v", provider.Name(), diags)
	}

	// 2. Canonical: normalization is idempotent, and re-serializing the answer
	// (as a subprocess transport would) must not move the digest.
	digest := ProbeResultDigest(*result)
	normalized := *result
	NormalizeProbeResult(&normalized)
	if ProbeResultDigest(normalized) != digest {
		t.Errorf("provider %s: digest moved under normalization", provider.Name())
	}
	reencoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	roundTripped, diags := ParseAndValidateProbeResult(reencoded)
	if diag.HasErrors(diags) {
		t.Fatalf("provider %s: canonical form does not re-parse: %v", provider.Name(), diags)
	}
	if ProbeResultDigest(*roundTripped) != digest {
		t.Errorf("provider %s: digest is not stable across a serialization round trip", provider.Name())
	}

	// 3. Path-keyed.
	seen := make(map[string]bool, len(result.Projects))
	for _, project := range result.Projects {
		cleaned, ok := NormalizeProbePath(project.Path)
		if !ok {
			t.Errorf("provider %s: project path %q is not repo-relative", provider.Name(), project.Path)
			continue
		}
		if seen[cleaned] {
			t.Errorf("provider %s: project path %q reported twice", provider.Name(), cleaned)
		}
		seen[cleaned] = true
	}

	// 4. Mergeable with itself.
	if _, mergeDiags := MergeProbeResults([]ProbeResult{*result, *result}, nil); diag.HasErrors(mergeDiags) {
		t.Errorf("provider %s disagrees with itself under the merge rules: %v", provider.Name(), mergeDiags)
	}
}

func TestConformance_ProbeFixtureProviders(t *testing.T) {
	files, err := filepath.Glob("fixtures/probe/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid probe fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			runProbeProviderConformance(t, loadFixtureProvider(t, path))
		})
	}
}

// A provider whose fixture is one of the invalid corpus entries must be
// REJECTED by the same suite. Without this, the suite above could be vacuous.
func TestConformance_ProbeFixtureProvidersRejectInvalid(t *testing.T) {
	files, err := filepath.Glob("fixtures/probe/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid probe fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			provider := loadFixtureProvider(t, path)
			payload, err := provider.Probe(ProbeRequest{
				Version: ProbeProtocolVersion, Extension: provider.Name(), Reason: ProbeReasonLoad})
			if err != nil {
				return // refusing to answer is a conformant outcome
			}
			if _, diags := ParseAndValidateProbeResult(payload); !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s was accepted by the conformance parser", path)
			}
		})
	}
}

// A provider must refuse a request it cannot understand rather than answering
// a question it did not parse.
func TestConformance_ProbeProviderRejectsUnsupportedRequest(t *testing.T) {
	provider := loadFixtureProvider(t, "fixtures/probe/valid/minimal.json")
	_, err := provider.Probe(ProbeRequest{Version: 99, Extension: provider.Name()})
	if err == nil {
		t.Fatal("provider answered a request carrying an unsupported protocol version")
	}
	failure := &ProbeFailure{}
	ok := errors.As(err, &failure)
	if !ok {
		t.Fatalf("provider returned an untyped error %T; graph-dependent commands need the cause", err)
	}
	if !failure.Kind.Valid() {
		t.Errorf("failure kind %q is not in the closed set", failure.Kind)
	}
}

func TestConformance_ProbeResultFixtures(t *testing.T) {
	runShapeConformance(t, "probe", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateProbeResult(b)
		return d
	})
}

func TestConformance_ProbeRequestFixtures(t *testing.T) {
	runShapeConformance(t, "probe-request", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateProbeRequest(b)
		return d
	})
}

func TestDrift_ProbeResult(t *testing.T) {
	assertFieldParity(t, "ProbeResult",
		jsonSchemaProperties(t, "schemas/probe.json"),
		goTypeJSONFields(t, ProbeResult{}))
}

func TestDrift_ProbeRequest(t *testing.T) {
	assertFieldParity(t, "ProbeRequest",
		jsonSchemaDefinitionProperties(t, "schemas/probe.json", "probeRequest"),
		goTypeJSONFields(t, ProbeRequest{}))
}

func TestDrift_ProbeProject(t *testing.T) {
	assertFieldParity(t, "ProbeProject",
		jsonSchemaDefinitionProperties(t, "schemas/probe.json", "probeProject"),
		goTypeJSONFields(t, ProbeProject{}))
}

func TestDrift_MergedProject(t *testing.T) {
	assertFieldParity(t, "MergedProject",
		jsonSchemaDefinitionProperties(t, "schemas/probe.json", "mergedProject"),
		goTypeJSONFields(t, MergedProject{}))
}

func TestDrift_ProbeFailure(t *testing.T) {
	assertFieldParity(t, "ProbeFailure",
		jsonSchemaDefinitionProperties(t, "schemas/probe.json", "probeFailure"),
		goTypeJSONFields(t, ProbeFailure{}))
}
