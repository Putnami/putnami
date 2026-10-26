// Package agentsession runs the agent host hooks that @putnami/contributor
// declares. It publishes a Claude Code or Codex session to its mission
// through the missions collaboration contract, and gives the agent the
// owner's messages and answers.
//
// putnami runs `putnami agent hook` as an interactive subcommand, so the hook
// reaches the bound missions provider through the host RPC channel and never
// starts putnami again. The hook never fails a turn: every failure, a missing
// channel and every outcome other than ok exit 0 and leave one line in
// hook.log. Only the idle wake exits with another code, 75, to ask the host
// for a new turn.
package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/putnamihome"

	"go.putnami.dev/tooling/contributor/internal/childenv"
)

// Host is the channel into the putnami process that launched the hook.
// *host.Channel is one.
type Host interface {
	// Call routes one collaboration operation. A failed call is an error; an
	// operation that fails at the provider is an envelope.
	Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error)
	// Close drops the connection a call opened. The next call dials again.
	Close() error
}

// Env is what the hook and attach read from the process. Tests replace the
// clock, git and the host.
type Env struct {
	// Home is the Putnami home. The sessions live under
	// <Home>/contributor/sessions, shared by every checkout.
	Home string
	// Root is the working directory putnami launched the command in: the
	// workspace root.
	Root string
	// Host is the host channel, or nil when the launch has none.
	Host Host
	// Git runs git in dir and answers its trimmed standard output.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// Now is the clock.
	Now func() time.Time
	// Sleep waits d unless ctx ends first.
	Sleep func(ctx context.Context, d time.Duration) error
	// Getenv reads the process environment.
	Getenv func(string) string
}

// NewEnv is the Env of a launch: the process clock and environment, git on
// PATH, and the Putnami home the environment names, else .putnami under the
// workspace root.
func NewEnv(root string, channel Host) *Env {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		home = filepath.Join(root, putnamihome.DirName)
	}
	return &Env{Home: home, Root: root, Host: channel, Git: runGit, Now: time.Now, Sleep: sleepContext, Getenv: os.Getenv}
}

// sessionsDir holds every session cache, the mission declarations, the
// replied markers and hook.log.
func (e *Env) sessionsDir() string {
	return filepath.Join(e.Home, "contributor", "sessions")
}

func (e *Env) files(engine, session string) sessionFiles {
	return sessionFilesFor(e.sessionsDir(), sessionKey(engine, session))
}

func (e *Env) log(engine, event string, failure error) {
	logHookFailure(e.sessionsDir(), e.Now(), engine, event, failure)
}

// The time each hook may take. A call into the host starts the bound
// provider's process and waits for its answer, so it costs much more than
// one HTTP request: the hook cannot keep a short fixed bound per call.
// Instead each event gets a budget that stays inside the timeout its hook
// entry declares, and each call takes at most callTimeout of what is left.
// The common longest path of each event fits its budget: two git commands
// and two calls for an ordinary hook. A rare third call, the plan retry,
// runs on what is left; when nothing is left, the next sync sends what it
// would have sent.
const (
	// callTimeout bounds one call. It leaves room for a provider that starts
	// cold, and two calls still fit the budget of an ordinary hook.
	callTimeout = 10 * time.Second
	// launcherMargin is what each budget leaves of the declared timeout to
	// the launcher, which loads the workspace before the hook runs and stops
	// the host channel after it.
	launcherMargin = 5 * time.Second
	// The timeouts the hook entries of putnami.extension.json declare.
	hookTimeout      = 30 * time.Second
	codexStopTimeout = 240 * time.Second
	idleWakeTimeout  = 660 * time.Second
)

// eventBudget is how long one hook runs at most. The Codex Stop waits up to
// sessionDecisionWait for an answer and the idle wake up to
// sessionIdleWakeWindow, so their entries declare longer timeouts.
func eventBudget(engine, event string, idleWake bool) time.Duration {
	switch {
	case idleWake:
		return idleWakeTimeout - launcherMargin
	case engine == sessionEngineCodex && event == hookEventStop:
		return codexStopTimeout - launcherMargin
	}
	return hookTimeout - launcherMargin
}

// call runs one operation within callTimeout and what is left of ctx. A call
// that fails on the channel, a timeout included, leaves the connection broken
// for every later call, so the hook closes it and the next call dials again.
func (e *Env) call(ctx context.Context, contract, operation string, input any) (collab.Envelope, error) {
	if e.Host == nil {
		return collab.Envelope{}, errors.New("no host channel: putnami did not launch the hook")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return collab.Envelope{}, err
	}
	callContext, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	envelope, err := e.Host.Call(callContext, contract, operation, data)
	if err != nil {
		_ = e.Host.Close()
		return collab.Envelope{}, err
	}
	return envelope, nil
}

// outcomeFailure is an envelope whose outcome is not ok.
type outcomeFailure struct {
	envelope collab.Envelope
}

func (f *outcomeFailure) Error() string {
	message, reason := "", ""
	if f.envelope.Error != nil {
		message, reason = f.envelope.Error.Message, f.envelope.Error.Reason
	}
	text := fmt.Sprintf("%s.%s answered %s", f.envelope.Contract, f.envelope.Operation, f.envelope.Outcome)
	if reason != "" {
		text += " (" + boundedLine(reason, 64) + ")"
	}
	if message != "" {
		text += ": " + boundedLine(message, collab.MaxErrorLength)
	}
	return text
}

// outcomeError describes an envelope whose outcome is not ok.
func outcomeError(envelope collab.Envelope) error {
	return &outcomeFailure{envelope: envelope}
}

// runGit runs git in dir and answers its trimmed standard output. Its
// environment holds no repository selector and no host channel.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed git subcommands.
	command.Env = childenv.Repository(os.Environ())
	out, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// gitTimeout bounds one git command of the hook. The hook runs only git
// commands that read local files: the branch and the origin URL.
const gitTimeout = 2 * time.Second

func (e *Env) git(ctx context.Context, dir string, args ...string) (string, error) {
	gitContext, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	return e.Git(gitContext, dir, args...)
}
