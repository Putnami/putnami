package commandmeta

import (
	"sort"
	"strings"
)

// The command catalog is the single command vocabulary of the CLI (ADR 0001,
// decision 1). It owns paths and subcommands, categories and summaries, usage
// prose, typed flags, positionals, examples, related commands, structured-output
// capability, and workspace requirements — as data. Every command surface is
// derived from it: the categorized text help, the "Related Commands" block, the
// per-command structured help, and the man/Markdown references.
//
// A second hand-maintained table describing commands is a defect, not an
// addition: internal/cli/structural_baseline_test.go fails when one appears.
//
// The catalog grew in stages, each one extending rather than forking it:
//
//   - The catalog was filled first, deriving structuredCommandCategories,
//     relatedCommands, and structuredCommandHelp from it.
//   - Shell completion was derived from Command.Path/Summary and the
//     global-flag table, the structured-output capability check was derived
//     from Command.StructuredOutput, and Command.DefaultSub was added — the
//     bare-root default subcommand canonicalStructuredCommand used to
//     hand-key.
//   - Command.Flags and the GlobalFlag table were bound to the argument
//     parser. Flag.Type/Short drive arity and alias resolution, Flag.Values is
//     an ENFORCED enum (see Flag.Values), and ResolveFlags is the lookup the
//     parser uses to decide whether it may reject a flag at all.
//     Command.Positionals stays display-only: the catalog spells the usage
//     line, the handlers still consume their own positionals.
//   - The catalog became the sole VOCABULARY: IsStructuredRoot now answers
//     "does this word take a subcommand" (internal/cli.commandRegistry used
//     to), and internal/cli.registerCommand panics on a name this catalog
//     does not declare, so the handler map cannot describe a command the
//     catalog has never heard of. That change deliberately did NOT consume
//     Command.Workspace in the dispatcher. The handlers still call
//     env.requireWorkspace() themselves, and
//     internal/cli.TestCatalog_WorkspaceRequirementMatchesHandlers proves the
//     catalog's declaration matches what they do. Moving the check into
//     dispatch changes WHEN the error fires relative to subcommand
//     validation, which is a behavior change that belongs on its own.

// Kind separates the two command vocabularies the CLI dispatches.
type Kind int

const (
	// KindJob is an extension-provided job command ("build", "test"): it runs
	// through the scheduler and accepts comma-composition.
	KindJob Kind = iota
	// KindStructured is a built-in structured command ("projects list") with a
	// registered handler.
	KindStructured
)

// FlagType is the argument shape of a command flag.
type FlagType int

const (
	// FlagBool is a flag that takes no value ("--dry-run").
	FlagBool FlagType = iota
	// FlagValue is a flag that takes a value ("--project <selector>").
	FlagValue
)

// Flag is one command-specific flag.
type Flag struct {
	// Long is the "--name" spelling; every catalog flag has one.
	Long string
	// Short is the single-dash alias ("-g"), or "" when the flag has none.
	Short string
	// Type is the argument shape. FlagValue flags carry a ValueName.
	Type FlagType
	// ValueName is the metavariable shown in help ("<selector>"); empty for
	// FlagBool.
	ValueName string
	// Description is the one-line help text.
	Description string
	// Default is the value used when the flag is absent, or nil when the flag
	// has no documented default. No catalog flag documents one today; A1c
	// populates this from the parser.
	Default any
	// Values enumerates the accepted values of a FlagValue flag, or nil when the
	// value is unbounded (a path, a version, a project selector). A non-nil list
	// is ENFORCED by the parser (slice A1c), not merely offered as a hint, so an
	// incomplete list rejects a legitimate value — leave it nil unless the set is
	// genuinely closed. Mirrors GlobalFlag.Values.
	Values []string
}

