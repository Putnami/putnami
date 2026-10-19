package registrycred

import (
	"strings"
	"testing"
)

const (
	npmMissing = "no registry credential for npm.putnami.dev: needs a signed-in user; PUTNAMI_CLOUD_TOKEN cannot read it; continuing with the existing credential"
	goMissing  = "not signed in for go.putnami.dev; " + SignInHint
)

func resetHeldForTest() {
	heldMu.Lock()
	defer heldMu.Unlock()
	heldWarnings = nil
}

// report runs ReportRefusal and returns the warn lines it logged.
func report(t *testing.T, evidence string) []string {
	t.Helper()
	var lines []string
	ReportRefusal(evidence, func(level, message string) {
		if level != "warn" {
			t.Errorf("level = %q, want warn", level)
		}
		lines = append(lines, message)
	})
	return lines
}

func TestReportRefusal_LogsTheHeldWarningOnlyOnA401Or403(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence string
		want     bool
	}{
		{name: "bun 401", evidence: "error: GET https://npm.putnami.dev/@putnami%2fcore - 401", want: true},
		{name: "bun 403 with a reason", evidence: "error: 403 Forbidden", want: true},
		{name: "a metadata lookup", evidence: "dist-tag \"next\" for @putnami/cli: packument request failed (registry npm.putnami.dev, HTTP 401, no credential sent)", want: true},
		{name: "a refusal from another host", evidence: "go: downloading go.putnami.dev/x v1.0.0\ngo: go.putnami.dev/x@v1.0.0: reading https://go.putnami.dev/x/@v/v1.0.0.mod: 403 Forbidden", want: false},
		{name: "unauthorized word", evidence: "error: Unauthorized", want: true},
		{name: "not found", evidence: "error: GET https://npm.putnami.dev/@putnami%2fcore - 404"},
		{name: "a version ending in 401", evidence: "error: no matching version 1.4.401"},
		{name: "a port ending in 401", evidence: "dial tcp 127.0.0.1:4013: connection refused"},
		{name: "a timeout", evidence: "timed out after 2m0s"},
		{name: "empty", evidence: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHeldForTest()
			t.Cleanup(resetHeldForTest)
			HoldWarning("npm.putnami.dev", npmMissing)

			lines := report(t, tc.evidence)
			if (len(lines) == 1) != tc.want {
				t.Fatalf("lines = %q, want a warning: %t", lines, tc.want)
			}
			if tc.want {
				if !strings.Contains(lines[0], "no registry credential for npm.putnami.dev") {
					t.Errorf("warning = %q, want it to name the missing credential", lines[0])
				}
				if strings.Contains(lines[0], "continuing") {
					t.Errorf("warning = %q, want no tail that says the run continues", lines[0])
				}
				if again := report(t, tc.evidence); len(again) != 0 {
					t.Errorf("the warning was logged twice: %q", again)
				}
			}
		})
	}
}

func TestReportRefusal_ReadsTheHostOfTheRefusedURL(t *testing.T) {
	resetHeldForTest()
	t.Cleanup(resetHeldForTest)
	HoldWarning("npm.putnami.dev", npmMissing)
	HoldWarning("go.putnami.dev", goMissing)

	// Another registry refused: neither held host is concerned.
	if lines := report(t, "error: GET https://registry.npmjs.org/left-pad - 401"); len(lines) != 0 {
		t.Fatalf("another host's refusal logged %q", lines)
	}
	// The Go origin refused: only its warning is logged, and the npm one stays held.
	lines := report(t, "go: x@v1.0.0: reading https://go.putnami.dev:443/x/@v/v1.0.0.mod: 403 Forbidden")
	if len(lines) != 1 || !strings.Contains(lines[0], "go.putnami.dev") || !strings.Contains(lines[0], SignInHint) {
		t.Fatalf("lines = %q, want the Go origin warning only", lines)
	}
	if lines := report(t, "error: 401"); len(lines) != 1 || !strings.Contains(lines[0], "npm.putnami.dev") {
		t.Fatalf("lines = %q, want the npm warning held until a refusal", lines)
	}
}

func TestHoldWarning_DeduplicatesAndReportsTheFullTailOnlyOnce(t *testing.T) {
	resetHeldForTest()
	t.Cleanup(resetHeldForTest)
	HoldWarning("NPM.putnami.dev ", npmMissing)
	HoldWarning("npm.putnami.dev", npmMissing)

	if lines := report(t, "error: 403 Forbidden"); len(lines) != 1 {
		t.Fatalf("lines = %q, want one", lines)
	}
}
