package compose

import (
	"context"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// compose honors the invocation's output flags: each member's serve run takes
// --verbose, --quiet and --no-color as typed, so it renders the member's serve
// log as `putnami serve` would with them. A structured run renders without
// color whatever was typed, because its stderr is a log.
func TestRun_EachServeRunTakesTheInvocationsOutputFlags(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", serveRunRequirement,
		"each-serve-run-takes-the-invocations-output-flags")
	for _, tc := range []struct {
		name                    string
		mode                    protocolcli.OutputMode
		verbose, quiet, noColor bool
		want                    []string
	}{
		{name: "text without flags"},
		{name: "verbose", verbose: true, want: []string{"--verbose"}},
		{name: "quiet without color", quiet: true, noColor: true, want: []string{"--quiet", "--no-color"}},
		{name: "jsonl", mode: protocolcli.OutputJSONL, want: []string{"--no-color"}},
		{name: "jsonl verbose", mode: protocolcli.OutputJSONL, verbose: true, want: []string{"--verbose", "--no-color"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
			run := startRequest(t, fixture, Request{
				Args: []string{"app", "--port", "0", "--no-watch"}, Output: tc.mode,
				Verbose: tc.verbose, Quiet: tc.quiet, NoColor: tc.noColor,
			})
			_ = run.line(t)
			argv := fixture.argv(t, "/app")
			run.cancel()
			if code, _ := run.wait(t); code != protocolcli.ExitSuccess {
				t.Fatalf("exit %d\nstderr:\n%s", code, run.stderr.String())
			}
			base := []string{"serve", "--projects", "/app", "--port", "0", "--no-watch", "--serve-env", "-", "--output=jsonl"}
			if want := slices.Concat(base, tc.want); !slices.Equal(argv, want) {
				t.Errorf("the serve run started as %v, want %v", argv, want)
			}
		})
	}
}

// A caller that discards the serve logs still learns why a preparation
// failed: Failures receives the member, why, and the output of the step that
// failed, from the session stream: its log events but debug ones, its
// diagnostics and its error. The steps that succeeded and the serve log are
// not in it. A run that failed without a failed step, as on a refused flag,
// reports the last lines it wrote. A preparation that succeeds reports
// nothing there.
func TestUp_FailuresReceiveOnlyAFailedPreparationsOutput(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", serveOutputRequirement,
		"a-caller-without-serve-logs-sees-only-a-failed-preparation")
	for _, tc := range []struct {
		cli, want string
	}{
		{
			cli: fakeCLIFailPrepare,
			want: "/app: the serve run's step /app:serve~generate failed before the serve step started (failed, exit code 1):\n" +
				"  | error: " + fakeCLIGenerateLog + "\n" +
				"  | " + fakeCLIGenerateFile + ":3: error: " + fakeCLIGenerateProblem + "\n" +
				"  | " + fakeCLIGenerateStderr + "\n",
		},
		{
			cli: fakeCLIUsage,
			want: "/app: the serve run exited before the serve step started (exit code 2):\n" +
				"  | putnami: serve: unknown flag --fixture-refused\n",
		},
	} {
		t.Run(tc.cli, func(t *testing.T) {
			fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", cli: tc.cli})
			var failures lockedBuffer
			ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
			defer cancel()
			_, err := up(ctx, Options{
				Workspace: fixture.ws, Executable: fixture.executable, Target: fixture.projects["/app"],
				ReadyTimeout: 10 * hangDetector, Failures: &failures,
			}, fixture.deps(nil))
			if composeErr := composeError(t, err); composeErr.Code != CodePrepareFailed {
				t.Fatalf("error = %v, want compose.prepare_failed", err)
			}
			if got := failures.String(); got != tc.want {
				t.Errorf("failures:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
	t.Run("passed", func(t *testing.T) {
		fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
		var failures, logs lockedBuffer
		ctx, cancel := context.WithTimeout(context.Background(), hangDetector)
		defer cancel()
		comp, err := up(ctx, Options{
			Workspace: fixture.ws, Executable: fixture.executable, Target: fixture.projects["/app"],
			ReadyTimeout: hangDetector, Failures: &failures, Logs: &logs,
		}, fixture.deps(nil))
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		if report := comp.Close(context.Background()); report.State != CleanupClean {
			t.Fatalf("cleanup = %+v", report)
		}
		if got := failures.String(); got != "" {
			t.Errorf("failures = %q, want nothing from a preparation that succeeded", got)
		}
		if got := logs.String(); !strings.Contains(got, fakeCLIGenerated) {
			t.Errorf("logs = %q, want the preparation's output as it arrived", got)
		}
	})
}
