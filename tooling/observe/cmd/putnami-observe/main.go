// Command putnami-observe is the runtime of the @putnami/observe extension.
//
// Usage:
//
//	putnami-observe __putnami runtime-info   (the runtime handshake)
//	putnami-observe run [flags]              (putnami observe)
//
// Putnami launches it as `putnami observe`, an interactive command that runs
// in the caller's directory, inside a workspace or not. It exits 0 whatever it
// observes, 2 on a usage error and 1 when it cannot write its output.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"go.putnami.dev/protocol/job"
	sdkcli "go.putnami.dev/sdk/extension/cli"
	sdkcontext "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/runtimeinfo"
	"go.putnami.dev/tooling/observe/internal/observe"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/observe"

// runArg routes the executable to the observe command.
const runArg = "run"

// contextFlag carries the job context file the CLI appends to the arguments.
const contextFlag = "--putnamiContext"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// knownFlags are the flags `putnami observe` takes.
var knownFlags = []string{"base", "head", "junit", "coverage", "fetch"}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-observe: %v\n", err)
		return 1
	}
	if handled {
		return 0
	}
	contextFile, args := splitContext(args)
	if len(args) == 0 || args[0] != runArg {
		fmt.Fprintln(stderr, "putnami-observe: unknown invocation; this runtime answers the Putnami runtime handshake and `run`")
		return 2
	}
	opts, jsonOutput, err := parseRun(args[1:], contextFile)
	if err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 2
	}
	opts.Env = getenv
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := observe.Run(ctx, opts)
	write := observe.WriteText
	if jsonOutput {
		write = observe.WriteJSON
	}
	if err := write(stdout, o); err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 1
	}
	return 0
}

// parseRun reads the flags of `run` and the job context.
func parseRun(args []string, contextFile string) (observe.Options, bool, error) {
	var launch *job.Context
	if contextFile != "" {
		parsed, err := job.ParseFile(contextFile)
		if err != nil {
			return observe.Options{}, false, fmt.Errorf("read the job context: %w", err)
		}
		launch = parsed
	}
	for _, arg := range args {
		if len(arg) == 0 || arg[0] != '-' {
			continue
		}
		name := flagName(arg)
		if !slices.Contains(knownFlags, name) {
			return observe.Options{}, false, fmt.Errorf("unknown flag %s", arg)
		}
	}
	if positional := positionals(args); len(positional) > 0 {
		return observe.Options{}, false, fmt.Errorf("takes no argument, got %q", positional[0])
	}
	flags := sdkcli.ParseFlags(args)
	fetch := true
	switch value, ok := flags["fetch"]; {
	case !ok, value == "true":
	case value == "false":
		fetch = false
	default:
		return observe.Options{}, false, fmt.Errorf("--fetch takes no value, got %q", value)
	}
	for _, name := range []string{"base", "head", "junit", "coverage"} {
		if value, ok := flags[name]; ok && (value == "" || value == "true") {
			return observe.Options{}, false, fmt.Errorf("--%s needs a value", name)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return observe.Options{}, false, err
	}
	opts := observe.Options{
		Dir:      dir,
		JUnit:    reports.SplitList(flags["junit"]),
		Coverage: reports.SplitList(flags["coverage"]),
		Fetch:    fetch,
		Producer: observe.Producer{Name: extensionName, Version: runtimeVersion},
		Resolve:  func(p string) string { return absolute(dir, sdkcontext.ResolvePath(launch, p)) },
	}
	opts.Overrides.Base = flags["base"]
	opts.Overrides.Head = flags["head"]
	jsonOutput := false
	if launch != nil {
		switch launch.Params.String("output") {
		case "json", "jsonl", "cloud-logging":
			jsonOutput = true
		}
	}
	return opts, jsonOutput, nil
}

// flagName returns the name of a flag token, without dashes, value or the
// no- negation.
func flagName(arg string) string {
	name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	if negated, ok := strings.CutPrefix(name, "no-"); ok {
		return negated
	}
	return name
}

// positionals returns the arguments that are neither a flag nor a flag's
// value, by the rule sdkcli.ParseFlags reads values with.
func positionals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") {
			continue
		}
		if i+1 < len(args) && (!strings.HasPrefix(args[i+1], "-") || strings.ContainsFunc(args[i+1], unicode.IsSpace)) {
			i++
		}
	}
	return out
}

// splitContext removes the --putnamiContext <file> pairs and returns the last
// file.
func splitContext(args []string) (string, []string) {
	file := ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == contextFlag && i+1 < len(args) {
			file = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return file, out
}

func absolute(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
