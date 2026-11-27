package cli

import (
	"errors"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/machineproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// servesThroughMachine reports whether args may run in a worker of the
// machine process (CLI ADR 0116 §4): a command that runs the engine, not a
// hosted run, not watch mode, not a structured command (install, config,
// machine…), and not serve, which keeps its own process.
func servesThroughMachine(args []string) bool {
	if runcredential.Hosted() || len(args) == 0 {
		return false
	}
	first := args[0]
	if first == "" || strings.HasPrefix(first, "-") || isStructuredCommand(first) {
		return false
	}
	for _, verb := range strings.Split(first, ",") {
		if verb == "serve" {
			return false
		}
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "--watch" || strings.HasPrefix(arg, "--watch=") {
			return false
		}
	}
	return true
}

// machineStatusResult is the structured answer of `machine status`.
type machineStatusResult struct {
	Running bool                `json:"running"`
	Use     bool                `json:"use"`
	Reason  string              `json:"reason,omitempty"`
	Setting string              `json:"setting,omitempty"`
	Machine *machineproc.Status `json:"machine,omitempty"`
}

func machineStatus(output string) error {
	setting := machineproc.ReadSetting(os.Getenv)
	decision := machineproc.Decide(os.Getenv, setting, machineproc.DefaultOn)
	result := machineStatusResult{Use: decision.Use, Reason: decision.Reason, Setting: setting}
	if r, err := machineproc.Query("status"); err == nil && r.OK {
		result.Running, result.Machine = true, r.Status
	}
	if protocolcli.OutputMode(output).IsStructured() {
		return shared.WriteResult(output, "machine status", result, nil)
	}
	iox.Fprintln(os.Stdout)
	switch {
	case decision.Use:
		iox.Fprintln(os.Stdout, "  Machine process: on")
	default:
		iox.Fprintf(os.Stdout, "  Machine process: off (%s)\n", decision.Reason)
	}
	if !result.Running {
		iox.Fprintln(os.Stdout, "  Supervisor: not running")
		iox.Fprintln(os.Stdout)
		return nil
	}
	st := result.Machine
	iox.Fprintf(os.Stdout, "  Supervisor: pid %d since %s, protocol %d\n", st.PID, st.Started, st.Protocol)
	if len(st.Workers) == 0 {
		iox.Fprintln(os.Stdout, "  Workers: none")
	}
	for _, w := range st.Workers {
		state := "idle"
		if w.Busy {
			state = "running"
		}
		iox.Fprintf(os.Stdout, "  Worker %s: pid %d, %s, %d runs, last %s, %d MiB held\n",
			shortBuild(w.Build), w.PID, state, w.Runs, w.LastUsed, w.HeldBytes>>20)
	}
	iox.Fprintln(os.Stdout)
	return nil
}

func machineStop() error {
	if _, err := machineproc.Query("stop"); err != nil {
		if errors.Is(err, machineproc.ErrNotRunning) {
			iox.Fprintln(os.Stdout, "  No machine process is running.")
			return nil
		}
		return err
	}
	iox.Fprintln(os.Stdout, "  Machine process stopping once no run is active.")
	return nil
}

func shortBuild(build string) string {
	if len(build) > 12 {
		return build[:12]
	}
	return build
}