// GlobalFlag is one CLI-wide flag, accepted alongside any command rather than
// declared by one. It is catalog data for the same reason command flags are:
// before A1b the same list was retyped in the parser, in help, and once more in
// each of the three completion generators, with a different wording every time.
//
// The parser (internal/cli/flags.go) stays the behavioral source of truth until
// slice A1c binds it to this table; this table is what every DISPLAY surface
// reads.
type GlobalFlag struct {
	// Long is the "--name" spelling; every global flag has one.
	Long string
	// Short is the single-dash alias ("-v"), or "" when there is none.
	Short string
	// Type is the argument shape. FlagValue flags carry a ValueName.
	Type FlagType
	// ValueName is the metavariable shown in help ("<format>"); empty for
	// FlagBool.
	ValueName string
	// Description is the one-line help text. It is the only wording: help
	// renders it, and so does every shell's completion menu.
	Description string
	// Category groups the flag in help's categorized flag listing. "" means the
	// flag is accepted and completed but deliberately not listed — today only
	// --version, which the version path prints rather than the flag catalog
	// (pinned by internal/cli's TestHelpCatalogCoversReservedGlobals).
	Category string
	// Values are the candidate values shell completion offers for a FlagValue
	// flag, or nil when the value is unbounded (a path, a project selector).
	Values []string
}

// GlobalFlagCategory is one group of help's categorized global-flag listing.
type GlobalFlagCategory struct {
	Name  string
	Flags []GlobalFlag
}

// Positional is one positional argument of a command.
type Positional struct {
	// Name is the metavariable as spelled in Usage, without brackets
	// ("name", "bash|zsh|fish").
	Name string
	// Required reports whether Usage spells it <name> rather than [name].
	Required bool
	// Description is optional prose; the text surfaces render Usage instead.
	Description string
}

// WorkspaceNeed states whether a command needs a workspace root, and under which
// flags today's handler skips the check.
//
// This codifies the CLI's current, deliberately non-uniform behavior rather than
// normalizing it: `cache clean --all` and `cache gc` act on the machine-global
// store, `version list|use --global` and `upgrade --global` manage the installed
// CLI binaries in ~/.putnami/bin, and `init`/`migrate` exist to bootstrap a
// workspace that does not exist yet.
type WorkspaceNeed struct {
	// Required reports whether the command needs a workspace by default.
	Required bool
	// ExemptFlags drop the requirement when any one of them is present.
	ExemptFlags []string
	// OverrideFlags reinstate the requirement even when an ExemptFlag is
	// present ("upgrade --from-source --global" still needs the source tree).
	OverrideFlags []string
	// Note records why a command is not unconditionally required. Every entry
	// whose requirement is anything other than a plain Required must carry one
	// (asserted by catalog_test.go).
	Note string
}

// RequiresWorkspace reports whether an invocation carrying args needs a
// workspace root. OverrideFlags win over ExemptFlags.
func (n WorkspaceNeed) RequiresWorkspace(args []string) bool {
	if !n.Required {
		return false
	}
	for _, flag := range n.OverrideFlags {
		if containsArg(args, flag) {
			return true
		}
	}
	for _, flag := range n.ExemptFlags {
		if containsArg(args, flag) {
			return false
		}
	}
	return true
}

// Conditional reports whether the requirement depends on the invocation's flags.
func (n WorkspaceNeed) Conditional() bool {
	return n.Required && len(n.ExemptFlags) > 0
}

func containsArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// Command is one entry in the catalog, keyed by its invocation path.
type Command struct {
	// Path is the space-separated invocation path ("projects list", "doctor").
	// It is the key every derived table uses, so it must be what the dispatcher
	// resolves — not a display spelling.
	Path string
	// Kind separates job commands from built-in structured commands.
	Kind Kind
	// Category names the group this command heads in the categorized help
	// listing, or "" when the command is reachable but not its own listing row
	// (a bare group root, or a subcommand collapsed into a sibling's row).
	Category string
	// Label overrides Path in the categorized listing, for rows that collapse
	// several paths ("version get|set|bump|tag|list|use") or name an argument
	// ("completion <shell>"). Display only.
	Label string
	// Summary is the one-line listing description. It is deliberately shorter
	// than Description: the listing renders it in a 28-column table, while
	// Description is the full prose `putnami help <command>` prints.
	Summary string
	// Description is the full prose for per-command help. Empty means the
	// command has no help entry of its own (see DetailFrom).
	Description string
	// Usage is the single usage line.
	Usage string
	// Flags are the command-specific flags, in display order.
	Flags []Flag
	// Positionals are the positional arguments, in order.
	Positionals []Positional
	// Examples are runnable invocations, each starting with "putnami ".
	Examples []string
	// Related are invocation paths suggested under "Related Commands:".
	Related []string
	// DefaultSub names the subcommand a bare `putnami <root>` runs, for the
	// roots whose handler treats an empty subcommand as an alias for one of its
	// subcommands (the `case "list", "":` arms in registry_commands.go). Every
	// surface that keys a path must expand a bare root through it, or the bare
	// spelling silently loses the metadata its expansion carries — which is
	// exactly how `putnami scopes --json` came to exit 2 while
	// `putnami scopes list --json` worked.
	//
	// Empty means the bare root is NOT an alias: it either has behavior of its
	// own (`putnami mcp` serves over stdio) or is a usage error
	// (`putnami contracts`). internal/cli's TestCatalog_DefaultSubMatchesHandlers
	// reads registry_commands.go and fails when this field and the handlers
	// disagree in either direction.
	DefaultSub string
	// DetailFrom names another catalog path whose Description, Usage, Flags,
	// Positionals, and Examples this path reuses verbatim, because the two are
	// the same command under two spellings ("scopes" ≡ "scopes list",
	// "init" ≡ "workspace init"). Both spellings get a help entry; the
	// reference documentation renders the canonical one only.
	DetailFrom string
	// StructuredOutput reports whether this exact path emits --output=json|jsonl.
	StructuredOutput bool
	// Workspace states when the command needs a workspace root.
	Workspace WorkspaceNeed
	// Recovery marks a command that must stay reachable when the workspace
	// probe fails.
	//
	// A failed metadata probe leaves core with a half-known project graph, and
	// every command that plans or executes over that graph must fail loudly
	// rather than build the wrong thing. That rule is only safe because a
	// declared minority of commands survives it: the ones that can REPAIR the
	// situation (install the missing extension, re-scan the tree) or that never
	// needed the graph at all (help, version). Without them a broken provider
	// would be an unrecoverable workspace — the user could not even install the
	// fix.
	//
	// It lives on the catalog rather than in a second allowlist table because
	// the catalog is the CLI's single command vocabulary (ADR 0001 §1): a
	// separate list would be a second description of the same commands, which
	// internal/cli/structural_baseline_test.go exists to prevent.
	Recovery bool
}

// DisplayName returns the spelling used in the categorized listing.
func (c Command) DisplayName() string {
	if c.Label != "" {
		return c.Label
	}
	return c.Path
}

// Root returns the first segment of Path ("projects" for "projects list").
func (c Command) Root() string {
	root, _, _ := strings.Cut(c.Path, " ")
	return root
}

// Documented reports whether this path has per-command help of its own or
// inherits it from another path.
func (c Command) Documented() bool {
	return c.Description != "" || c.DetailFrom != ""
}

// Category is one group of the categorized help listing.
type Category struct {
	Name     string
	Commands []Command
}

// Commands returns the whole catalog in declaration order.
func Commands() []Command {
	return append([]Command(nil), catalog...)
}

// StructuredCommands returns the built-in structured commands in declaration
// order.
func StructuredCommands() []Command {
	out := make([]Command, 0, len(catalog))
	for _, command := range catalog {
		if command.Kind == KindStructured {
			out = append(out, command)
		}
	}
	return out
}

// StructuredRoots returns, in declaration order, the bare-root entry of every
// built-in structured command ("projects", not "projects list"). Shell
// completion offers exactly this set as its first-word vocabulary.
func StructuredRoots() []Command {
	out := make([]Command, 0, len(catalog))
	for _, command := range catalog {
		if command.Kind == KindStructured && command.Path == command.Root() {
			out = append(out, command)
		}
	}
	return out
}

