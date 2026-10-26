package scheduler

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
)

// TestBuildJobContext_AnInteractiveLaunchHandsItsLoadWarnings pins the
// producer half of workspaceWarnings: the context of an interactive launch
// whose load warned carries the warnings in order, in a v2 document the
// protocol's strict parser accepts. A job without warnings, which every
// planned job is, omits the member, so the context a planned job reads keeps
// its bytes.
func TestBuildJobContext_AnInteractiveLaunchHandsItsLoadWarnings(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory",
		"an-interactive-launch-hands-its-load-warnings-to-the-subcommand")
	t.Parallel()
	warnings := []protocolcli.WorkspaceWarning{
		{Code: "workspace.jobs_ignored", Message: "project /app declares jobs, which putnami ignores"},
		{Code: "workspace.name_divergence", Message: "project /lib is named lib in putnami.json and @acme/lib in package.json"},
	}
	ws, job := interactiveTestJob(t, t.TempDir())
	job.WorkspaceWarnings = warnings

	data, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the context violates the job context protocol: %v\n%s", diags, data)
	}
	if !reflect.DeepEqual(parsed.WorkspaceWarnings, warnings) {
		t.Fatalf("workspaceWarnings = %+v, want %+v", parsed.WorkspaceWarnings, warnings)
	}

	ws, job = interactiveTestJob(t, "")
	data, err = json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"workspaceWarnings"`)) {
		t.Fatalf("a job without warnings carries the member:\n%s", data)
	}
}
