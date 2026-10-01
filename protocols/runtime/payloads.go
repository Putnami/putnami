package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Typed per-verb payloads. Verb-specific outputs travel inside generic
// events — result data, artifact extras — and before these types each
// extension invented its own keys, so consumers could not aggregate
// test results or compare coverage across languages. These shapes are
// the contract; extensions may add fields, consumers must ignore
// unknown ones.

// ReleaseSetResultDataKey is the sole result-data location of a successful
// release-set publish outcome. The payload is the v2 outcome owned by
// go.putnami.dev/protocol/distribution: the stored set's {id, digest} plus the
// head each advanced channel now points at, with its generation, under
// "current" per channel. One release advances several channels, so a consumer
// reads that map rather than a single channel name.
const ReleaseSetResultDataKey = "releaseSet"

// Test-case result-data keys. A test job carries one entry per test case it
// ran under TestCasesResultDataKey, beside its testSummary, and, when it left
// cases out, their count under TestCasesDroppedResultDataKey. The entry shape
// and its bounds are protocols/cli's TestCase (go.putnami.dev/protocol/cli),
// bounded with its BoundTestCases: the CLI turns each entry into one test:case
// session-stream record attributed to the task, so the two layers share one
// definition instead of two copies. A runtime without structured per-case
// output omits both keys; it never guesses.
const (
	TestCasesResultDataKey        = "testCases"
	TestCasesDroppedResultDataKey = "testCasesDropped"
)

// TestSummary is the canonical test outcome payload, carried in the
// result event's data under the "testSummary" key.
type TestSummary struct {
	// Total is the number of tests the run considered. Passed+Failed+Skipped may
	// be lower (a run can abort) but never higher; ValidateTestSummary rejects
	// the latter.
	Total int `json:"total"`
	// Passed is the number of tests that succeeded.
	Passed int `json:"passed"`
	// Failed is the number of tests that failed.
	Failed int `json:"failed"`
	// Skipped is the number of tests the runner did not execute.
	Skipped int `json:"skipped"`
	// FailureDetailsTruncated is the number of causal failure diagnostics the
	// producer omitted after applying the bounded presentation profile. Zero
	// means no details were omitted. The complete transcript remains in log
	// events and is not counted here.
	FailureDetailsTruncated int `json:"failureDetailsTruncated,omitempty"`
}

// Coverage granularity values. Languages measure coverage differently
// (Go: statements, TypeScript: lines); the payload carries percentage
// plus the granularity it was measured at, so consumers can aggregate
// percentages without pretending the units agree.
const (
	CoverageStatements = "statements"
	CoverageLines      = "lines"
	CoverageFunctions  = "functions"
	CoverageBranches   = "branches"
)

// CoverageSummary is the canonical coverage payload, carried in the
// result event's data under the "coverageSummary" key.
type CoverageSummary struct {
	// Percentage in [0,100] at the declared granularity.
	Percentage float64 `json:"percentage"`
	// Granularity the percentage was measured at (statements, lines,
	// functions, branches).
	Granularity string `json:"granularity"`
	// Covered/Total counts at the same granularity, when available.
	Covered int `json:"covered,omitempty"`
	Total   int `json:"total,omitempty"`
	// Threshold that was enforced, when one applied.
	Threshold float64 `json:"threshold,omitempty"`
}

// LintSummary is the canonical lint outcome payload, carried in the
// result event's data under the "lintSummary" key. Individual findings
// travel as per-issue diagnostic events (severity, message, code,
// location) — a single aggregate diagnostic of raw tool output is not
// conformant.
type LintSummary struct {
	// Errors is the number of error-severity findings. Required, because zero
	// errors is the claim that makes a lint job's success meaningful.
	Errors int `json:"errors"`
	// Warnings is the number of warning-severity findings.
	Warnings int `json:"warnings,omitempty"`
	// Infos is the number of info-severity findings.
	Infos int `json:"infos,omitempty"`
}

// Artifact kinds with defined semantics. Extensions may emit other
// kinds; these are the ones tooling understands.
const (
	ArtifactKindBinary     = "binary"
	ArtifactKindBundle     = "bundle"
	ArtifactKindPackage    = "package"
	ArtifactKindReport     = "report"
	ArtifactKindCoverage   = "coverage"
	ArtifactKindPublished  = "published"
	ArtifactKindDeployment = "deployment"
)

