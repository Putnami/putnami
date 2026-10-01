package distribution

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ConformanceExpectation describes how an embedded fixture must behave.
type ConformanceExpectation string

const (
	// ConformanceValid must strict-parse and validate.
	ConformanceValid ConformanceExpectation = "valid"
	// ConformanceInvalid must fail with ExpectedCode.
	ConformanceInvalid ConformanceExpectation = "invalid"
	// ConformanceNonCanonical must be semantically valid but fail canonical parsing.
	ConformanceNonCanonical ConformanceExpectation = "non-canonical"
	// ConformanceEquivalent must derive the embedded golden bytes and ref.
	ConformanceEquivalent ConformanceExpectation = "equivalent"
)

// ConformanceFixture is one package-owned input that an external consumer can
// execute without locating module source files on disk.
type ConformanceFixture struct {
	// Name is the embedded fixture path and stable corpus identity.
	Name string
	// Expectation selects the valid, invalid, non-canonical, or equivalent check.
	Expectation ConformanceExpectation
	// ExpectedCode is the required diagnostic code for an invalid fixture.
	ExpectedCode string
	// JSON is a fresh copy of the fixture document supplied to the parser.
	JSON []byte
}

// ConformanceResult is one deterministic embedded-corpus verdict.
type ConformanceResult struct {
	// Name identifies the fixture whose deterministic verdict is reported.
	Name string
	// Passed reports whether the fixture met its declared expectation.
	Passed bool
	// Detail describes diagnostics or mismatches when Passed is false.
	Detail string
}

//go:embed fixtures/valid/*.json fixtures/invalid/*.json fixtures/equivalence/*.json fixtures/golden.json
var fixtureFS embed.FS

// invalidFixtureCodes is the corpus code table: every invalid fixture names the
// exact diagnostic code an external consumer must reproduce.
var invalidFixtureCodes = map[string]string{
	"bad-ecosystem-id.json":        ErrorCodeInvalidEcosystem,
	"bad-platform.json":            ErrorCodeInvalidPlatform,
	"bad-project.json":             ErrorCodeInvalidProject,
	"bad-source-tree.json":         ErrorCodeInvalidSourceTree,
	"duplicate-dependency.json":    ErrorCodeDuplicateDependency,
	"duplicate-field.json":         ErrorCodeDuplicateField,
	"duplicate-member.json":        ErrorCodeDuplicateMember,
	"explicit-null.json":           ErrorCodeNullField,
	"invalid-artifact-digest.json": ErrorCodeInvalidArtifactDigest,
	"malformed.json":               ErrorCodeParseError,
	"missing-field.json":           ErrorCodeMissingField,
	"missing-provenance.json":      ErrorCodeInvalidSourceRevision,
	"non-canonical.json":           ErrorCodeNonCanonical,
	"oversized-namespace.json":     ErrorCodeBoundsExceeded,
	"unclosed-dependency.json":     ErrorCodeUnclosedDependency,
	"unknown-field.json":           ErrorCodeUnknownField,
	"unknown-kind.json":            ErrorCodeInvalidKind,
	"unknown-version.json":         ErrorCodeInvalidProtocolVersion,
	"version-mismatch.json":        ErrorCodeDependencyVersionMismatch,
}

// EmbeddedConformanceFixtures returns fresh copies of the valid, invalid, and
// cross-language equivalence corpus in deterministic name order.
func EmbeddedConformanceFixtures() []ConformanceFixture {
	var fixtures []ConformanceFixture
	load := func(pattern string, expectation ConformanceExpectation) {
		files, err := fs.Glob(fixtureFS, pattern)
		if err != nil {
			return // compile-time embed patterns make this unreachable
		}
		for _, name := range files {
			data, err := fixtureFS.ReadFile(name)
			if err != nil {
				continue
			}
			fixture := ConformanceFixture{Name: name, Expectation: expectation, JSON: append([]byte(nil), data...)}
			if expectation == ConformanceInvalid {
				fixture.ExpectedCode = invalidFixtureCodes[path.Base(name)]
				if fixture.ExpectedCode == ErrorCodeNonCanonical {
					fixture.Expectation = ConformanceNonCanonical
				}
			}
			fixtures = append(fixtures, fixture)
		}
	}
	load("fixtures/valid/*.json", ConformanceValid)
	load("fixtures/invalid/*.json", ConformanceInvalid)
	load("fixtures/equivalence/*.json", ConformanceEquivalent)
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
	return fixtures
}

