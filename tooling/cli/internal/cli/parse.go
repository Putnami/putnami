package cli

import (
	"io"
	"sort"
	"strings"
	"unicode"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// The shared pieces of the one parse-and-validate pass.
//
// Phase 1 (syntax only) is splitInlineValues + the token predicates below: they
// answer "what SHAPE is this token" without knowing any command.
// Phase 2 (catalog resolution) is commandmeta.CanonicalPath/ResolveFlags plus
// the extension manifest lookups the callers supply.
// Phase 3 (semantics) is parseGlobalFlags' binding pass in flags.go and the
// validation in this file.
//
// The strictness policy is settled (epic refinement Q1) and is deliberately
// asymmetric:
//
//   - A BUILT-IN structured command whose exact path the catalog knows is
//     authoritative about its own flags, so an undeclared flag is a hard usage
//     error. Nothing else can own that flag.
//   - A JOB command or an EXTENSION command group is served by manifests that
//     ship on their own release cadence, and internal/extension/flags.go has
//     never validated supplied flags against declarations. Hard-failing an
//     undeclared flag there would break every workspace pinning an extension
//     whose manifest under-declares — a known failure mode. Those get a deprecation
//     warning on stderr for one minor version instead.
//   - Everything after a bare "--" is passthrough and is never validated on any
//     surface.
//
// What this file must never do is CHANGE which tokens survive into
// ParsedArgs.RawJobArgs for an invocation that still succeeds: that slice is
// what buildCommandParams hashes into every job cache key and every run marker
// (parse_key_stability_test.go pins it). Validation reads the tokens; it does
// not rewrite them.

// undeclaredFlagDeprecation is the one-minor-version warning window for a flag
// no manifest declares. It becomes a hard error in the next minor release, when
// the extension ecosystem has had a release cycle to declare what it accepts.
const undeclaredFlagDeprecation = "deprecated: it will be rejected in the next minor release"

// isFlagToken reports whether a raw argument is a flag rather than a positional.
// A bare "--", a lone "-", and a negative number ("-5", a legitimate positional
// value) are not flags.
func isFlagToken(arg string) bool {
	trimmed := strings.TrimLeft(arg, "-")
	if trimmed == "" || trimmed == arg {
		return false
	}
	first := rune(trimmed[0])
	return first == '_' || unicode.IsLetter(first)
}

// flagSpelling returns a flag token's canonical spelling, dropping any inline
// value ("--target=wasm" → "--target").
func flagSpelling(arg string) string {
	spelling, _, _ := strings.Cut(arg, "=")
	return spelling
}

// flagBaseName returns the declaration name a flag token refers to: leading
// dashes and any inline value removed, and the "no-" negation prefix stripped
// because buildCommandParams maps "--no-minify" onto the declared "minify" flag.
func flagBaseName(arg string) string {
	name := strings.TrimLeft(flagSpelling(arg), "-")
	return strings.TrimPrefix(name, "no-")
}

// passthroughCut returns the number of leading arguments that precede a bare
// "--" separator, or len(args) when there is none. Tokens at or after the cut
// are the user's passthrough payload and are never validated.
func passthroughCut(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i
		}
	}
	return len(args)
}

// promoteProjectSelector is the ONE positional-promotion implementation, shared
// by the job-command path (ParseArgs) and the extension-alias path
// (runPlannedExtensionAlias). It moves a leading positional argument into the
// project selector and returns the remaining arguments.
//
// An explicit selector wins: --projects, --all, and --impacted all populate
// g.Projects before this runs, and a stray positional must not silently replace
// what the user asked for. Before A1c the job path overwrote it and the alias
// path did not; unifying on "explicit wins" cannot move a cache key, because
// only args[0] is ever promoted and buildCommandParams never turns a LEADING
// positional into a param either way.
func promoteProjectSelector(g *GlobalFlags, rawArgs []string) []string {
	if g.Projects != "" || len(rawArgs) == 0 {
		return rawArgs
	}
	first := rawArgs[0]
	if strings.HasPrefix(first, "-") || strings.Contains(first, "=") {
		return rawArgs
	}
	g.Projects = first
	return rawArgs[1:]
}

// commandFlagValues is one built-in command's parsed flag surface: which
// boolean flags were present, the value each FlagValue flag carried, and the
// positional arguments in order. The fields are deliberately typed rather than
// one untyped payload map: the catalog states each flag's shape, so nothing here
// has to be inferred from the token.
type commandFlagValues struct {
	present     map[string]bool
	values      map[string]string
	positionals []string
}

// has reports whether a boolean flag (or any spelling of it) was supplied.
func (v commandFlagValues) has(spellings ...string) bool {
	for _, spelling := range spellings {
		if v.present[spelling] {
			return true
		}
	}
	return false
}

// value returns the value supplied for a FlagValue flag, or "".
func (v commandFlagValues) value(long string) string { return v.values[long] }

