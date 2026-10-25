package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/resultv2"

	"go.putnami.dev/tooling/dev/internal/cliargs"
)

// CommandPath is the `command` of every result envelope `putnami compose`
// writes. The group's default subcommand answers it, so a consumer bound to
// the documents of `putnami compose` reads the same documents.
const CommandPath = "compose"

// commandFlags are the flags `putnami compose` accepts after putnami consumed
// its own: --watch=false reaches this command as --no-watch.
var commandFlags = []cliargs.Flag{
	{Long: "--port", ValueName: "<n>"},
	{Long: "--no-watch"},
	{Long: "--ready-timeout", ValueName: "<duration>"},
}

// Request is one `putnami compose` invocation.
type Request struct {
	Ctx context.Context
	// Workspace is the membership the composition plans against.
	Workspace *Workspace
	// Args are the invocation's arguments after the subcommand: the target's
	// project id or name and the command's flags.
	Args []string
	// Output is the output mode putnami resolved for the invocation.
	Output protocolcli.OutputMode
	// Executable is the putnami CLI the members' serve runs start.
	Executable string
	// Verbose, Quiet and NoColor are the invocation's --verbose, --quiet and
	// --no-color, which shape the members' serve logs.
	Verbose bool
	Quiet   bool
	NoColor bool
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run composes the selected workload and blocks until its target exits or the
// process receives SIGINT or SIGTERM, then tears the composition down. It
// returns the exit code.
//
// The documents are the composition at start and at exit: the result
// envelope in a structured output mode, a short member table otherwise. They
// are compose.Status projections, which carry section names, never
// configuration values. A failure is one envelope whose data names the
// composition, the failed member and what the teardown left behind.
//
// The exit status is success only when the composition stopped on a signal
// and its teardown left nothing behind: a target that exits on its own and a
// partial cleanup are failures.
func Run(req Request) int {
	stdout, stderr := req.Stdout, req.Stderr
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	structured := structuredOutput(req.Output)
	emit := func(data any, err error) int {
		return resultv2.Emit(stdout, stderr, req.Output, CommandPath, "", data, err)
	}

	parsed, err := cliargs.Parse(CommandPath, req.Args, commandFlags)
	if err != nil {
		return emit(nil, err)
	}
	if len(parsed.Positionals) != 1 {
		return emit(nil, protocolcli.Usagef("compose takes exactly one project: putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>]"))
	}
	target, err := resolveTarget(req.Workspace, parsed.Positionals[0])
	if err != nil {
		return emit(nil, err)
	}
	proxyPort, err := resolvePort(parsed.Value("--port"), target)
	if err != nil {
		return emit(nil, err)
	}
	readyTimeout, err := resolveReadyTimeout(parsed.Value("--ready-timeout"))
	if err != nil {
		return emit(nil, err)
	}

	signals, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// Each member's serve run renders its serve log on stderr, as the
	// invocation's output flags ask, and it reaches this stderr as it arrives;
	// stdout stays the start and exit documents.
	comp, err := Up(signals, Options{
		Workspace:    req.Workspace,
		Target:       target,
		ProxyPort:    proxyPort,
		WatchTarget:  !parsed.Has("--no-watch"),
		ReadyTimeout: readyTimeout,
		Executable:   req.Executable,
		Logs:         stderr,
		Verbose:      req.Verbose,
		Quiet:        req.Quiet,
		NoColor:      structured || req.NoColor,
	})
	if err != nil {
		writeDetail(stderr, err, structured)
		return emit(failureData(err), err)
	}

	if err := writeStatus(stdout, req.Output, comp.Status()); err != nil {
		fmt.Fprintf(stderr, "putnami: %v\n", err)
	}

	waitErr := comp.Wait(signals)
	writeDetail(stderr, waitErr, structured)

	teardown, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	report := comp.Close(teardown)
	cancel()

	status := comp.Status()
	status.Cleanup = &report
	status.Reaped = nil
	var runErr error
	switch {
	case waitErr != nil:
		runErr = waitErr
	case report.State != CleanupClean:
		runErr = fmt.Errorf("composition %s stopped with a partial cleanup: %s", comp.ID(), strings.Join(report.Leftovers, "; "))
	}
	if runErr != nil && structured {
		// The one failure envelope carries the exit document, so a caller
		// reads the cleanup either way.
		return emit(status, runErr)
	}
	if err := writeStatus(stdout, req.Output, status); err != nil {
		fmt.Fprintf(stderr, "putnami: %v\n", err)
	}
	if runErr == nil {
		return protocolcli.ExitSuccess
	}
	return emit(nil, runErr)
}

// structuredOutput reports whether mode writes documents rather than text.
func structuredOutput(mode protocolcli.OutputMode) bool {
	return mode.IsStructured() || mode == protocolcli.OutputCloudLogging
}

// failureData returns the `data` of a failed composition's envelope: the
// composition error's code, composition id, member, phase and teardown
// report, so a start that failed after acquiring resources says what it left
// behind. Any other error has none.
func failureData(err error) any {
	var composeErr *Error
	if !errors.As(err, &composeErr) {
		return nil
	}
	return failureDocument{
		Code:    composeErr.Code,
		ID:      composeErr.ID,
		Member:  composeErr.Member,
		Phase:   composeErr.Phase,
		Cleanup: composeErr.Cleanup,
		Reaped:  composeErr.Reaped,
	}
}

// failureDocument is the `data` member of a failed composition's envelope. It
// carries no member output.
type failureDocument struct {
	Code    string         `json:"code"`
	ID      string         `json:"id,omitempty"`
	Member  string         `json:"member,omitempty"`
	Phase   string         `json:"phase"`
	Cleanup *CleanupReport `json:"cleanup,omitempty"`
	Reaped  []ReapedLease  `json:"reaped,omitempty"`
}

// writeDetail prints the last lines of a failed member's output on stderr, in
// text output only. The output is the workload's own and may repeat the
// configuration it received, so it stays out of the error message and out of
// every structured document.
func writeDetail(stderr io.Writer, err error, structured bool) {
	var composeErr *Error
	if structured || !errors.As(err, &composeErr) || len(composeErr.Detail) == 0 {
		return
	}
	fmt.Fprintf(stderr, "last output of %s:\n", composeErr.Member)
	for _, line := range composeErr.Detail {
		fmt.Fprintf(stderr, "  | %s\n", line)
	}
}

func resolveTarget(ws *Workspace, selector string) (*Project, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, protocolcli.Usagef("compose needs exactly one project: putnami compose <project>")
	}
	if project := ws.ProjectByID(selector); project != nil {
		return project, nil
	}
	if project := ws.ProjectByName(selector); project != nil {
		return project, nil
	}
	return nil, protocolcli.Usagef("compose: %q names no project of this workspace; pass a project id (/path/to/project) or name", selector)
}

