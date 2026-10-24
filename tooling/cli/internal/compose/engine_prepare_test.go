package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestEnginePrepare_ReturnsAnEngineRefusalWithoutPrintingIt holds the
// preparation run to returning an engine refusal: the error becomes the
// composition's failure, which the caller reports, so a copy on stderr would
// be a second report.
func TestEnginePrepare_ReturnsAnEngineRefusalWithoutPrintingIt(t *testing.T) {
	t.Setenv(git.SourceRevisionEnv, "not-a-commit")
	opts := Options{WorkspaceRoot: t.TempDir(), Target: &workspace.Project{ID: "/app"}}

	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = writer
	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		captured <- string(data)
	}()
	_, prepareErr := enginePrepare(context.Background(), opts, []string{"/app"}, []string{"serve"})
	os.Stderr = original
	_ = writer.Close()
	stderr := <-captured
	_ = reader.Close()

	if prepareErr == nil || !strings.Contains(prepareErr.Error(), git.SourceRevisionEnv) {
		t.Fatalf("enginePrepare error = %v, want the refusal of the source revision", prepareErr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want the refusal returned and not printed", stderr)
	}
}

// TestPrepareThrough_ReportsTheWarningsOfItsEngineRun holds the preparation
// run to the caller's --quiet, and to reporting the warnings of its workspace
// load into Options.Warnings, whether the preparation succeeded, the run
// failed, or the engine returned an error.
func TestPrepareThrough_ReportsTheWarningsOfItsEngineRun(t *testing.T) {
	t.Parallel()
	warning := workspace.Warning{Code: workspace.WarningCodeJobsIgnored, Message: "project /app declares jobs"}
	for _, tc := range []struct {
		name    string
		result  engine.SessionResult
		err     error
		success bool
	}{
		{"a successful preparation", engine.SessionResult{ExitCode: engine.ExitSuccess}, nil, true},
		{"a failed run", engine.SessionResult{ExitCode: engine.ExitError}, nil, false},
		{"an engine error", engine.SessionResult{ExitCode: engine.ExitError}, errors.New("refused"), false},
	} {
		for _, quiet := range []bool{false, true} {
			name := tc.name
			if quiet {
				name += " under --quiet"
			}
			t.Run(name, func(t *testing.T) {
				report := workspace.NewWarningReport(nil)
				root := t.TempDir()
				opts := Options{
					WorkspaceRoot: root, Target: &workspace.Project{ID: "/app"}, Quiet: quiet, Warnings: report,
				}
				var request engine.Request
				run := func(_ context.Context, r engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
					request = r
					r.WarningReport.Keep([]workspace.Warning{warning})
					result := tc.result
					if tc.success {
						result.Workspace = workspace.NewWorkspace(root, &wsproto.Config{}, nil)
						result.Withheld = []*jobs.ScheduledJob{{}}
					}
					return result, tc.err
				}
				_, err := prepareThrough(context.Background(), run, opts, []string{"/app"}, []string{"serve"})
				if tc.success != (err == nil) {
					t.Fatalf("prepareThrough error = %v, want success = %v", err, tc.success)
				}
				if request.Global.Quiet != quiet {
					t.Fatalf("request Quiet = %v, want the caller's %v", request.Global.Quiet, quiet)
				}
				if request.WarningReport != report {
					t.Fatal("the preparation run does not report into the caller's report")
				}
				got := report.Warnings()
				if len(got) != 1 || got[0] != warning {
					t.Fatalf("the report holds %+v, want the run's warning", got)
				}
			})
		}
	}
}
