package main

import (
	"path/filepath"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"

	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// writeDebtFiles commits an empty adoption baseline and an empty waivers file
// at the workspace root. Empty lists keep the verdict the fixture graph's own:
// an entry the graph never produces would turn stale and fail the run.
func writeDebtFiles(t *testing.T, root string) {
	t.Helper()
	writeFixture(t, filepath.Join(root, archproto.BaselineFilename), `{"protocolVersion": 1, "findings": []}`)
	writeFixture(t, filepath.Join(root, archproto.WaiverFilename), `{"protocolVersion": 1, "waivers": []}`)
}

func findingIDWarnings(events []runtimeproto.Event) []runtimeproto.Event {
	var found []runtimeproto.Event
	for _, event := range events {
		if event.Code == sdd.WarningCodeFindingIDsNotPromised {
			found = append(found, event)
		}
	}
	return found
}

func TestArchitectureValidateWarnsOnCommittedFindingIDs(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "unpromised-finding-ids-warn",
		"a-committed-baseline-or-waivers-file-warns")
	root := architectureFixture(t, true)
	writeDebtFiles(t, root)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = architectureWorkspaceOptions(featureproto.VerificationModeEnforce)

	outcome := runJob(t, runArchitectureValidate, ctx)
	warnings := findingIDWarnings(outcome.diagnostics())
	if len(warnings) != 2 {
		t.Fatalf("warnings = %+v, want one per committed file", warnings)
	}
	for index, file := range []string{archproto.BaselineFilename, archproto.WaiverFilename} {
		warning := warnings[index]
		if warning.Severity == nil || *warning.Severity != runtimeproto.SeverityWarning {
			t.Errorf("%s: severity = %v, want warning", file, warning.Severity)
		}
		if warning.Location == nil || warning.Location.File != file {
			t.Errorf("%s: location = %+v, want the committed file", file, warning.Location)
		}
	}
}

func TestArchitectureValidateWithoutDebtFilesDrawsNoFindingIDWarning(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "unpromised-finding-ids-warn",
		"a-workspace-without-them-draws-no-warning")
	root := architectureFixture(t, true)
	ctx := workspaceJobContext(root, architectureMembers(root))
	ctx.Workspace.Options = architectureWorkspaceOptions(featureproto.VerificationModeEnforce)

	outcome := runJob(t, runArchitectureValidate, ctx)
	if warnings := findingIDWarnings(outcome.diagnostics()); len(warnings) != 0 {
		t.Fatalf("a workspace without a baseline or waivers drew %+v", warnings)
	}
}

func TestArchitectureValidateFindingIDWarningNeverChangesTheVerdict(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "unpromised-finding-ids-warn",
		"the-warning-never-changes-the-verdict")
	for _, test := range []struct {
		name    string
		binding bool
		passes  bool
	}{
		{name: "a clean graph still passes", binding: true, passes: true},
		{name: "an undeclared edge still fails", binding: false, passes: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := architectureFixture(t, test.binding)
			writeDebtFiles(t, root)
			ctx := workspaceJobContext(root, architectureMembers(root))
			ctx.Workspace.Options = architectureWorkspaceOptions(featureproto.VerificationModeEnforce)

			outcome := runJob(t, runArchitectureValidate, ctx)
			if len(findingIDWarnings(outcome.diagnostics())) != 2 {
				t.Fatalf("the warnings are missing: %+v", outcome.diagnostics())
			}
			passed := outcome.err == nil && outcome.exitCode() == protocolcli.ExitSuccess
			if passed != test.passes {
				t.Fatalf("passed = %t, want %t: status=%q err=%v", passed, test.passes, outcome.status, outcome.err)
			}
			if outcome.data["valid"] != test.passes {
				t.Errorf("valid = %#v, want %t", outcome.data["valid"], test.passes)
			}
		})
	}
}
