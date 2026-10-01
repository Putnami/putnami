package qualify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// expectations is fixtures/expectations.json: the corpus index both runtimes
// read. Every valid fixture must validate cleanly; every invalid fixture must
// produce EXACTLY the listed distinct codes, so a validator that drifts in
// either language fails against a corpus the other still passes.
type expectations struct {
	Valid   []string            `json:"valid"`
	Invalid map[string][]string `json:"invalid"`
}

func readExpectations(t *testing.T) expectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", "expectations.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var index expectations
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	return index
}

// parseFixture dispatches on the file name: a contract- prefix is a contract
// document, anything else a verdict. The TypeScript twin uses the same rule.
func parseFixture(name string, data []byte) []diag.Diagnostic {
	if strings.HasPrefix(name, "contract-") {
		_, diags := ParseAndValidateContract(data)
		return diags
	}
	_, diags := ParseAndValidateVerdict(data)
	return diags
}

func distinctCodes(diags []diag.Diagnostic) []string {
	seen := map[string]bool{}
	var codes []string
	for _, d := range diag.Errors(diags) {
		if !seen[d.Code] {
			seen[d.Code] = true
			codes = append(codes, d.Code)
		}
	}
	sort.Strings(codes)
	return codes
}

func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_StateVocabularyIsClosed pins the nine states. Adding one is a
// protocol change: every consumer that branches on State must learn it, and the
// TypeScript twin must list it too.
func TestConformance_StateVocabularyIsClosed(t *testing.T) {
	want := []State{"passed", "failed", "unsupported", "not_run", "timed_out", "canceled",
		"target_unreachable", "digest_mismatch", "composition_failed"}
	if !slices.Equal(ValidStates, want) {
		t.Fatalf("ValidStates = %v, want %v", ValidStates, want)
	}
	for _, state := range ValidStates {
		if state.IsPass() != (state == StatePassed) {
			t.Errorf("%q.IsPass() = %v; only passed is a pass", state, state.IsPass())
		}
	}
	if State("ok").IsValid() {
		t.Error(`"ok" must not be a valid state`)
	}
	wantPhases := []string{"resolve-target", "readiness", "version-binding", "smoke", "teardown"}
	if !slices.Equal(PhaseNames, wantPhases) {
		t.Fatalf("PhaseNames = %v, want %v", PhaseNames, wantPhases)
	}
}

func TestConformance_ErrorCodesArePrefixed(t *testing.T) {
	for code := range ValidErrorCodes {
		if !strings.HasPrefix(code, "qualify.") {
			t.Errorf("error code %q must use the qualify.* prefix", code)
		}
	}
	for _, code := range []string{PhaseCodeTargetUnreachable, PhaseCodeCompositionFailed, PhaseCodeNotReady,
		PhaseCodeVersionMissing, PhaseCodeVersionMismatch, PhaseCodeRequestFailed, PhaseCodeNoRouteInventory,
		PhaseCodeNoDerivableRequest, PhaseCodeTeardownPartial, PhaseCodeCanceled} {
		if !strings.HasPrefix(code, "qualify.") {
			t.Errorf("phase code %q must use the qualify.* prefix", code)
		}
	}
}

func TestConformance_Corpus(t *testing.T) {
	index := readExpectations(t)
	for _, kind := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(filepath.Join("fixtures", kind))
		if err != nil {
			t.Fatalf("read fixtures/%s: %v", kind, err)
		}
		var onDisk []string
		for _, entry := range entries {
			onDisk = append(onDisk, entry.Name())
		}
		var listed []string
		if kind == "valid" {
			listed = append(listed, index.Valid...)
		} else {
			for name := range index.Invalid {
				listed = append(listed, name)
			}
		}
		sort.Strings(listed)
		if !slices.Equal(onDisk, listed) {
			t.Fatalf("fixtures/%s holds %v but expectations.json lists %v", kind, onDisk, listed)
		}
		if len(onDisk) == 0 {
			t.Fatalf("fixtures/%s is empty, so the corpus proves nothing", kind)
		}
	}

	for _, name := range index.Valid {
		t.Run("valid/"+name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
			if err != nil {
				t.Fatal(err)
			}
			if diags := parseFixture(name, data); diag.HasErrors(diags) {
				t.Errorf("valid fixture produced errors: %v", diags)
			}
		})
	}
	for name, want := range index.Invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			got := distinctCodes(parseFixture(name, data))
			if !slices.Equal(got, want) {
				t.Errorf("codes = %v, want %v", got, want)
			}
			for _, code := range got {
				if !ValidErrorCodes[code] {
					t.Errorf("code %q is not in ValidErrorCodes", code)
				}
			}
		})
	}
}

