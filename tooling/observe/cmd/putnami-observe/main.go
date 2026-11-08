// Command putnami-observe is the runtime of the @putnami/observe extension.
//
// Usage:
//
//	putnami-observe __putnami runtime-info   (the runtime handshake)
//	putnami-observe run [flags]              (putnami observe)
//
// Putnami launches it as `putnami observe`, an interactive command that runs
// in the caller's directory, inside a workspace or not. It exits 0 whatever it
// observes, 2 on a usage error, and 1 when it cannot read its job context or
// write its output.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/job"
	sdkcli "go.putnami.dev/sdk/extension/cli"
	sdkcontext "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/runtimeinfo"
	"go.putnami.dev/tooling/observe/internal/observe"
	"go.putnami.dev/tooling/observe/internal/publish"
	"go.putnami.dev/tooling/observe/internal/reports"
	"go.putnami.dev/tooling/observe/internal/upload"
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
var knownFlags = []string{"base", "head", "junit", "coverage", "sbom", "base-sbom", "architecture", "archunit-classes", "moon-graph", "fetch", "evidence", "upload", "publish", "pants-graph", "bazel-graph", "decisions", "details", "open"}

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
	var launch *job.Context
	if contextFile != "" {
		parsed, err := job.ParseFile(contextFile)
		if err != nil {
			fmt.Fprintf(stderr, "putnami observe: read the job context: %v\n", err)
			return 1
		}
		launch = parsed
	}
	if len(args) == 0 || args[0] != runArg {
		fmt.Fprintln(stderr, "putnami-observe: unknown invocation; this runtime answers the Putnami runtime handshake and `run`")
		return 2
	}
	opts, out, err := parseRun(args[1:], launch)
	if err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 2
	}
	opts.Env = getenv
	if out.upload {
		// The code host lookup is a request off the machine: only a run that
		// uploads makes it.
		opts.ProjectID = func(ctx context.Context, provider, host, path string) (string, error) {
			return publish.ProjectID(ctx, publishClient, getenv, provider, host, path)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := observe.Run(ctx, opts)
	uploaded, reportURL, diags := writeAndUpload(ctx, o, out, getenv, stderr)
	o.Diagnostics = append(o.Diagnostics, diags...)
	if out.open && reportURL != "" {
		if _, inCI := evidence.DetectCISystem(getenv); !inCI {
			if err := openBrowser(reportURL); err != nil {
				fmt.Fprintf(stderr, "putnami observe: the browser did not open (%v); open %s\n", err, reportURL)
			}
		}
	}
	if out.publish {
		o.Diagnostics = append(o.Diagnostics, publish.Run(ctx, publish.Request{
			Observation: o, Upload: uploaded, ReportURL: reportURL, Getenv: getenv, Client: publishClient, Output: stderr,
		})...)
	}
	if out.json {
		if err := observe.WriteJSON(stdout, o); err != nil {
			fmt.Fprintf(stderr, "putnami observe: %v\n", err)
			return 1
		}
		return 0
	}
	write := observe.WriteText
	if out.details {
		write = observe.WriteDetails
	}
	err = write(stdout, o)
	if err == nil {
		_, err = fmt.Fprintln(stdout, upload.Next(upload.Step{Getenv: getenv, Upload: uploaded, ReportURL: reportURL}))
	}
	if err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 1
	}
	return 0
}

// outputs are what a run writes besides its summary.
type outputs struct {
	// evidence is the absolute path of the evidence document, "" for none.
	evidence string
	// upload hands the evidence document to the evidence reporter.
	upload bool
	// json prints the observation as one JSON document.
	json bool
	// publish posts a commit status and a summary note on the pull or merge
	// request.
	publish bool
	// details prints the detailed summary instead of the short one.
	details bool
	// open uploads with sign-in and opens the report outside a CI.
	open bool
}

// openBrowser opens the report URL outside a CI. Tests replace it.
var openBrowser = upload.OpenBrowser

// publishClient sends the requests of --publish; nil is the default client.
// Tests replace it.
var publishClient *http.Client

