package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

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

// TestPrepareThrough_KeepsTheWarningsOfItsEngineRun holds the preparation run
// to the caller's --quiet, and to handing the warnings its workspace load
// reported to Options.Warnings, whether the preparation succeeded, the run
// failed, or the engine returned an error.
func TestPrepareThrough_KeepsTheWarningsOfItsEngineRun(t *testing.T) {
	t.Parallel()
	warning := workspace.Warning{Code: workspace.WarningCodeJobsIgnored, Message: "project /app declares jobs"}
	for _, tc := range []struct {
		name   string
		result engine.SessionResult
		err    error
	}{
		{"a failed run", engine.SessionResult{ExitCode: engine.ExitError}, nil},
		{"an engine error", engine.SessionResult{ExitCode: engine.ExitError}, errors.New("refused")},
	} {
		for _, quiet := range []bool{false, true} {
			name := tc.name
			if quiet {
				name += " under --quiet"
			}
			t.Run(name, func(t *testing.T) {
				report := workspace.NewWarningReport(nil)
				opts := Options{
					WorkspaceRoot: t.TempDir(), Target: &workspace.Project{ID: "/app"}, Quiet: quiet, Warnings: report,
				}
				var request engine.Request
				run := func(_ context.Context, r engine.Request, _ engine.EventSink) (engine.SessionResult, error) {
					request = r
					result := tc.result
					result.WorkspaceWarnings = []workspace.Warning{warning}
					return result, tc.err
				}
				if _, err := prepareThrough(context.Background(), run, opts, []string{"/app"}, []string{"serve"}); err == nil {
					t.Fatal("prepareThrough succeeded, want the failure of the run")
				}
				if request.Global.Quiet != quiet {
					t.Fatalf("request Quiet = %v, want the caller's %v", request.Global.Quiet, quiet)
				}
				got := report.Warnings()
				if len(got) != 1 || got[0] != warning {
					t.Fatalf("the report holds %+v, want the run's warning", got)
				}
			})
		}
	}
}