// parseCatalogCommandFlags parses a built-in command's arguments against the
// flag surface the catalog declares for path. It replaces the per-command
// hand-rolled parsers (the upgrade one knew its own value-taking flags in a
// second table, `flagTakesValue`) with one pass driven by Flag.Type,
// Flag.Short, and Flag.Values.
//
// Errors: an unknown flag, a value flag with no value, and a value outside a
// closed Flag.Values enum. Everything at or after a bare "--" is returned in
// positionals untouched, never validated.
func parseCatalogCommandFlags(path string, args []string) (commandFlagValues, error) {
	parsed := commandFlagValues{present: map[string]bool{}, values: map[string]string{}}
	declared, known := commandmeta.ResolveFlags(path)
	if !known {
		return parsed, usageErrorf("no command catalog entry for %q", path)
	}

	bySpelling := make(map[string]commandmeta.Flag, 2*len(declared))
	for _, flag := range declared {
		bySpelling[flag.Long] = flag
		if flag.Short != "" {
			bySpelling[flag.Short] = flag
		}
	}

	cut := passthroughCut(args)
	for i := 0; i < cut; i++ {
		arg := args[i]
		if !isFlagToken(arg) {
			parsed.positionals = append(parsed.positionals, arg)
			continue
		}

		spelling, inline, hasInline := strings.Cut(arg, "=")
		flag, ok := bySpelling[spelling]
		if !ok {
			if isGlobalFlag(spelling) {
				continue
			}
			return parsed, usageErrorf("unknown flag %s for `putnami %s`%s\n  Run `putnami help %s` for the accepted flags.",
				spelling, path, acceptedFlagHint(declared), path)
		}
		parsed.present[spelling] = true
		if flag.Type != commandmeta.FlagValue {
			continue
		}

		value := inline
		if !hasInline {
			if i+1 >= cut {
				return parsed, usageErrorf("flag %s requires a value: %s %s", spelling, flag.Long, flag.ValueName)
			}
			i++
			value = args[i]
		}
		if err := checkFlagEnum(flag, value); err != nil {
			return parsed, err
		}
		parsed.values[flag.Long] = value
	}
	parsed.positionals = append(parsed.positionals, args[cut:]...)
	return parsed, nil
}

// checkFlagEnum rejects a value outside a flag's closed value set. A flag with
// no declared Values accepts anything.
func checkFlagEnum(flag commandmeta.Flag, value string) error {
	if len(flag.Values) == 0 {
		return nil
	}
	for _, accepted := range flag.Values {
		if value == accepted {
			return nil
		}
	}
	return usageErrorf("invalid %s value %q: expected one of %s", flag.Long, value, strings.Join(flag.Values, ", "))
}

// suppliedFlag reports whether any of the given spellings appears before the
// passthrough separator. It is the one membership test the built-in handlers
// share (`--global`/`-g`), replacing the per-file containsFlag copies.
func suppliedFlag(args []string, spellings ...string) bool {
	for _, arg := range args[:passthroughCut(args)] {
		for _, spelling := range spellings {
			if arg == spelling {
				return true
			}
		}
	}
	return false
}

