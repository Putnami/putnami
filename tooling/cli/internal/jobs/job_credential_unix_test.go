//go:build unix

package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// jobCredentialFixture is a job of command that this test binary runs as its
// extension's runtime, doing script.
func jobCredentialFixture(t *testing.T, name, command string, script fixtureScript) (*workspace.Workspace, *ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	def := &extension.JobDefinition{
		ExtensionName: "@putnami/test",
		Name:          name,
		CommandName:   command,
		Kind:          "command",
		TimeoutMs:     unboundedJobTimeoutMs,
	}
	fixtureTask(t, def, script)
	executable := def.Command
	def.Command = runtimeCommand
	ext := &extension.ExtensionDescription{
		Name:              "@putnami/test",
		Path:              root,
		Runtime:           &extension.RuntimeDefinition{},
		RuntimeExecutable: executable,
	}
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	return &workspace.Workspace{Root: root, Name: "test-ws"},
		&ScheduledJob{Project: project, Extension: ext, JobDef: def}
}

// hostedJobTest makes the test a hosted run whose own temporary directory is
// a fresh one, and returns that directory.
func hostedJobTest(t *testing.T) string {
	t.Helper()
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	engineTemp := t.TempDir()
	t.Setenv("TMPDIR", engineTemp)
	return engineTemp
}

func readReport(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the job wrote no report: %v", err)
	}
	return string(data)
}