// PublishRecord is the payload of a kind="published" artifact event's
// extra fields. This codifies the shape the CLI's published-artifacts
// renderer consumes.
type PublishRecord struct {
	// Registry the artifact went to (npm, docker, go, ...).
	Registry string `json:"registry"`
	// TargetRegistry is the concrete OCI registry host the Docker image was
	// published to. Registry above remains the publisher kind for backwards
	// compatible artifact renderers ("docker").
	TargetRegistry string `json:"targetRegistry,omitempty"`
	// Name is the published artifact's identity in its registry (package name,
	// module path, image repository).
	Name string `json:"name"`
	// Version is the version the artifact was published under. Empty for
	// publishers whose identity is a digest rather than a version.
	Version string `json:"version,omitempty"`
	// Tags are the mutable references published alongside the immutable one
	// ("latest", "v1"). Order is the publisher's.
	Tags []string `json:"tags,omitempty"`
	// DryRun reports that the publish was simulated: nothing was transferred and
	// no reference was created.
	DryRun bool `json:"dryRun,omitempty"`
	// ContentStatus retains the Docker publisher vocabulary: "pushed" means this
	// invocation transferred image content, "retagged" means mutable references
	// moved to existing content, and "reused" means the exact immutable content
	// already existed and no tag moved. It is empty for non-Docker publishers and
	// dry runs.
	ContentStatus string `json:"contentStatus,omitempty"`
	// CacheOutcome records the registry content lookup that precedes a Docker
	// publish: "hit" means a verified digest was reused, "miss" means content
	// was transferred. It is deliberately separate from ContentStatus so a
	// consumer does not have to infer a cache outcome from human wording.
	CacheOutcome string `json:"cacheOutcome,omitempty"`
	// ImageDigest is the verified immutable OCI digest (sha256:<hex>), never a
	// mutable image tag. It is omitted when a dry/local-only publish cannot
	// verify one.
	ImageDigest string `json:"imageDigest,omitempty"`
	// ImmutableRef is the exact verified remote reference (repository@digest).
	// It is publish evidence and must never be emitted by a package task.
	ImmutableRef string `json:"immutableRef,omitempty"`
	// DigestVerified reports that ImageDigest was read back from the registry
	// after the publish rather than assumed from what was pushed.
	DigestVerified bool `json:"digestVerified,omitempty"`
	// DigestReused reports that the registry already held this content, whether
	// references moved or the exact immutable content was simply reused. It is
	// the artifact-level view of
	// CacheOutcome == "hit".
	DigestReused bool `json:"digestReused,omitempty"`
	// PublishTimings is the optional producer-measured breakdown of the
	// registry-facing work.
	PublishTimings *PublishTimings `json:"publishTimings,omitempty"`
}

// PublishTimings is the producer-measured timing breakdown of a Docker
// publication. All values are non-negative milliseconds. Build time belongs to
// the package task and is joined by the CLI's terminal projection; these fields
// cover the registry-facing work owned by the Docker publisher itself.
type PublishTimings struct {
	// CacheLookupMs is the time spent asking the registry whether it already
	// holds this content.
	CacheLookupMs int64 `json:"cacheLookupMs"`
	// CacheTransferMs is the time spent moving content between caches (for
	// example a local layer store and the registry's).
	CacheTransferMs int64 `json:"cacheTransferMs"`
	// RegistryPushMs is the time spent transferring image content to the
	// registry. Zero when the lookup hit.
	RegistryPushMs int64 `json:"registryPushMs"`
	// ReferencePublishMs is the time spent creating the immutable and mutable
	// references (tags) once the content is in place.
	ReferencePublishMs int64 `json:"referencePublishMs"`
	// DigestResolveMs is the time spent reading the published digest back for
	// verification.
	DigestResolveMs int64 `json:"digestResolveMs"`
}

