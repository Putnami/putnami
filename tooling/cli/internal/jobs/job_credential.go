package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync/atomic"

	extensionproto "go.putnami.dev/protocol/extension"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// JobReadCredential answers the read credential of this process's credential
// provider. A nil credential with a nil error means the provider holds none.
// An error is a refusal or a provider failure.
type JobReadCredential func(ctx context.Context) (*registryproto.Credential, error)

// jobReadCredential is the source of the credential a hosted fetch job
// receives. Without one, the job receives absence.
var jobReadCredential atomic.Pointer[JobReadCredential]

// InstallJobReadCredential makes read the source of the credential a hosted
// fetch job receives (receivesJobCredential), and returns the function that
// restores the previous source. A nil read removes the source.
func InstallJobReadCredential(read JobReadCredential) (restore func()) {
	var next *JobReadCredential
	if read != nil {
		next = &read
	}
	previous := jobReadCredential.Swap(next)
	return func() { jobReadCredential.Store(previous) }
}

// ReadJobCredential answers the credential a hosted fetch job receives: the
// installed source's answer, or nil without a source.
func ReadJobCredential(ctx context.Context) (*registryproto.Credential, error) {
	read := jobReadCredential.Load()
	if read == nil {
		return nil, nil
	}
	return (*read)(ctx)
}

// dependencyFetchKey marks the context of a hosted install's dependency fetch
// (WithDependencyFetch).
type dependencyFetchKey struct{}

// WithDependencyFetch marks ctx as the dependency fetch of a hosted install:
// the run of every workspace-fetch that the install finishes before any
// repository code starts. On a hosted run, only a job started with a marked
// context receives the job credential, and every job started with one must
// receive it (attachJobCredential). A workspace-fetch that a user's plan
// selects, such as through the alias "lint": "workspace-fetch", is an
// ordinary job.
func WithDependencyFetch(ctx context.Context) context.Context {
	return context.WithValue(ctx, dependencyFetchKey{}, true)
}

// dependencyFetch reports whether ctx is marked by WithDependencyFetch.
func dependencyFetch(ctx context.Context) bool {
	marked, _ := ctx.Value(dependencyFetchKey{}).(bool)
	return marked
}

// pathExtensionFetchKey marks the context of a hosted install's path-extension
// fetch (WithPathExtensionFetch).
type pathExtensionFetchKey struct{}

// WithPathExtensionFetch marks ctx as the path-extension fetch of a hosted
// install: the run of the workspace-fetch of every extension that is not
// installed from the artifact store, the workspace's own path extensions. The
// install starts it after the dependency fetch (WithDependencyFetch) and the
// start of the remote cache provider, and before its hooks and installers. Its
// jobs receive no credential and run offline, like every job outside the
// dependency fetch.
func WithPathExtensionFetch(ctx context.Context) context.Context {
	return context.WithValue(ctx, pathExtensionFetchKey{}, true)
}

// pathExtensionFetch reports whether ctx is marked by WithPathExtensionFetch.
func pathExtensionFetch(ctx context.Context) bool {
	marked, _ := ctx.Value(pathExtensionFetchKey{}).(bool)
	return marked
}

// HostedFetchStep names the step of a hosted install's dependency fetch that
// ctx belongs to: "dependency fetch" (WithDependencyFetch), "path-extension
// fetch" (WithPathExtensionFetch), or "" for any other run.
func HostedFetchStep(ctx context.Context) string {
	switch {
	case dependencyFetch(ctx):
		return "dependency fetch"
	case pathExtensionFetch(ctx):
		return "path-extension fetch"
	}
	return ""
}

// CredentialedFetchView returns discovered as the dependency fetch of a hosted
// install (WithDependencyFetch) sees it: with only the extensions installed
// from the artifact store (extension.InArtifactStore). The run then prepares
// no runtime, probes no workspace adapter and plans no job of a path extension
// while it hands out the job credential. Any other run gets discovered.
func CredentialedFetchView(ctx context.Context, workspaceRoot string, discovered *extension.DiscoveryResult) *extension.DiscoveryResult {
	if discovered == nil || !dependencyFetch(ctx) {
		return discovered
	}
	view := *discovered
	view.Extensions = make([]*extension.ExtensionDescription, 0, len(discovered.Extensions))
	for _, ext := range discovered.Extensions {
		if extension.InArtifactStore(workspaceRoot, ext) {
			view.Extensions = append(view.Extensions, ext)
		}
	}
	return &view
}