// IsStructuredRoot reports whether name is the bare root of a built-in
// structured command ("projects", not the job command "build" and not the child
// path "projects list").
//
// This is the CLI's answer to "does this word take a subcommand", asked on the
// argument-parsing path. It moved here from internal/cli.commandRegistry so
// the catalog is the only vocabulary and the
// handler registry is only dispatch (ADR 0001 §1). It scans rather than caching
// a set: the catalog is a few dozen entries, and a package-level set built from
// it would be a second copy of the vocabulary — precisely what §1 forbids.
func IsStructuredRoot(name string) bool {
	if name == "" {
		return false
	}
	for _, command := range catalog {
		if command.Kind == KindStructured && command.Path == name {
			return true
		}
	}
	return false
}

// PositionalLeaf reports whether root is a structured command whose first bare
// argument is its own positional rather than a subcommand: it declares
// positionals, emits structured output, and no catalog path extends it.
//
// The parser otherwise moves that argument into the subcommand slot, and the
// invocation path becomes "<root> <argument>" — a path no catalog entry has — so
// the --output gate would refuse a mode the command supports, flag validation
// would skip the command, and the result envelope would name the argument as
// part of the command. Commands that do not emit structured output (pin, help)
// keep reading that argument as their subcommand, as their handlers expect.
func PositionalLeaf(root string) bool {
	command, ok := Lookup(root)
	if !ok || command.Kind != KindStructured || !command.StructuredOutput || len(command.Positionals) == 0 {
		return false
	}
	for _, other := range catalog {
		if strings.HasPrefix(other.Path, root+" ") {
			return false
		}
	}
	return true
}

// RecoveryCommands returns, in declaration order, every command that stays
// available when the workspace probe fails.
func RecoveryCommands() []Command {
	out := make([]Command, 0, 8)
	for _, command := range catalog {
		if command.Recovery {
			out = append(out, command)
		}
	}
	return out
}

// IsRecoveryCommand reports whether an invocation path survives a probe
// failure. A bare structured root is expanded through DefaultSub first, so
// `putnami extensions` is answered the same way as the `extensions install` it
// dispatches to — the gap between the two spellings is exactly how
// `putnami scopes --json` once came to behave differently from
// `putnami scopes list --json`.
func IsRecoveryCommand(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	for _, command := range catalog {
		if command.Path != path {
			continue
		}
		if command.Recovery {
			return true
		}
		if command.DefaultSub != "" {
			return IsRecoveryCommand(command.Path + " " + command.DefaultSub)
		}
		return false
	}
	return false
}

// Subcommands returns the immediate children of path in declaration order:
// Subcommands("projects") is [list create describe sync tag], and
// Subcommands("projects list") is empty. A path with no catalog entry has no
// children.
func Subcommands(path string) []string {
	prefix := path + " "
	var out []string
	for _, command := range catalog {
		child, found := strings.CutPrefix(command.Path, prefix)
		if !found || child == "" || strings.Contains(child, " ") {
			continue
		}
		out = append(out, child)
	}
	return out
}

// CanonicalPath expands a command root and subcommand into the catalog path
// every derived table keys. A bare root resolves through DefaultSub, so the two
// spellings a handler treats identically ("scopes" and "scopes list") also look
// up identical metadata. A root the catalog does not know — an extension
// command, say — is returned unchanged.
func CanonicalPath(root, sub string) string {
	if sub == "" {
		if command, ok := Lookup(root); ok {
			sub = command.DefaultSub
		}
	}
	if sub == "" {
		return root
	}
	return root + " " + sub
}

// EmitsStructuredOutput reports whether an exact invocation path supports
// --output=json|jsonl. Callers pass a CanonicalPath result so a bare root is
// judged by the subcommand it actually runs.
func EmitsStructuredOutput(path string) bool {
	command, ok := Lookup(path)
	return ok && command.StructuredOutput
}

// StructuredOutputPaths returns every path that supports --output=json|jsonl,
// sorted, for the "structured-output commands: …" hint the CLI prints when the
// flag is rejected.
func StructuredOutputPaths() []string {
	out := make([]string, 0, len(catalog))
	for _, command := range catalog {
		if command.StructuredOutput {
			out = append(out, command.Path)
		}
	}
	sort.Strings(out)
	return out
}

