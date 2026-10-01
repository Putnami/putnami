// Package workspacejob holds what the workspace lifecycle jobs of the Go
// extension share: workspace-install and deps-upgrade.
//
// It replaces the shell helpers those jobs used to source
// (bin/putnami-go-common.sh and bin/putnami-jsonl.sh). A job built on it starts
// no shell, no curl, no tar, no unzip and no jq on any platform: every process
// it runs is the go command or a pinned Go tool, started directly.
//
// The behavior is a port of those scripts. Comments that say "the script" name
// the shell implementation this code replaced, so a reader can tell a
// deliberate behavior from an accident.
package workspacejob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
)

// Emitter is the part of the job event stream the lifecycle jobs write.
// *jsonl.Emitter satisfies it; tests record the events instead.
type Emitter interface {
	Log(level, message string)
	PhaseStart(name string)
	PhaseEnd(name, status string)
	Diagnostic(severity, message, file string, line int)
	Metric(name string, value any, unit string)
}

// Job is one run of a lifecycle job: where it writes its events, the
// environment it exports to the commands it starts, and the workspace it acts
// on.
type Job struct {
	// Ctx ends the commands the job runs. The signal trap cancels it.
	Ctx context.Context
	// Emit receives the job events.
	Emit Emitter
	// Env is the environment every command inherits. Setting a variable here
	// is what exporting it did in the script.
	Env *Env
	// Stdout and Stderr receive the output of a command the script let
	// inherit its own streams.
	Stdout io.Writer
	Stderr io.Writer
	// WorkspaceRoot is the workspace the job acts on.
	WorkspaceRoot string
	// ProjectPath is the project the job runs for, or "" for none.
	ProjectPath string
	// GoBinary is the go command ResolveGoBinary selected.
	GoBinary string

	// outsideWorkspaceOnly makes the job find and run no program inside the
	// workspace tree (OnlyProgramsOutsideTheWorkspace).
	outsideWorkspaceOnly bool
	trap                 *Trap
}

// New returns a job that writes its events to emit, exports environ (the
// process environment, as "KEY=value" entries) to its commands, and exits on
// SIGINT and SIGTERM through the trap once a caller arms it.
//
// The workspace root and the project path come from the variables the CLI
// projects into every job, which is what the script's parse_context read,
// falling back to the context document.
func New(ctx context.Context, emit Emitter, environ []string, workspaceRoot, projectPath string) *Job {
	ctx, cancel := context.WithCancel(ctx)
	env := NewEnv(environ)
	if root := env.Get("PUTNAMI_WORKSPACE_ROOT"); root != "" {
		workspaceRoot = root
	}
	for _, key := range []string{"PUTNAMI_PROJECT_PATH", "PUTNAMI_PROJECT_ROOT"} {
		if path := env.Get(key); path != "" {
			projectPath = path
			break
		}
	}
	return &Job{
		Ctx:           ctx,
		Emit:          emit,
		Env:           env,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		WorkspaceRoot: workspaceRoot,
		ProjectPath:   projectPath,
		trap:          NewTrap(cancel, os.Exit),
	}
}

// Trap returns the job's signal trap, or nil for a job built without one.
func (j *Job) Trap() *Trap { return j.trap }

// UserAgent is the User-Agent of every request the job makes.
func (j *Job) UserAgent() string {
	if agent := j.Env.Get("PUTNAMI_CLI_USER_AGENT"); agent != "" {
		return agent
	}
	return "putnami-cli/dev"
}

// ExtensionStateRoot is the workspace directory the script called the
// extension root: the last-resort cache lives under it, and so does a Go
// release installed inside the workspace (workspaceGoRoot).
func (j *Job) ExtensionStateRoot() string {
	return filepath.Join(j.WorkspaceRoot, ".putnami", "extensions", "@putnami-go")
}

// GoToolchainRoot is the directory the managed Go releases are installed in,
// one go-<version> directory each, shared by every workspace of the machine:
// toolchains/go under the Putnami home (toolchain.ResolveGoToolchainRoot).
func (j *Job) GoToolchainRoot() string {
	return toolchain.ResolveGoToolchainRoot(j.Env.Get, j.WorkspaceRoot)
}

// workspaceGoRoot is the directory inside the workspace that holds Go
// releases, one go-<version> directory each. The job installs none there; it
// runs one it finds there when the Putnami home holds no install of that
// release.
func (j *Job) workspaceGoRoot() string {
	return filepath.Join(j.ExtensionStateRoot(), "libs")
}