// A hosted fetch reads the provider's answer from its descriptor: the
// credential, or "{}" when the provider holds none or no provider runs. The
// bearer reaches no argument, variable or context file of the job. The fetch's
// environment carries runcredential.HostedFetchEnv, and names this process as
// the CLI a Putnami command it starts runs.
func TestHostedFetchReadsTheCredentialFromItsDescriptor(t *testing.T) {
	hostedJobTest(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	credentialLine, _ := jobCredentialLine(testJobCredential())
	cases := []struct {
		name    string
		install bool
		answer  *registryproto.Credential
		want    string
	}{
		{"a provider that holds a credential", true, testJobCredential(), strings.TrimSuffix(string(credentialLine), "\n")},
		{"a provider that holds none", true, nil, "{}"},
		{"no provider", false, nil, "{}"},
	}
	for _, tc := range cases {
		if tc.install {
			restore := InstallJobReadCredential(func(context.Context) (*registryproto.Credential, error) {
				return tc.answer, nil
			})
			t.Cleanup(restore)
		} else {
			t.Cleanup(InstallJobReadCredential(nil))
		}
		report := filepath.Join(t.TempDir(), "report")
		marker, cli := filepath.Join(t.TempDir(), "marker"), filepath.Join(t.TempDir(), "cli")
		ws, job := jobCredentialFixture(t, extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand,
			fixtureScript{{"read-job-credential", report}, {"write-env", runcredential.HostedFetchEnv, marker},
				{"write-env", registryproto.CLIExecutableEnv, cli}})

		result, err := RunJob(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, nil)
		if err != nil || result.Status != "success" {
			t.Fatalf("%s: RunJob = %+v, %v", tc.name, result, err)
		}
		want := "handed=true\ncredential=" + tc.want + "\nexposed=none\n"
		if got := readReport(t, report); got != want {
			t.Errorf("%s: the job reported\n%s\nwant\n%s", tc.name, got, want)
		}
		if got := readReport(t, marker); got != "1" {
			t.Errorf("%s: the fetch's %s = %q, want 1", tc.name, runcredential.HostedFetchEnv, got)
		}
		if got := readReport(t, cli); got != self {
			t.Errorf("%s: the fetch's %s = %q, want this process's executable %s", tc.name, registryproto.CLIExecutableEnv, got, self)
		}
	}
}

// Every other job of a hosted run gets no descriptor and no
// runcredential.HostedFetchEnv, so a Putnami command it starts runs, and the
// provider is never asked for it.
func TestHostedJobsOtherThanTheFetchGetNoDescriptor(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "only-fetch-gets-the-job-credential")
	hostedJobTest(t)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	for _, command := range []string{toolchainProvisioningCommand, "build"} {
		report, marker := filepath.Join(t.TempDir(), "report"), filepath.Join(t.TempDir(), "marker")
		ws, job := jobCredentialFixture(t, command, command, fixtureScript{{"read-job-credential", report},
			{"write-env", runcredential.HostedFetchEnv, marker}})
		result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
		if err != nil || result.Status != "success" {
			t.Fatalf("%s: RunJob = %+v, %v", command, result, err)
		}
		if got, want := readReport(t, report), "handed=false\ncredential={}\nexposed=none\n"; got != want {
			t.Errorf("%s: the job reported\n%s\nwant\n%s", command, got, want)
		}
		if got := readReport(t, marker); got != "" {
			t.Errorf("%s: the job's %s = %q, want it unset", command, runcredential.HostedFetchEnv, got)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// The offline signal and the cache token, as each job of a hosted run sees
// them, and as each job of a local run sees them. Only the fetch of an
// install's dependency fetch is online on a hosted run: a workspace-fetch that
// a plan selects, such as through an alias, runs offline without the job
// credential.
func TestJobsSeeTheOfflineSignalOfTheirRun(t *testing.T) {
	offline := extensionproto.OfflineDependenciesEnv
	token := runcredential.CacheTokenEnv
	cases := []struct {
		name            string
		hosted          bool
		command         string
		dependencyFetch bool
		conditions      []string
	}{
		{"a hosted build", true, "build", false, []string{"equal:" + offline + "=1", "unset:" + token}},
		{"a hosted fetch", true, extensionproto.WorkspaceFetchCommand, true, []string{"unset:" + offline, "unset:" + token}},
		{"a hosted fetch that a plan selects", true, extensionproto.WorkspaceFetchCommand, false, []string{"equal:" + offline + "=1", "unset:" + token}},
		{"a local build", false, "build", false, []string{"unset:" + offline, "equal:" + token + "=cache-token"}},
		{"a local fetch", false, extensionproto.WorkspaceFetchCommand, true, []string{"unset:" + offline, "equal:" + token + "=cache-token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(InstallJobReadCredential(nil))
			if tc.hosted {
				hostedJobTest(t)
				// What runcredential.Capture leaves in a hosted run's environment.
				t.Setenv(offline, "1")
			} else {
				t.Cleanup(runcredential.SetForTest(""))
				t.Setenv(offline, "")
				_ = os.Unsetenv(offline)
			}
			t.Setenv(token, "cache-token")
			conditions := append([]string{"unset:" + extensionproto.JobCredentialFDEnv}, tc.conditions...)
			ctx := t.Context()
			if tc.dependencyFetch {
				ctx = WithDependencyFetch(ctx)
				if tc.hosted {
					conditions[0] = "!" + conditions[0]
				}
			}
			ws, job := jobCredentialFixture(t, tc.command, tc.command, fixtureScript{append([]string{"verdict"}, conditions...)})
			result, err := RunJob(ctx, ws, job, nil, nil, nil, nil)
			if err != nil || result.Status != "success" {
				t.Fatalf("RunJob = %+v, %v: the job did not see %q", result, err, conditions)
			}
		})
	}
}

// expiringContext is a context a test expires the way a job's timeout does:
// once expire runs, its Err is context.DeadlineExceeded.
type expiringContext struct {
	done chan struct{}
	once sync.Once
}

func newExpiringContext() *expiringContext { return &expiringContext{done: make(chan struct{})} }

func (c *expiringContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *expiringContext) Done() <-chan struct{}       { return c.done }
func (c *expiringContext) Value(any) any               { return nil }
func (c *expiringContext) expire()                     { c.once.Do(func() { close(c.done) }) }
func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A hosted fetch keeps the files that hold its credential in a private
// directory of the engine's temporary directory, which the engine removes
// once the job ended, however it ended.
func TestHostedFetchTemporaryDirectoryIsRemovedHoweverTheJobEnds(t *testing.T) {
	engineTemp := hostedJobTest(t)
	installCountingJobReadCredential(t, testJobCredential(), nil)
	// The job names its temporary directory, then leaves a credential file in
	// it, as a fetch that writes a private .netrc does.
	holdCredential := fixtureScript{
		{"append-env", "${TMPDIR}", "${TMPDIR_REPORT}"},
		{"append-env", "secret", "${TMPDIR}/netrc"},
	}
	cases := []struct {
		name  string
		then  fixtureScript
		run   func(ws *workspace.Workspace, job *ScheduledJob) (*JobResult, error)
		check func(result *JobResult) bool
	}{
		{name: "success", then: fixtureScript{{"exit", "0"}},
			check: func(r *JobResult) bool { return r.Status == "success" }},
		{name: "failure", then: fixtureScript{{"exit", "3"}},
			check: func(r *JobResult) bool { return r.Status == "failed" && r.ExitCode == 3 }},
		{name: "SIGKILL", then: fixtureScript{{"kill-self"}},
			check: func(r *JobResult) bool { return r.Status == "failed" && !r.TimedOut }},
		{name: "cancellation", then: fixtureScript{{"log", "holding"}, {"sleep", "60"}},
			run: func(ws *workspace.Workspace, job *ScheduledJob) (*JobResult, error) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				return RunJob(WithDependencyFetch(ctx), ws, job, nil, nil, nil, func(event RawJobEvent) {
					if event.Message == "holding" {
						cancel()
					}
				})
			},
			check: func(r *JobResult) bool { return r.Status == "canceled" }},
		{name: "timeout", then: fixtureScript{{"log", "holding"}, {"sleep", "60"}},
			run: func(ws *workspace.Workspace, job *ScheduledJob) (*JobResult, error) {
				ctx := newExpiringContext()
				defer ctx.expire()
				return RunJob(WithDependencyFetch(ctx), ws, job, nil, nil, nil, func(event RawJobEvent) {
					if event.Message == "holding" {
						ctx.expire()
					}
				})
			},
			check: func(r *JobResult) bool { return r.Status == "failed" && r.TimedOut }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "tmpdir")
			ws, job := jobCredentialFixture(t, extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand,
				holdCredential.then(tc.then))
			job.JobDef.Env["TMPDIR_REPORT"] = report
			run := tc.run
			if run == nil {
				run = func(ws *workspace.Workspace, job *ScheduledJob) (*JobResult, error) {
					return RunJob(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, nil)
				}
			}
			result, err := run(ws, job)
			if err != nil {
				t.Fatalf("RunJob: %v", err)
			}
			if !tc.check(result) {
				t.Errorf("result = %+v, want the job to end by %s", result, tc.name)
			}
			dir := readReport(t, report)
			if filepath.Dir(dir) != engineTemp || !strings.HasPrefix(filepath.Base(dir), "putnami-job-credential-") {
				t.Fatalf("the job's temporary directory %q is not a fresh directory of %s", dir, engineTemp)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the job's temporary directory %s survives the job: %v", dir, err)
			}
		})
	}
}

