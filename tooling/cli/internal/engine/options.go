package engine

import (
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/env"
)

// GlobalFlags are the run-shaping flags the engine reads. The CLI shell parses
// them (internal/cli aliases this type as its GlobalFlags, so there is exactly
// one declaration) and hands them over on Request.Global; adapters without a
// command line fill the fields they need directly.
type GlobalFlags struct {
	// Where selects execution placement. Empty means local. It is execution
	// policy only: it never reaches task params, cache keys, or run markers.
	Where   string
	Verbose bool
	Debug   bool
	Quiet   bool
	NoCache bool
	// NoCacheExplicit records that the USER typed --no-cache. NoCache alone
	// cannot say so: the lifecycle adapter and non-serve watch iterations set it
	// internally to run the host uncached. Only an explicit request is delivered
	// to extensions as `no-cache`/`cache: false`.
	NoCacheExplicit bool
	// NoCacheProjects is --no-cache-projects: the raw project selector whose
	// tasks refuse cache reuse while every other project in the same run keeps
	// it. It is the SCOPED form of NoCache, for a run that must re-derive one
	// project's outputs from source without paying to rebuild its dependency
	// closure as well.
	//
	// It is execution policy and never a cache-key input, exactly like
	// --max-parallel: the selection stage resolves it to project ids the
	// scheduler consults when it decides whether to look a task up, and nothing
	// derived from it reaches a key, a run marker, or task params. NoCache wins
	// when both are given, because it is the broader statement of the same
	// intent.
	NoCacheProjects string
	// RetryFailed is --retry-failed: re-execute tasks whose last failure at the
	// same cache key is recorded locally, instead of replaying it. It is
	// execution policy and never a cache-key input, exactly like --max-parallel.
	RetryFailed     bool
	CacheTrust      string
	MaxParallel     int
	MaxParallelMode string
	// ResourceBudgets is --resource <name>=<units>, repeated: how many units of
	// each named external resource this run has. Tasks that declare a claim on a
	// budgeted resource are admitted against it beside the worker count; a name
	// absent here is unlimited.
	ResourceBudgets map[string]int
	// CPUBudgetPolicy is --cpu-policy: how each task's exported CPU ceiling is
	// derived (critical-path | measured). Empty means the default.
	CPUBudgetPolicy string
	Plan            bool
	Retry           int
	ContinueOnErr   bool
	Watch           bool
	DryRun          bool
	Output          string // "text", "json", "jsonl", "cloud-logging", "" (auto)
	JSON            bool   // --json shorthand for --output=json (resolved into Output)
	ImpactedStrict  bool
	Baseline        string
	TraceProfile    string // --trace-profile: path for Chrome trace output (empty = disabled)
	EnvProfile      string // --profile: resolved deployment profile (dev|test|production)
	// Providers is --providers, else PUTNAMI_PROVIDERS: the sorted, unique
	// invocation providers (runner.InvocationProviders) whose credentials the
	// workspace's credential provider serves — install for the read
	// credential, publish for the publish credential. Empty consults no
	// provider. It is execution policy and never a cache-key input.
	Providers []string

	// Project selection
	Projects string // raw --projects value or positional "."
	All      bool
	Impacted bool
	// AutoSelected is set internally when a bare job command is resolved through
	// smart default project selection.
	AutoSelected bool
	FilterTag    string
	ExcludeTag   string
	Exclude      string

	// Output control
	Color *bool // nil=auto, true=force, false=disable

	// Version/help
	Version bool
	Help    bool
	HelpMan bool // --man flag for man page output
	HelpMD  bool // --markdown flag for markdown output
}

// ApplyEnvOverrides layers PUTNAMI_* environment variables and workspace config
// onto the parsed flags. The engine applies it once per run, after the workspace
// loads; the CLI shell also calls it for the paths that resolve flags before the
// engine takes over (bootstrap gating, extension command groups).
func ApplyEnvOverrides(g *GlobalFlags, cfg *wsproto.Config) {
	if v := env.String("OUTPUT"); v != "" && g.Output == "" {
		g.Output = v
	}
	if v, ok := env.Bool("DEBUG"); ok && !g.Debug {
		g.Debug = v
		if v {
			g.Verbose = true
		}
	}
	if v, ok := env.Bool("VERBOSE"); ok && !g.Verbose {
		g.Verbose = v
	}
	if v, ok := env.Bool("QUIET"); ok && !g.Quiet {
		g.Quiet = v
	}

	// Resolve color: CLI flag > PUTNAMI_COLOR > NO_COLOR/PUTNAMI_NO_COLOR > auto
	if g.Color == nil {
		if vb, ok := env.Bool("COLOR"); ok {
			g.Color = boolPtr(vb)
		}
	}
	if g.Color == nil {
		if v := os.Getenv("NO_COLOR"); v != "" {
			g.Color = boolPtr(false)
		} else if v, ok := env.Bool("NO_COLOR"); ok && v {
			g.Color = boolPtr(false)
		}
	}

	// Apply config-level output and verbose/quiet
	if cfg == nil {
		return
	}
	if cfg.Output != "" && g.Output == "" {
		g.Output = cfg.Output
	}
	if cfg.Verbose != nil && *cfg.Verbose && !g.Verbose {
		g.Verbose = true
	}
	if cfg.Quiet != nil && *cfg.Quiet && !g.Quiet {
		g.Quiet = true
	}
}

func boolPtr(v bool) *bool { return &v }