// firstPositionalArg returns the first non-flag argument, or "" if there is
// none (e.g. the <name> in `version use <name>`).
func firstPositionalArg(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

// validateStructuredFlags is the hard-rejection half of the strictness policy:
// a built-in structured command whose exact catalog path is known accepts only
// its declared flags plus the CLI-wide globals. An unknown path stays lenient —
// see commandmeta.ResolveFlags for why.
func validateStructuredFlags(root, sub string, rawArgs []string) error {
	path := commandmeta.CanonicalPath(root, sub)
	declared, known := commandmeta.ResolveFlags(path)
	if !known {
		return nil
	}

	accepted := make(map[string]bool, 2*len(declared))
	for _, flag := range declared {
		accepted[flag.Long] = true
		if flag.Short != "" {
			accepted[flag.Short] = true
		}
	}

	for _, arg := range rawArgs[:passthroughCut(rawArgs)] {
		if !isFlagToken(arg) {
			continue
		}
		spelling := flagSpelling(arg)
		if accepted[spelling] || isGlobalFlag(spelling) {
			continue
		}
		return usageErrorf("unknown flag %s for `putnami %s`%s\n  Run `putnami help %s` for the accepted flags.",
			spelling, path, acceptedFlagHint(declared), path)
	}
	return nil
}

// acceptedFlagHint renders a command's declared flags for an error message, or
// "" when it declares none (in which case only globals are accepted and the
// message stays short).
func acceptedFlagHint(declared []commandmeta.Flag) string {
	if len(declared) == 0 {
		return ""
	}
	names := make([]string, 0, len(declared))
	for _, flag := range declared {
		names = append(names, flag.Long)
	}
	return "\n  Accepted: " + strings.Join(names, ", ")
}

// coreJobFlags are the flags the CORE reads from a job-running command rather
// than an extension manifest: the release-set channels, the channel a first
// publish measures against without advancing it, the visibility a publication
// confers, the version line a tagged publish releases, the environment a deploy
// synchronizes, and the immutable release an upgrade or a deploy names. They are
// not global flags — they mean nothing outside publish, upgrade and deploy —
// and no manifest declares them, because the coordinator that reads them is the
// CLI itself. Without this exemption every release-set publish would warn about
// the exact flag it requires.
//
// `baseline-channel` is deliberately its own name: the global `--baseline` is
// the git ref `--impacted` resolves against, and a release-set publish never
// measures against git (D16), so the two could not share a spelling.
var coreJobFlags = map[string]bool{
	"channel": true, "baseline-channel": true, "visibility": true, "scope": true,
	"release": true, "env": true,
}

// undeclaredFlagWarnings is the warning-window half of the policy: one notice
// per supplied flag that neither the CLI globals nor the manifest-declared
// surface knows. scope names the command in the message ("build", "cloud
// deploy"). Order is deterministic and each flag is reported once.
func undeclaredFlagWarnings(scope string, rawArgs []string, declared map[string]extension.FlagDefinition) []string {
	var (
		seen     = map[string]bool{}
		reported []string
	)
	for _, arg := range rawArgs[:passthroughCut(rawArgs)] {
		if !isFlagToken(arg) {
			continue
		}
		spelling := flagSpelling(arg)
		if isGlobalFlag(spelling) || coreJobFlags[flagBaseName(arg)] || seen[spelling] {
			continue
		}
		if isDeclaredExtensionFlag(arg, declared) {
			continue
		}
		seen[spelling] = true
		reported = append(reported, spelling)
	}
	if len(reported) == 0 {
		return nil
	}
	warnings := make([]string, 0, len(reported))
	for _, spelling := range reported {
		warnings = append(warnings, "flag "+spelling+" is not declared by `"+scope+"`; passing undeclared flags is "+undeclaredFlagDeprecation)
	}
	return warnings
}

// isDeclaredExtensionFlag reports whether arg names a manifest-declared flag.
// Flag maps are keyed by the long name, while manifests may spell their short
// alias with or without its leading dash.
func isDeclaredExtensionFlag(arg string, declared map[string]extension.FlagDefinition) bool {
	if _, ok := declared[flagBaseName(arg)]; ok {
		return true
	}
	spelling := flagSpelling(arg)
	for _, def := range declared {
		short := def.Short
		if short == "" {
			continue
		}
		if !strings.HasPrefix(short, "-") {
			short = "-" + short
		}
		if spelling == short {
			return true
		}
	}
	return false
}

// declaredTaskFlags returns the union of the flags the selected tasks declare,
// which is the surface an invocation of those tasks may legitimately carry.
func declaredTaskFlags(commands []string, jobMap map[string][]*extension.JobDefinition) map[string]extension.FlagDefinition {
	declared := make(map[string]extension.FlagDefinition)
	for _, command := range commands {
		for name, def := range extension.CollectCommandFlags(jobMap[command]) {
			if _, seen := declared[name]; !seen {
				declared[name] = def
			}
		}
	}
	return declared
}

// conflictingTaskFlags rejects a comma-composed invocation in which two of the
// selected tasks declare the SAME supplied flag with different behavior, naming
// the conflicting tasks. One token cannot carry two meanings, and
// extension.MergeCommandFlags only ever compared declarations WITHIN a single
// command, so `putnami build,publish --stable` used to bind two different flags
// from one spelling with no diagnostic.
func conflictingTaskFlags(commands []string, jobMap map[string][]*extension.JobDefinition, rawArgs []string) error {
	if len(commands) < 2 {
		return nil
	}
	declaredBy := make(map[string][]string)
	definitions := make(map[string]extension.FlagDefinition)

	for _, command := range commands {
		for name, def := range extension.CollectCommandFlags(jobMap[command]) {
			if previous, seen := definitions[name]; seen {
				if !extension.FlagDefsCompatible(previous, def) {
					declaredBy[name] = append(declaredBy[name], command)
				}
				continue
			}
			definitions[name] = def
			declaredBy[name] = []string{command}
		}
	}

	for _, arg := range rawArgs[:passthroughCut(rawArgs)] {
		if !isFlagToken(arg) {
			continue
		}
		tasks := declaredBy[flagBaseName(arg)]
		if len(tasks) < 2 {
			continue
		}
		sorted := append([]string(nil), tasks...)
		sort.Strings(sorted)
		return usageErrorf("flag %s means different things to %s\n  Run them separately, or drop the flag.",
			flagSpelling(arg), strings.Join(sorted, " and "))
	}
	return nil
}

// printParseWarnings writes the parse pass's deprecation notices to stderr, so
// a structured stdout stream stays machine-clean. Quiet suppresses them like
// every other advisory line.
func printParseWarnings(w io.Writer, g GlobalFlags, warnings []string) {
	if g.Quiet {
		return
	}
	for _, warning := range warnings {
		iox.Fprintf(w, "putnami: %s\n", warning)
	}
}
