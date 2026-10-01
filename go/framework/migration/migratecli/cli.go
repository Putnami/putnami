// Package migratecli is the kind-agnostic migrate command-line entry
// point. A service binary's cmd/migrate/main.go calls Run with the same
// AppBuilder its production cmd/server uses — same plugin chain, same
// DI graph — and the CLI dispatches per-subcommand against the per-app
// *migration.Registry. No HTTP listener starts, no Invoke functions
// run, and no per-feature wiring lives inside cmd/migrate.
//
// Subcommand surface:
//
//	up                    Apply all pending migrations across kinds.
//	up to <name>          Apply forward through <name> (inclusive).
//	down                  Roll back the most recently applied migration per kind.
//	down to <name>        Roll back every migration applied after <name>.
//	status                Tabular per-row state of every kind.
//	verify                Drift report: hash drift, missing rows, pending.
//	inspect               JSON dump of the registry view (runs no migrations; still runs Prepare).
//
// Every subcommand — inspect included — first calls app.Prepare, which runs
// the plugin Configure phase; a database plugin may open its connection pool
// there. inspect drives no migration of its own, but it is not guaranteed to
// run without DB access.
//
// Exit codes:
//
//	0  success
//	1  user error (unknown subcommand, missing argument)
//	2  operational error (DB unreachable, runner error, prepare failed)
//	3  drift detected by verify
package migratecli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"go.putnami.dev/app"
	"go.putnami.dev/migration"
)

// AppBuilder constructs the production *app.Application. The CLI calls
// it but never invokes Start/ListenAndServe — only Prepare runs, so
// HTTP/event/background plugins do not start.
type AppBuilder func() *app.Application

// Run is the entry point a service's cmd/migrate/main.go calls. It
// parses os.Args[1:] and dispatches against the registry resolved from
// build(). Returns the exit code; callers wrap with os.Exit.
func Run(build AppBuilder) int {
	return RunWith(build, os.Args[1:], os.Stdout, os.Stderr)
}

// RunWith is the testable form — caller controls argv and the output
// streams. The CLI dispatches each subcommand against the per-app
// *migration.Registry after Prepare; build() and Prepare are invoked
// for every subcommand (including inspect, where it's effectively a
// load-time validation that the plugin chain is well-formed).
func RunWith(build AppBuilder, args []string, stdout, stderr io.Writer) int {
	if build == nil {
		wln(stderr, "migrate: AppBuilder is required")
		return 1
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 1
	}

	sub, rest := args[0], args[1:]

	// --output=json|jsonl is shared by the record-emitting subcommands
	// (up/down/status). Parse and strip it before dispatch so the
	// positional parser (parseToArg) never sees it as a stray argument.
	format, rest, code := parseOutputFlag(rest, stderr)
	if code != 0 {
		return code
	}

	a := build()
	if a == nil {
		wln(stderr, "migrate: AppBuilder returned nil application")
		return 1
	}

	// Root context is canceled on Ctrl-C/SIGTERM so an in-flight up/down can
	// observe cancellation through ctx instead of the process being killed
	// outright. Migrations run without a wall-clock deadline (they can be
	// long), so the only bound is the operator's signal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Drain on a fresh context: ctx may already be canceled by a signal. Prepare
	// currently does not mark the application as running, so direct container
	// close is the active cleanup path. Stop is a future guard in case Prepare ever
	// marks the application as running. Register cleanup before Prepare so failures
	// after container creation also release eager DI resources.
	defer func() {
		_ = a.Stop(context.Background()) //nolint:errcheck // one-shot CLI cleanup; the command result takes precedence
		if cc := a.Context(); cc != nil {
			_ = cc.Close() //nolint:errcheck // one-shot CLI cleanup; the command result takes precedence
		}
	}()
	if err := a.Prepare(ctx); err != nil {
		wf(stderr, "migrate: prepare failed: %v\n", err)
		return 2
	}

	reg := a.MigrationRegistry()
	if reg == nil {
		wln(stderr, "migrate: no migration registry available")
		return 2
	}

	switch sub {
	case "up":
		return runUp(ctx, reg, rest, format, stdout, stderr)
	case "down":
		return runDown(ctx, reg, rest, format, stdout, stderr)
	case "status":
		if code := rejectExtraArgs(sub, rest, stderr); code != 0 {
			return code
		}
		return runStatus(ctx, reg, format, stdout, stderr)
	case "verify":
		if code := rejectExtraArgs(sub, rest, stderr); code != 0 {
			return code
		}
		return runVerify(ctx, reg, stdout, stderr)
	case "inspect":
		if code := rejectExtraArgs(sub, rest, stderr); code != 0 {
			return code
		}
		return runInspect(reg, stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		wf(stderr, "migrate: unknown subcommand %q\n", sub)
		printUsage(stderr)
		return 1
	}
}

// CLI output writes to stdout/stderr; failures are not actionable in a
// one-shot command, and the writer is never an HTTP response — so
// gosec's XSS taint warning and errcheck's "ignored error" warning both
// suppress here.
func wf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck,gosec // CLI output
}

