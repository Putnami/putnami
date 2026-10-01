package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseAndValidateManifest_AcceptsSubcommandWorkspaceAndGroupDefault(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/command-groups-without-workspace.json")
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	group := m.CommandGroups["audit"]
	if group.Default != "run" {
		t.Fatalf("group default = %q, want run", group.Default)
	}
	run := group.Subcommands["run"]
	if run.Workspace != SubcommandWorkspaceOptional || !run.RunsWithoutWorkspace() {
		t.Fatalf("run subcommand = %+v, want an optional interactive subcommand", run)
	}
	report := group.Subcommands["report"]
	if report.Workspace != SubcommandWorkspaceRequired || report.RunsWithoutWorkspace() {
		t.Fatalf("report subcommand = %+v, want a required subcommand", report)
	}
}

func TestValidateManifest_RejectsOptionalWorkspaceWithoutInteractive(t *testing.T) {
	diags := validateFixture(t, "fixtures/invalid/subcommand-workspace-optional-not-interactive.json")
	d := requireDiagnostic(t, diags, "invalid-workspace-requirement", "commandGroups.audit.subcommands.run.workspace")
	if !strings.Contains(d.Message, `"interactive": true`) {
		t.Fatalf("message %q does not name the interactive requirement", d.Message)
	}
}

func TestValidateManifest_RejectsUnknownWorkspaceValue(t *testing.T) {
	diags := validateFixture(t, "fixtures/invalid/subcommand-workspace-unknown.json")
	d := requireDiagnostic(t, diags, "invalid-enum", "commandGroups.audit.subcommands.run.workspace")
	if !strings.Contains(d.Message, `"never"`) {
		t.Fatalf("message %q does not name the rejected value", d.Message)
	}
}

func TestValidateManifest_RejectsUnknownGroupDefault(t *testing.T) {
	diags := validateFixture(t, "fixtures/invalid/command-group-unknown-default.json")
	d := requireDiagnostic(t, diags, "unresolved-default-subcommand", "commandGroups.audit.default")
	if !strings.Contains(d.Message, `"scan"`) || !strings.Contains(d.Message, "run") {
		t.Fatalf("message %q does not name the default and the declared subcommands", d.Message)
	}
}

func TestValidateManifest_NestedOptionalWorkspaceRequiresInteractive(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"audit-run": {Run: []PipelineStep{{ID: "run", Task: "audit-exec"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"audit": {
				Subcommands: map[string]SubcommandDefinition{
					"run": {
						Command:     "audit-run",
						Interactive: true,
						Workspace:   SubcommandWorkspaceOptional,
						Subcommands: map[string]SubcommandDefinition{
							"deep": {Workspace: SubcommandWorkspaceOptional},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"audit-exec": {Kind: "command", Command: "echo"},
		},
	}
	requireDiagnostic(t, ValidateManifest(m), "invalid-workspace-requirement",
		"commandGroups.audit.subcommands.run.subcommands.deep.workspace")
}

func TestSubcommandDefinition_RunsWithoutWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  SubcommandDefinition
		want bool
	}{
		{"optional interactive", SubcommandDefinition{Workspace: SubcommandWorkspaceOptional, Interactive: true}, true},
		{"optional without interactive", SubcommandDefinition{Workspace: SubcommandWorkspaceOptional}, false},
		{"absent interactive", SubcommandDefinition{Interactive: true}, false},
		{"required interactive", SubcommandDefinition{Workspace: SubcommandWorkspaceRequired, Interactive: true}, false},
		{"unknown value", SubcommandDefinition{Workspace: "never", Interactive: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.RunsWithoutWorkspace(); got != tc.want {
				t.Fatalf("RunsWithoutWorkspace() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConformance_SubcommandWorkspaceSchemaMatchesStrictParser runs the schema's
// subcommand rules against the same fixtures the strict parser judges, so the
// authoring schema and the parser agree on the workspace vocabulary.
func TestConformance_SubcommandWorkspaceSchemaMatchesStrictParser(t *testing.T) {
	def := readSchemaDefinition(t, "subcommandDefinition")
	for _, fixture := range []string{
		"fixtures/invalid/subcommand-workspace-optional-not-interactive.json",
		"fixtures/invalid/subcommand-workspace-unknown.json",
	} {
		t.Run("reject "+filepath.Base(fixture), func(t *testing.T) {
			rejected := false
			for _, sub := range fixtureSubcommands(t, fixture) {
				if conditionalSchemaViolations(def, sub) > 0 || !schemaSubsetMatches(def, sub) {
					rejected = true
				}
			}
			if !rejected {
				t.Fatalf("the schema accepts every subcommand of %s", fixture)
			}
		})
	}
	for _, sub := range fixtureSubcommands(t, "fixtures/valid/command-groups-without-workspace.json") {
		if conditionalSchemaViolations(def, sub) > 0 || !schemaSubsetMatches(def, sub) {
			t.Errorf("the schema rejects a valid subcommand: %v", sub)
		}
	}
}

func validateFixture(t *testing.T, path string) []diag.Diagnostic {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, diags := ParseAndValidateManifest(data)
	return diags
}

func requireDiagnostic(t *testing.T, diags []diag.Diagnostic, code, field string) diag.Diagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && d.Field == field {
			return d
		}
	}
	t.Fatalf("no %s diagnostic on %s, got %v", code, field, diags)
	return diag.Diagnostic{}
}

func fixtureSubcommands(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var subs []map[string]any
	groups, _ := document["commandGroups"].(map[string]any)
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		rawSubs, _ := group["subcommands"].(map[string]any)
		for _, rawSub := range rawSubs {
			if sub, ok := rawSub.(map[string]any); ok {
				subs = append(subs, sub)
			}
		}
	}
	if len(subs) == 0 {
		t.Fatalf("%s declares no subcommand", path)
	}
	return subs
}
