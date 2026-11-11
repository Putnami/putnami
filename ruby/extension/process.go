package ruby

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

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
)

const (
	// CodeNoRackConfig is the diagnostic of an application without config.ru.
	CodeNoRackConfig = "ruby.no_rack_config"
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "ruby.run_failed"
)

const (
	defaultServePort  = 3000
	defaultEntrypoint = "bin/rails"
	railsExecutable   = "bin/rails"
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
)

// ServeJob runs the application's server through Bundler until the user stops
// it, and announces it ready once it accepts connections: rails server for a
// Rails application, rackup for any other Rack application. Port 0 selects a
// free port. A library is skipped.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == TypeLibrary {
			return "SKIP", nil, nil
		}
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if !isFile(filepath.Join(root.Project, rackConfig)) {
			emit.DiagnosticWithCode("error", fmt.Sprintf("%s has no %s: it is no Rack application", root.Project, rackConfig), "", 0, 0, CodeNoRackConfig)
			return "FAILED", nil, errors.New(CodeNoRackConfig)
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
				return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
			}
		}
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		argv := append(serverArgs(root.Project, strconv.Itoa(port)), strings.Fields(ctx.Params.String("args"))...)
		code, stopped, err := stacks.RunForeground(root, root.Program("bundle"), env, emit, argv...)
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

// serverArgs are the bundle arguments that serve the application at dir on
// every interface at port: rails server when it holds bin/rails, rackup
// otherwise.
func serverArgs(dir, port string) []string {
	if isFile(filepath.Join(dir, filepath.FromSlash(railsExecutable))) {
		return []string{"exec", "rails", "server", "-b", "0.0.0.0", "-p", port}
	}
	return []string{"exec", "rackup", "-o", "0.0.0.0", "-p", port}
}

// RunJob runs the application's entrypoint once through bundle exec ruby with
// the args parameter, and answers its exit code. A library is skipped.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		if ctx.Project.Type == TypeLibrary {
			return "SKIP", nil, nil
		}
		entrypoint := ctx.Params.String("entrypoint")
		if entrypoint == "" {
			entrypoint = defaultEntrypoint
		}
		arguments := strings.Fields(ctx.Params.String("args"))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		code, _, err := stacks.RunForeground(root, root.Program("bundle"), root.Env, nil, append([]string{"exec", "ruby", entrypoint}, arguments...)...)
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