func wln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...) //nolint:errcheck,gosec // CLI output
}

func ws(w io.Writer, s string) {
	_, _ = fmt.Fprint(w, s) //nolint:errcheck,gosec // CLI output
}

// --- subcommands -----------------------------------------------------------

func runUp(ctx context.Context, reg *migration.Registry, args []string, format outputFormat, stdout, stderr io.Writer) int {
	target, code := parseToArg("up", args, stderr)
	if code != 0 {
		return code
	}
	records, err := reg.ApplyAll(ctx, migration.ApplyOpts{To: target, Force: true})
	if err != nil {
		wf(stderr, "migrate up: %v\n", err)
		writeRecords(stdout, records, format)
		return 2
	}
	if len(records) == 0 {
		emitEmpty(stdout, format, "no pending migrations")
		return 0
	}
	writeRecords(stdout, records, format)
	return 0
}

func runDown(ctx context.Context, reg *migration.Registry, args []string, format outputFormat, stdout, stderr io.Writer) int {
	target, code := parseToArg("down", args, stderr)
	if code != 0 {
		return code
	}
	records, err := reg.RollbackAll(ctx, migration.RollbackOpts{To: target})
	if err != nil {
		wf(stderr, "migrate down: %v\n", err)
		writeRecords(stdout, records, format)
		return 2
	}
	if len(records) == 0 {
		emitEmpty(stdout, format, "nothing to roll back")
		return 0
	}
	writeRecords(stdout, records, format)
	return 0
}

func runStatus(ctx context.Context, reg *migration.Registry, format outputFormat, stdout, stderr io.Writer) int {
	records, err := reg.StatusAll(ctx)
	if err != nil {
		wf(stderr, "migrate status: %v\n", err)
		return 2
	}
	if len(records) == 0 {
		emitEmpty(stdout, format, "no migrations registered")
		return 0
	}
	writeRecords(stdout, records, format)
	return 0
}

func runVerify(ctx context.Context, reg *migration.Registry, stdout, stderr io.Writer) int {
	reports, err := reg.VerifyAll(ctx)
	if err != nil {
		wf(stderr, "migrate verify: %v\n", err)
		return 2
	}
	exit := 0
	for _, r := range reports {
		if r.Empty() {
			wf(stdout, "[%s] no drift\n", r.Kind)
			continue
		}
		exit = 3
		wf(stdout, "[%s] DRIFT DETECTED\n", r.Kind)
		for _, h := range r.HashDrifts {
			wf(stdout, "  hash-drift: %s/%s target=%s stored=%s current=%s\n",
				h.Namespace, h.Name, h.Target, short(h.StoredHash), short(h.CurrentHash))
		}
		for _, rec := range r.MissingFromRegistry {
			wf(stdout, "  missing-from-registry: %s target=%s (applied row not present in registered migrations)\n",
				rec.Name, rec.Target)
		}
		for _, rec := range r.MissingFromStore {
			wf(stdout, "  pending: %s target=%s source=%s\n",
				rec.Name, rec.Target, rec.Source)
		}
	}
	return exit
}

