package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so
// any bump is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_CanonicalPaths pins the exact set of mandatory
// operational paths every runtime must expose. Adding or removing a
// path is a protocol change.
func TestConformance_CanonicalPaths(t *testing.T) {
	want := []string{"/livez", "/healthz", "/readyz", "/version"}
	if len(CanonicalPaths) != len(want) {
		t.Fatalf("CanonicalPaths has %d entries, want %d", len(CanonicalPaths), len(want))
	}
	for i, p := range want {
		if CanonicalPaths[i] != p {
			t.Errorf("CanonicalPaths[%d] = %q, want %q", i, CanonicalPaths[i], p)
		}
	}
}

// TestConformance_EndpointKinds validates that every canonical endpoint
// is classified and that gating flags match the documented behavior.
func TestConformance_EndpointKinds(t *testing.T) {
	c := DefaultContract()

	want := map[EndpointKind]struct {
		path             string
		aggregatesProbes bool
		gatedByRunning   bool
		optIn            bool
	}{
		EndpointLiveness:  {PathLivez, false, false, false},
		EndpointHealth:    {PathHealthz, true, true, false},
		EndpointReadiness: {PathReadyz, true, true, false},
		EndpointVersion:   {PathVersion, false, false, false},
		EndpointPprof:     {PathPprofPrefix, false, false, true},
	}

	seen := make(map[EndpointKind]bool)
	for _, e := range c.Endpoints {
		w, ok := want[e.Kind]
		if !ok {
			t.Errorf("unknown endpoint kind %q", e.Kind)
			continue
		}
		seen[e.Kind] = true
		if e.Path != w.path {
			t.Errorf("endpoint %s: path = %q, want %q", e.Kind, e.Path, w.path)
		}
		if e.AggregatesProbes != w.aggregatesProbes {
			t.Errorf("endpoint %s: aggregatesProbes = %v, want %v", e.Kind, e.AggregatesProbes, w.aggregatesProbes)
		}
		if e.GatedByRunning != w.gatedByRunning {
			t.Errorf("endpoint %s: gatedByRunning = %v, want %v", e.Kind, e.GatedByRunning, w.gatedByRunning)
		}
		if e.OptIn != w.optIn {
			t.Errorf("endpoint %s: optIn = %v, want %v", e.Kind, e.OptIn, w.optIn)
		}
		if e.Method != "GET" {
			t.Errorf("endpoint %s: method = %q, want GET", e.Kind, e.Method)
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("endpoint kind %q missing from default contract", kind)
		}
	}
}

// TestConformance_Capabilities pins the canonical capability set.
// Adding a capability is a protocol change.
func TestConformance_Capabilities(t *testing.T) {
	c := DefaultContract()
	want := map[CapabilityKind]string{
		CapabilityHealth:    PathHealthz,
		CapabilityReadiness: PathReadyz,
	}
	if len(c.Capabilities) != len(want) {
		t.Fatalf("Capabilities has %d entries, want %d", len(c.Capabilities), len(want))
	}
	for _, cap := range c.Capabilities {
		wantPath, ok := want[cap.Kind]
		if !ok {
			t.Errorf("unknown capability kind %q", cap.Kind)
			continue
		}
		if cap.Endpoint != wantPath {
			t.Errorf("capability %q: endpoint = %q, want %q", cap.Kind, cap.Endpoint, wantPath)
		}
		if cap.FailureConsequence == "" {
			t.Errorf("capability %q: failureConsequence is required for diagnostics/docs", cap.Kind)
		}
	}
}

// TestConformance_DiscoveryContract validates that every runtime walks
// the module tree, lets explicit registrations win, and keys by plugin
// name. These are non-negotiable: a runtime that violates any of them
// breaks the cross-language UX.
func TestConformance_DiscoveryContract(t *testing.T) {
	d := DefaultContract().Discovery
	if !d.AutoDiscoverFromModuleTree {
		t.Error("discovery must require AutoDiscoverFromModuleTree")
	}
	if !d.ExplicitRegistrationsWin {
		t.Error("discovery must require ExplicitRegistrationsWin")
	}
	if !d.KeyedByPluginName {
		t.Error("discovery must require KeyedByPluginName")
	}
}

// TestConformance_ProbeContract pins probe behavior. The 5s default
// timeout is generous on purpose; lower values are fine, higher values
// break SLOs.
func TestConformance_ProbeContract(t *testing.T) {
	p := DefaultContract().Probe
	if p.DefaultTimeoutMS != 5000 {
		t.Errorf("default probe timeout = %dms, want 5000ms", p.DefaultTimeoutMS)
	}
	if !p.MustRespectContextCancellation {
		t.Error("probes must respect context cancellation")
	}
	if !p.MustBeConcurrencySafe {
		t.Error("probes must be concurrency-safe")
	}
	if !p.ExposesErrorAsString {
		t.Error("probe failures must surface as error.Error() strings in the checks map")
	}
}

