// Package composecmd is the `putnami compose` vertical: it resolves the
// command line into a composition, runs it until the target exits or the user
// stops it, and reports the composition at start and at exit.
//
// The composition itself — planning, databases, proxies, members, lease and
// reaping — is internal/compose; this package only chooses what reaches the
// terminal. It never prints configuration: the start and exit documents are
// compose.Status projections, which carry section names, not values.
package composecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/compose"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// CommandPath is the catalog path this vertical serves.
const CommandPath = "compose"

// teardownTimeout bounds Close after the composition stops. Members get the
// job runner's SIGTERM-then-SIGKILL window; the rest is proxies and databases.
const teardownTimeout = 90 * time.Second

// Request is one `putnami compose` invocation, as the dispatcher parsed it.
type Request struct {
	Ctx           context.Context
	WorkspaceRoot string
	Config        *wsproto.Config
	// Selector names the target by project id or name.
	Selector string
	// Port is the raw --port value; empty uses the target's default.
	Port string
	// NoWatch runs the target production-mode without watch.
	NoWatch bool
	// ReadyTimeout is the raw --ready-timeout value; empty is 60s.
	ReadyTimeout string
	// OutputFormat is the resolved --output mode.
	OutputFormat string
	Verbose      bool
	Quiet        bool
	NoColor      bool

	// Stdout and Stderr default to the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

// Run composes the selected workload and blocks until its target exits or the
// process receives SIGINT or SIGTERM, then tears the composition down.
//
// The exit status is success only when the composition stopped on a signal
// and its teardown left nothing behind: a target that exits on its own and a
// partial cleanup are failures.
func Run(req Request) error {
	stdout, stderr := req.Stdout, req.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	structured := output.StructuredOutput(req.OutputFormat)

	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	target, err := resolveTarget(ws, req.Selector)
	if err != nil {
		return err
	}
	proxyPort, err := resolvePort(req.Port, target)
	if err != nil {
		return err
	}
	readyTimeout, err := resolveReadyTimeout(req.ReadyTimeout)
	if err != nil {
		return err
	}

	signals, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	comp, err := compose.Up(signals, compose.Options{
		WorkspaceRoot: req.WorkspaceRoot,
		Config:        req.Config,
		Target:        target,
		ProxyPort:     proxyPort,
		WatchTarget:   !req.NoWatch,
		ReadyTimeout:  readyTimeout,
		Output:        serveRenderer(req, stdout, stderr, structured),
		Preparation:   preparationRenderer(req, stderr),
	})
	if err != nil {
		writeDetail(stderr, err, structured)
		return failure(err)
	}

	if err := writeStatus(stdout, req.OutputFormat, comp.Status()); err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
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
	case report.State != compose.CleanupClean:
		runErr = fmt.Errorf("composition %s stopped with a partial cleanup: %s", comp.ID(), strings.Join(report.Leftovers, "; "))
	}
	if runErr != nil && structured {
		// The dispatcher writes the one failure envelope; its data is the exit
		// document, so a caller reads the cleanup either way.
		return shared.WithResultData(runErr, status)
	}
	if err := writeStatus(stdout, req.OutputFormat, status); err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
	}
	return runErr
}

// failure keeps the composition error's code, composition id, member, phase
// and teardown report available to the structured failure envelope, so a start
// that failed after acquiring resources says what it left behind.
func failure(err error) error {
	var composeErr *compose.Error
	if errors.As(err, &composeErr) {
		return shared.WithResultData(err, failureData{
			Code:    composeErr.Code,
			ID:      composeErr.ID,
			Member:  composeErr.Member,
			Phase:   composeErr.Phase,
			Cleanup: composeErr.Cleanup,
			Reaped:  composeErr.Reaped,
		})
	}
	return err
}

// failureData is the `data` member of a failed composition's envelope. It
// carries no member output.
type failureData struct {
	Code    string                 `json:"code"`
	ID      string                 `json:"id,omitempty"`
	Member  string                 `json:"member,omitempty"`
	Phase   string                 `json:"phase"`
	Cleanup *compose.CleanupReport `json:"cleanup,omitempty"`
	Reaped  []compose.ReapedLease  `json:"reaped,omitempty"`
}

