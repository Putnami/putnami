package rust

import (
	"context"
	"fmt"
	"net"
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
	// CodeRunFailed is the diagnostic of a program the job could not start.
	CodeRunFailed = "rust.run_failed"
)

const (
	defaultServePort = 3000
	// exitCodeKey is the result member the CLI forwards as the exit code of
	// putnami run.
	exitCodeKey = "exit-code"
)

// cargoRun is the cargo run of a crate's binary, with the program's own
// arguments after "--".
func cargoRun(component string, args []string) []string {
	return append([]string{"run", "--package", component, "--locked", "--"}, args...)
}

// RunJob builds a binary crate for the host with cargo run, runs it once with
// the args parameter and PORT set from the port parameter, and answers its
// exit code. It takes no stack-root lock: cargo locks its build directory.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if root.Component == "" {
			emit.Info(ctx.Project.Path + " is a Cargo workspace with no crate: it has no program to run")
			return "OK", nil, nil
		}
		env := root.Env
		if port := ctx.Params.Int("port", 0); port > 0 {
			env = append(slices.Clip(env), "PORT="+strconv.Itoa(port))
		}
		code, _, err := stacks.RunForeground(root, root.Program("cargo"), env, nil, cargoRun(root.Component, strings.Fields(ctx.Params.String("args")))...)
		if err != nil {
			return failRun(emit, err)
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

// ServeJob runs a binary crate with cargo run and PORT set until the user stops
// it, and announces it ready once its port accepts connections. Port 0 selects
// a free port. It takes no stack-root lock, which a server would hold until it
// stops.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if root.Component == "" {
			emit.Info(ctx.Project.Path + " is a Cargo workspace with no crate: it has no program to serve")
			return "OK", nil, nil
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				return failRun(emit, err)
			}
		}
		emit.Info("serving " + root.Component + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		code, stopped, err := stacks.RunForeground(root, root.Program("cargo"), env, emit, cargoRun(root.Component, strings.Fields(ctx.Params.String("args")))...)
		if err != nil {
			return failRun(emit, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

func failRun(emit *jsonl.Emitter, err error) (string, map[string]any, error) {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
	return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
}
