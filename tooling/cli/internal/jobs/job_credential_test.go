package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// testJobBearer is the read credential's bearer in these tests. No
// diagnostic, environment, argument or file may ever show it.
const testJobBearer = "job-read-bearer-<&>-7f3c"

func testJobCredential() *registryproto.Credential {
	return &registryproto.Credential{
		Bearer:    testJobBearer,
		ExpiresAt: "2026-09-29T12:00:00Z",
		Hosts:     []string{"registry.example.test"},
	}
}

// jobCredentialReport is what a fixture task reports about the credential the
// engine handed it: whether the descriptor variable was set, what
// registrycred.ReadJobCredential answered, and where else the bearer showed:
// "none", "args", "env" or "context" (the job context file).
func jobCredentialReport(args []string) string {
	_, handed := os.LookupEnv(extensionproto.JobCredentialFDEnv)
	credential, err := registrycred.ReadJobCredential()
	if err != nil {
		return "handed=" + strconv.FormatBool(handed) + "\nerror=" + err.Error() + "\n"
	}
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(registryproto.CredentialResult{Credential: credential})
	exposed := "none"
	if credential != nil {
		holds := func(value string) bool { return strings.Contains(value, credential.Bearer) }
		switch {
		case slices.ContainsFunc(os.Args, holds):
			exposed = "args"
		case slices.ContainsFunc(os.Environ(), holds):
			exposed = "env"
		default:
			for i, arg := range args {
				if arg != "--putnamiContext" || i+1 >= len(args) {
					continue
				}
				if data, err := os.ReadFile(args[i+1]); err != nil || holds(string(data)) {
					exposed = "context"
				}
			}
		}
	}
	return "handed=" + strconv.FormatBool(handed) + "\ncredential=" + strings.TrimSuffix(line.String(), "\n") + "\nexposed=" + exposed + "\n"
}

// jobCredentialTestJob is a job of command, named name, whose program is
// command.
func jobCredentialTestJob(name, commandName, command string) *ScheduledJob {
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Runtime: &extension.RuntimeDefinition{}},
		JobDef: &extension.JobDefinition{
			Name: name, CommandName: commandName, ExtensionName: "@test/ext", Command: command,
		},
	}
}

// localSourceJob marks the extension of job as a local source: repository
// code.
func localSourceJob(job *ScheduledJob) *ScheduledJob {
	job.Extension.LocalSource = true
	return job
}

var runtimeCommand = "{" + extensionproto.TemplateVarExtensionRuntime + "}"

// jobCredentialHandedOnThisPlatform is false on Windows, where a hosted run
// hands no job a credential.
var jobCredentialHandedOnThisPlatform = runtime.GOOS != "windows"

// Only the fetch its extension's own runtime runs receives the credential, and
// never from a local extension. A fetch any other program runs, and every
// other command, receives nothing.
func TestReceivesJobCredential(t *testing.T) {
	t.Parallel()
	fetch := extensionproto.WorkspaceFetchCommand
	cases := []struct {
		name     string
		job      *ScheduledJob
		runtime  string
		fetches  bool
		receives bool
	}{
		{"the fetch its runtime runs", jobCredentialTestJob(fetch, fetch, runtimeCommand), "/ext/runtime", true, true},
		{"a step of the fetch", jobCredentialTestJob(fetch+"~download", "", runtimeCommand), "/ext/runtime", true, true},
		{"the fetch without a resolved runtime", jobCredentialTestJob(fetch, fetch, runtimeCommand), "", true, false},
		{"the fetch another program runs", jobCredentialTestJob(fetch, fetch, "{extensionRoot}/bin/fetch"), "/ext/runtime", true, false},
		{"the fetch of a local extension", localSourceJob(jobCredentialTestJob(fetch, fetch, runtimeCommand)), "/ext/runtime", true, false},
		{"the install", jobCredentialTestJob(toolchainProvisioningCommand, toolchainProvisioningCommand, runtimeCommand), "/ext/runtime", false, false},
		{"a build", jobCredentialTestJob("build~compile", "build", runtimeCommand), "/ext/runtime", false, false},
		{"a command that only starts like the fetch", jobCredentialTestJob(fetch+"er", fetch+"er", runtimeCommand), "/ext/runtime", false, false},
		{"no job", nil, "/ext/runtime", false, false},
		{"no definition", &ScheduledJob{}, "/ext/runtime", false, false},
	}
	for _, tc := range cases {
		if got := fetchesDependencies(tc.job); got != tc.fetches {
			t.Errorf("%s: fetchesDependencies = %v, want %v", tc.name, got, tc.fetches)
		}
		if got := receivesJobCredential(tc.job, tc.runtime); got != tc.receives {
			t.Errorf("%s: receivesJobCredential = %v, want %v", tc.name, got, tc.receives)
		}
	}
}

