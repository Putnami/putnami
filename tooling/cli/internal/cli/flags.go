package cli

import (
	"strconv"
	"strings"
	"unicode"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The global-flag pass reads its VOCABULARY
// (which flags exist, which take a value, which single-dash alias each has) from
// the command catalog, and keeps only the BINDING — flag name to GlobalFlags
// field — as source here.
//
// Before this change the vocabulary lived in a single switch that answered both
// questions at once, which is how --cache-trust, --profile, --json, --color,
// --no-color, and --trace-profile came to be accepted by the parser and unknown
// to help and to all three completion generators. The catalog now answers
// "what is a flag"; applyGlobalFlag answers only "what does it set".
//
// applyGlobalFlag stays a switch on literal flag names on purpose: it is what
// catalog_source_test.go's TestGlobalFlags_CoverTheParser reads with go/ast to
// prove the catalog's global-flag table and the parser accept the same set.

// parseFlags extracts known global flags and returns remaining args, consuming
// --version as a request to print the CLI version.
func parseFlags(args []string) (GlobalFlags, []string, error) {
	return parseGlobalFlags(args, flagParseOptions{consumeVersion: true})
}

// parseCommandFlags is parseFlags after a command has been selected, where
// --version belongs to the command (`putnami upgrade --version 1.2.3`) rather
// than to the CLI.
func parseCommandFlags(args []string) (GlobalFlags, []string, error) {
	return parseGlobalFlags(args, flagParseOptions{consumeVersion: false})
}

// parseJobFlags is parseCommandFlags for a job command, whose leftover tokens
// become job params (buildCommandParams) and nothing else; see
// splitInlineValues for what that changes.
func parseJobFlags(args []string) (GlobalFlags, []string, error) {
	return parseGlobalFlags(args, flagParseOptions{consumeVersion: false, jobArgs: true})
}

type flagParseOptions struct {
	consumeVersion bool
	// jobArgs marks a job command's arguments (parseJobFlags).
	jobArgs bool
}

// reservedGlobalFlags are consumed alongside any command but deliberately kept
// out of the catalog's global-flag table, because they are cataloged as flags of
// the `help` command instead (`putnami help --man`): listing them as globals
// would offer them after every command. catalog_source_test.go's
// parserFlagsExemptFromCatalog records the same pair from the other side.
var reservedGlobalFlags = []commandmeta.GlobalFlag{
	{Long: "--man", Description: "Render the full reference as a man page"},
	{Long: "--markdown", Description: "Render the full reference as Markdown"},
}

// globalFlagSpec resolves a token to the global flag it spells, by long form or
// single-dash alias.
func globalFlagSpec(arg string) (commandmeta.GlobalFlag, bool) {
	for _, flag := range commandmeta.GlobalFlags() {
		if arg == flag.Long || (flag.Short != "" && arg == flag.Short) {
			return flag, true
		}
	}
	for _, flag := range reservedGlobalFlags {
		if arg == flag.Long {
			return flag, true
		}
	}
	return commandmeta.GlobalFlag{}, false
}

// isGlobalFlag reports whether a token is one of the CLI-wide flags. Used by the
// command-scoped validation pass, which must not re-reject a flag the global
// pass already consumed.
func isGlobalFlag(arg string) bool {
	_, ok := globalFlagSpec(arg)
	return ok
}

// splitInlineValues rewrites "--flag=value" into "--flag" "value" so both
// spellings reach one code path. It is applied to EVERY "--"-prefixed token,
// known or not, because the tokens it does not consume are what
// buildCommandParams turns into job params: splitting only known flags would
// move every cache key carrying an unknown "--flag=value".
//
// For a job command (jobArgs), a token before the passthrough separator stays
// whole when splitting would misread it (keepsInlineToken), and
// buildCommandParams binds its inline value itself. A built-in command reads its
// tokens by exact spelling, and an extension command group forwards them as its
// process argv, so both always get the split.
func splitInlineValues(args []string, jobArgs bool) []string {
	cut := passthroughCut(args)
	normalized := make([]string, 0, len(args))
	for i, arg := range args {
		idx := strings.Index(arg, "=")
		if idx <= 0 || !strings.HasPrefix(arg, "--") || (jobArgs && i < cut && keepsInlineToken(arg[:idx], arg[idx+1:])) {
			normalized = append(normalized, arg)
			continue
		}
		normalized = append(normalized, arg[:idx], arg[idx+1:])
	}
	return normalized
}

// keepsInlineToken reports whether a job command's "--spelling=value" token
// stays whole. A global flag's token is always split, so the global pass binds
// it. Any other token stays whole when the split would bind its value to no
// flag:
//
//   - its spelling contains whitespace, so the token is a multi-word value that
//     holds "=" (`--args "--watch --port=3000"`), not a flag;
//   - its value is one word that begins with a hyphen (`--args=--gate`,
//     `--port=-1`), which the split would turn into a flag of its own.
//
// A split that misreads the token never bound what the user spelled, so keeping
// it whole changes no params that were right. A multi-word value
// (`--args="--check --dry-run"`) is still split: takesNextAsValue binds it to
// the flag before it, exactly as the space form binds. Kept whole, it would be
// a multi-word token that a preceding switch takes as its value.
func keepsInlineToken(spelling, value string) bool {
	if _, global := globalFlagSpec(spelling); global {
		return false
	}
	if strings.ContainsFunc(spelling, unicode.IsSpace) {
		return true
	}
	return strings.HasPrefix(value, "-") && !strings.ContainsFunc(value, unicode.IsSpace)
}

// parseGlobalFlags is phase 2 of the parse: syntax-normalized tokens in, bound
// GlobalFlags plus the untouched remainder out. A value-taking flag with no
// value, an unparseable --retry, and an unrecognized --max-parallel mode are
// hard usage errors here; before this change all three were silently dropped.
// Unknown flags are NOT rejected here — only the command-scoped pass in
// parse.go knows whether an extension could own them.
func parseGlobalFlags(args []string, opts flagParseOptions) (GlobalFlags, []string, error) {
	g := GlobalFlags{}
	normalized := splitInlineValues(args, opts.jobArgs)
	remaining := make([]string, 0, len(normalized))

	for i := 0; i < len(normalized); i++ {
		arg := normalized[i]

		spec, known := globalFlagSpec(arg)
		if !known {
			remaining = append(remaining, arg)
			continue
		}
		if spec.Long == "--version" && !opts.consumeVersion {
			remaining = append(remaining, arg)
			continue
		}

		value := ""
		if spec.Type == commandmeta.FlagValue {
			if i+1 >= len(normalized) {
				return g, nil, usageErrorf("flag %s requires a value: %s %s", arg, spec.Long, spec.ValueName)
			}
			i++
			value = normalized[i]
		}
		if err := applyGlobalFlag(&g, arg, value); err != nil {
			return g, nil, err
		}
	}

	return g, remaining, nil
}

// applyGlobalFlag binds one recognized global flag onto g. It is a cohesive
// linear dispatch with one branch per flag; the high cyclomatic count reflects
// the breadth of the flag table, not nested logic, so splitting it would scatter
// the binding without reducing real complexity.
//
//nolint:gocyclo // breadth of the flag table, not nested complexity
func applyGlobalFlag(g *GlobalFlags, arg, value string) error {
	switch arg {
	case "--where":
		if value != "local" && value != "remote" {
			return usageErrorf("invalid --where value %q: expected local or remote", value)
		}
		g.Where = value
	case "--verbose", "-v":
		g.Verbose = true
	case "--debug", "-d":
		g.Debug = true
		g.Verbose = true
	case "--quiet":
		g.Quiet = true
	case "--no-cache":
		g.NoCache = true
		g.NoCacheExplicit = true
	case "--retry-failed":
		g.RetryFailed = true
	case "--plan":
		g.Plan = true
	case "--continue-on-error":
		g.ContinueOnErr = true
	case "--watch", "-w":
		g.Watch = true
	case "--dry-run":
		g.DryRun = true
	case "--color":
		g.Color = boolPtr(true)
	case "--no-color":
		g.Color = boolPtr(false)
	case "--man":
		g.HelpMan = true
		g.Help = true
	case "--markdown":
		g.HelpMD = true
		g.Help = true
	case "--all":
		g.All = true
		g.Projects = "*"
	case "--impacted":
		g.Impacted = true
		g.Projects = "[impacted]"
	case "--impacted-strict":
		g.ImpactedStrict = true
	case "--help", "-h":
		g.Help = true
	case "--version", "-V":
		g.Version = true
	case "--json":
		// Shorthand for --output=json; resolved (and conflict-checked
		// against --output) in App.Run via protocolcli.ResolveOutputMode.
		g.JSON = true

	case "--max-parallel":
		return applyMaxParallel(g, value)
	case "--resource":
		return applyResourceBudget(g, value)
	case "--providers":
		return applyProviders(g, value)
	case "--credential-fd":
		// runcredential.Capture read the descriptor before the parse; the flag
		// binds nothing.
		_, err := runcredential.Descriptor(value)
		return err
	case "--cpu-policy":
		// Validated in resolveCPUPolicy with the env and workspace sources, so
		// one message names whichever source actually supplied the value.
		g.CPUBudgetPolicy = value
	case "--retry":
		attempts, err := strconv.Atoi(value)
		if err != nil {
			return usageErrorf("invalid --retry value %q: expected a whole number of attempts", value)
		}
		g.Retry = attempts
	case "--cache-trust":
		g.CacheTrust = value
	case "--no-cache-projects":
		// Execution policy, like --max-parallel: the selection it names shapes
		// which tasks consult the cache, never what any task's key contains.
		g.NoCacheProjects = value
	case "--output":
		g.Output = value
	case "--projects":
		g.Projects = value
	case "--tag":
		g.FilterTag = value
	case "--exclude-tag":
		g.ExcludeTag = value
	case "--exclude":
		g.Exclude = value
	case "--baseline":
		g.Baseline = value
	case "--profile":
		g.EnvProfile = value
	case "--trace-profile":
		g.TraceProfile = value
	}
	return nil
}

// applyMaxParallel binds --max-parallel, which accepts either a worker count or
// one of the named modes. An unrecognized value used to fall through with no
// default and normalize to "auto" in internal/jobs/parallel.go, so a user who
// asked for something specific silently got auto.
func applyMaxParallel(g *GlobalFlags, value string) error {
	if workers, err := strconv.Atoi(value); err == nil {
		g.MaxParallel = workers
		return nil
	}
	switch mode := strings.ToLower(value); mode {
	case "auto", "eco", "max":
		g.MaxParallelMode = mode
		return nil
	default:
		return usageErrorf("invalid --max-parallel value %q: expected a worker count or one of %s",
			value, strings.Join(commandmeta.GlobalFlagValues("--max-parallel"), ", "))
	}
}

// applyResourceBudget binds one --resource <name>=<units> pair: how many units
// of a named external resource this run has, which the scheduler admits
// claiming tasks against beside the worker count.
//
// The flag is REPEATABLE — one occurrence per resource — because the set of
// scarce things a runner owns is open-ended and a single comma-joined value
// would need its own escaping rules. A repeated name is last-wins, matching
// every other global flag's override behavior.
func applyResourceBudget(g *GlobalFlags, value string) error {
	name, units, split := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	if !split || name == "" {
		return usageErrorf("invalid --resource value %q: expected <name>=<units>, e.g. --resource db-connections=400", value)
	}
	budget, err := strconv.Atoi(strings.TrimSpace(units))
	if err != nil || budget < 0 {
		return usageErrorf("invalid --resource budget %q for %q: expected a whole number of units", units, name)
	}
	if g.ResourceBudgets == nil {
		g.ResourceBudgets = make(map[string]int)
	}
	g.ResourceBudgets[name] = budget
	return nil
}

func boolPtr(v bool) *bool { return &v }
