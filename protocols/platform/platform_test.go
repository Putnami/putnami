package platform

import (
	"strings"
	"testing"
)

// --- ValidateProbeName ---

func TestValidateProbeName_AcceptsCanonical(t *testing.T) {
	for _, name := range []string{
		"database",
		"db-pool",
		"cache.l1",
		"upstream/billing",
		"a",
		"a_b",
		"x.y-z/w",
	} {
		if diags := ValidateProbeName(name); len(diags) > 0 {
			t.Errorf("ValidateProbeName(%q): unexpected diags %v", name, diags)
		}
	}
}

func TestValidateProbeName_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",                      // empty
		"-leading-dash",         // must start with [a-z0-9]
		".leading-dot",          // same
		"Database",              // uppercase
		"db pool",               // space
		"db:pool",               // colon (used by metric flatten)
		strings.Repeat("a", 65), // too long
	}
	for _, name := range cases {
		if diags := ValidateProbeName(name); len(diags) == 0 {
			t.Errorf("ValidateProbeName(%q): expected diagnostics, got none", name)
		}
	}
}

// --- ValidateStatus ---

func TestValidateStatus(t *testing.T) {
	for _, s := range []Status{StatusOK, StatusUnavailable, StatusDegraded} {
		if diags := ValidateStatus(s); len(diags) > 0 {
			t.Errorf("ValidateStatus(%q): unexpected diags %v", s, diags)
		}
	}
	if diags := ValidateStatus("bogus"); len(diags) != 1 {
		t.Errorf("ValidateStatus(%q): expected 1 diag, got %d", "bogus", len(diags))
	}
}

// --- ValidateEnvelope ---

func TestValidateEnvelope_OkWithChecks(t *testing.T) {
	e := Envelope{
		Status: StatusOK,
		Checks: map[string]CheckEntry{"database": "ok", "cache": "ok"},
	}
	if diags := ValidateEnvelope(e); len(diags) > 0 {
		t.Errorf("unexpected diags: %v", diags)
	}
}

func TestValidateEnvelope_OkRejectsFailingProbe(t *testing.T) {
	// "ok" envelope with a failing probe is a contract violation —
	// the runtime should have set status to "degraded".
	e := Envelope{
		Status: StatusOK,
		Checks: map[string]CheckEntry{"database": "ok", "cache": "conn refused"},
	}
	diags := ValidateEnvelope(e)
	if len(diags) == 0 {
		t.Fatal("expected contract violation for ok status with failing probe")
	}
}

func TestValidateEnvelope_DegradedRequiresFailure(t *testing.T) {
	// "degraded" with all-ok checks is a contract violation.
	e := Envelope{
		Status: StatusDegraded,
		Checks: map[string]CheckEntry{"database": "ok"},
	}
	diags := ValidateEnvelope(e)
	if len(diags) == 0 {
		t.Fatal("expected contract violation for degraded status with no failure")
	}
}

func TestValidateEnvelope_DegradedAccepted(t *testing.T) {
	e := Envelope{
		Status: StatusDegraded,
		Checks: map[string]CheckEntry{"database": "conn refused", "cache": "ok"},
	}
	if diags := ValidateEnvelope(e); len(diags) > 0 {
		t.Errorf("unexpected diags: %v", diags)
	}
}

func TestValidateEnvelope_UnavailableForbidsChecks(t *testing.T) {
	// Before Start / after Stop the runtime short-circuits before
	// running probes; emitting check entries is a contract violation.
	e := Envelope{
		Status: StatusUnavailable,
		Checks: map[string]CheckEntry{"database": "ok"},
	}
	diags := ValidateEnvelope(e)
	if len(diags) == 0 {
		t.Fatal("expected contract violation for unavailable status with checks")
	}
}

// --- NormalizePrefix ---

func TestNormalizePrefix_Canonical(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{" ", ""},
		{"/", ""},
		{"//", ""},
		{"/_", "/_"},
		{"/_/", "/_"},
		{"_", "/_"},
		{"/admin", "/admin"},
		{"/admin/", "/admin"},
		{"/admin//", "/admin"},
		{"//admin", "/admin"},
		{" admin", "/admin"},
		{" /admin/ ", "/admin"},
		{"admin/", "/admin"},
		{"/api/v1", "/api/v1"},
		{"/ /", ""},
		{"/ v1", "/v1"},
		{"v1 /", "/v1"},
		{"\t/v1/\n", "/v1"},
		{"/api//v1", "/api//v1"},
		{"\v/v1/\f\r", "/v1"},
		{"\u00a0v1", "/\u00a0v1"},
	}
	for _, c := range cases {
		if got := NormalizePrefix(c.in); got != c.want {
			t.Errorf("NormalizePrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- EndpointByKind ---

func TestEndpointByKind(t *testing.T) {
	if e, ok := EndpointByKind(EndpointHealth); !ok || e.Path != PathHealthz {
		t.Errorf("EndpointByKind(health) = %v ok=%v", e, ok)
	}
	if _, ok := EndpointByKind("bogus"); ok {
		t.Error("EndpointByKind(bogus): expected !ok")
	}
}