// PathExtensionFetchPlan returns the extensions the path-extension fetch of a
// hosted install (WithPathExtensionFetch) plans: every one not installed from
// the artifact store, whose workspace-fetch the dependency fetch did not run.
// The run still discovers every extension, so the workspace probe binds every
// adapter. Any other run plans extensions.
func PathExtensionFetchPlan(ctx context.Context, workspaceRoot string, extensions []*extension.ExtensionDescription) []*extension.ExtensionDescription {
	if !pathExtensionFetch(ctx) {
		return extensions
	}
	plan := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if !extension.InArtifactStore(workspaceRoot, ext) {
			plan = append(plan, ext)
		}
	}
	return plan
}

// fetchesDependencies reports whether job belongs to
// extensionproto.WorkspaceFetchCommand, the one command a hosted run lets
// download the workspace's dependencies.
func fetchesDependencies(job *ScheduledJob) bool {
	return job != nil && job.JobDef != nil &&
		extensionproto.BaseCommandName(job.CommandName()) == extensionproto.WorkspaceFetchCommand
}

// receivesJobCredential reports whether job receives the read credential on a
// hosted run: a fetch job whose command is its extension's own runtime,
// extensionRuntime, and whose extension is not a local source. A fetch that
// any other program runs receives nothing. The dependency fetch plans no path
// extension (CredentialedFetchView); the check here holds without it.
func receivesJobCredential(job *ScheduledJob, extensionRuntime string) bool {
	return fetchesDependencies(job) && extensionRuntime != "" &&
		job.Extension != nil && !job.Extension.LocalSource &&
		job.JobDef.Command == "{"+extensionproto.TemplateVarExtensionRuntime+"}"
}

// jobCredentialLine encodes credential as the one line the job reads from its
// descriptor: a registry CredentialResult, "{}" when credential is nil, and a
// trailing newline. HTML escaping stays off, so the line is never longer than
// the provider answer the credential came from.
func jobCredentialLine(credential *registryproto.Credential) ([]byte, error) {
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(registryproto.CredentialResult{Credential: credential}); err != nil {
		clear(line.Bytes())
		return nil, fmt.Errorf("encode the job credential: %w", err)
	}
	if line.Len() > registryproto.MaxCredentialLineBytes {
		size := line.Len()
		clear(line.Bytes())
		return nil, fmt.Errorf("the job credential line is %d bytes, over %d", size, registryproto.MaxCredentialLineBytes)
	}
	return line.Bytes(), nil
}

// jobTempDirEnv are the variables that name a process's temporary directory:
// TMPDIR on Unix, TMP and TEMP on Windows.
var jobTempDirEnv = []string{"TMPDIR", "TMP", "TEMP"}

// jobCredentialHandoff is the pipe that carries one job's read credential, and
// the private temporary directory the job keeps the files that hold it in.
type jobCredentialHandoff struct {
	read, write *os.File
	line        []byte
	tempDir     string
}