// TestConformance_ContractDigestIsPinned pins the canonical serialization: the
// corpus digests were computed by an independent serializer, and the
// TypeScript twin recomputes the same values.
func TestConformance_ContractDigestIsPinned(t *testing.T) {
	if got, want := ContractDigest(nil), "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"; got != want {
		t.Errorf("ContractDigest(nil) = %s, want %s (the digest of [])", got, want)
	}
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", "contract-service-to-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	contract, diags := ParseAndValidateContract(data)
	if diag.HasErrors(diags) {
		t.Fatalf("parse: %v", diags)
	}
	if got, want := ContractDigest(contract.Requests), "sha256:19782e4140d2d58581b8821654e85ea9c96deaf1b2683dd55d2912856c4d8adc"; got != want {
		t.Errorf("ContractDigest = %s, want %s", got, want)
	}
	// HTML-significant bytes are legal in a route path and must not be escaped.
	escaped := ContractDigest([]Request{{ID: "GET /a&b", Method: "GET", Path: "/a&b", MaxStatus: 499, Provenance: "manual"}})
	if escaped != "sha256:"+sha256Hex(`[{"id":"GET /a&b","maxStatus":499,"method":"GET","path":"/a&b","provenance":"manual"}]`) {
		t.Errorf("ContractDigest escaped HTML-significant bytes: %s", escaped)
	}
}

// TestPassedRequiresEveryPhaseAndRequestPassed is the fail-closed rule: no
// non-pass phase or request state, and no missing phase, can hide under a
// passed verdict.
func TestPassedRequiresEveryPhaseAndRequestPassed(t *testing.T) {
	base := readVerdictFixture(t, "passed-url.json")
	for index := range base.Phases {
		for _, state := range ValidStates {
			if state.IsPass() {
				continue
			}
			verdict := cloneVerdict(t, base)
			verdict.Phases[index].State = state
			if codes := distinctCodes(ValidateVerdict(verdict)); !slices.Equal(codes, []string{ErrorCodeUnprovenPass}) {
				t.Errorf("phase %s %s under passed: codes %v, want unproven_pass", verdict.Phases[index].Name, state, codes)
			}
		}
	}
	for index := range base.Requests {
		for _, state := range ValidStates {
			if state.IsPass() {
				continue
			}
			verdict := cloneVerdict(t, base)
			verdict.Requests[index].State = state
			if codes := distinctCodes(ValidateVerdict(verdict)); !slices.Equal(codes, []string{ErrorCodeUnprovenPass}) {
				t.Errorf("request %s %s under passed: codes %v, want unproven_pass", verdict.Requests[index].ID, state, codes)
			}
		}
	}
	partial := cloneVerdict(t, base)
	partial.Cleanup = &Cleanup{State: CleanupPartial, Leftovers: []string{"database compose_x"}}
	if codes := distinctCodes(ValidateVerdict(partial)); !slices.Equal(codes, []string{ErrorCodeUnprovenPass}) {
		t.Errorf("partial cleanup under passed: codes %v, want unproven_pass", codes)
	}
	if diags := ValidateVerdict(base); diag.HasErrors(diags) {
		t.Fatalf("the unmodified passed fixture must stay valid: %v", diags)
	}
}