func runInspect(reg *migration.Registry, stdout, stderr io.Writer) int {
	view := inspectView{Kinds: []inspectKind{}}
	for _, k := range reg.Kinds() {
		ik := inspectKind{Kind: string(k), Sources: []inspectSource{}}
		if _, ok := reg.Runner(k); ok {
			ik.RunnerRegistered = true
		}
		for _, src := range reg.Sources(k) {
			ik.Sources = append(ik.Sources, inspectSource{
				Namespace: src.Namespace(),
			})
		}
		view.Kinds = append(view.Kinds, ik)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(view); err != nil {
		wf(stderr, "migrate inspect: %v\n", err)
		return 2
	}
	return 0
}

// --- helpers ---------------------------------------------------------------

// outputFormat selects how record-emitting subcommands (up/down/status)
// render their result.
type outputFormat int

const (
	// outputTable is the human-oriented tabwriter table (TTY default).
	outputTable outputFormat = iota
	// outputJSON emits a single JSON array of migration.Record.
	outputJSON
	// outputJSONL emits one JSON-encoded migration.Record per line.
	outputJSONL
)

// parseOutputFlag extracts the shared --output flag from args and returns
// the resolved format plus args with the flag removed (so the positional
// parser never sees it). Accepted forms: --output=VALUE, --output VALUE,
// and the -output single-dash spelling. VALUE is one of table|json|jsonl.
// On an unknown value or a missing operand it writes a user error to
// stderr and returns exit code 1.
func parseOutputFlag(args []string, stderr io.Writer) (outputFormat, []string, int) {
	format := outputTable
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var value string
		switch {
		case arg == "--output" || arg == "-output":
			if i+1 >= len(args) {
				wf(stderr, "migrate: --output requires a value (table|json|jsonl)\n")
				return outputTable, nil, 1
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--output="):
			value = strings.TrimPrefix(arg, "--output=")
		case strings.HasPrefix(arg, "-output="):
			value = strings.TrimPrefix(arg, "-output=")
		default:
			rest = append(rest, arg)
			continue
		}
		switch strings.ToLower(value) {
		case "table", "":
			format = outputTable
		case "json":
			format = outputJSON
		case "jsonl":
			format = outputJSONL
		default:
			wf(stderr, "migrate: unknown --output value %q (want table|json|jsonl)\n", value)
			return outputTable, nil, 1
		}
	}
	return format, rest, 0
}

// parseToArg accepts:
//   - no args (returns "")
//   - ["to", "<name>"] (returns "<name>")
//   - ["<name>"] is NOT accepted; the "to" keyword is required so
//     subcommands stay self-documenting.
//
// rejectExtraArgs fails a subcommand that takes no positional arguments when
// leftover args are present, so a typo like `migrate status pending` errors
// instead of being silently ignored — matching up/down's arg rejection.
func rejectExtraArgs(sub string, args []string, stderr io.Writer) int {
	if len(args) > 0 {
		wf(stderr, "migrate %s: unexpected arguments %v (%s takes no positional arguments)\n", sub, args, sub)
		return 1
	}
	return 0
}

func parseToArg(sub string, args []string, stderr io.Writer) (string, int) {
	if len(args) == 0 {
		return "", 0
	}
	if len(args) == 2 && strings.ToLower(args[0]) == "to" {
		if args[1] == "" {
			wf(stderr, "migrate %s: 'to' requires a migration name\n", sub)
			return "", 1
		}
		return args[1], 0
	}
	wf(stderr, "migrate %s: unexpected arguments %v (use '%s' or '%s to <name>')\n",
		sub, args, sub, sub)
	return "", 1
}

func writeRecords(out io.Writer, records []migration.Record, format outputFormat) {
	switch format {
	case outputJSON:
		// A single JSON array of Records — stable to parse with one
		// json.Unmarshal in a CI/deploy step. Always emit (even []) so
		// consumers can rely on valid JSON.
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if records == nil {
			records = []migration.Record{}
		}
		_ = enc.Encode(records) //nolint:errcheck // CLI output; nothing actionable on encode failure
		return
	case outputJSONL:
		// One JSON object per line — streamable; a consumer can read
		// records as they arrive without buffering the whole array.
		enc := json.NewEncoder(out)
		for i := range records {
			_ = enc.Encode(records[i]) //nolint:errcheck // CLI output
		}
		return
	}
	if len(records) == 0 {
		return
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	wln(tw, "KIND\tNAMESPACE\tNAME\tSTATUS\tTARGET\tSOURCE")
	for _, r := range records {
		wf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Kind, r.Namespace, r.Name, r.Status, r.Target, r.Source)
	}
	_ = tw.Flush() //nolint:errcheck // CLI output; nothing actionable on flush failure
}

// emitEmpty renders the "nothing happened" outcome. For machine-readable
// formats this is an empty record set (empty JSON array / no JSONL lines)
// so a CI step always parses valid output; for the table default it keeps
// the human-friendly status line.
func emitEmpty(out io.Writer, format outputFormat, humanMsg string) {
	switch format {
	case outputJSON, outputJSONL:
		writeRecords(out, nil, format)
	default:
		wln(out, humanMsg)
	}
}

func short(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8] + "..."
}

func printUsage(out io.Writer) {
	ws(out, `migrate — Putnami migration driver

Usage:
  migrate <subcommand> [args]

Subcommands:
  up                    Apply all pending migrations across kinds
  up to <name>          Apply forward through <name> (inclusive)
  down                  Roll back the most recently applied migration per kind
  down to <name>        Roll back every migration applied after <name>
  status                Tabular per-row state of every kind
  verify                Drift report (exit 3 on drift)
  inspect               JSON dump of the registry view (runs no migrations; still runs Prepare)

Flags:
  --output=table|json|jsonl   Record output format for up/down/status
                              (default table; json = one array, jsonl = one object per line)
`)
}

// --- inspect JSON shape ----------------------------------------------------

type inspectView struct {
	Kinds []inspectKind `json:"kinds"`
}

type inspectKind struct {
	Kind             string          `json:"kind"`
	RunnerRegistered bool            `json:"runnerRegistered"`
	Sources          []inspectSource `json:"sources"`
}

type inspectSource struct {
	Namespace string `json:"namespace"`
}