func resolvePort(raw string, target *Project) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultTargetProxyPort(target), nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 0 || port > 65535 {
		return 0, protocolcli.Usagef("--port %q is not a TCP port (0-65535; 0 picks an ephemeral port)", raw)
	}
	return port, nil
}

func resolveReadyTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultReadyTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || timeout <= 0 {
		return 0, protocolcli.Usagef("--ready-timeout %q is not a positive duration (for example 60s or 2m)", raw)
	}
	return timeout, nil
}

// writeStatus prints a composition document: the result envelope in a
// structured mode, a short member table otherwise.
func writeStatus(w io.Writer, mode protocolcli.OutputMode, status Status) error {
	if structuredOutput(mode) {
		if !mode.IsStructured() {
			mode = protocolcli.OutputJSONL
		}
		_, err := protocolcli.WriteResultV2(w, mode, protocolcli.NewResultV2(CommandPath, status, nil))
		return err
	}
	if status.Cleanup == nil {
		for _, reaped := range status.Reaped {
			fmt.Fprintf(w, "  reaped composition %s (%s)\n", reaped.ID, reaped.Target)
			for _, leftover := range reaped.Leftovers {
				fmt.Fprintf(w, "    left behind: %s\n", leftover)
			}
		}
		fmt.Fprintf(w, "  composition %s: %s (isolation %s)\n", status.ID, status.Target, status.Isolation)
		for _, member := range status.Members {
			sections := "no injected configuration"
			if len(member.ConfigSections) > 0 {
				sections = strings.Join(member.ConfigSections, ", ")
			}
			fmt.Fprintf(w, "    %s  %s  ready in %dms  (%s)\n", member.Project, member.ProxyURL, member.ReadyMs, sections)
			for _, note := range member.Notes {
				fmt.Fprintf(w, "      note: %s\n", note)
			}
		}
		return nil
	}
	fmt.Fprintf(w, "  composition %s stopped: cleanup %s\n", status.ID, status.Cleanup.State)
	for _, leftover := range status.Cleanup.Leftovers {
		fmt.Fprintf(w, "    left behind: %s\n", leftover)
	}
	return nil
}
