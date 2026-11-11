package dart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
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

// Diagnostic codes of the run and serve jobs.
const (
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "dart.run_failed"
	// CodeNotRunnable is the diagnostic of a package the job cannot run.
	CodeNotRunnable = "dart.not_runnable"
)

const (
	defaultServePort = 3000
	// serverEntrypoint is the entrypoint serve runs when the package has it;
	// otherwise dart run picks the package's executable.
	serverEntrypoint = "bin/server.dart"
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
)

// RunJob runs a Dart command-line package once, with dart run, its entrypoint
// and args, and answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		if variant(ctx) == VariantFlutter {
			return notRunnable(emit, fmt.Sprintf("%s is a Flutter package: run it with flutter run on a device", ctx.Project.Path))
		}
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		args := []string{"run"}
		if entrypoint := ctx.Params.String("entrypoint"); entrypoint != "" {
			args = append(args, entrypoint)
		}
		args = append(args, strings.Fields(ctx.Params.String("args"))...)
		code, _, err := stacks.RunForeground(root, dartProgram(ctx, root), root.Env, nil, args...)
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

// ServeJob serves a package until the user stops it, and announces it ready
// once its port accepts connections: a Dart server with dart run and PORT set,
// a Flutter app with web/ through flutter run -d web-server. Port 0 selects a
// free port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		flutter := variant(ctx) == VariantFlutter
		if flutter {
			if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "web")); err != nil {
				return notRunnable(emit, fmt.Sprintf("%s is a Flutter package with no web/: serve runs a Flutter app on the web server device", ctx.Project.Path))
			}
		}
		root, err := stack.Selected(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				return failRun(emit, err)
			}
		}
		program, serveArgs := dartProgram(ctx, root), []string{"run"}
		if flutter {
			program = root.Program("flutter")
			serveArgs = []string{"run", "-d", "web-server", "--web-port", strconv.Itoa(port), "--web-hostname", "0.0.0.0"}
		} else if entrypoint := ctx.Params.String("entrypoint"); entrypoint != "" {
			serveArgs = append(serveArgs, entrypoint)
		} else if _, err := os.Stat(filepath.Join(root.Project, filepath.FromSlash(serverEntrypoint))); err == nil {
			serveArgs = append(serveArgs, serverEntrypoint)
		}
		if !flutter {
			serveArgs = append(serveArgs, strings.Fields(ctx.Params.String("args"))...)
		}
		emit.Info("serving " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		code, stopped, err := stacks.RunForeground(root, program, env, emit, serveArgs...)
		if err != nil {
			return failRun(emit, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// dartProgram is the dart a package runs: the one beside the selected flutter
// for a package that resolves with Flutter, whatever comes first on PATH.
func dartProgram(ctx *pctx.Context, root stacks.RootStep) string {
	if v := variant(ctx); v == VariantFlutter || v == VariantDartInFlutter {
		// The selected flutter is first on the PATH of the root's env.
		if dart, ok := taskDart(stacks.Metadata{Variant: v}, root.Env); ok {
			return dart
		}
	}
	return root.Program("dart")
}

// variant is the profile variant the probe recorded for the job's project.
func variant(ctx *pctx.Context) string {
	var meta stacks.Metadata
	_ = json.Unmarshal(ctx.Project.Metadata[ExtensionName], &meta)
	return meta.Variant
}

func notRunnable(emit *jsonl.Emitter, message string) (string, map[string]any, error) {
	emit.DiagnosticWithCode("error", message, "", 0, 0, CodeNotRunnable)
	return "FAILED", nil, errors.New(CodeNotRunnable)
}

func failRun(emit *jsonl.Emitter, err error) (string, map[string]any, error) {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRunFailed)
	return "FAILED", nil, fmt.Errorf("%s: %w", CodeRunFailed, err)
}