// DecodePublishRecord projects the open artifact event payload onto the typed
// publish fields. Unknown extension fields are deliberately ignored so the
// runtime-event protocol stays additive.
func DecodePublishRecord(data map[string]any) (*PublishRecord, error) {
	if data == nil {
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var record PublishRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// resultPayloads is the typed view of a result event's data.
type resultPayloads struct {
	TestSummary     *TestSummary     `json:"testSummary,omitempty"`
	CoverageSummary *CoverageSummary `json:"coverageSummary,omitempty"`
	LintSummary     *LintSummary     `json:"lintSummary,omitempty"`
}

// ExtractResultPayloads decodes the typed per-verb payloads from a
// result event's data map. Missing payloads return nil; unknown fields
// are ignored (additive evolution).
func ExtractResultPayloads(data map[string]any) (*TestSummary, *CoverageSummary, *LintSummary, error) {
	if data == nil {
		return nil, nil, nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, nil, nil, err
	}
	var p resultPayloads
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, nil, err
	}
	return p.TestSummary, p.CoverageSummary, p.LintSummary, nil
}

// ExtractReleaseSetPublishOutcome locates data.releaseSet while deliberately
// keeping its bytes opaque. The runtime protocol owns location and cardinality;
// go.putnami.dev/protocol/distribution alone parses and validates the fields.
// A missing key is not an error because most result events are not release-set
// publication coordinators. Use RequireReleaseSetPublishOutcome for a managed
// publish run and RejectReleaseSetPublishOutcome for dry-run/failure paths.
func ExtractReleaseSetPublishOutcome(data map[string]any) (json.RawMessage, []diag.Diagnostic) {
	if data == nil {
		return nil, nil
	}
	value, present := data[ReleaseSetResultDataKey]
	if !present {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf("invalid-release-set-candidate", ReleaseSetResultDataKey,
			"encode opaque release-set candidate as JSON: %v", err)}
	}
	return raw, nil
}

// ReleaseSetPublishOutcome extracts the sole successful data.releaseSet from a
// runtime stream. It validates the ordinary stream invariants, rejects an
// outcome on FAILED/SKIP, and rejects duplicate successful outcomes. Absence
// remains valid here so generic callers can inspect any job stream.
func ReleaseSetPublishOutcome(events []*Event) (json.RawMessage, []diag.Diagnostic) {
	outcome, _, diagnostics := releaseSetPublishOutcome(events)
	return outcome, diagnostics
}

// RequireReleaseSetPublishOutcome applies runtime's half of the managed
// successful-publish rule: exactly one opaque candidate must occur at
// data.releaseSet on an OK result. The consumer must parse and validate those
// bytes with go.putnami.dev/protocol/distribution before using the handoff.
func RequireReleaseSetPublishOutcome(events []*Event) (json.RawMessage, []diag.Diagnostic) {
	outcome, present, diagnostics := releaseSetPublishOutcome(events)
	if !present {
		diagnostics = append(diagnostics, diag.Errorf("missing-release-set-outcome", "",
			"successful managed publication requires exactly one result data.releaseSet candidate"))
	}
	return outcome, diagnostics
}

// RejectReleaseSetPublishOutcome applies to dry-run and unsuccessful publish
// paths. Those paths may report diagnostics or previews, but they cannot claim
// an immutable release set was successfully published.
func RejectReleaseSetPublishOutcome(events []*Event) []diag.Diagnostic {
	_, present, diagnostics := releaseSetPublishOutcome(events)
	if present {
		diagnostics = append(diagnostics, diag.Errorf("unexpected-release-set-outcome", "",
			"dry-run or unsuccessful publication must not emit result data.releaseSet"))
	}
	return diagnostics
}

