package swift

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// Process job names.
const (
	ServeJobName = "serve"
	RunJobName   = "run"
)

// Diagnostic codes of serve and run.
const (
	// CodeNoExecutable is the diagnostic of a component with no executable
	// product to start.
	CodeNoExecutable = "swift.no_executable"
	// CodeRunFailed is the diagnostic of a process the job could not start.
	CodeRunFailed = "swift.run_failed"
)

const (
	defaultServePort = 3000
	// exitCodeKey is the result member that carries a workload's exit code.
	exitCodeKey = "exit-code"
)

// ServeJob runs the executable product of a package with swift run, PORT set,
// until the user stops it, and announces it ready once the port accepts a
// connection. Port 0 selects a free port.
func ServeJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		port := cli.FlagInt(cli.ParseFlags(args), "port", ctx.Params.Int("port", defaultServePort))
		root, product, err := executableOf(stack, ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		if port == 0 {
			if port, err = stacks.FreePort(); err != nil {
				return "FAILED", nil, fail(emit, CodeRunFailed, err)
			}
		}
		emit.Info("serving " + product + " of " + root.Project + " on http://" + net.JoinHostPort("localhost", strconv.Itoa(port)))
		env := append(slices.Clip(root.Env), "PORT="+strconv.Itoa(port))
		serving, done := context.WithCancel(context.Background())
		defer done()
		go stacks.AnnounceReady(serving, emit, port)
		argv := []string{"run", "--force-resolved-versions", product}
		if extra := strings.Fields(ctx.Params.String("args")); len(extra) > 0 {
			argv = append(append(argv, "--"), extra...)
		}
		code, stopped, err := stacks.RunForeground(root, root.Program("swift"), env, emit, argv...)
		if err != nil {
			return "FAILED", nil, fail(emit, CodeRunFailed, err)
		}
		if code != 0 && !stopped {
			return "FAILED", map[string]any{exitCodeKey: code}, nil
		}
		return "OK", nil, nil
	}
}

// RunJob runs the executable product once with the args parameter and
// answers its exit code.
func RunJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		root, product, err := executableOf(stack, ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		argv := []string{"run", "--force-resolved-versions", product}
		if extra := strings.Fields(ctx.Params.String("args")); len(extra) > 0 {
			argv = append(append(argv, "--"), extra...)
		}
		code, _, err := stacks.RunForeground(root, root.Program("swift"), root.Env, nil, argv...)
		if err != nil {
			return "FAILED", nil, fail(emit, CodeRunFailed, err)
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

// executableOf selects the job's component and the executable product it
// runs: the entrypoint parameter, else the only executable of the package,
// else the one named after the package.
func executableOf(stack stacks.Stack, ctx *pctx.Context, emit *jsonl.Emitter) (stacks.RootStep, string, error) {
	root, err := stack.Selected(ctx, emit)
	if err != nil {
		return stacks.RootStep{}, "", err
	}
	product, err := executableAt(root, ctx, emit)
	return root, product, err
}

// executableAt answers the executable product of the selected component, as
// executableOf chooses it.
func executableAt(root stacks.RootStep, ctx *pctx.Context, emit *jsonl.Emitter) (string, error) {
	if !isPackage(root.Project) {
		return "", fail(emit, CodeNoExecutable, fmt.Errorf("%s is an Xcode project: serve and run start a package's executable product", root.Project))
	}
	if named := ctx.Params.String("entrypoint"); named != "" {
		return named, nil
	}
	manifest, _ := os.ReadFile(filepath.Join(root.Project, "Package.swift")) //nolint:gosec // the package's manifest
	executables := ExecutableProducts(manifest)
	switch {
	case len(executables) == 1:
		return executables[0], nil
	case len(executables) == 0:
		return "", fail(emit, CodeNoExecutable, fmt.Errorf("%s declares no executable product or target", root.Project))
	}
	if slices.Contains(executables, ctx.Project.Name) {
		return ctx.Project.Name, nil
	}
	return "", fail(emit, CodeNoExecutable, fmt.Errorf("%s declares several executables (%s): name one with --entrypoint", root.Project, strings.Join(executables, ", ")))
}

var (
	executableProduct = regexp.MustCompile(`\.executable\s*\(\s*name\s*:\s*"([^"]+)"`)
	executableTarget  = regexp.MustCompile(`\.executableTarget\s*\(\s*name\s*:\s*"([^"]+)"`)
	lineComment       = regexp.MustCompile(`//[^\n]*`)
	blockComment      = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// ExecutableProducts reads the executable products a Package.swift declares,
// sorted; with none, the executable targets SwiftPM makes a product of. Its
// comments are left out.
func ExecutableProducts(manifest []byte) []string {
	source := lineComment.ReplaceAllString(blockComment.ReplaceAllString(string(manifest), " "), "")
	for _, re := range []*regexp.Regexp{executableProduct, executableTarget} {
		var out []string
		for _, m := range re.FindAllStringSubmatch(source, -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
		if len(out) > 0 {
			slices.Sort(out)
			return out
		}
	}
	return nil
}