// The line is a registry CredentialResult the SDK parses back to the same
// credential, "{}" when the provider holds none, and never escapes the
// bearer's bytes.
func TestJobCredentialLine(t *testing.T) {
	t.Parallel()
	for name, credential := range map[string]*registryproto.Credential{
		"a credential": testJobCredential(),
		"none":         nil,
	} {
		line, err := jobCredentialLine(credential)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, ok := bytes.CutSuffix(line, []byte("\n"))
		if !ok || bytes.Contains(body, []byte("\n")) {
			t.Fatalf("%s: line %q is not one line ending in a newline", name, line)
		}
		result, err := registryproto.ParseCredentialResult(body)
		if err != nil {
			t.Fatalf("%s: the SDK cannot parse the line: %v", name, err)
		}
		if !reflect.DeepEqual(result.Credential, credential) {
			t.Errorf("%s: parsed %v, want %v", name, result.Credential, credential)
		}
		if credential == nil && string(line) != "{}\n" {
			t.Errorf("%s: line %q, want {}", name, line)
		}
		if credential != nil && !bytes.Contains(line, []byte(testJobBearer)) {
			t.Errorf("%s: line %q escapes the bearer", name, line)
		}
	}

	oversized := testJobCredential()
	oversized.Bearer = strings.Repeat("b", registryproto.MaxCredentialLineBytes)
	if _, err := jobCredentialLine(oversized); err == nil || strings.Contains(err.Error(), oversized.Bearer[:64]) {
		t.Errorf("an oversized line: err = %v, want a refusal that shows no bearer", err)
	}
}

// installCountingJobReadCredential makes answer the job credential source for
// the test, and returns how many times a job asked it.
func installCountingJobReadCredential(t *testing.T, credential *registryproto.Credential, err error) *atomic.Int32 {
	t.Helper()
	calls := new(atomic.Int32)
	restore := InstallJobReadCredential(func(context.Context) (*registryproto.Credential, error) {
		calls.Add(1)
		return credential, err
	})
	t.Cleanup(restore)
	return calls
}