// Every other job keeps the engine's temporary directory, on a hosted run as
// on a local one.
func TestOtherJobsKeepTheEngineTemporaryDirectory(t *testing.T) {
	cases := []struct {
		name    string
		hosted  bool
		command string
	}{
		{"a hosted build", true, "build"},
		{"a hosted install", true, toolchainProvisioningCommand},
		{"a local fetch", false, extensionproto.WorkspaceFetchCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var engineTemp string
			if tc.hosted {
				engineTemp = hostedJobTest(t)
			} else {
				t.Cleanup(runcredential.SetForTest(""))
				engineTemp = t.TempDir()
				t.Setenv("TMPDIR", engineTemp)
			}
			t.Cleanup(InstallJobReadCredential(nil))
			ws, job := jobCredentialFixture(t, tc.command, tc.command,
				fixtureScript{{"verdict", "equal:TMPDIR=" + engineTemp}})
			result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
			if err != nil || result.Status != "success" {
				t.Fatalf("RunJob = %+v, %v: the job's TMPDIR is not %s", result, err, engineTemp)
			}
			if entries, _ := os.ReadDir(engineTemp); len(entries) != 0 {
				t.Errorf("the job left %d entries in the engine's temporary directory", len(entries))
			}
		})
	}
}

// An interactive hosted fetch receives the credential and its private
// directory the same way.
func TestInteractiveHostedFetchReadsTheCredential(t *testing.T) {
	engineTemp := hostedJobTest(t)
	installCountingJobReadCredential(t, testJobCredential(), nil)
	report := filepath.Join(t.TempDir(), "report")
	tmpReport := filepath.Join(t.TempDir(), "tmpdir")
	ws, job := jobCredentialFixture(t, extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand,
		fixtureScript{{"read-job-credential", report}, {"write-env", "TMPDIR", tmpReport}})

	var stdout, stderr strings.Builder
	result, err := RunJobInteractiveWithStreams(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, strings.NewReader(""), &stdout, &stderr)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJobInteractiveWithStreams = %+v, %v\nstderr:\n%s", result, err, stderr.String())
	}
	credentialLine, _ := jobCredentialLine(testJobCredential())
	want := "handed=true\ncredential=" + strings.TrimSuffix(string(credentialLine), "\n") + "\nexposed=none\n"
	if got := readReport(t, report); got != want {
		t.Errorf("the job reported\n%s\nwant\n%s", got, want)
	}
	dir := readReport(t, tmpReport)
	if filepath.Dir(dir) != engineTemp {
		t.Errorf("the job's temporary directory %q is not a directory of %s", dir, engineTemp)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the job's temporary directory %s survives the job: %v", dir, err)
	}
	if strings.Contains(stdout.String()+stderr.String(), testJobBearer) {
		t.Error("the bearer reached the job's output")
	}
}