func TestStrictDecode_RejectsNonDocuments(t *testing.T) {
	cases := map[string]string{
		"invalid JSON":      `{"protocolVersion":`,
		"trailing document": `{} {}`,
		"top-level array":   `[]`,
		"top-level null":    `null`,
		"wrong member type": `{"protocolVersion":"1"}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, diags := ParseAndValidateVerdict([]byte(input)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeParseError}) {
				t.Errorf("verdict codes = %v, want parse_error", distinctCodes(diags))
			}
			if _, diags := ParseAndValidateContract([]byte(input)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeParseError}) {
				t.Errorf("contract codes = %v, want parse_error", distinctCodes(diags))
			}
		})
	}
	nested := `{"requests":[{"id":"GET /","extra":true}]}`
	if _, diags := ParseAndValidateContract([]byte(nested)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeUnknownField}) ||
		diags[0].Field != "requests[0].extra" {
		t.Errorf("nested unknown member: %v", diags)
	}
}

func TestValidateContract_RefusesMalformedRequests(t *testing.T) {
	requests := []Request{
		{ID: "GET /a", Method: "GET", Path: "/a", MaxStatus: 499, Provenance: "typed-api"},
		{ID: "GET /a", Method: "GET", Path: "/a", MaxStatus: 499, Provenance: "typed-api"},
		{ID: "wrong", Method: "HEAD", Path: "b", MaxStatus: 99, Provenance: " "},
	}
	contract := &Contract{
		ProtocolVersion: 2,
		DerivedFrom:     []Source{{Kind: "openapi"}},
		Requests:        requests,
		Digest:          "sha256:not-hex",
	}
	got := distinctCodes(ValidateContract(contract))
	want := []string{ErrorCodeInvalidDigest, ErrorCodeInvalidRequest, ErrorCodeInvalidSource, ErrorCodeRequired, ErrorCodeUnsupportedProtocolVersion}
	if !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func TestValidateVerdict_ShapeRules(t *testing.T) {
	base := readVerdictFixture(t, "digest-mismatch.json")
	cases := map[string]struct {
		mutate func(*Verdict)
		want   string
	}{
		"url target without url": {func(v *Verdict) { v.Target.URL = "" }, ErrorCodeInvalidTarget},
		"unknown target kind":    {func(v *Verdict) { v.Target.Kind = "ssh" }, ErrorCodeInvalidTarget},
		"unknown binding kind":   {func(v *Verdict) { v.Binding.Kind = "image" }, ErrorCodeInvalidBinding},
		"repeated phase":         {func(v *Verdict) { v.Phases[1].Name = v.Phases[0].Name }, ErrorCodeInvalidPhase},
		"negative phase time":    {func(v *Verdict) { v.Phases[0].DurationMs = -1 }, ErrorCodeInvalidPhase},
		"unknown phase state":    {func(v *Verdict) { v.Phases[0].State = "ok" }, ErrorCodeInvalidState},
		"unknown request state":  {func(v *Verdict) { v.Requests[0].State = "skipped" }, ErrorCodeInvalidState},
		"request without id":     {func(v *Verdict) { v.Requests[0].ID = "" }, ErrorCodeRequired},
		"impossible status":      {func(v *Verdict) { v.Requests[0].Status = 42 }, ErrorCodeInvalidRequest},
		"negative request time":  {func(v *Verdict) { v.Requests[0].DurationMs = -3 }, ErrorCodeInvalidRequest},
		"negative request count": {func(v *Verdict) { v.Contract.Requests = -1 }, ErrorCodeInvalidRequest},
		"malformed digest":       {func(v *Verdict) { v.Contract.Digest = "abc" }, ErrorCodeInvalidDigest},
		"unknown source kind":    {func(v *Verdict) { v.Contract.DerivedFrom[0].Kind = "openapi" }, ErrorCodeInvalidSource},
		"local time":             {func(v *Verdict) { v.StartedAt = "2026-09-17 10:00:00" }, ErrorCodeInvalidTimestamp},
		"missing project":        {func(v *Verdict) { v.Project = "" }, ErrorCodeRequired},
		"clean with leftovers": {func(v *Verdict) {
			v.Cleanup = &Cleanup{State: CleanupClean, Leftovers: []string{"pid 1"}}
		}, ErrorCodeInvalidCleanup},
		"partial without leftovers": {func(v *Verdict) { v.Cleanup = &Cleanup{State: CleanupPartial} }, ErrorCodeInvalidCleanup},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verdict := cloneVerdict(t, base)
			tc.mutate(verdict)
			if got := distinctCodes(ValidateVerdict(verdict)); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("codes = %v, want [%s]", got, tc.want)
			}
		})
	}
}
