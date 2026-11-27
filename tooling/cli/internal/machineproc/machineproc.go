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

// inWorker is set in a worker: the commands it runs never go through the
// machine process again. It is no environment variable, which an env-input
// glob of a key could read (CLI ADR 0116 §8).
var inWorker bool

// jobEnv is set in the environment of every job a run starts
// (scheduler.JobEnvironment): a CLI that sees it is a nested run.
const jobEnv = "PUTNAMI_JOB_NAME"

// Refusal codes a supervisor or worker answers.
const (
	CodeOutdated = "machine.supervisor_outdated"
	CodeIdentity = "machine.identity"
	CodeBusy     = "machine.busy"
	CodeStopping = "machine.stopping"
	CodeContext  = "machine.context"
	CodeTerminal = "machine.terminal"
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
	// Fingerprint is processFingerprint of the client: a worker whose own
	// differs refuses the run.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Terminal says the client's stdin and stdout are a terminal: the worker
	// gives the run a pseudo-terminal whose master the client relays.
	Terminal bool `json:"terminal,omitempty"`
	// StderrTerminal says the client's stderr is that terminal too.
	StderrTerminal bool `json:"stderrTerminal,omitempty"`
}

// MemoryDecision is whether the last request a worker served could have been
// answered from memory, and why not.
type MemoryDecision struct {
	Time       string `json:"time"`
	Root       string `json:"root"`
	Answerable bool   `json:"answerable"`
	Reason     string `json:"reason"`
	// Answered says the scheduler answered every plan of the run from the
	// records the held run left, with no lookup, key or restore.
	Answered bool `json:"answered"`
	// Kept says the run that followed is held for the next request, or why
	// not.
	Kept string `json:"kept"`
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
	// Ready says the worker took the request and waits for the client's go:
	// a client that waited too long runs the command itself and never sends
	// it, so a command never runs twice.
	Ready bool `json:"ready,omitempty"`
	// Terminal says Ready carries the master of the run's pseudo-terminal.
	Terminal bool `json:"terminal,omitempty"`
	// Started says the run has begun.
	Started bool `json:"started,omitempty"`
	// Exit is the run's exit code, its last event.
	Exit *int `json:"exit,omitempty"`
	// Refused is a refusal before the run began; the client runs it itself.
	Refused string `json:"refused,omitempty"`
	// Lost says the worker ended during the run, after the supervisor ended
	// what the run had started: the client runs the command itself.
	Lost bool `json:"lost,omitempty"`
}

// clientSignal is what a client sends the worker that serves it: "go" once it
// read Ready, then "interrupt" or "terminate" as it receives them.
type clientSignal struct {
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
	case inWorker || getenv(jobEnv) != "":
		return Decision{Reason: "nested"}
	case inCI(getenv):
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

// CIVariables are set by CI services; those that set none of them set CI.
var CIVariables = []string{"CI", "TF_BUILD", "JENKINS_URL", "BUILD_ID", "TEAMCITY_VERSION", "CODEBUILD_BUILD_ID", "BUILDKITE", "GITLAB_CI", "GITHUB_ACTIONS", "CIRCLECI", "BITBUCKET_BUILD_NUMBER"}

// inCI reports whether the command runs in a CI service.
func inCI(getenv func(string) string) bool {
	for _, name := range CIVariables {
		if truthy(getenv(name)) {
			return true
		}
	}
	return false
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
