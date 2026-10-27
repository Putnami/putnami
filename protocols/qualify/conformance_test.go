package qualify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
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
	spectest.Proves(t, "qualify/workload-qualification-contract", "version-1-and-closed-vocabularies", "the-protocol-version-is-1")
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_StateVocabularyIsClosed pins the nine states. Adding one is a
// protocol change: every consumer that branches on State must learn it, and the
// TypeScript twin must list it too.
func TestConformance_StateVocabularyIsClosed(t *testing.T) {
	spectest.Proves(t, "qualify/workload-qualification-contract", "version-1-and-closed-vocabularies", "states-and-phases-are-closed")
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
	spectest.Proves(t, "qualify/workload-qualification-contract", "version-1-and-closed-vocabularies", "every-code-starts-with-qualify")
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
	spectest.Proves(t, "qualify/workload-qualification-contract", "one-fixture-corpus", "every-fixture-gets-its-listed-codes")
	spectest.Proves(t, "qualify/workload-qualification-contract", "one-fixture-corpus", "the-corpus-is-what-expectations-lists")
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
	spectest.Proves(t, "qualify/workload-qualification-contract", "the-contract-digest-is-canonical", "the-digest-bytes-are-pinned")
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
	spectest.Proves(t, "qualify/workload-qualification-contract", "a-pass-carries-its-proof", "an-unproven-pass-is-refused")
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
	proofless := map[string]func(*Verdict){
		"partial cleanup": func(v *Verdict) {
			v.Cleanup = &Cleanup{State: CleanupPartial, Leftovers: []string{"database compose_x"}}
		},
		"no teardown phase": func(v *Verdict) {
			v.Phases = slices.DeleteFunc(v.Phases, func(p Phase) bool { return p.Name == "teardown" })
		},
		"no request":               func(v *Verdict) { v.Requests = nil },
		"a different observed sha": func(v *Verdict) { v.Binding.ObservedSHA = "0000000000000000000000000000000000000000" },
	}
	for name, mutate := range proofless {
		verdict := cloneVerdict(t, base)
		mutate(verdict)
		if codes := distinctCodes(ValidateVerdict(verdict)); !slices.Equal(codes, []string{ErrorCodeUnprovenPass}) {
			t.Errorf("%s under passed: codes %v, want unproven_pass", name, codes)
		}
	}
	if diags := ValidateVerdict(base); diag.HasErrors(diags) {
		t.Fatalf("the unmodified passed fixture must stay valid: %v", diags)
	}
}

func TestStrictDecode_RejectsNonDocuments(t *testing.T) {
	spectest.Proves(t, "qualify/workload-qualification-contract", "parsing-is-strict", "non-documents-are-parse-errors")
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

// TestValidateContract_RefusesMalformedRequests mutates one member of a valid
// contract per case and pins the one code and the one field it gives. A case
// that changes the requests recomputes the digest, so only its defect shows.
func TestValidateContract_RefusesMalformedRequests(t *testing.T) {
	spectest.Proves(t, "qualify/workload-qualification-contract", "contract-defects-get-their-codes", "each-contract-defect-class-gets-its-code")
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", "contract-service-to-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, diags := ParseAndValidateContract(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture: %v", diags)
	}
	cases := map[string]struct {
		mutate func(*Contract)
		code   string
		field  string
	}{
		"version 2":          {func(c *Contract) { c.ProtocolVersion = 2 }, ErrorCodeUnsupportedProtocolVersion, "protocolVersion"},
		"missing project":    {func(c *Contract) { c.Project = "" }, ErrorCodeRequired, "project"},
		"openapi source":     {func(c *Contract) { c.DerivedFrom[0].Kind = "openapi" }, ErrorCodeInvalidSource, "derivedFrom[0].kind"},
		"malformed digest":   {func(c *Contract) { c.Digest = "sha256:not-hex" }, ErrorCodeInvalidDigest, "digest"},
		"repeated id":        {func(c *Contract) { c.Requests = append(c.Requests, c.Requests[0]) }, ErrorCodeInvalidRequest, fmt.Sprintf("requests[%d].id", len(base.Requests))},
		"id not method path": {func(c *Contract) { c.Requests[0].ID = "wrong" }, ErrorCodeInvalidRequest, "requests[0].id"},
		"relative path":      {func(c *Contract) { c.Requests[0].Path = "items"; c.Requests[0].ID = c.Requests[0].Method + " items" }, ErrorCodeInvalidRequest, "requests[0].path"},
		"maxStatus 99":       {func(c *Contract) { c.Requests[0].MaxStatus = 99 }, ErrorCodeInvalidRequest, "requests[0].maxStatus"},
		"blank provenance":   {func(c *Contract) { c.Requests[0].Provenance = " " }, ErrorCodeInvalidRequest, "requests[0].provenance"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			contract := *base
			contract.DerivedFrom = slices.Clone(base.DerivedFrom)
			contract.Requests = slices.Clone(base.Requests)
			tc.mutate(&contract)
			if tc.code != ErrorCodeInvalidDigest {
				contract.Digest = ContractDigest(contract.Requests)
			}
			got := diag.Errors(ValidateContract(&contract))
			if len(got) != 1 || got[0].Code != tc.code || got[0].Field != tc.field {
				t.Errorf("diagnostics = %v, want one %s at %s", got, tc.code, tc.field)
			}
		})
	}
}

func TestValidateVerdict_ShapeRules(t *testing.T) {
	spectest.Proves(t, "qualify/workload-qualification-contract", "verdict-defects-get-their-codes", "each-verdict-defect-gets-its-code")
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
