// Package upload hands a change evidence document to the evidence reporter a
// CI job selects.
//
// The reporter is an installed extension. PUTNAMI_EVIDENCE_REPORTER names it,
// @putnami/cloud when unset, and the extension answers the subcommand
// `<name> evidence-report --file <path>`, where <name> is its name without the
// scope. Observe runs that subcommand through the CLI that started it, so a
// third party writes its own reporter as one workspace-optional subcommand.
// Every failure is a warning diagnostic: an upload never fails the job.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// SelectorEnv names the reporter's extension.
	SelectorEnv = "PUTNAMI_EVIDENCE_REPORTER"
	// DefaultReporter is the reporter when SelectorEnv is unset.
	DefaultReporter = "@putnami/cloud"
	// Subcommand is the subcommand a reporter answers.
	Subcommand = "evidence-report"
	// FileFlag carries the path of the document.
	FileFlag = "--file"
	// CodeUploadFailed is the diagnostic of every failed hand-off.
	CodeUploadFailed = "observe.upload_failed"
	// Timeout bounds the reporter.
	Timeout = 2 * time.Minute

	// cliExecutableEnv is the absolute path of the CLI that started this
	// process.
	cliExecutableEnv = "PUTNAMI_CLI_EXECUTABLE"
	// cliName is the CLI on PATH when none is advertised.
	cliName = "putnami"
	// waitDelay bounds the wait for the reporter's output after it exits or is
	// killed.
	waitDelay = 5 * time.Second
)

// extensionName is an extension name with an optional scope; the second group
// is the command group of its reporter.
var extensionName = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?([a-z0-9][a-z0-9-]*)$`)

// Request is one hand-off.
type Request struct {
	// Path is the absolute path of the evidence document.
	Path string
	// Getenv reads the environment.
	Getenv func(string) string
	// Output receives the reporter's standard output and standard error.
	Output io.Writer
	// Timeout bounds the reporter; zero is Timeout.
	Timeout time.Duration
}

// Reporter returns the selected reporter's extension name and command group.
func Reporter(getenv func(string) string) (name, group string, err error) {
	name = strings.TrimSpace(getenv(SelectorEnv))
	if name == "" {
		name = DefaultReporter
	}
	match := extensionName.FindStringSubmatch(name)
	if match == nil {
		return name, "", fmt.Errorf("%s=%q is not an extension name", SelectorEnv, name)
	}
	return name, match[1], nil
}

// Command returns the CLI executable, its arguments and its environment for
// the reporter group.
func Command(getenv func(string) string, environ []string, group, path string) (string, []string, []string) {
	executable := cliName
	env := append(append([]string{}, environ...), "PUTNAMI_NO_AUTO_INSTALL=1")
	if advertised := strings.TrimSpace(getenv(cliExecutableEnv)); filepath.IsAbs(advertised) {
		executable = advertised
		env = append(env, "PUTNAMI_NO_RELAUNCH=1")
	}
	return executable, []string{group, Subcommand, FileFlag, path}, env
}

// Run hands the document to the selected reporter and returns the diagnostic
// of a failure, nil on success.
func Run(ctx context.Context, req Request) []diag.Diagnostic {
	name, group, err := Reporter(req.Getenv)
	if err != nil {
		return failed("%v", err)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	executable, args, env := Command(req.Getenv, os.Environ(), group, req.Path)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	cmd.Stdout = req.Output
	cmd.Stderr = req.Output
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failed("the evidence reporter %s ran past %s and was stopped", name, timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return failed("the evidence reporter %s exited with code %d: `putnami %s %s` refused the upload", name, exit.ExitCode(), group, Subcommand)
	}
	return failed("the evidence reporter %s did not start: %v", name, err)
}

// Skipped returns the diagnostic of an upload that has no document to hand.
func Skipped() []diag.Diagnostic {
	return failed("the evidence document was not written, so nothing was uploaded")
}

func failed(format string, args ...any) []diag.Diagnostic {
	return []diag.Diagnostic{diag.Warningf(CodeUploadFailed, "upload", format, args...)}
}