type embeddedGolden struct {
	Canonical string        `json:"canonical"`
	Ref       ReleaseSetRef `json:"ref"`
}

// EmbeddedGolden returns fresh canonical release-set bytes and their pinned
// content address: the one document every equivalence fixture must reproduce.
// The canonical projection is encoded as a JSON string in the fixture container
// so the bytes themselves contain no trailing newline.
func EmbeddedGolden() ([]byte, ReleaseSetRef) {
	return embeddedGoldenFile("fixtures/golden.json")
}

func embeddedGoldenFile(name string) ([]byte, ReleaseSetRef) {
	data, err := fixtureFS.ReadFile(name)
	if err != nil {
		return nil, ReleaseSetRef{}
	}
	var golden embeddedGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		return nil, ReleaseSetRef{}
	}
	return []byte(golden.Canonical), golden.Ref
}

// RunEmbeddedConformance executes the package corpus. External consumers such
// as a provider repository can call this in their tests while importing only
// this module; no CLI or framework package participates.
func RunEmbeddedConformance() []ConformanceResult {
	goldenBytes, goldenRef := EmbeddedGolden()
	fixtures := EmbeddedConformanceFixtures()
	results := make([]ConformanceResult, 0, len(fixtures))
	for _, fixture := range fixtures {
		result := ConformanceResult{Name: fixture.Name}
		switch fixture.Expectation {
		case ConformanceValid:
			value, diagnostics := ParseAndValidateReleaseSet(fixture.JSON)
			if value == nil || diag.HasErrors(diagnostics) {
				result.Detail = diagnosticsDetail(diagnostics)
				break
			}
			canonical, canonicalDiagnostics := CanonicalReleaseSetBytes(value)
			roundTripped, _, roundTripDiagnostics := ParseCanonicalReleaseSet(canonical)
			result.Passed = !diag.HasErrors(canonicalDiagnostics) && roundTripped != nil && !diag.HasErrors(roundTripDiagnostics)
			if !result.Passed {
				result.Detail = fmt.Sprintf("canonical round trip failed: %v/%v", canonicalDiagnostics, roundTripDiagnostics)
			}
		case ConformanceInvalid:
			_, diagnostics := ParseAndValidateReleaseSet(fixture.JSON)
			result.Passed = hasDiagnosticCode(diagnostics, fixture.ExpectedCode)
			result.Detail = diagnosticsDetail(diagnostics)
		case ConformanceNonCanonical:
			value, _, diagnostics := ParseCanonicalReleaseSet(fixture.JSON)
			result.Passed = value == nil && hasDiagnosticCode(diagnostics, fixture.ExpectedCode)
			result.Detail = diagnosticsDetail(diagnostics)
		case ConformanceEquivalent:
			value, diagnostics := ParseAndValidateReleaseSet(fixture.JSON)
			if value == nil || diag.HasErrors(diagnostics) {
				result.Detail = diagnosticsDetail(diagnostics)
				break
			}
			canonical, canonicalDiagnostics := CanonicalReleaseSetBytes(value)
			ref, refDiagnostics := DeriveReleaseSetRef(value)
			result.Passed = !diag.HasErrors(canonicalDiagnostics) && !diag.HasErrors(refDiagnostics) && bytes.Equal(canonical, goldenBytes) && refsEqual(ref, goldenRef)
			if !result.Passed {
				result.Detail = fmt.Sprintf("canonical/ref mismatch: canonical=%q ref=%#v diagnostics=%v/%v", canonical, ref, canonicalDiagnostics, refDiagnostics)
			}
		default:
			result.Detail = fmt.Sprintf("unknown expectation %q", fixture.Expectation)
		}
		results = append(results, result)
	}
	return results
}

func hasDiagnosticCode(diagnostics []diag.Diagnostic, code string) bool {
	for _, finding := range diagnostics {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func diagnosticsDetail(diagnostics []diag.Diagnostic) string {
	if len(diagnostics) == 0 {
		return ""
	}
	return fmt.Sprint(diagnostics)
}
