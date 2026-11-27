// Package machineproc is the machine process of CLI ADR 0116: one supervisor per
// machine and user, and behind it one engine worker per CLI build in use. A
// run that a worker serves is the same code as a run in its own process; the
// worker only keeps the process warm between runs. A run never depends on the
// machine process: any doubt, refusal or failure runs it in its own process.
package machineproc

import (
	"os"
	"runtime"
	"strings"
)

// ProtocolVersion is the version of the client ↔ supervisor and supervisor ↔
// worker protocols. It changes by addition only (CLI ADR 0116 §3): a
// supervisor serves every version from 1 to its own.
const ProtocolVersion = 1

// Env turns the machine process on or off for one command: "on" or "off".
// It overrides the user's setting (Setting).
const Env = "PUTNAMI_MACHINE"

// RunEnv marks a run that a worker serves. A CLI started by one of its jobs
// sees it and runs in its own process: a nested run through the machine
// process could wait on the run that started it.
const RunEnv = "PUTNAMI_MACHINE_RUN"

// jobEnv is set in the environment of every job a run starts
// (scheduler.JobEnvironment): a CLI that sees it is a nested run.
const jobEnv = "PUTNAMI_JOB_NAME"

// Refusal codes a supervisor or worker answers.
const (
	CodeOutdated = "machine.supervisor_outdated"
	CodeIdentity = "machine.identity"
	CodeBusy     = "machine.busy"
	CodeStopping = "machine.stopping"
)

// request is what a client sends the supervisor, and the supervisor the
// worker it routes a run to.
type request struct {
	Protocol int `json:"protocol"`
	// Op is "run", "status" or "stop".
	Op string `json:"op"`
	// Build is the client's build identity (fileindex.BuildIdentity).
	Build string `json:"build,omitempty"`
	// Executable is the path of the client's executable, which the supervisor
	// starts as the worker of its build.
	Executable string `json:"executable,omitempty"`
	// The run's process context.
	Args  []string `json:"args,omitempty"`
	Env   []string `json:"env,omitempty"`
	Dir   string   `json:"dir,omitempty"`
	Umask int      `json:"umask,omitempty"`
}

// Reply answers a request.
type Reply struct {
	Protocol int    `json:"protocol"`
	OK       bool   `json:"ok"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	// Status is the answer to a status request.
	Status *Status `json:"status,omitempty"`
}

// event is what a worker sends a client it serves.
type event struct {
	// Started says the run has begun: from then on a lost connection is a
	// lost run, not one the client may run again.
	Started bool `json:"started,omitempty"`
	// Exit is the run's exit code, its last event.
	Exit *int `json:"exit,omitempty"`
	// Refused is a refusal before the run began; the client runs it itself.
	Refused string `json:"refused,omitempty"`
}

// signal is what a client sends the worker that serves it.
type signal struct {
	Signal string `json:"signal"`
}

// control is what a worker and its supervisor exchange.
type control struct {
	Op string `json:"op"` // "ready", "done"
	// HeldBytes is the worker's heap in use after a run.
	HeldBytes uint64 `json:"heldBytes,omitempty"`
}

// Status describes the supervisor and its workers.
type Status struct {
	Protocol   int            `json:"protocol"`
	PID        int            `json:"pid"`
	Started    string         `json:"started"`
	Executable string         `json:"executable"`
	Workers    []WorkerStatus `json:"workers"`
}

// WorkerStatus describes one worker.
type WorkerStatus struct {
	Build      string `json:"build"`
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Busy       bool   `json:"busy"`
	Runs       int    `json:"runs"`
	LastUsed   string `json:"lastUsed"`
	HeldBytes  uint64 `json:"heldBytes"`
}

// supported reports whether this platform has a machine process: Linux and
// macOS (CLI ADR 0116 §12; Windows support is suspended).
func supported() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "darwin"
}

// Decision is whether a command may go through the machine process, and why
// not when it may not.
type Decision struct {
	Use    bool
	Reason string
}

// Decide is the machine process's switch for one command, from the
// environment getenv reads and the user's setting: off in CI, in a hosted or
// nested run, when PUTNAMI_MACHINE or the setting says off, and on platforms
// without one. defaultOn is the default when neither says.
func Decide(getenv func(string) string, setting string, defaultOn bool) Decision {
	switch {
	case !supported():
		return Decision{Reason: "platform"}
	case getenv(RunEnv) != "" || getenv(jobEnv) != "":
		return Decision{Reason: "nested"}
	case truthy(getenv("CI")):
		return Decision{Reason: "ci"}
	}
	switch strings.ToLower(strings.TrimSpace(getenv(Env))) {
	case "off", "0", "false", "no":
		return Decision{Reason: "env"}
	case "on", "1", "true", "yes":
		return Decision{Use: true}
	}
	switch setting {
	case SettingOff:
		return Decision{Reason: "setting"}
	case SettingOn:
		return Decision{Use: true}
	}
	if !defaultOn {
		return Decision{Reason: "default"}
	}
	return Decision{Use: true}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// processEnv is the environment the process started with, before the CLI
// takes any variable out of it (SaveProcessContext).
var processEnv []string

// SaveProcessContext records the process's environment as it started. The
// client sends it to the worker, which runs the command with it: the CLI
// later removes variables it captures (reporter selections, credentials).
func SaveProcessContext() {
	processEnv = os.Environ()
}
