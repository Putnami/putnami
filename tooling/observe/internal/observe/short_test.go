package observe

import (
	"bytes"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

func TestTheTextSummaryStaysShort(t *testing.T) {
	spectest.Proves(t, feature, "short-log-summary", "the-text-summary-stays-short")
	o := observeSBOMDemo(t, nil, "head.cdx.json", "base.cdx.json")
	o.Diagnostics = append(o.Diagnostics,
		diag.Diagnostic{Severity: diag.Info, Code: "observe.example_info", Message: "an info the short summary leaves out"},
		diag.Warningf("observe.example_warning", "", "a warning the short summary keeps"))
	var text bytes.Buffer
	if err := WriteText(&text, o); err != nil {
		t.Fatal(err)
	}
	got := text.String()
	requireLines(t, got,
		"putnami observe: GitHub Actions, ",
		"  changed  ",
		"  components  ",
		"  changed lines  ",
		"  dependencies  3 changes, 0 license changes, vulnerabilities 1 introduced and 3 fixed\n",
		"  warning observe.example_warning: a warning the short summary keeps\n",
	)
	for _, absent := range []string{"an info the short summary leaves out", "  reports  ", "tests and coverage per component", "    upgraded   lodash", "package-lock.json", "  specs  ", "  decisions  "} {
		if strings.Contains(got, absent) {
			t.Errorf("the short summary holds %q:\n%s", absent, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines > 20 {
		t.Errorf("the short summary has %d lines:\n%s", lines, got)
	}
}

func TestDependencyCountsWithoutAnSBOM(t *testing.T) {
	spectest.Proves(t, feature, "short-log-summary", "the-text-summary-stays-short")
	changes := []evidence.DependencyChange{{Name: "zod", Change: evidence.DependencyAdded, To: "3.0.0"}}
	if got, want := dependencyCounts(changes, nil), "1 change; without an SBOM, their licenses and vulnerabilities are unknown"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := dependencyCounts(nil, nil); got != "0 changes" {
		t.Errorf("got %q", got)
	}
	licensed := []evidence.DependencyChange{{
		Name: "left-pad", Change: evidence.DependencyUpgraded,
		Before: &evidence.DependencyState{Licenses: []string{"MIT"}},
		After:  &evidence.DependencyState{Licenses: []string{"Apache-2.0"}},
	}}
	if got, want := dependencyCounts(licensed, &evidence.SBOM{}), "1 change, 1 license change, vulnerabilities 0 introduced and 0 fixed"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