// writeDetail prints the last lines of a failed member's output on stderr, in
// text output only. The output is the workload's own and may repeat the
// configuration it received, so it stays out of the error message and out of
// every structured document.
func writeDetail(stderr io.Writer, err error, structured bool) {
	var composeErr *compose.Error
	if structured || !errors.As(err, &composeErr) || len(composeErr.Detail) == 0 {
		return
	}
	iox.Fprintf(stderr, "last output of %s:\n", composeErr.Member)
	for _, line := range composeErr.Detail {
		iox.Fprintf(stderr, "  | %s\n", line)
	}
}

func resolveTarget(ws *workspace.Workspace, selector string) (*workspace.Project, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, cmderr.Usagef("compose needs exactly one project: putnami compose <project>")
	}
	if project := ws.ProjectByID(selector); project != nil {
		return project, nil
	}
	if project := ws.ProjectByName(selector); project != nil {
		return project, nil
	}
	return nil, cmderr.Usagef("compose: %q names no project of this workspace; pass a project id (/path/to/project) or name", selector)
}

func resolvePort(raw string, target *workspace.Project) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return compose.DefaultTargetProxyPort(target), nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 0 || port > 65535 {
		return 0, cmderr.Usagef("--port %q is not a TCP port (0-65535; 0 picks an ephemeral port)", raw)
	}
	return port, nil
}

func resolveReadyTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return compose.DefaultReadyTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || timeout <= 0 {
		return 0, cmderr.Usagef("--ready-timeout %q is not a positive duration (for example 60s or 2m)", raw)
	}
	return timeout, nil
}

// serveRenderer renders the members' serve logs: on stdout for a human, on
// stderr when stdout carries the structured documents.
func serveRenderer(req Request, stdout, stderr io.Writer, structured bool) jobs.Renderer {
	out := stdout
	if structured {
		out = stderr
	}
	output.SetColorsEnabled(!req.NoColor && !structured)
	return output.NewTextRenderer(out, stderr, output.TextRendererConfig{
		ServeMode: true,
		Verbose:   req.Verbose,
		Quiet:     req.Quiet,
	})
}

// preparationRenderer renders the engine run that prepares the serve
// pipelines. It always writes to stderr, so a structured stdout stays two
// documents.
func preparationRenderer(req Request, stderr io.Writer) jobs.Renderer {
	return output.NewTextRenderer(stderr, stderr, output.TextRendererConfig{
		Verbose: req.Verbose,
		Quiet:   req.Quiet,
	})
}

// writeStatus prints a composition document: the result envelope in a
// structured mode, a short member table otherwise.
func writeStatus(w io.Writer, format string, status compose.Status) error {
	if output.StructuredOutput(format) {
		mode := protocolcli.OutputMode(format)
		if !mode.IsStructured() {
			mode = protocolcli.OutputJSONL
		}
		_, err := protocolcli.WriteResultV2(w, mode, protocolcli.NewResultV2(CommandPath, status, nil))
		return err
	}
	if status.Cleanup == nil {
		for _, reaped := range status.Reaped {
			iox.Fprintf(w, "  reaped composition %s (%s)\n", reaped.ID, reaped.Target)
			for _, leftover := range reaped.Leftovers {
				iox.Fprintf(w, "    left behind: %s\n", leftover)
			}
		}
		iox.Fprintf(w, "  composition %s: %s (isolation %s)\n", status.ID, status.Target, status.Isolation)
		for _, member := range status.Members {
			sections := "no injected configuration"
			if len(member.ConfigSections) > 0 {
				sections = strings.Join(member.ConfigSections, ", ")
			}
			iox.Fprintf(w, "    %s  %s  ready in %dms  (%s)\n", member.Project, member.ProxyURL, member.ReadyMs, sections)
			for _, note := range member.Notes {
				iox.Fprintf(w, "      note: %s\n", note)
			}
		}
		return nil
	}
	iox.Fprintf(w, "  composition %s stopped: cleanup %s\n", status.ID, status.Cleanup.State)
	for _, leftover := range status.Cleanup.Leftovers {
		iox.Fprintf(w, "    left behind: %s\n", leftover)
	}
	return nil
}