// GoCacheRoot is the machine-global Go cache root, resolved exactly as every
// other job of the extension resolves it, with the script's final fallback
// under the workspace.
func (j *Job) GoCacheRoot() string {
	if root := toolchain.ResolveGoCacheRoot(j.Env.Get); root != "" {
		return root
	}
	return filepath.Join(j.ExtensionStateRoot(), "cache")
}

// stream says where one output stream of a command goes.
type stream int

const (
	discard stream = iota
	capture
	inherit
)

// command is one process a job starts. env holds "KEY=value" entries that
// apply to this command only, the script's `KEY=value cmd` form.
type command struct {
	name     string
	args     []string
	env      []string
	stdout   stream
	stderr   stream
	combined bool
}

// run starts c and waits for it. Captured output loses its trailing newlines,
// as a shell command substitution does. After the command ends, a signal the
// trap received ends the job.
//
// A job limited to programs outside the workspace
// (OnlyProgramsOutsideTheWorkspace) refuses to start a program inside it,
// whichever lookup found the program.
func (j *Job) run(c command) (stdout, stderr string, err error) {
	//nolint:gosec // G204: the lifecycle jobs start the resolved go command and pinned tools with argument lists, never through a shell.
	cmd := exec.CommandContext(j.Ctx, c.name, c.args...)
	if program, refused := j.refusesProgram(cmd.Path); refused {
		return "", "", fmt.Errorf("refuse to run %s: this job runs no program inside the workspace", program)
	}
	cmd.Env = j.Env.Environ(c.env...)
	var outBuf, errBuf bytes.Buffer
	switch {
	case c.combined:
		cmd.Stdout = &outBuf
		cmd.Stderr = &outBuf
	default:
		cmd.Stdout = j.writerFor(c.stdout, &outBuf, j.Stdout)
		cmd.Stderr = j.writerFor(c.stderr, &errBuf, j.Stderr)
	}
	err = cmd.Run()
	j.trap.check()
	return trimNewlines(outBuf.String()), trimNewlines(errBuf.String()), err
}

// refusesProgram reports whether the job refuses to run the program at path,
// and names it absolute.
func (j *Job) refusesProgram(path string) (string, bool) {
	if !j.outsideWorkspaceOnly {
		return path, false
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, !j.admits(path)
}

func (j *Job) writerFor(s stream, buf *bytes.Buffer, inherited io.Writer) io.Writer {
	switch s {
	case capture:
		return buf
	case inherit:
		return inherited
	default:
		return nil
	}
}

// Output runs a command and returns its standard output. Its standard error
// is discarded: the script's `$(cmd 2>/dev/null)`.
func (j *Job) Output(env []string, name string, args ...string) (string, error) {
	out, _, err := j.run(command{name: name, args: args, env: env, stdout: capture})
	return out, err
}

// Combined runs a command and returns its standard output and standard error
// interleaved: the script's `$(cmd 2>&1)`.
func (j *Job) Combined(env []string, name string, args ...string) (string, error) {
	out, _, err := j.run(command{name: name, args: args, env: env, combined: true})
	return out, err
}

// Split runs a command and returns its standard output and its standard error
// apart: the script's `$(cmd 2>"$file")`.
func (j *Job) Split(env []string, name string, args ...string) (string, string, error) {
	return j.run(command{name: name, args: args, env: env, stdout: capture, stderr: capture})
}

// StderrOf runs a command, discards its standard output and returns its
// standard error: the script's `$(cmd 2>&1 >/dev/null)`.
func (j *Job) StderrOf(env []string, name string, args ...string) (string, error) {
	_, errOut, err := j.run(command{name: name, args: args, env: env, stderr: capture})
	return errOut, err
}

// Inherit runs a command whose standard output goes to the job's own. Its
// standard error goes to the job's own too, or is discarded when quiet is
// set: the script's `cmd` and `cmd 2>/dev/null`.
func (j *Job) Inherit(quiet bool, env []string, name string, args ...string) error {
	errStream := inherit
	if quiet {
		errStream = discard
	}
	_, _, err := j.run(command{name: name, args: args, env: env, stdout: inherit, stderr: errStream})
	return err
}

func trimNewlines(s string) string {
	return strings.TrimRight(s, "\n")
}

// mapLines applies f to every line of s, as a line-oriented filter such as
// sed or awk does.
func mapLines(s string, f func(string) string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = f(line)
	}
	return trimNewlines(strings.Join(lines, "\n"))
}

// field returns the one-based whitespace-separated field n of line, or "",
// as awk '{print $n}' does.
func field(line string, n int) string {
	fields := strings.Fields(line)
	if n < 1 || n > len(fields) {
		return ""
	}
	return fields[n-1]
}
