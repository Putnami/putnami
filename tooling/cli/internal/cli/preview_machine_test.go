package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// TestAppRun_PreviewMachineOutputIsATypedPlan pins the preview boundary at the
// real terminal adapter: --plan and plan-only --dry-run return a plan document,
// never a synthetic run verdict and never a human table on a machine channel.
func TestAppRun_PreviewMachineOutputIsATypedPlan(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "machine-previews-are-typed-plans-not-runs")

	for _, previewFlag := range []string{"--plan", "--dry-run"} {
		for _, format := range []string{"json", "jsonl"} {
			t.Run(strings.TrimPrefix(previewFlag, "--")+"/"+format, func(t *testing.T) {
				wsRoot := t.TempDir()
				writeWorkspaceAliasFixture(t, wsRoot)

				stdout, _ := runPreviewCLI(t, wsRoot, previewFlag, "--output="+format)
				trimmed := strings.TrimSpace(stdout)
				if strings.Contains(trimmed, " jobs  · ") {
					t.Fatalf("machine stdout contains the human plan table:\n%s", stdout)
				}

				var document struct {
					ProtocolVersion int                      `json:"protocolVersion"`
					Record          string                   `json:"record"`
					Status          string                   `json:"status"`
					Plan            *protocolcli.PlanSummary `json:"plan"`
					Run             json.RawMessage          `json:"run"`
					MachineOutput   json.RawMessage          `json:"machineOutput"`
				}
				if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
					t.Fatalf("machine preview is not one JSON document: %v\nstdout:\n%s", err, stdout)
				}
				kind := protocolcli.DocumentResultEnvelope
				if format == "jsonl" {
					kind = protocolcli.DocumentSessionStreamRecord
					if got := document.Record; got != "plan:end" {
						t.Fatalf("JSONL record = %v, want plan:end", got)
					}
				} else {
					if got := document.Status; got != protocolcli.StatusSuccess {
						t.Fatalf("JSON status = %v, want success", got)
					}
				}
				if violations := protocolcli.ValidateDocument(kind, []byte(trimmed)); len(violations) != 0 {
					t.Fatalf("machine preview violates %s: %+v\n%s", kind, violations, stdout)
				}
				if len(document.Run) != 0 {
					t.Fatalf("plan-only preview claims an executed run:\n%s", stdout)
				}
				if len(document.MachineOutput) != 0 {
					t.Fatalf("plan-only preview claims a session artifact:\n%s", stdout)
				}
				assertNonEmptyPreviewPlan(t, document.Plan)

				if _, err := os.Stat(filepath.Join(wsRoot, ".putnami", "sessions")); !os.IsNotExist(err) {
					t.Fatalf("preview created a session directory: %v", err)
				}
			})
		}
	}
}

// TestAppRun_PreviewHumanOutputIsUnchanged guards the default terminal surface
// while the explicit machine formats acquire their typed preview document.
func TestAppRun_PreviewHumanOutputIsUnchanged(t *testing.T) {
	outputs := make(map[string]string, 2)
	for _, previewFlag := range []string{"--plan", "--dry-run"} {
		wsRoot := t.TempDir()
		writeWorkspaceAliasFixture(t, wsRoot)
		stdout, _ := runPreviewCLI(t, wsRoot, previewFlag)
		outputs[previewFlag] = stdout
		for _, want := range []string{"app", "deploy", "1 jobs", "1 projects"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%s human preview misses %q:\n%s", previewFlag, want, stdout)
			}
		}
	}
	if outputs["--plan"] != outputs["--dry-run"] {
		t.Fatalf("--plan and plan-only --dry-run human tables differ:\n--plan:\n%s\n--dry-run:\n%s",
			outputs["--plan"], outputs["--dry-run"])
	}
}

// TestAppRun_EmptyImpactedPreviewIsOneMachinePlan covers the no-op path before
// planning: selection notices must not corrupt stdout, and the terminal record
// remains an explicit empty plan that a caller can reject by metrics.tasks.
func TestAppRun_EmptyImpactedPreviewIsOneMachinePlan(t *testing.T) {
	requireGit(t)
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	initAliasGitRepo(t, wsRoot)

	stdout, _ := runPreviewCLI(t, wsRoot,
		"--dry-run", "--impacted", "--baseline=HEAD", "--output=jsonl")
	trimmed := strings.TrimSpace(stdout)
	if strings.Contains(trimmed, "No jobs matched") || strings.Contains(trimmed, "No impacted projects") {
		t.Fatalf("machine stdout contains a human no-op notice:\n%s", stdout)
	}
	if got := len(strings.Split(trimmed, "\n")); got != 1 {
		t.Fatalf("machine preview wrote %d lines, want one terminal record:\n%s", got, stdout)
	}
	var record protocolcli.SessionStreamRecord
	if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
		t.Fatalf("empty impacted preview is not JSON: %v\n%s", err, stdout)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, []byte(trimmed)); len(violations) != 0 {
		t.Fatalf("empty impacted preview violates protocol: %+v\n%s", violations, stdout)
	}
	if record.Record != protocolcli.RecordPlanEnd || record.Plan == nil ||
		record.Plan.Metrics.Tasks != 0 || len(record.Plan.Tasks) != 0 {
		t.Fatalf("empty impacted preview = %+v, want plan:end with an explicit zero-task plan", record)
	}
	if record.Run != nil || record.MachineOutput != nil {
		t.Fatalf("empty impacted preview claims a run/session artifact: %+v", record)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("empty impacted preview created a session directory: %v", err)
	}
}

func runPreviewCLI(t *testing.T, wsRoot string, args ...string) (stdout, stderr string) {
	t.Helper()
	argv := append([]string{"deploy", "app"}, args...)
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			app, err := NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			t.Chdir(wsRoot)
			if code := app.Run(context.Background(), argv); code != ExitSuccess {
				t.Fatalf("putnami %s exit = %d, want %d", strings.Join(argv, " "), code, ExitSuccess)
			}
		})
	})
	return stdout, stderr
}

func assertNonEmptyPreviewPlan(t *testing.T, plan *protocolcli.PlanSummary) {
	t.Helper()
	if plan == nil {
		t.Fatal("machine preview has no typed plan")
	}
	if plan.Metrics.Tasks != 1 || plan.Metrics.Projects != 1 {
		t.Fatalf("plan metrics = %#v, want one task in one project", plan.Metrics)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("plan tasks = %#v, want one typed task", plan.Tasks)
	}
}