// attachJobCredential hands job the read credential on a fresh pipe when this
// process holds a run credential, ctx is a hosted install's dependency fetch
// (WithDependencyFetch), and job receives one (receivesJobCredential). The
// read end joins cmd.ExtraFiles, and extensionproto.JobCredentialFDEnv in
// cmd.Env, which the caller has already set, names its descriptor number. On
// Windows nothing is handed: a hosted run never runs there.
//
// In the dependency fetch, a job that would not receive the credential fails
// before it starts: it would run beside the jobs that hold it, and the job
// credential then sits in their files. Outside the dependency fetch, a
// workspace-fetch job receives nothing and runs offline, like every other job.
//
// The job also gets a private temporary directory: a fresh 0700 directory
// under this process's temporary directory, never the workspace, which every
// variable of jobTempDirEnv names. The job writes the credential only there,
// and close removes the directory however the job ended.
//
// The job's environment carries runcredential.HostedFetchEnv, so a Putnami
// command the job starts refuses to run: a fetch built on an older SDK starts
// `putnami cloud registry-token`, which inherits the environment and the
// descriptor. That command is this CLI, because the job's
// registryproto.CLIExecutableEnv names this process's executable; a job whose
// environment names another fails before it starts.
//
// Once this process started repository code, a job that would receive the
// credential fails with a *runcredential.CustodyError before the provider is
// asked (runcredential.RequireCustody).
//
// It returns nil when job receives nothing. Otherwise the caller calls send,
// and close once the process exited or failed to start. The credential reaches
// the job only through the pipe: no environment, argument or file of this
// process carries it.
func attachJobCredential(ctx context.Context, cmd *exec.Cmd, job *ScheduledJob, extensionRuntime string) (*jobCredentialHandoff, error) {
	if !runcredential.Hosted() || runtime.GOOS == "windows" {
		return nil, nil
	}
	if !dependencyFetch(ctx) {
		if fetchesDependencies(job) {
			cmd.Env = runcredential.ChildEnv(cmd.Env, false)
		}
		return nil, nil
	}
	if !receivesJobCredential(job, extensionRuntime) {
		return nil, fmt.Errorf("%s: job %s runs in the dependency fetch of a hosted install, which runs only the workspace-fetch "+
			"that an artifact-store extension's own runtime starts", runcredential.Flag, job.Key())
	}
	// A Putnami command the fetch starts must be this CLI, which refuses to
	// run inside it (runcredential.HostedFetchEnv).
	if self, err := os.Executable(); err != nil || self == "" || envLastValue(cmd.Env, registryproto.CLIExecutableEnv) != self {
		return nil, fmt.Errorf("%s: job %s would call back into a CLI other than this one: its %s is not this "+
			"process's executable", runcredential.Flag, job.Key(), registryproto.CLIExecutableEnv)
	}
	// The job starts as a credential holder, which it cannot once repository
	// code ran; the provider is not asked in vain.
	if err := runcredential.RequireCustody("job " + job.Key()); err != nil {
		return nil, err
	}
	credential, err := ReadJobCredential(ctx)
	if err != nil {
		return nil, fmt.Errorf("read credential for %s: %w", job.Key(), err)
	}
	line, err := jobCredentialLine(credential)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "putnami-job-credential-")
	if err != nil {
		clear(line)
		return nil, fmt.Errorf("job credential temporary directory: %w", err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		clear(line)
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("job credential pipe: %w", err)
	}
	// ExtraFiles[i] is descriptor 3+i in the child.
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, read)
	cmd.Env = setEnv(cmd.Env, extensionproto.JobCredentialFDEnv, strconv.Itoa(fd))
	cmd.Env = setEnv(cmd.Env, runcredential.HostedFetchEnv, "1")
	for _, name := range jobTempDirEnv {
		cmd.Env = setEnv(cmd.Env, name, tempDir)
	}
	return &jobCredentialHandoff{read: read, write: write, line: line, tempDir: tempDir}, nil
}

// send writes the line from a goroutine, then closes the write end, so the job
// reads the line and then the end of its data. A line longer than the pipe's
// buffer waits for the job to read it and never delays the job. This process
// keeps its copy of the read end until close: the end of the job's data
// depends only on the write end.
func (h *jobCredentialHandoff) send() {
	if h == nil {
		return
	}
	line := h.line
	h.line = nil
	go func() {
		_, _ = h.write.Write(line)
		clear(line)
		_ = h.write.Close()
	}()
}

// close releases both ends of the pipe and removes the job's temporary
// directory with every file the job left in it. A write the job never read
// then fails, and its goroutine ends.
func (h *jobCredentialHandoff) close() {
	if h == nil {
		return
	}
	_ = h.read.Close()
	_ = h.write.Close()
	clear(h.line)
	if err := os.RemoveAll(h.tempDir); err != nil {
		slog.Warn("remove the temporary directory of a job that received a credential", "dir", h.tempDir, "error", err)
	}
}
