// Package exec provides subprocess execution utilities for Putnami extensions.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result holds the output of a subprocess execution.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Success  bool
}

// Option configures a command execution.
type Option func(*options)

type options struct {
	dir      string
	env      map[string]string
	unsetEnv []string
	timeout  time.Duration
	stdin    string
	ctx      context.Context
}

// Dir sets the working directory.
func Dir(dir string) Option {
	return func(o *options) { o.dir = dir }
}

// Env sets additional environment variables.
func Env(env map[string]string) Option {
	return func(o *options) { o.env = env }
}

// UnsetEnv removes inherited environment variables from the subprocess
// environment. It is the counterpart to Env: Env can only ADD to what the
// extension process itself inherited, so without UnsetEnv there is no way to
// spawn a child that does NOT see a variable its parent has.
//
// The filter applies to the inherited environment only, and applies BEFORE
// Env's additions — so `UnsetEnv("X")` together with `Env{"X": "v"}` yields
// `X=v`, not an absent X. Naming a variable that is not set is a no-op, and
// repeated calls accumulate. Matching is exact and case-sensitive.
//
// The canonical caller is a test harness scrubbing host platform identity
// (go.putnami.dev/sdk/extension/hostenv); this option deliberately knows
// nothing about which names those are.
func UnsetEnv(names ...string) Option {
	return func(o *options) { o.unsetEnv = append(o.unsetEnv, names...) }
}

// Timeout sets a timeout for the command.
func Timeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithContext sets a context for cancellation.
func WithContext(ctx context.Context) Option {
	return func(o *options) { o.ctx = ctx }
}

// Stdin sets stdin content for the command.
func Stdin(s string) Option {
	return func(o *options) { o.stdin = s }
}

// filterEnv drops every `KEY=VALUE` entry whose key is named in unset. An
// entry with no "=" is not an assignment and is passed through untouched.
func filterEnv(env, unset []string) []string {
	if len(unset) == 0 {
		return env
	}
	drop := make(map[string]struct{}, len(unset))
	for _, name := range unset {
		drop[name] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if _, found := drop[key]; found {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

// Run executes a command and returns the result.
func Run(name string, args []string, opts ...Option) (*Result, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)

	if o.dir != "" {
		cmd.Dir = o.dir
	}

	cmd.Env = filterEnv(os.Environ(), o.unsetEnv)
	for k, v := range o.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if o.stdin != "" {
		cmd.Stdin = strings.NewReader(o.stdin)
	}

	err := cmd.Run()

	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			return nil, fmt.Errorf("executing %s: %w", name, err)
		}
		exitCode = exitErr.ExitCode()
	}

	return &Result{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Success:  exitCode == 0,
	}, nil
}
