package php

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

const (
	// ServeJobName is the job of the serve command.
	ServeJobName = "serve"
	// RunJobName is the job of the run command.
	RunJobName = "run"
	// DepsUpgradeJobName is the job of the deps-upgrade command.
	DepsUpgradeJobName = "deps-upgrade"
)

const (
	// CodeNoFrontController is the diagnostic of an application without
	// public/index.php.
	CodeNoFrontController = "php.no_front_controller"
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "php.run_failed"
	// CodeComposerUpdateFailed is the diagnostic of a failed composer update.
	CodeComposerUpdateFailed = "php.composer_update_failed"
)

const (
	defaultServePort  = 3000
	defaultEntrypoint = "bin/console"
	frontController   = "public/index.php"
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
	// stopGrace is how long a stopped process has to exit before it is killed.
	stopGrace = 5 * time.Second
)

// ServeJob runs the PHP built-in server on an application until the user stops
// it. The document root answers a path that is no file with its index.php, so
// the server needs no router script and still serves the static files.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if _, err := os.Stat(filepath.Join(root.Project, filepath.FromSlash(frontController))); err != nil {
			emit.DiagnosticWithCode("error", fmt.Sprintf("%s has no %s: the server has no front controller", root.Project, frontController), "", 0, 0, CodeNoFrontController)
			return "FAILED", nil, errors.New(CodeNoFrontController)
		}
		address := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		code, stopped, err := runForeground(root, env, "-S", address, "-t", "public")
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs the application's entrypoint once with the args parameter and
// answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		entrypoint := ctx.Params.String("entrypoint")
		if entrypoint == "" {
			entrypoint = defaultEntrypoint
		}
		arguments := strings.Fields(ctx.Params.String("args"))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		code, _, err := runForeground(root, root.Env, append([]string{entrypoint}, arguments...)...)
		if err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
		}
		data := map[string]any{exitCodeKey: code}
		if code != 0 {
			// A nil error: the job ran, and the SDK keeps the result data that
			// carries the exit code only when there is none.
			return "FAILED", data, nil
		}
		return "OK", data, nil
	}
}

// runForeground runs php in the project directory until it exits or the user
// stops the job. The job's stdout carries its protocol, so the program's output
// goes to stderr. It answers the exit code and whether the user stopped it; an
// error says the program did not start.
func runForeground(root stacks.RootStep, env []string, args ...string) (int, bool, error) {
	stopping, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(stopping, root.Program("php"), args...) //nolint:gosec // the selected PHP
	cmd.Dir = root.Project
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	err := cmd.Run()
	stopped := stopping.Err() != nil
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stopped, nil
	}
	if err != nil && stopped {
		return 0, true, nil
	}
	return 0, stopped, err
}

// DepsUpgradeJob runs composer update at each stack root.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, upgradeRoot)
}

func upgradeRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if err := prepareWindowsPHP(root, emit); err != nil {
		return err
	}
	phar, err := provideComposer(ctx)
	if err == nil {
		emit.Info("$ composer update in " + root.Dir)
		err = runComposer(ctx, root, composerEnv(root.Env), phar, "update", "--no-interaction", "--no-progress")
	}
	if err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("composer update in %s: %v", root.Dir, err), "", 0, 0, CodeComposerUpdateFailed)
		return fmt.Errorf("%s: %w", CodeComposerUpdateFailed, err)
	}
	return nil
}