// Without a run credential, and for every job outside a hosted install's
// dependency fetch that is no workspace-fetch, the spawn is left as it was: no
// descriptor, no variable, no temporary directory, and no question to the
// provider.
func TestAttachJobCredentialLeavesOtherSpawnsAlone(t *testing.T) {
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	cases := []struct {
		name   string
		bearer string
		ctx    context.Context
		job    *ScheduledJob
	}{
		{"a local run's fetch", "", WithDependencyFetch(context.Background()), jobCredentialTestJob(fetch, fetch, runtimeCommand)},
		{"a hosted run's install", "run-bearer", context.Background(), jobCredentialTestJob(toolchainProvisioningCommand, toolchainProvisioningCommand, runtimeCommand)},
		{"a hosted run's build", "run-bearer", context.Background(), jobCredentialTestJob("build~compile", "build", runtimeCommand)},
	}
	for _, tc := range cases {
		restore := runcredential.SetForTest(tc.bearer)
		cmd := exec.Command("true")
		cmd.Env = []string{"PATH=/bin", "TMPDIR=/engine/tmp"}
		want := slices.Clone(cmd.Env)
		handoff, err := attachJobCredential(tc.ctx, cmd, tc.job, "/ext/runtime")
		restore()
		if handoff != nil || err != nil {
			t.Fatalf("%s: attachJobCredential = %v, %v; want nothing", tc.name, handoff, err)
		}
		if !reflect.DeepEqual(cmd.Env, want) || cmd.ExtraFiles != nil {
			t.Errorf("%s: env %q and descriptors %v changed", tc.name, cmd.Env, cmd.ExtraFiles)
		}
		handoff.send()
		handoff.close()
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// thisCLIEnv is the registryproto.CLIExecutableEnv entry the runner gives a
// job: this process's executable.
func thisCLIEnv(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return registryproto.CLIExecutableEnv + "=" + self
}

// A fetch that would receive the credential fails before it
// starts when its environment names no CLI, or another CLI than this process:
// the Putnami command it starts would then not be this CLI, which refuses to
// run inside the fetch. The provider is not asked.
func TestAttachJobCredentialRefusesAFetchThatNamesAnotherCLI(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "only-fetch-gets-the-job-credential")
	if !jobCredentialHandedOnThisPlatform {
		t.Skip("no credential is handed on this platform")
	}
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	for name, env := range map[string][]string{
		"no CLI":                             {"PATH=/bin"},
		"another CLI":                        {registryproto.CLIExecutableEnv + "=/usr/local/bin/putnami"},
		"a CLI that is this one, overridden": {thisCLIEnv(t), registryproto.CLIExecutableEnv + "=/usr/local/bin/putnami"},
	} {
		job := jobCredentialTestJob(fetch, fetch, runtimeCommand)
		cmd := exec.Command("true")
		cmd.Env = env
		handoff, err := attachJobCredential(WithDependencyFetch(context.Background()), cmd, job, "/ext/runtime")
		if handoff != nil {
			handoff.close()
		}
		if handoff != nil || err == nil || !strings.Contains(err.Error(), job.Key()) || !strings.Contains(err.Error(), registryproto.CLIExecutableEnv) {
			t.Errorf("%s: attachJobCredential handed %v, error %v; want a refusal that names the job and %s", name, handoff != nil, err, registryproto.CLIExecutableEnv)
		}
		if cmd.ExtraFiles != nil || envHasKey(cmd.Env, runcredential.HostedFetchEnv) {
			t.Errorf("%s: a refused fetch got %d descriptors, env %q", name, len(cmd.ExtraFiles), cmd.Env)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// A provider that refuses fails the job before it starts, with an error that
// names the job, and leaves no directory behind.
func TestAttachJobCredentialReportsARefusal(t *testing.T) {
	if !jobCredentialHandedOnThisPlatform {
		t.Skip("no credential is handed on this platform")
	}
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	engineTemp := t.TempDir()
	t.Setenv("TMPDIR", engineTemp)
	installCountingJobReadCredential(t, nil, errors.New("the provider refused"))

	job := jobCredentialTestJob(extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand, runtimeCommand)
	cmd := exec.Command("true")
	cmd.Env = []string{thisCLIEnv(t)}
	handoff, err := attachJobCredential(WithDependencyFetch(context.Background()), cmd, job, "/ext/runtime")
	if handoff != nil || err == nil || !strings.Contains(err.Error(), job.Key()) || !strings.Contains(err.Error(), "the provider refused") {
		t.Fatalf("attachJobCredential = %v, %v; want the refusal for %s", handoff, err, job.Key())
	}
	if entries, _ := os.ReadDir(engineTemp); len(entries) != 0 {
		t.Errorf("a refused job left %d entries in the engine's temporary directory", len(entries))
	}
}

// In a hosted install's dependency fetch, a job that would not receive the
// credential fails before it starts: it would run beside the fetches that
// hold it. The provider is not asked.
func TestAttachJobCredentialRefusesAnotherJobInTheDependencyFetch(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "only-fetch-gets-the-job-credential")
	if !jobCredentialHandedOnThisPlatform {
		t.Skip("no credential is handed on this platform")
	}
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	for _, job := range []*ScheduledJob{
		jobCredentialTestJob(fetch, fetch, "/bin/fetch"),
		jobCredentialTestJob("build~compile", "build", runtimeCommand),
	} {
		cmd := exec.Command("true")
		handoff, err := attachJobCredential(WithDependencyFetch(context.Background()), cmd, job, "/ext/runtime")
		if handoff != nil {
			handoff.close()
		}
		if handoff != nil || err == nil || !strings.Contains(err.Error(), job.Key()) || !strings.Contains(err.Error(), runcredential.Flag) {
			t.Errorf("%s: attachJobCredential handed %v, error %v; want a refusal that names the job and %s", job.Key(), handoff != nil, err, runcredential.Flag)
		}
		if cmd.ExtraFiles != nil {
			t.Errorf("%s: a refused job got %d descriptors", job.Key(), len(cmd.ExtraFiles))
		}
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// A workspace-fetch that a user's plan selects, such as through the alias
// "lint": "workspace-fetch", runs outside a hosted install's dependency
// fetch: it is an ordinary job. It gets no credential descriptor, runs
// offline, and records repository code before it starts, so no credential
// holder starts after it.
func TestAPlannedWorkspaceFetchIsAnOrdinaryJob(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "only-fetch-gets-the-job-credential")
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	job := jobCredentialTestJob(extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand, runtimeCommand)
	inv := &jobInvocation{command: "true", extensionRuntime: "/ext/runtime"}

	cmd, err := inv.jobCommand(context.Background(), job, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.credential != nil {
		inv.credential.close()
	}
	if inv.credential != nil || cmd.ExtraFiles != nil || envHasKey(cmd.Env, extensionproto.JobCredentialFDEnv) ||
		envHasKey(cmd.Env, runcredential.HostedFetchEnv) {
		t.Errorf("the planned fetch got a credential: handed %v, %d descriptors, env %q", inv.credential != nil, len(cmd.ExtraFiles), cmd.Env)
	}
	if got := envLastValue(cmd.Env, extensionproto.OfflineDependenciesEnv); got != "1" {
		t.Errorf("%s = %q, want 1: the planned fetch runs offline", extensionproto.OfflineDependenciesEnv, got)
	}

	started := false
	err = inv.startJob(job, func() error {
		started = true
		if custodyErr := runcredential.RequireCustody("a probe holder"); custodyErr == nil {
			t.Error("the planned fetch started before this process recorded repository code")
		}
		return nil
	})
	if err != nil || !started {
		t.Fatalf("startJob = %v, started %v", err, started)
	}
	var custody *runcredential.CustodyError
	holderErr := runcredential.StartHolder("the remote cache provider", func() error { return nil })
	if !errors.As(holderErr, &custody) || custody.Reason != "job "+job.Key() {
		t.Errorf("a holder after the planned fetch = %v, want a custody error naming job %s", holderErr, job.Key())
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// A hosted fetch gets the next descriptor after the ones the spawn already
// hands, a variable that names it, and a private directory under this
// process's temporary directory that close removes with its content.
func TestAttachJobCredentialHandsAPipeAndAPrivateDirectory(t *testing.T) {
	if !jobCredentialHandedOnThisPlatform {
		t.Skip("no credential is handed on this platform")
	}
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	engineTemp := t.TempDir()
	t.Setenv("TMPDIR", engineTemp)
	installCountingJobReadCredential(t, testJobCredential(), nil)

	lease, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	job := jobCredentialTestJob(extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand, runtimeCommand)
	cmd := exec.Command("true")
	cmd.Env = []string{"PATH=/bin", "TMPDIR=" + engineTemp, thisCLIEnv(t)}
	cmd.ExtraFiles = []*os.File{lease}
	handoff, err := attachJobCredential(WithDependencyFetch(context.Background()), cmd, job, "/ext/runtime")
	if err != nil || handoff == nil {
		t.Fatalf("attachJobCredential = %v, %v", handoff, err)
	}
	defer handoff.close()

	if got := envLastValue(cmd.Env, extensionproto.JobCredentialFDEnv); got != "4" {
		t.Errorf("%s = %q, want 4: descriptor 3 is the spawn's own", extensionproto.JobCredentialFDEnv, got)
	}
	if got := envLastValue(cmd.Env, runcredential.HostedFetchEnv); got != "1" {
		t.Errorf("%s = %q, want 1", runcredential.HostedFetchEnv, got)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[1] != handoff.read {
		t.Errorf("descriptors = %v, want the lease then the read end", cmd.ExtraFiles)
	}
	dir := handoff.tempDir
	if filepath.Dir(dir) != engineTemp || !strings.HasPrefix(filepath.Base(dir), "putnami-job-credential-") {
		t.Errorf("temporary directory %s is not a fresh directory of %s", dir, engineTemp)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("temporary directory %s: %v, %v; want a 0700 directory", dir, info, err)
	}
	for _, name := range jobTempDirEnv {
		if got := envLastValue(cmd.Env, name); got != dir {
			t.Errorf("%s = %q, want %s", name, got, dir)
		}
	}
	if slices.ContainsFunc(cmd.Env, func(entry string) bool { return strings.Contains(entry, testJobBearer) }) ||
		slices.ContainsFunc(cmd.Args, func(arg string) bool { return strings.Contains(arg, testJobBearer) }) {
		t.Fatal("the bearer reached the job's environment or arguments")
	}

	handoff.send()
	line, err := readAll(handoff.read)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := jobCredentialLine(testJobCredential())
	if !bytes.Equal(line, want) {
		t.Errorf("the pipe carried %d bytes, want the %d bytes of the line", len(line), len(want))
	}

	if err := os.WriteFile(filepath.Join(dir, "netrc"), []byte(testJobBearer), 0o600); err != nil {
		t.Fatal(err)
	}
	handoff.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temporary directory %s survives close: %v", dir, err)
	}
}

func readAll(file *os.File) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(file)
	return buf.Bytes(), err
}

// jobEnvironment is the environment buildJobInvocation gives job.
func jobEnvironment(t *testing.T, job *ScheduledJob, runtime string) []string {
	t.Helper()
	ws := makeExecutorTestWorkspace(t)
	return buildJobInvocation(ws, job, &JobCommandContext{
		WorkspaceRoot: ws.Root,
		Workspace:     JobContextWorkspace{Name: "ws"},
		Extension:     JobContextExtension{Name: "@test/ext"},
		Job:           JobContextJob{Name: job.JobDef.Name},
	}, nil, runtime).env
}

// A local run hands a job the environment it always did: an inherited offline
// signal and cache token pass through. Only an inherited credential
// descriptor variable goes, on every run, because it names nothing the job
// holds.
func TestJobEnvironmentOfALocalRun(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "flag-off-changes-nothing", "flag-off-job-env-unchanged")
	restore := runcredential.SetForTest("")
	t.Cleanup(restore)
	t.Setenv(runcredential.CacheTokenEnv, "cache-token")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "inherited")
	t.Setenv(extensionproto.JobCredentialFDEnv, "9")

	for _, job := range []*ScheduledJob{
		jobCredentialTestJob("build", "build", "true"),
		jobCredentialTestJob(extensionproto.WorkspaceFetchCommand, extensionproto.WorkspaceFetchCommand, runtimeCommand),
	} {
		job.JobDef.Env = map[string]string{extensionproto.JobCredentialFDEnv: "5"}
		env := jobEnvironment(t, job, "/ext/runtime")
		if got := envValues(env, runcredential.CacheTokenEnv); !reflect.DeepEqual(got, []string{"cache-token"}) {
			t.Errorf("%s: %s = %q, want the inherited value", job.JobDef.Name, runcredential.CacheTokenEnv, got)
		}
		if got := envValues(env, extensionproto.OfflineDependenciesEnv); !reflect.DeepEqual(got, []string{"inherited"}) {
			t.Errorf("%s: %s = %q, want the inherited value", job.JobDef.Name, extensionproto.OfflineDependenciesEnv, got)
		}
		if envHasKey(env, extensionproto.JobCredentialFDEnv) {
			t.Errorf("%s: %s reached the job", job.JobDef.Name, extensionproto.JobCredentialFDEnv)
		}
	}
}

// A hosted run states, after every other entry, that a job must not download
// dependencies, and hands no job a framework credential. The fetch is the one
// job without the offline signal.
func TestJobEnvironmentOfAHostedRun(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "every-other-job-runs-offline")
	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	t.Setenv(runcredential.CacheTokenEnv, "cache-token")
	t.Setenv(runcredential.CloudTokenEnv, "cloud-token")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")

	offline := extensionproto.OfflineDependenciesEnv
	fetch := extensionproto.WorkspaceFetchCommand
	cases := []struct {
		name    string
		job     *ScheduledJob
		offline []string
	}{
		{"a build", jobCredentialTestJob("build~compile", "build", "true"), []string{"1"}},
		{"the install", jobCredentialTestJob(toolchainProvisioningCommand, toolchainProvisioningCommand, runtimeCommand), []string{"1"}},
		{"the fetch", jobCredentialTestJob(fetch, fetch, runtimeCommand), nil},
		{"the fetch another program runs", jobCredentialTestJob(fetch, fetch, "/bin/fetch"), nil},
	}
	for _, tc := range cases {
		tc.job.JobDef.Env = map[string]string{
			offline:                     "0",
			runcredential.CacheTokenEnv: "declared-token",
			runcredential.CloudTokenEnv: "declared-token",
		}
		env := jobEnvironment(t, tc.job, "/ext/runtime")
		if got := envValues(env, offline); !reflect.DeepEqual(got, tc.offline) {
			t.Errorf("%s: %s = %q, want %q", tc.name, offline, got, tc.offline)
		}
		if tc.offline != nil && env[len(env)-1] != offline+"=1" {
			t.Errorf("%s: the offline signal is not the last entry: %q", tc.name, env[len(env)-1])
		}
		for _, name := range []string{runcredential.CacheTokenEnv, runcredential.CloudTokenEnv} {
			if envHasKey(env, name) {
				t.Errorf("%s: %s reached the job", tc.name, name)
			}
		}
	}
}

// A hosted run declares itself offline in its own environment
// (runcredential.Capture), and the key of a task that declares the offline
// signal as an input reads it there: the task keys apart from the same task
// on a run that may download. A task that does not declare it keys the same.
func TestOfflineSignalKeysADeclaringTaskApart(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	newJob := func(env ...string) *ScheduledJob {
		job := &ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build~tidy", CommandName: "build", ExtensionName: "@test/ext", Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Env: env}},
			},
		}
		return declareCacheTestTask(job, "tidy")
	}
	declaring, other := newJob(extensionproto.OfflineDependenciesEnv), newJob()
	key := func(job *ScheduledJob) string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		got, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return got
	}

	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	localDeclaring, localOther := key(declaring), key(other)
	// What runcredential.Capture leaves in the environment of a hosted run.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if key(declaring) == localDeclaring {
		t.Error("the offline signal left the key of a task that declares it unmoved")
	}
	if key(other) != localOther {
		t.Error("the offline signal moved the key of a task that does not declare it")
	}
}
