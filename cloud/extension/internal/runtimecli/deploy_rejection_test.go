package runtimecli

import (
	"strings"
	"testing"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
)

func rejection(index int64, project, message string) controlapiclient.RejectionDetail {
	out := controlapiclient.RejectionDetail{Index: &index, Error: &message}
	if project != "" {
		out.Project = &project
	}
	return out
}

// TestDeployRejectionSummary_RendersAll pins the CLI contract: when the
// control plane returns an aggregated deploy-preflight refusal, EVERY rejection
// is rendered on its own line, naming its project, so a multi-problem release
// is diagnosed in one round-trip.
func TestDeployRejectionSummary_RendersAll(t *testing.T) {
	got, ok := deployRejectionSummary([]controlapiclient.RejectionDetail{
		rejection(0, "accounts/workloads/a-server", "projects[0]: image not found"),
		rejection(2, "accounts/workloads/c-server", "projects[2]: bundle not published"),
	})
	if !ok {
		t.Fatalf("expected ok=true when rejections present; got %q", got)
	}
	for _, want := range []string{
		"preflight rejected 2 problem(s):",
		"accounts/workloads/a-server: projects[0]: image not found",
		"accounts/workloads/c-server: projects[2]: bundle not published",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(got, "\n  - "); lines != 2 {
		t.Errorf("want 2 rejection lines, got %d:\n%s", lines, got)
	}
}

// TestDeployRejectionSummary_FallsBackWhenEmpty pins the fallback: no
// rejection, or one without an error, keeps the refusal's single message.
func TestDeployRejectionSummary_FallsBackWhenEmpty(t *testing.T) {
	if got, ok := deployRejectionSummary(nil); ok {
		t.Errorf("no rejections: expected ok=false, got %q", got)
	}
	if got, ok := deployRejectionSummary([]controlapiclient.RejectionDetail{rejection(0, "a", "")}); ok {
		t.Errorf("rejection without an error: expected ok=false, got %q", got)
	}
}

// TestDeployRejectionSummary_RendersWithoutProject pins that a rejection missing a
// project name still renders its message (no leading ": ").
func TestDeployRejectionSummary_RendersWithoutProject(t *testing.T) {
	got, ok := deployRejectionSummary([]controlapiclient.RejectionDetail{rejection(0, "", "projects[0]: bad manifest")})
	if !ok {
		t.Fatalf("expected ok=true; got %q", got)
	}
	if !strings.Contains(got, "\n  - projects[0]: bad manifest") {
		t.Errorf("want a project-less rejection line, got:\n%s", got)
	}
}