func releaseSetPublishOutcome(events []*Event) (json.RawMessage, bool, []diag.Diagnostic) {
	diagnostics := ValidateEventStream(events)
	var outcome json.RawMessage
	present := false
	successfulCount := 0

	for index, event := range events {
		if event == nil || event.Type != EventResult {
			continue
		}
		status, rawOutcome, occurrences, err := releaseSetRawFromResult(event.Data)
		field := fmt.Sprintf("event[%d].data.data.%s", index, ReleaseSetResultDataKey)
		if err != nil {
			diagnostics = append(diagnostics, diag.Errorf("invalid-result-data", fmt.Sprintf("event[%d].data", index), "%v", err))
			continue
		}
		if occurrences == 0 {
			continue
		}
		present = true
		if occurrences > 1 {
			diagnostics = append(diagnostics, diag.Errorf("duplicate-release-set-outcome", field,
				"result data contains %d releaseSet members; exactly one is allowed", occurrences))
		}
		if status != ResultOK {
			diagnostics = append(diagnostics, diag.Errorf("release-set-on-unsuccessful-result", field,
				"result status %q cannot carry a successful release-set outcome", status))
			continue
		}
		successfulCount += occurrences
		if outcome == nil {
			outcome = append(json.RawMessage(nil), rawOutcome...)
		}
	}
	if successfulCount > 1 {
		diagnostics = append(diagnostics, diag.Errorf("duplicate-release-set-outcome", "",
			"publish coordination stream contains %d successful data.releaseSet outcomes; exactly one is allowed", successfulCount))
	}
	return outcome, present, diagnostics
}

func releaseSetRawFromResult(data json.RawMessage) (ResultStatus, json.RawMessage, int, error) {
	if len(data) == 0 {
		return "", nil, 0, nil
	}
	var result struct {
		Status ResultStatus    `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", nil, 0, fmt.Errorf("decode result payload: %w", err)
	}
	if len(result.Data) == 0 || bytes.Equal(bytes.TrimSpace(result.Data), []byte("null")) {
		return result.Status, nil, 0, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Data))
	token, err := decoder.Token()
	if err != nil {
		return "", nil, 0, fmt.Errorf("decode result data: %w", err)
	}
	if token != json.Delim('{') {
		return "", nil, 0, fmt.Errorf("result data must be an object")
	}
	var raw json.RawMessage
	count := 0
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return "", nil, 0, fmt.Errorf("decode result data key: %w", keyErr)
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", nil, 0, fmt.Errorf("result data contains a non-string key")
		}
		var value json.RawMessage
		if valueErr := decoder.Decode(&value); valueErr != nil {
			return "", nil, 0, fmt.Errorf("decode result data.%s: %w", key, valueErr)
		}
		if key == ReleaseSetResultDataKey {
			count++
			raw = append(raw[:0], value...)
		}
	}
	if _, err = decoder.Token(); err != nil {
		return "", nil, 0, fmt.Errorf("close result data: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", nil, 0, fmt.Errorf("result data contains trailing JSON")
		}
		return "", nil, 0, fmt.Errorf("decode trailing result data: %w", err)
	}
	return result.Status, raw, count, nil
}

// ValidateTestSummary checks the structural invariants of a test summary.
func ValidateTestSummary(s *TestSummary) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if s.Total < 0 || s.Passed < 0 || s.Failed < 0 || s.Skipped < 0 || s.FailureDetailsTruncated < 0 {
		diags = append(diags, diag.Errorf("invalid-test-summary", "",
			"test summary counts and omission accounting must be non-negative"))
	}
	if s.Passed+s.Failed+s.Skipped > s.Total {
		diags = append(diags, diag.Errorf("invalid-test-summary", "total",
			"passed+failed+skipped (%d) exceeds total (%d)",
			s.Passed+s.Failed+s.Skipped, s.Total))
	}
	return diags
}

// validCoverageGranularities is the closed set of coverage units.
var validCoverageGranularities = map[string]bool{
	CoverageStatements: true,
	CoverageLines:      true,
	CoverageFunctions:  true,
	CoverageBranches:   true,
}

// ValidateCoverageSummary checks the structural invariants of a
// coverage summary.
func ValidateCoverageSummary(s *CoverageSummary) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if s.Percentage < 0 || s.Percentage > 100 {
		diags = append(diags, diag.Errorf("invalid-coverage-summary", "percentage",
			"percentage %v out of range [0,100]", s.Percentage))
	}
	if !validCoverageGranularities[s.Granularity] {
		diags = append(diags, diag.Errorf("invalid-coverage-summary", "granularity",
			"granularity %q is not canonical", s.Granularity))
	}
	if s.Covered < 0 || s.Total < 0 || s.Covered > s.Total {
		diags = append(diags, diag.Errorf("invalid-coverage-summary", "covered",
			"covered/total counts inconsistent (%d/%d)", s.Covered, s.Total))
	}
	return diags
}
