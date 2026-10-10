package clicore

import (
	"context"
	"net/http"
	"time"
)

// IO is the capability surface a command uses to talk to the user and the
// outside world: output sinks, the JSONL event hooks the parent CLI renders,
// the shared HTTP client, a clock, and an interactive confirm prompt.
type IO struct {
	Env    map[string]string
	Stdout func(string)
	Stderr func(string)
	JSON   func(any)
	// Artifact reports one structured runtime artifact. The callback owns the
	// runtime envelope; commands provide the provider-neutral identity and data
	// only after the side effect they describe has completed.
	Artifact func(id, name, kind, path string, data map[string]any) error
	Phase    func(string)
	Progress func(current, total float64, message string)
	TTY      func(string)
	Result   func(status string, data any)
	Client   *http.Client
	Now      func() time.Time
	// OpenBrowser opens one URL in the user's default browser. Commands fall
	// back to the package-level OpenBrowser helper when this seam is nil.
	OpenBrowser func(string) error
	// Prompt reads one free-form interactive answer. The extension binary supplies
	// a /dev/tty-backed default; tests and embedding callers can inject it.
	Prompt func(question string) (answer string, ok bool)
	// Context optionally supplies an invocation lifecycle. Long-running
	// commands fall back to a signal-aware context when it is nil.
	Context context.Context
	// Sleep pauses a polling loop while remaining cancelable. Commands fall
	// back to a context-aware timer when this test seam is nil.
	Sleep func(context.Context, time.Duration) error
	// Confirm prompts the user for y/n. Returns the user's raw reply plus
	// `ok` for whether a prompt was possible — non-TTY environments return
	// ok=false so callers can decide whether to skip the prompt entirely
	// (the right behavior for `--reveal` in CI: print without prompting).
	// When nil, the CLI builds a default prompt against /dev/tty.
	Confirm func(question string) (answer string, ok bool)
}