// writeAndUpload writes the evidence document when --evidence or --upload asks
// for it, hands it to the evidence reporter on --upload, and returns what the
// upload did, the report URL the reporter answered and the diagnostics of
// both. Without --evidence the document lives in a temporary file for the
// upload alone.
func writeAndUpload(ctx context.Context, o observe.Observation, out outputs, getenv func(string) string, stderr io.Writer) (publish.Upload, string, []diag.Diagnostic) {
	path := out.evidence
	if path == "" && out.upload {
		dir, err := os.MkdirTemp("", "putnami-evidence-")
		if err != nil {
			return publish.UploadFailed, "", upload.Skipped()
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "evidence.json")
	}
	if path == "" {
		return publish.NotUploaded, "", nil
	}
	diags := observe.WriteEvidence(path, o, time.Now())
	if !out.upload {
		return publish.NotUploaded, "", diags
	}
	if len(diags) > 0 {
		return publish.UploadFailed, "", append(diags, upload.Skipped()...)
	}
	_, inCI := evidence.DetectCISystem(getenv)
	reportURL, diags := upload.Run(ctx, upload.Request{Path: path, Getenv: getenv, Output: stderr, SignIn: out.open && !inCI})
	if len(diags) > 0 {
		return publish.UploadFailed, "", diags
	}
	return publish.Uploaded, reportURL, nil
}

// parseRun reads the flags of `run` against the job context, nil outside
// Putnami, and returns the options and what the run writes.
func parseRun(args []string, launch *job.Context) (observe.Options, outputs, error) {
	for _, arg := range args {
		if len(arg) == 0 || arg[0] != '-' {
			continue
		}
		name := flagName(arg)
		if !slices.Contains(knownFlags, name) {
			return observe.Options{}, outputs{}, fmt.Errorf("unknown flag %s", arg)
		}
	}
	if positional := positionals(args); len(positional) > 0 {
		return observe.Options{}, outputs{}, fmt.Errorf("takes no argument, got %q", positional[0])
	}
	flags := sdkcli.ParseFlags(args)
	fetch, err := boolFlag(flags, "fetch", true)
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	uploadFlag, err := boolFlag(flags, "upload", false)
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	publishFlag, err := boolFlag(flags, "publish", false)
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	details, err := boolFlag(flags, "details", false)
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	openFlag, err := boolFlag(flags, "open", false)
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	for _, name := range []string{"base", "head", "junit", "coverage", "sbom", "base-sbom", "architecture", "archunit-classes", "evidence", "moon-graph", "pants-graph", "bazel-graph", "decisions"} {
		if value, ok := flags[name]; ok && (value == "" || value == "true") {
			return observe.Options{}, outputs{}, fmt.Errorf("--%s needs a value", name)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return observe.Options{}, outputs{}, err
	}
	opts := observe.Options{
		Dir:             dir,
		JUnit:           reports.SplitList(flags["junit"]),
		Coverage:        reports.SplitList(flags["coverage"]),
		SBOM:            reports.SplitList(flags["sbom"]),
		BaseSBOM:        reports.SplitList(flags["base-sbom"]),
		Architecture:    reports.SplitList(flags["architecture"]),
		ArchUnitClasses: reports.SplitList(flags["archunit-classes"]),
		Decisions:       reports.SplitList(flags["decisions"]),
		Fetch:           fetch,
		Producer:        observe.Producer{Name: extensionName, Version: runtimeVersion},
		Resolve:         func(p string) string { return absolute(dir, sdkcontext.ResolvePath(launch, p)) },
	}
	if flags["moon-graph"] != "" {
		opts.MoonGraph = opts.Resolve(flags["moon-graph"])
	}
	if flags["pants-graph"] != "" {
		opts.PantsGraph = opts.Resolve(flags["pants-graph"])
	}
	if flags["bazel-graph"] != "" {
		opts.BazelGraph = opts.Resolve(flags["bazel-graph"])
	}
	opts.Overrides.Base = flags["base"]
	opts.Overrides.Head = flags["head"]
	out := outputs{upload: uploadFlag || openFlag, publish: publishFlag, details: details, open: openFlag}
	if launch != nil {
		switch launch.Params.String("output") {
		case "json", "jsonl", "cloud-logging":
			out.json = true
		}
	}
	if flags["evidence"] != "" {
		out.evidence = opts.Resolve(flags["evidence"])
	}
	return opts, out, nil
}

// boolFlag reads a boolean flag: absent is fallback, --name is true and
// --no-name is false.
func boolFlag(flags map[string]string, name string, fallback bool) (bool, error) {
	switch value, ok := flags[name]; {
	case !ok:
		return fallback, nil
	case value == "true":
		return true, nil
	case value == "false":
		return false, nil
	default:
		return false, fmt.Errorf("--%s takes no value, got %q", name, value)
	}
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
