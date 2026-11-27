package cli

import (
	"errors"
	"os"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/machineproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// servesThroughMachine reports whether a parsed invocation may run in a
// worker of the machine process (CLI ADR 0116 §4): a job command, read after
// its aliases resolved, that is not a hosted run, does not watch (--watch,
// -w, or a serve workload, which watches by default), is no structured or
// extension command, and is not publish, deploy or release, which a lost
// worker's rerun could repeat.
func servesThroughMachine(parsed *ParsedArgs, aliases commandAliases, extensionGroups map[string]bool) bool {
	if runcredential.Hosted() || parsed == nil || parsed.Err != nil || len(parsed.Commands) == 0 {
		return false
	}
	if parsed.Global.Help || parsed.Global.Version || parsed.Global.Watch {
		return false
	}
	first := parsed.Commands[0]
	if isStructuredCommand(first) || extensionGroups[first] {
		return false
	}
	if ownsWatchFlag(watchCommands(parsed, aliases, extensionGroups), aliases.serveWorkloads) {
		return false
	}
	for _, command := range parsed.Commands {
		switch command {
		case "serve", "publish", "deploy", "release":
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
	// LastLocal is why the last command meant for the machine process ran in
	// its own process.
	LastLocal *machineproc.LastLocal `json:"lastLocal,omitempty"`
	// LastMemory is whether the last run a worker served could have been
	// answered from memory, and whether it is held for the next one.
	LastMemory *machineproc.MemoryDecision `json:"lastMemory,omitempty"`
}

func machineStatus(output string) error {
	setting := machineproc.ReadSetting(os.Getenv)
	decision := machineproc.Decide(os.Getenv, setting, machineproc.DefaultOn)
	result := machineStatusResult{Use: decision.Use, Reason: decision.Reason, Setting: setting, LastLocal: machineproc.ReadLastLocal(), LastMemory: machineproc.ReadLastMemory()}
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
	if last := result.LastLocal; last != nil {
		iox.Fprintf(os.Stdout, "  Last run in its own process: %s, %s (%s)\n", last.Time, last.Reason, last.Dir)
	}
	if m := result.LastMemory; m != nil {
		answer := "no, " + m.Reason
		if m.Answerable {
			answer = "yes"
		}
		if m.Answered {
			answer = "yes, answered from memory"
		}
		if m.Answered && m.PlanSkipped {
			answer = "yes, answered from memory without loading or planning"
		}
		iox.Fprintf(os.Stdout, "  Last worker run answerable from memory: %s; %s (%s)\n", answer, m.Kept, m.Root)
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