// GlobalFlags returns the CLI-wide flag vocabulary in declaration order.
func GlobalFlags() []GlobalFlag {
	return append([]GlobalFlag(nil), globalFlags...)
}

// GlobalFlagCategories returns the categorized global-flag listing help renders:
// every global flag that declares a Category, grouped in first-appearance order
// of the category. Flags with no Category are accepted and completed but not
// listed (see GlobalFlag.Category).
func GlobalFlagCategories() []GlobalFlagCategory {
	var out []GlobalFlagCategory
	index := map[string]int{}
	for _, flag := range globalFlags {
		if flag.Category == "" {
			continue
		}
		position, seen := index[flag.Category]
		if !seen {
			position = len(out)
			index[flag.Category] = position
			out = append(out, GlobalFlagCategory{Name: flag.Category})
		}
		out[position].Flags = append(out[position].Flags, flag)
	}
	return out
}

// GlobalFlagValues returns the candidate values completion offers for a global
// flag ("--output" → text json jsonl cloud-logging), or nil when the flag takes
// no value or its values are not enumerable.
func GlobalFlagValues(long string) []string {
	for _, flag := range globalFlags {
		if flag.Long == long {
			return append([]string(nil), flag.Values...)
		}
	}
	return nil
}

// ResolveFlags returns the flag surface an invocation path accepts, following
// DetailFrom so a path that shares another path's documentation also shares its
// flags ("init" ≡ "workspace init"). The second result reports whether the
// catalog knows the path at all.
//
// Slice A1c binds this to the parser: a KNOWN path's flag list is authoritative,
// so a flag it does not declare is a usage error. An UNKNOWN path — an extension
// group, or a spelling whose subcommand is really a positional
// (`upgrade 1.2.3`) — leaves the parser lenient, because the catalog cannot
// prove such a flag wrong. That asymmetry is deliberate: rejecting on an
// unknown path would turn every undocumented spelling into an outage.
func ResolveFlags(path string) ([]Flag, bool) {
	command, ok := Lookup(path)
	if !ok {
		return nil, false
	}
	if command.DetailFrom == "" {
		return command.Flags, true
	}
	source, ok := Lookup(command.DetailFrom)
	if !ok {
		return command.Flags, true
	}
	return source.Flags, true
}

// Lookup returns the catalog entry for an exact invocation path.
func Lookup(path string) (Command, bool) {
	for _, command := range catalog {
		if command.Path == path {
			return command, true
		}
	}
	return Command{}, false
}

// Detail returns the entry carrying path's per-command help, following
// DetailFrom for paths that share another path's documentation. The returned
// Command keeps the requested Path so callers can render it under its own name.
func Detail(path string) (Command, bool) {
	command, ok := Lookup(path)
	if !ok {
		return Command{}, false
	}
	if command.DetailFrom == "" {
		return command, command.Description != ""
	}
	source, ok := Lookup(command.DetailFrom)
	if !ok || source.Description == "" {
		return Command{}, false
	}
	command.Description = source.Description
	command.Usage = source.Usage
	command.Flags = source.Flags
	command.Positionals = source.Positionals
	command.Examples = source.Examples
	return command, true
}

// ReferenceCommands returns, in declaration order, the commands whose full help
// the man page and Markdown reference render: every documented path except the
// alternate spellings, which would otherwise print the same section twice.
func ReferenceCommands() []Command {
	out := make([]Command, 0, len(catalog))
	for _, command := range catalog {
		if command.Kind != KindStructured || command.DetailFrom != "" || command.Description == "" {
			continue
		}
		out = append(out, command)
	}
	return out
}

// Categories returns the categorized command listing: every catalog entry that
// declares a Category, grouped in first-appearance order of the category and, in
// each group, in catalog order.
func Categories() []Category {
	var out []Category
	index := map[string]int{}
	for _, command := range catalog {
		if command.Category == "" {
			continue
		}
		position, seen := index[command.Category]
		if !seen {
			position = len(out)
			index[command.Category] = position
			out = append(out, Category{Name: command.Category})
		}
		out[position].Commands = append(out[position].Commands, command)
	}
	return out
}