// TestConformance_PprofContract pins the opt-in posture and the
// sub-paths every runtime must expose when pprof is enabled.
func TestConformance_PprofContract(t *testing.T) {
	p := DefaultContract().Pprof
	if !p.OptInOnly {
		t.Error("pprof must be opt-in only — leaking heap details by default is unacceptable")
	}
	want := []string{"", "/cmdline", "/profile", "/symbol", "/trace", "/{name}"}
	if len(p.SubPaths) != len(want) {
		t.Fatalf("pprof sub-paths: got %d, want %d", len(p.SubPaths), len(want))
	}
	for i, sp := range want {
		if p.SubPaths[i] != sp {
			t.Errorf("pprof sub-path[%d] = %q, want %q", i, p.SubPaths[i], sp)
		}
	}
}

// TestConformance_HTTPStatusMapping pins the status-to-HTTP-code rules.
// 200 for "ok", 503 for everything else — operators key on these.
func TestConformance_HTTPStatusMapping(t *testing.T) {
	cases := []struct {
		status   Status
		wantHTTP int
	}{
		{StatusOK, 200},
		{StatusUnavailable, 503},
		{StatusDegraded, 503},
	}
	for _, c := range cases {
		got, ok := HTTPStatusFor(c.status)
		if !ok {
			t.Errorf("HTTPStatusFor(%q) reports invalid status", c.status)
		}
		if got != c.wantHTTP {
			t.Errorf("HTTPStatusFor(%q) = %d, want %d", c.status, got, c.wantHTTP)
		}
	}
	if _, ok := HTTPStatusFor("bogus"); ok {
		t.Error("HTTPStatusFor must reject unknown statuses")
	}
}

// TestConformance_ErrorCodes validates that every protocol error code
// uses the platform.* prefix and that the canonical set is complete.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"platform.invalid_probe_name",
		"platform.duplicate_probe",
		"platform.invalid_status",
		"platform.invalid_endpoint",
		"platform.invalid_envelope",
		"platform.invalid_capability",
		"platform.invalid_prefix",
		"platform.probe_timeout",
		"platform.probe_contract_broken",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "platform.") {
			t.Errorf("error code %q must use platform.* prefix", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

// TestConformance_MissingProbeCode pins the platform.missing_probe
// taxonomy code. It labels the required-readiness behavior (a name in a
// workload's required list that was never registered or discovered) and
// is expressed through the EXISTING degraded envelope, so it is a
// behavioral/documentation label rather than a strict-validation
// diagnostic: it must NOT appear in ValidErrorCodes (which enumerates
// only codes the validators emit), and adding it required no protocol
// bump.
func TestConformance_MissingProbeCode(t *testing.T) {
	if ErrorCodeMissingProbe != "platform.missing_probe" {
		t.Errorf("ErrorCodeMissingProbe = %q, want platform.missing_probe", ErrorCodeMissingProbe)
	}
	if !strings.HasPrefix(ErrorCodeMissingProbe, "platform.") {
		t.Errorf("ErrorCodeMissingProbe %q must use the platform.* prefix", ErrorCodeMissingProbe)
	}
	if ValidErrorCodes[ErrorCodeMissingProbe] {
		t.Error("platform.missing_probe is a behavioral taxonomy label, not a strict-validation diagnostic; it must not be in ValidErrorCodes")
	}
}

// TestConformance_EnvelopeFixtures parses + validates every fixture
// under fixtures/envelope: those in valid/ must produce no errors,
// those in invalid/ must produce at least one. Non-Go runtimes validate
// their envelopes against the same corpus.
func TestConformance_EnvelopeFixtures(t *testing.T) {
	valid, err := filepath.Glob(filepath.Join("fixtures", "envelope", "valid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) == 0 {
		t.Fatal("no valid fixtures in fixtures/envelope/valid")
	}
	for _, path := range valid {
		t.Run("valid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidateEnvelope(data); diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}

	invalid, err := filepath.Glob(filepath.Join("fixtures", "envelope", "invalid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) == 0 {
		t.Fatal("no invalid fixtures in fixtures/envelope/invalid")
	}
	for _, path := range invalid {
		t.Run("invalid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidateEnvelope(data); !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

// TestConformance_JoinPrefix validates prefix-normalisation and path
// joining. Workloads with equivalent prefixes must mount on identical
// final paths.
func TestConformance_JoinPrefix(t *testing.T) {
	cases := []struct {
		prefix, endpoint, want string
	}{
		{"", "/healthz", "/healthz"},
		{"/", "/healthz", "/healthz"},
		{"//", "/healthz", "/healthz"},
		{"/_", "/healthz", "/_/healthz"},
		{"/_/", "/healthz", "/_/healthz"},
		{"_", "/healthz", "/_/healthz"},
		{"/admin", "/livez", "/admin/livez"},
		{"/admin/", "/livez", "/admin/livez"},
		{"//admin", "/livez", "/admin/livez"},
		{" admin", "/livez", "/admin/livez"},
		{"/ admin /", "/livez", "/admin/livez"},
	}
	for _, c := range cases {
		got := JoinPrefix(c.prefix, c.endpoint)
		if got != c.want {
			t.Errorf("JoinPrefix(%q, %q) = %q, want %q", c.prefix, c.endpoint, got, c.want)
		}
	}
}
