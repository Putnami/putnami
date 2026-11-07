// Package upload hands a change evidence document to the evidence reporter a
// CI job selects.
//
// The reporter is an installed extension. PUTNAMI_EVIDENCE_REPORTER names it,
// @putnami/cloud when unset, and the extension answers the subcommand
// `<name> evidence-report --file <path> --output=json`, where <name> is its
// name without the scope. Observe runs that subcommand through the CLI that
// started it, so a third party writes its own reporter as one
// workspace-optional subcommand. The reporter answers on its standard output
// one protocol CLI version 2 result, whose data.reportUrl is the URL of the
// report it stored. Every failure is a warning diagnostic: an upload never
// fails the job.
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	// OutputFlag asks the reporter for its protocol CLI result.
	OutputFlag = "--output=json"
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
	// maxResultBytes bounds the standard output observe reads as the
	// reporter's result.
	maxResultBytes = 1 << 20
	// maxReportURLBytes bounds the report URL observe keeps.
	maxReportURLBytes = 2048
	// resultProtocolVersion is the protocol CLI result version a reporter
	// answers.
	resultProtocolVersion = 2
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
	// Output receives the reporter's standard error, its standard output when
	// that is not a protocol CLI result, and the line naming the report.
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
	return executable, []string{group, Subcommand, FileFlag, path, OutputFlag}, env
}

// Run hands the document to the selected reporter. It returns the report URL
// the reporter answered, "" for none, and the diagnostic of a failure, nil on
// success.
func Run(ctx context.Context, req Request) (reportURL string, diags []diag.Diagnostic) {
	output := req.Output
	if output == nil {
		output = io.Discard
	}
	name, group, err := Reporter(req.Getenv)
	if err != nil {
		return "", failed("%v", err)
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
	stdout := &boundedBuffer{limit: maxResultBytes}
	cmd.Stdout = stdout
	cmd.Stderr = output
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	answer, isResult := parseResult(stdout.Bytes())
	if !isResult {
		_, _ = output.Write(stdout.Bytes())
	}
	switch {
	case err == nil:
		reportURL = answer.reportURL()
		if reportURL == "" {
			fmt.Fprintf(output, "putnami observe: the evidence reporter %s stored the evidence\n", name)
		} else {
			fmt.Fprintf(output, "putnami observe: the evidence reporter %s stored the evidence; the report is at %s\n", name, reportURL)
		}
		return reportURL, nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", failed("the evidence reporter %s ran past %s and was stopped", name, timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		message := fmt.Sprintf("the evidence reporter %s exited with code %d: `putnami %s %s` refused the upload", name, exit.ExitCode(), group, Subcommand)
		if reason := answer.errorMessage(); reason != "" {
			message += ": " + reason
		}
		return "", failed("%s", message)
	}
	return "", failed("the evidence reporter %s did not start: %v", name, err)
}

// result is the part of a protocol CLI version 2 result observe reads.
type result struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Status          string          `json:"status"`
	Data            json.RawMessage `json:"data"`
	Error           *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseResult reads the reporter's standard output as a stream of JSON
// documents and returns the last protocol CLI version 2 result, and whether it
// holds one. A reporter answers one result; the last one wins when it writes
// more.
func parseResult(stdout []byte) (result, bool) {
	var last result
	found := false
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var r result
		if decoder.Decode(&r) != nil {
			return last, found
		}
		if r.ProtocolVersion == resultProtocolVersion && r.Status != "" {
			last, found = r, true
		}
	}
}

// reportURL returns data.reportUrl of a successful result when it is an
// absolute https URL without credentials, "" otherwise.
func (r result) reportURL() string {
	if r.Status != "success" {
		return ""
	}
	var data struct {
		ReportURL string `json:"reportUrl"`
	}
	if json.Unmarshal(r.Data, &data) != nil {
		return ""
	}
	return ReportURL(data.ReportURL)
}

// errorMessage is the message of a failure result, on one line.
func (r result) errorMessage() string {
	if r.Error == nil {
		return ""
	}
	return strings.Join(strings.Fields(r.Error.Message), " ")
}

// ReportURL returns raw when it is an absolute https URL without credentials,
// at most 2048 bytes, that a Markdown link carries as is; "" otherwise.
func ReportURL(raw string) string {
	if raw == "" || len(raw) > maxReportURLBytes || strings.ContainsAny(raw, " \t\r\n<>()\"'`\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	return raw
}

// boundedBuffer keeps the first limit bytes written to it and discards the
// rest, so a reporter that prints without end cannot exhaust the memory.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// Skipped returns the diagnostic of an upload that has no document to hand.
func Skipped() []diag.Diagnostic {
	return failed("the evidence document was not written, so nothing was uploaded")
}

func failed(format string, args ...any) []diag.Diagnostic {
	return []diag.Diagnostic{diag.Warningf(CodeUploadFailed, "upload", format, args...)}
}
