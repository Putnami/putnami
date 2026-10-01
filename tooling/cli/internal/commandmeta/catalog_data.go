package commandmeta

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// catalog is the CLI's command vocabulary. See catalog.go for the type contract.
//
// Conventions in this table:
//
//   - Path is the invocation path the dispatcher resolves. A row that is
//     displayed under a different spelling sets Label; a spelling that shares
//     another path's help sets DetailFrom.
//   - Flag.Type is FlagBool by default; value-taking flags spell Type: FlagValue
//     and carry a ValueName that appears in Usage.
//   - Workspace records TODAY's behavior, including the conditional and absent
//     cases, and every non-plain requirement carries a Note naming the handler
//     that implements it. catalog_test.go and internal/cli's
//     TestCatalog_WorkspaceRequirementMatchesHandlers keep the two honest.
//   - Entries are grouped by Category in listing order, with each group's bare
//     root and non-listed subcommands adjacent to it.
var catalog = []Command{
	// ── Job commands ─────────────────────────────────────────────────────
	// Extension-provided verbs. Summary is only a fallback description for a
	// verb the protocol does not describe (proto.WellKnownVerbs wins), and job
	// commands are listed by PrintHelp's "Build (from extensions)" block rather
	// than by a catalog Category. A workspace is unconditional for all of them
	// (internal/cli/app.go).
	{
		Path:      "build",
		Kind:      KindJob,
		Related:   []string{"test", "lint", "serve"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "test",
		Kind:      KindJob,
		Related:   []string{"build", "lint"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "lint",
		Kind:      KindJob,
		Related:   []string{"build", "test", "format"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "serve",
		Kind:      KindJob,
		Related:   []string{"build", "run"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "run",
		Kind:      KindJob,
		Related:   []string{"build", "serve"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "format",
		Kind:      KindJob,
		Related:   []string{"lint"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "package",
		Kind:      KindJob,
		Related:   []string{"build", "publish"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "publish",
		Kind:      KindJob,
		Related:   []string{"build", "test", "package"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "deploy",
		Kind:      KindJob,
		Related:   []string{"build", "package", "publish"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "generate",
		Kind:      KindJob,
		Summary:   "Run code generators.",
		Related:   []string{"build"},
		Workspace: requiredWorkspace(),
	},

	// ── CI ───────────────────────────────────────────────────────────────
	{
		Path:        "change-plan",
		Kind:        KindStructured,
		Category:    "CI",
		Summary:     "Emit an immutable impacted CI ChangePlan",
		Description: "Emit a deterministic admission plan of the CI quality gate for the exact commit delta from --base to the checked-out HEAD. The worktree must be clean and --base must be a HEAD ancestor, so Cloud can recompute the document's digest from immutable repository state. Source contents, environment values, secrets, command arguments, and output paths are never emitted",
		Usage:       "putnami change-plan --base <commit> [--head <commit>] [--no-cache] [--output <format>]",
		Flags: []Flag{
			{Long: "--base", Type: FlagValue, ValueName: "<commit>", Description: "Required base commit or ref; emitted as its full immutable commit SHA"},
			{Long: "--head", Type: FlagValue, ValueName: "<commit>", Description: "Optional head commit or ref; it must resolve to the checked-out HEAD"},
		},
		Examples: []string{
			"putnami change-plan --base origin/main --output=json",
			"putnami change-plan --base 0123456789abcdef0123456789abcdef01234567 --no-cache --output=jsonl",
		},
		Related:          []string{"sessions inspect", "projects list", "doctor"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:       "ci",
		Kind:       KindStructured,
		Summary:    "Manage source-controlled workspace CI intent",
		DefaultSub: "validate",
		DetailFrom: "ci validate",
		Related:    []string{"ci validate", "ci explain", "change-plan"},
		Workspace:  requiredWorkspace(),
	},
	{
		Path:             "ci init",
		Kind:             KindStructured,
		Category:         "CI",
		Summary:          "Create a safe putnami.ci.json version 3 contract",
		Description:      "Create putnami.ci.json beside putnami.workspace.json using discovered framework graph jobs, with no distribution and no environment, so it requests no publish or deploy authority",
		Usage:            "putnami ci init [--force] [--output <format>]",
		Flags:            []Flag{{Long: "--force", Description: "Replace an existing putnami.ci.json"}},
		Examples:         []string{"putnami ci init", "putnami ci init --output=json"},
		Related:          []string{"ci validate", "ci fmt", "ci explain"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:             "ci validate",
		Kind:             KindStructured,
		Category:         "CI",
		Summary:          "Validate CI intent and workspace graph references",
		Description:      "Strictly validate putnami.ci.json, resolve its command references against discovered workspace jobs, refuse distribution and envs without an installed release-set provider, and report its canonical digest without contacting Putnami Cloud",
		Usage:            "putnami ci validate [--output <format>]",
		Examples:         []string{"putnami ci validate", "putnami ci validate --output=json"},
		Related:          []string{"ci fmt", "ci explain", "change-plan"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:             "ci fmt",
		Kind:             KindStructured,
		Category:         "CI",
		Summary:          "Canonicalize putnami.ci.json",
		Description:      "Validate and canonicalize putnami.ci.json while preserving semantic rule order; --check is read-only and fails when bytes differ",
		Usage:            "putnami ci fmt [--check] [--output <format>]",
		Flags:            []Flag{{Long: "--check", Description: "Check canonical formatting without writing"}},
		Examples:         []string{"putnami ci fmt", "putnami ci fmt --check"},
		Related:          []string{"ci validate", "ci explain"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "ci explain",
		Kind:        KindStructured,
		Category:    "CI",
		Summary:     "Explain first-match CI intent for local Source assumptions",
		Description: "Evaluate ordered rules locally and report the commands and flags the run executes, the matching rule or the implicit tag convention, the channels a publish advances, and the environments that follow each of them. Trust, entitlements, secrets, and remote authorization remain explicitly unresolved",
		Usage:       "putnami ci explain --event <push|tag|pull_request> [--branch <branch>] [--tag <tag>] [--pr <number>] [--output <format>]",
		Flags: []Flag{
			{Long: "--event", Type: FlagValue, ValueName: "<push|tag|pull_request>", Values: []string{"push", "tag", "pull_request"}, Description: "Normalized Source event assumption"},
			{Long: "--branch", Type: FlagValue, ValueName: "<branch>", Description: "Updated push branch or pull-request head branch"},
			{Long: "--tag", Type: FlagValue, ValueName: "<tag>", Description: "Created tag, for --event tag"},
			{Long: "--pr", Type: FlagValue, ValueName: "<number>", Description: "Pull-request number used to render the closed channel placeholder"},
		},
		Examples: []string{
			"putnami ci explain --event push --branch main --output=json",
			"putnami ci explain --event tag --tag ts/v0.3.0",
			"putnami ci explain --event pull_request --branch feature/x --pr 42",
		},
		Related:          []string{"ci validate", "ci fmt", "change-plan"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Distribution ─────────────────────────────────────────────────────
	{
		Path:     "channel",
		Kind:     KindStructured,
		Category: "Distribution",
		Label:    "channel set|status",
		Summary:  "Move a distribution channel and report how far it is served",
		Usage:    "putnami channel <set|status>",
		Description: "Move a channel to a release set that already exists, and report the head each registry has applied. " +
			"Both are metadata operations: they publish nothing, build nothing, and check nothing out",
		Positionals: []Positional{{Name: "set|status", Required: true}},
		Examples: []string{
			"putnami channel set latest --from canary",
			"putnami channel status latest --wait 2m",
		},
		Related:   []string{"channel set", "channel status", "ci validate"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "channel set",
		Kind:        KindStructured,
		Description: "Point a channel at a release set that already exists, taken from another channel or named by its immutable id. Promotion and rollback are this one command: no publisher runs, no artifact is uploaded, and the move is compare-and-swapped against the channel's current head",
		Usage:       "putnami channel set <channel> --from <channel|rs_id> [--expected <rs_id>]",
		Flags: []Flag{
			{Long: "--from", Type: FlagValue, ValueName: "<channel|rs_id>", Description: "The channel whose head is re-released, or the immutable release-set id to point at"},
			{Long: "--expected", Type: FlagValue, ValueName: "<rs_id>", Description: "The id the channel must currently name; without it the channel's current head is read and used"},
		},
		Positionals: []Positional{{Name: "channel", Required: true}},
		Examples: []string{
			"putnami channel set latest --from canary",
			"putnami channel set latest --from rs_<64-lowercase-hex>",
			"putnami channel set canary --from ts-v0.3.0 --expected rs_<64-lowercase-hex>",
		},
		Related:   []string{"channel status", "ci validate"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "channel status",
		Kind:        KindStructured,
		Description: "Report a channel's accepted head and the generation each registry has applied. It exits non-zero while any registry is behind, so a pipeline waits for the channel to be served instead of assuming it",
		Usage:       "putnami channel status <channel> [--wait <duration>]",
		Flags: []Flag{
			{Long: "--wait", Type: FlagValue, ValueName: "<duration>", Description: "Poll every 2s until every registry has applied the accepted head, or this Go duration elapses"},
		},
		Positionals: []Positional{{Name: "channel", Required: true}},
		Examples: []string{
			"putnami channel status latest",
			"putnami channel status canary --wait 2m",
		},
		Related:   []string{"channel set", "ci explain"},
		Workspace: requiredWorkspace(),
	},

	// ── Projects ─────────────────────────────────────────────────────────
	{
		Path:       "projects",
		Kind:       KindStructured,
		Summary:    "Manage projects",
		DefaultSub: "list",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "projects list",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "List canonical workspace projects",
		Description: "List every project in the loaded workspace graph with its canonical name, path, type, and tags; use projects describe for dependencies, dependents, extensions, and metadata",
		Usage:       "putnami projects list [--output <format>]",
		Examples: []string{
			"putnami projects list",
			"putnami projects list --output=jsonl",
		},
		Related:          []string{"projects describe", "projects create", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "projects create",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "Create a project from template",
		Description: "Create a project from template",
		Usage:       "putnami projects create <name> --template <template> [--path <path>] [--force]",
		Flags: []Flag{
			{Long: "--template", Type: FlagValue, ValueName: "<template>", Description: "Template to scaffold from"},
			{Long: "--path", Type: FlagValue, ValueName: "<path>", Description: "Workspace-relative project path"},
			{Long: "--force", Description: "Overwrite an existing project directory"},
		},
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami projects create my-app --template typescript-web",
			"putnami projects create my-app --template typescript-web --path apps/my-app",
		},
		Related:   []string{"projects list", "extensions list"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "projects describe",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "Show project info",
		Description: "Show detailed project information including dependencies, extensions, and configuration",
		Usage:       "putnami projects describe <name> [--output <format>]",
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami projects describe my-app",
			"putnami projects describe my-app --output=jsonl",
		},
		Related:          []string{"projects list", "config show"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "projects sync",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "Sync projects from disk",
		Description: "Sync project references from disk into workspace configuration",
		Usage:       "putnami projects sync [--prune] [--skip-install] [--dry-run]",
		Flags: []Flag{
			{Long: "--prune", Description: "Drop config entries whose project directory no longer exists"},
			{Long: "--skip-install", Description: "Do not run dependency installation after syncing"},
		},
		Examples: []string{
			"putnami projects sync",
			"putnami projects sync --dry-run",
		},
		Related:   []string{"projects list", "workspace describe"},
		Workspace: requiredWorkspace(),
		Recovery:  true,
	},
	{
		Path:        "projects tag",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "Manage project tags",
		Description: "Add or manage tags on a project",
		Usage:       "putnami projects tag <name> <tag> [--set] [--remove]",
		Flags: []Flag{
			{Long: "--set", Description: "Replace the project's tags with the given list"},
			{Long: "--remove", Description: "Remove the given tags from the project"},
		},
		Positionals: []Positional{
			{Name: "name", Required: true},
			{Name: "tag", Required: true},
		},
		Examples: []string{
			"putnami projects tag my-app frontend",
		},
		Related:   []string{"projects list", "projects describe"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:       "scopes",
		Kind:       KindStructured,
		Summary:    "List workspace scopes",
		DefaultSub: "list",
		DetailFrom: "scopes list",
		Related:    []string{"scopes list", "projects list", "workspace describe"},
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "scopes list",
		Kind:        KindStructured,
		Category:    "Projects",
		Summary:     "List workspace scopes (projects, groups, tags)",
		Description: "List workspace scopes with their projects, groups, and tags",
		Usage:       "putnami scopes list [--output <format>]",
		Examples: []string{
			"putnami scopes list",
			"putnami scopes list --output=jsonl",
		},
		Related:          []string{"projects list", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Workspace ────────────────────────────────────────────────────────
	{
		Path:       "init",
		Kind:       KindStructured,
		Category:   "Workspace",
		Summary:    "Initialize a new workspace",
		DetailFrom: "workspace init",
		Workspace: WorkspaceNeed{
			Note: "cmdInit creates the workspace, so it cannot need one (internal/cli/registry_commands.go)",
		},
	},
	{
		Path:        "install",
		Kind:        KindStructured,
		Category:    "Workspace",
		Summary:     "Install extensions and dependencies",
		Description: "Run workspace-level installation: extensions, templates (if configured), and dependencies",
		Usage:       "putnami install [--latest]",
		Flags: []Flag{
			{Long: "--latest", Description: "Ignore the lock file and resolve the latest extension and template versions"},
		},
		Examples: []string{
			"putnami install",
		},
		Related:   []string{"extensions install", "deps install"},
		Workspace: requiredWorkspace(),
		Recovery:  true,
	},
	{
		Path:        "upgrade",
		Kind:        KindStructured,
		Category:    "Workspace",
		Summary:     "Upgrade CLI, extensions, and dependencies",
		Description: "Upgrade the CLI, extensions, templates, and framework dependencies in one command; a channel resolves on each ecosystem's own registry",
		Usage:       "putnami upgrade [--global] [--cli] [--extensions] [--deps] [--channel <stable|canary>] [--release <rs_id> --namespace <namespace>] [--version <version>] [--from-source] [--dry-run]",
		Flags: []Flag{
			{Long: "--global", Short: "-g", Description: "Upgrade the global CLI in ~/.putnami/bin instead of the workspace pin; inside a workspace the other phases still run, outside one only the CLI is upgraded (alias: -g)"},
			{Long: "--cli", Description: "Only upgrade the CLI binary"},
			{Long: "--extensions", Description: "Only upgrade extensions and templates"},
			{Long: "--deps", Description: "Only upgrade framework dependencies"},
			{Long: "--channel", Type: FlagValue, ValueName: "<channel>", Description: "Resolve a release channel (stable by default; canary for latest prerelease)", Values: []string{"stable", "canary"}},
			{Long: "--release", Type: FlagValue, ValueName: "<rs_id>", Description: "Use one immutable release-set snapshot for npm and Go dependencies (needs --namespace)"},
			{Long: "--namespace", Type: FlagValue, ValueName: "<namespace>", Description: "Release-set namespace that published the --release id; accepted only with --release"},
			{Long: "--version", Type: FlagValue, ValueName: "<version>", Description: "Use an exact Putnami release version"},
			{Long: "--from-source", Description: "Build the CLI from this workspace's source and install it as the active binary, bypassing the download channel (self-host escape hatch; combine with --global to replace the global CLI)"},
			{Long: "--dry-run", Description: "Resolve and print the upgrade plan without modifying files"},
		},
		Examples: []string{
			"putnami upgrade",
			"putnami upgrade --global",
			"putnami upgrade --channel canary",
			"putnami upgrade --release rs_<64-lowercase-hex> --namespace putnami",
			"putnami upgrade --deps --version 1.2.3",
			"putnami upgrade --from-source --global",
		},
		Related: []string{"extensions update", "deps install"},
		Workspace: WorkspaceNeed{
			Required:      true,
			ExemptFlags:   []string{"--global", "-g"},
			OverrideFlags: []string{"--from-source"},
			Note:          "cmdUpgrade: --global retargets ~/.putnami/bin and needs no workspace, but --from-source builds from this workspace's source and needs one even with --global",
		},
	},
	{
		Path:        "pin",
		Kind:        KindStructured,
		Category:    "Workspace",
		Summary:     "Pin the CLI version this workspace uses",
		Description: "Pin the putnami CLI version this workspace uses, recording its per-platform digest in putnami.lock.json so `putnami` resolves and verifies the exact binary",
		Usage:       "putnami pin [<version>] [--remove]",
		Flags: []Flag{
			{Long: "--remove", Description: "Remove the CLI version pin"},
		},
		Positionals: []Positional{{Name: "version"}},
		Examples: []string{
			"putnami pin 1.2.3",
			"putnami pin",
			"putnami pin --remove",
		},
		Related:   []string{"upgrade", "version get"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:       "workspace",
		Kind:       KindStructured,
		Summary:    "Manage workspace",
		DefaultSub: "describe",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "workspace init",
		Kind:        KindStructured,
		Description: "Initialize a new workspace",
		Usage:       "putnami init [--workspace <name>] [--extension <ts|go|py>] [--project <name>] [--project-path <path>] [--channel <channel>] [--force]",
		Flags: []Flag{
			{Long: "--workspace", Type: FlagValue, ValueName: "<name>", Description: "Workspace name override"},
			{Long: "--extension", Type: FlagValue, ValueName: "<ts|go|py>", Description: "Default extension to install"},
			{Long: "--project", Type: FlagValue, ValueName: "<name>", Description: "Create an initial project"},
			{Long: "--project-path", Type: FlagValue, ValueName: "<path>", Description: "Workspace-relative path for the initial project"},
			{Long: "--channel", Type: FlagValue, ValueName: "<channel>", Description: "Resolve the extensions, the template and the starter dependencies on one release channel (default: PUTNAMI_CHANNEL, then the channel the CLI was installed from, then latest)"},
			{Long: "--force", Description: "Reinitialize an existing workspace"},
		},
		Examples: []string{
			"putnami init",
			"putnami init --project my-app",
			"putnami init --project my-app --project-path apps/my-app",
			"putnami init --project my-app --channel canary",
		},
		Related: []string{"extensions install", "deps install", "context generate"},
		Workspace: WorkspaceNeed{
			Note: "cmdWorkspace routes init to the same handler as cmdInit, which creates the workspace",
		},
	},
	{
		Path:        "workspace describe",
		Kind:        KindStructured,
		Category:    "Workspace",
		Summary:     "Describe the active workspace",
		Description: "Confirm the active workspace and show its name, version, project count, extension count, and configuration summary before selecting work",
		Usage:       "putnami workspace describe [--output <format>]",
		Examples: []string{
			"putnami workspace describe",
			"putnami workspace describe --output=jsonl",
		},
		Related:          []string{"projects list", "extensions list", "config show"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "version",
		Kind:        KindStructured,
		Category:    "Workspace",
		Label:       "version get|tag|list|use",
		Summary:     "Versions derived from git, release tags, and installed CLI binaries",
		DefaultSub:  "get",
		Description: "Show the version each line is at (get), release a line (tag), and manage the installed CLI binaries (list/use)",
		Usage:       "putnami version <get|tag|list|use>",
		Positionals: []Positional{{Name: "get|tag|list|use", Required: true}},
		Examples: []string{
			"putnami version get",
			"putnami version tag --scope typescript --dry-run",
			"putnami version tag --scope tooling --yes --push",
			"putnami version list --global",
			"putnami version use go-dev",
		},
		Workspace: requiredWorkspace(),
		Recovery:  true,
	},
	{
		Path:        "version get",
		Kind:        KindStructured,
		Description: "Show the version every version line is at, derived from its git tags",
		Usage:       "putnami version get [--scope <line>] [--output <format>]",
		Flags: []Flag{
			{Long: "--scope", Type: FlagValue, ValueName: "<line>", Description: "Report only this version line, by its scope path"},
		},
		Examples: []string{
			"putnami version get",
			"putnami version get --scope typescript",
			"putnami version get --output=jsonl",
		},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "version tag",
		Kind:        KindStructured,
		Description: "Release a version line: regenerate its changelog, create the release commit, then the annotated tag on it",
		Usage:       "putnami version tag [<tag>] [--scope <line>] [--push] [--dry-run] [--yes]",
		Flags: []Flag{
			{Long: "--scope", Type: FlagValue, ValueName: "<line>", Description: "The version line to release, by its scope path (required when the workspace declares several)"},
			{Long: "--push", Description: "Push the release commit and the tag to origin"},
			{Long: "--dry-run", Description: "Print the proposed tag and changelog without writing anything"},
			{Long: "--yes", Description: "Skip the confirmation and create the release commit and tag"},
		},
		Positionals: []Positional{{Name: "tag"}},
		Examples: []string{
			"putnami version tag --scope typescript --dry-run",
			"putnami version tag --scope tooling --yes --push",
			"putnami version tag tooling/v0.3.0 --scope tooling --yes",
		},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "version list",
		Kind:        KindStructured,
		Description: "List the installed putnami CLI binaries in .putnami/bin (or ~/.putnami/bin with --global) and mark the active one",
		Usage:       "putnami version list [--global]",
		Flags: []Flag{
			{Long: "--global", Short: "-g", Description: "List binaries in ~/.putnami/bin instead of the workspace (alias: -g)"},
		},
		Examples: []string{
			"putnami version list",
			"putnami version list --global",
		},
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--global", "-g"},
			Note:        "cmdVersion/cliBinDir: --global lists ~/.putnami/bin, which exists outside any workspace",
		},
	},
	{
		Path:        "version use",
		Kind:        KindStructured,
		Description: "Switch the active putnami CLI binary to an installed putnami-<name> (e.g. one built by `upgrade --from-source`)",
		Usage:       "putnami version use <name> [--global]",
		Flags: []Flag{
			{Long: "--global", Short: "-g", Description: "Switch the binary in ~/.putnami/bin instead of the workspace (alias: -g)"},
		},
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami version use go-dev",
			"putnami version use go-source-1a2b3c4 --global",
		},
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--global", "-g"},
			Note:        "cmdVersion/cliBinDir: --global switches the binary in ~/.putnami/bin, which exists outside any workspace",
		},
	},
	{
		Path:       "infra",
		Kind:       KindStructured,
		Summary:    "Plan workspace infrastructure",
		DefaultSub: "plan",
		DetailFrom: "infra plan",
		Related:    []string{"infra plan", "workspace describe", "projects list"},
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "infra plan",
		Kind:        KindStructured,
		Category:    "Workspace",
		Summary:     "Plan workspace infra (--output=jsonl)",
		Description: "Show the aggregated infrastructure plan inferred from project descriptors",
		Usage:       "putnami infra plan [--output <format>]",
		Examples: []string{
			"putnami infra plan",
			"putnami infra plan --output=jsonl",
		},
		Related:          []string{"workspace describe", "projects list"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Extensions & Templates ───────────────────────────────────────────
	{
		Path:       "extensions",
		Kind:       KindStructured,
		Summary:    "Manage extensions",
		DefaultSub: "install",
		Workspace:  userScopeWorkspace(),
		Recovery:   true,
	},
	{
		Path:        "extensions install",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Install extensions from config",
		Description: "Install extensions from workspace configuration and run extension install hooks",
		Usage:       "putnami extensions install [name] [--latest] [--user] [--platform <os/arch>] [--dest <dir>] [--output <format>]",
		Flags: []Flag{
			{Long: "--latest", Description: "Ignore the lock file and resolve the latest versions"},
			userScopeFlag("Pin one registry extension in the user scope (~/.putnami/user) so its commands run outside any workspace"),
			{
				Long:        "--platform",
				Type:        FlagValue,
				ValueName:   "<os/arch>",
				Description: "Materialize artifacts for another platform (skips install hooks and the lock write)",
			},
			{
				Long:        "--dest",
				Type:        FlagValue,
				ValueName:   "<dir>",
				Description: "Materialize into this artifact-store root instead of the machine-global one",
			},
		},
		Examples: []string{
			"putnami extensions install",
			"putnami extensions install --output=jsonl",
			"putnami extensions install --platform linux/amd64 --dest ./.gen/warm-artifacts",
			"putnami extensions install --user @acme/audit",
		},
		Related:          []string{"extensions list", "extensions update", "deps install"},
		StructuredOutput: true,
		Workspace:        userScopeWorkspace(),
		Recovery:         true,
	},
	{
		Path:        "extensions list",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "List installed extensions",
		Description: "List installed extensions with their jobs and activation files",
		Usage:       "putnami extensions list [--user] [--output <format>]",
		Flags: []Flag{
			userScopeFlag("List the extensions pinned in the user scope (~/.putnami/user) instead of the workspace"),
		},
		Examples: []string{
			"putnami extensions list",
			"putnami extensions list --output=jsonl",
			"putnami extensions list --user",
		},
		Related:          []string{"extensions install", "extensions update", "projects list"},
		StructuredOutput: true,
		Workspace:        userScopeWorkspace(),
	},
	{
		Path:        "extensions update",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Update extensions",
		Description: "Update extensions to latest compatible versions",
		Usage:       "putnami extensions update [name] [--output <format>]",
		Examples: []string{
			"putnami extensions update",
			"putnami extensions update --output=jsonl",
		},
		Related:          []string{"extensions install", "extensions list"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "extensions remove",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Remove an extension",
		Description: "Remove an installed extension",
		Usage:       "putnami extensions remove <name> [--user]",
		Flags: []Flag{
			userScopeFlag("Remove the extension from the user scope (~/.putnami/user) instead of the workspace"),
		},
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami extensions remove @putnami/go",
			"putnami extensions remove @acme/audit --user",
		},
		Related:   []string{"extensions list", "extensions install"},
		Workspace: userScopeWorkspace(),
	},
	{
		Path:       "templates",
		Kind:       KindStructured,
		Summary:    "Manage templates",
		DefaultSub: "install",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "templates install",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Install templates from config",
		Description: "Install templates from workspace configuration",
		Usage:       "putnami templates install [name] [--latest] [--output <format>]",
		Flags: []Flag{
			{Long: "--latest", Description: "Ignore the lock file and resolve the latest versions"},
		},
		Positionals: []Positional{{Name: "name"}},
		Examples: []string{
			"putnami templates install",
			"putnami templates install go-server",
			"putnami templates install --output=jsonl",
		},
		Related:          []string{"templates list", "templates update", "deps install"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "templates list",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "List installed templates",
		Description: "List configured and discovered templates",
		Usage:       "putnami templates list [--output <format>]",
		Examples: []string{
			"putnami templates list",
			"putnami templates list --output=jsonl",
		},
		Related:          []string{"templates install", "templates update"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "templates update",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Update templates",
		Description: "Update templates to latest compatible versions",
		Usage:       "putnami templates update [name] [--output <format>]",
		Examples: []string{
			"putnami templates update",
			"putnami templates update --output=jsonl",
		},
		Related:          []string{"templates install", "templates list"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "templates remove",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Remove a template",
		Description: "Remove an installed template",
		Usage:       "putnami templates remove <name>",
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami templates remove go-server",
		},
		Related:   []string{"templates list", "templates install"},
		Workspace: requiredWorkspace(),
	},

	// ── Dependencies ─────────────────────────────────────────────────────
	{
		Path:       "deps",
		Kind:       KindStructured,
		Summary:    "Manage dependencies",
		DefaultSub: "install",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "deps install",
		Kind:        KindStructured,
		Category:    "Dependencies",
		Summary:     "Install project dependencies",
		Description: "Install project dependencies by delegating to extension-provided installers",
		Usage:       "putnami deps install [--tag <tags>] [--exclude-tag <tags>]",
		Examples: []string{
			"putnami deps install",
			"putnami deps install --tag go",
		},
		Related:   []string{"extensions install", "upgrade"},
		Workspace: requiredWorkspace(),
	},
	// `deps add` and `deps remove` are dispatcher paths (cmdDeps) that carry no
	// help of their own. They are cataloged without a Description so shell
	// completion keeps offering them — before A1b they lived only in
	// completion's hand-maintained subcommand map — while the man/Markdown
	// reference, which renders documented paths, stays unchanged. Writing their
	// help is a documentation change, not a completion change, so it is not this
	// slice's; internal/commands' completionSubcommandsWithoutHelp records the
	// gap until then.
	{
		Path:      "deps add",
		Kind:      KindStructured,
		Workspace: requiredWorkspace(),
	},
	{
		Path:      "deps remove",
		Kind:      KindStructured,
		Workspace: requiredWorkspace(),
	},
	{
		Path:       "cache",
		Kind:       KindStructured,
		Summary:    "Manage cache",
		DefaultSub: "clean",
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--all"},
			Note:        "a bare `cache` runs `cache clean`, whose --all form wipes the machine-global store",
		},
	},
	{
		Path:        "cache clean",
		Kind:        KindStructured,
		Category:    "Dependencies",
		Summary:     "Clear this repo's shared build cache",
		Description: "Delete cached build artifacts from this repo's shared store (affects all worktrees of the repo); --all clears every repo's store",
		Usage:       "putnami cache clean [--all]",
		Examples: []string{
			"putnami cache clean",
			"putnami cache clean --all",
		},
		Related: []string{"build", "test"},
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--all"},
			Note:        "cmdCache: --all (the global flag) targets every repo's store, so it needs no workspace; a bare clean targets this repo's",
		},
	},
	{
		Path:        "cache gc",
		Kind:        KindStructured,
		Category:    "Dependencies",
		Summary:     "Garbage-collect global caches to their budgets",
		Description: "Garbage-collect the machine-global build, Go, Bun, and binary stores down to their byte budgets",
		Usage:       "putnami cache gc",
		Examples: []string{
			"putnami cache gc",
		},
		Workspace: WorkspaceNeed{
			Note: "cmdCache: gc acts on the machine-global stores only",
		},
	},
	{
		Path:        "cache verify",
		Kind:        KindStructured,
		Category:    "Dependencies",
		Summary:     "Verify cache determinism and restore equivalence",
		Description: "Run lint, test, and build twice in isolated worktrees, compare cache keys and captured artifacts, restore the first run into a pristine tree, and fail on undeclared writes or live-versus-hit differences; declared ambient key inputs are reported without failing",
		Usage:       "putnami cache verify [--projects <selector> | --impacted | --all] [--output <format>]",
		Examples: []string{
			"putnami cache verify --projects /my/project",
			"putnami cache verify --all --output=jsonl",
		},
		Related:          []string{"cache clean", "build", "test", "lint"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Configuration ────────────────────────────────────────────────────
	{
		Path:       "config",
		Kind:       KindStructured,
		Summary:    "Manage configuration",
		DefaultSub: "show",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "config show",
		Kind:        KindStructured,
		Category:    "Configuration",
		Summary:     "Show effective configuration",
		Description: "Display the fully merged configuration (global + workspace + local scopes)",
		Usage:       "putnami config show [--output <format>]",
		Examples: []string{
			"putnami config show",
			"putnami config show --output=jsonl",
		},
		Related:          []string{"config set", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "config set",
		Kind:        KindStructured,
		Category:    "Configuration",
		Summary:     "Set a configuration value",
		Description: "Set a configuration value in the workspace config",
		Usage:       "putnami config set <key> <value>",
		Positionals: []Positional{
			{Name: "key", Required: true},
			{Name: "value", Required: true},
		},
		Examples: []string{
			"putnami config set maxParallel 4",
		},
		Related:   []string{"config show"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:       "context",
		Kind:       KindStructured,
		Summary:    "Manage AI context",
		DefaultSub: "generate",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:        "context generate",
		Kind:        KindStructured,
		Category:    "Configuration",
		Summary:     "Regenerate AI context",
		Description: "Regenerate the Putnami guidance block in CLAUDE.md and AGENTS.md, and the agent skills the workspace declares by in-tree path. Bytes outside the block are never touched; an edited block is preserved and reported, and --force rewrites that block only. No other file is written: run `putnami mcp install` to register the MCP server",
		Usage:       "putnami context generate [--force]",
		Flags: []Flag{
			{Long: "--force", Description: "Rewrite a guidance block that was edited or written by an older release"},
		},
		Examples: []string{
			"putnami context generate",
			"putnami context generate --force",
		},
		Related:   []string{"extensions install", "workspace init"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "context map",
		Kind:        KindStructured,
		Category:    "Configuration",
		Summary:     "Build or print the workspace orientation map",
		Description: "Regenerate the ephemeral workspace map under .putnami/context-map/ (repo-map.json and repo-map.md) from committed artifacts (project manifests, schema/openapi.json, schema/config.jsonschema.json, READMEs, and the workspace docs tree). Byte-deterministic on an unchanged tree, gitignored, and never committed. --print renders it to stdout and writes nothing. `putnami build` refreshes it automatically outside CI; set PUTNAMI_CONTEXT_MAP=off|write to override. Agents get the same document fresh from the MCP workspace_map tool",
		Usage:       "putnami context map [--print[=json|md]] [--projects <list>] [--impacted] [--output <format>]",
		Flags: []Flag{
			{Long: "--print", Description: "Render the map to stdout (md by default, or json) and write nothing"},
		},
		Examples: []string{
			"putnami context map",
			"putnami context map --print",
			"putnami context map --print=json",
			"putnami context map --impacted",
		},
		Related:          []string{"context generate", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Diagnostics ──────────────────────────────────────────────────────
	{
		Path:        "doctor",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Preflight production readiness under a deployment profile",
		Description: "Run a read-only production-readiness preflight over the selected projects: derives production-safety findings (incomplete capabilities, missing required config, invalid committed schemas, config shadowing) from committed manifests and grades them by deployment profile. Under --profile production a high/critical finding fails with exit 2; dev/test stay advisory. Config values are never read or emitted",
		Usage:       "putnami doctor [--profile <dev|test|production>] [--project <selector>] [--output <format>]",
		Flags: []Flag{
			{Long: "--profile", Type: FlagValue, ValueName: "<profile>", Description: "Deployment profile to evaluate under (dev|test|production); production is strictest"},
			{Long: "--project", Type: FlagValue, ValueName: "<selector>", Description: "Project ID, path, or name (comma-separated); default is every workspace project"},
		},
		Examples: []string{
			"putnami doctor",
			"putnami doctor --profile production",
			"putnami doctor --profile production --project /tooling/cli",
			"putnami doctor --profile production --output=jsonl",
		},
		Related:          []string{"infra plan", "config show", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:       "sessions",
		Kind:       KindStructured,
		Summary:    "Inspect execution sessions",
		DefaultSub: "list",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:    "tree",
		Kind:    KindStructured,
		Summary: "Identify the worktree the CLI runs in and check evidence against it",
		Workspace: WorkspaceNeed{
			Note: "cmdTree reads git alone and is run inside repositories that are not workspaces",
		},
	},
	{
		Path:        "tree fingerprint",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Print the digest identifying this worktree's exact state",
		Description: "Print one digest covering HEAD, the tracked diff against it, and the content of every untracked non-ignored file. It exists because \"the same files are dirty\" is not \"the same bytes are on disk\": git status reports paths and status codes, so an agent that gates a tree and then edits a file it had already dirtied leaves that output identical. Human output is the bare digest; --output=json adds dirty and headSHA, the same members a recorded session's tree block carries. Reads the repository and writes nothing, and needs no workspace",
		Usage:       "putnami tree fingerprint [--output <format>]",
		Examples: []string{
			"putnami tree fingerprint",
			"putnami tree fingerprint --output=json",
		},
		Related:          []string{"sessions inspect", "sessions export"},
		StructuredOutput: true,
		Workspace: WorkspaceNeed{
			Note: "cmdTree reads git alone and is run inside repositories that are not workspaces",
		},
	},
	{
		Path:        "tree verify",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Check local workflow evidence against this worktree",
		Description: "Check the local evidence of an execute run against the worktree it runs in, and print one JSON verdict. --record verifies a dossier: its tree binding, changed files, policy files, consulted scopes, independent review, gate, qualification verdicts and acceptance evidence. --snapshot prints the dossier skeleton for the current tree, --ref prints a file reference with its SHA-256, and --gate checks that a recorded gate session and report can replace the gate the PR finalizer would run. A gate must cover the native impacted plan, which the workspace CLI of the checked repository answers as a dry run. Paths resolve against the Git top level. A failure prints {\"verdict\":\"not-verified\",\"reason\":...} on one line and exits 1 with nothing on stderr; --output does not change the document. It is a consistency check, not a signed review or an access boundary, and it writes nothing",
		Usage:       "putnami tree verify (--record <file> | --snapshot | --ref <file> | --gate <session> --report <report>) [--base <rev>]",
		Flags: []Flag{
			{Long: "--record", Type: FlagValue, ValueName: "<file>", Description: "Verify the dossier this file holds and print the verified verdict"},
			{Long: "--snapshot", Type: FlagBool, Description: "Print the dossier skeleton for the current tree"},
			{Long: "--ref", Type: FlagValue, ValueName: "<file>", Description: "Print the reference to this file: its path as given and its SHA-256"},
			{Long: "--gate", Type: FlagValue, ValueName: "<session>", Description: "Check that the recorded gate session can replace the finalizer's gate"},
			{Long: "--report", Type: FlagValue, ValueName: "<report>", Description: "Session report of the --gate session"},
			{Long: "--base", Type: FlagValue, ValueName: "<rev>", Description: "Base revision; default origin/main for --snapshot and --gate, and for --record the base the dossier must name"},
		},
		Examples: []string{
			"putnami tree verify --snapshot --base origin/main",
			"putnami tree verify --record .context/dossier.json",
			"putnami tree verify --gate .putnami/sessions/<id>/session.json --report .putnami/reports/<id>.json",
		},
		Related:          []string{"tree fingerprint", "sessions inspect"},
		StructuredOutput: true,
		Workspace: WorkspaceNeed{
			Note: "cmdTree reads git and evidence files and is run inside repositories that are not workspaces; only the native plan a gate is checked against comes from the workspace CLI",
		},
	},
	{
		Path:        "compose",
		Kind:        KindStructured,
		Category:    "Execution",
		Summary:     "Serve a workload with the workloads it runs with",
		Description: "Start the selected workload and the transitive closure of its runsWith projects in one invocation. Every process binds an ephemeral port and answers behind a local reverse proxy whose URL stays stable across restarts; dependency URLs travel through the clients config block and databases declared in infra/requirements.json are provisioned per composition. Dependencies run production-mode without watch; the target watches by default. Stops every owned process, proxy and database on exit; the next invocation reaps what an ungraceful death left behind",
		Usage:       "putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>] [--output <format>]",
		Positionals: []Positional{{Name: "project", Required: true}},
		Flags: []Flag{
			{Long: "--port", Type: FlagValue, ValueName: "<n>", Description: "Proxy port of the target; default options.serve.port then 3000; 0 picks an ephemeral port"},
			{Long: "--no-watch", Type: FlagBool, Description: "Run the target production-mode without watch, like its dependencies"},
			{Long: "--ready-timeout", Type: FlagValue, ValueName: "<duration>", Description: "Deadline for each member's typed ready event; default 60s"},
		},
		Examples:         []string{"putnami compose @example/go-items-consumer", "putnami compose /typescript/samples/06-database --port 0 --output=json"},
		Related:          []string{"serve", "qualify"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "qualify",
		Kind:        KindStructured,
		Category:    "Execution",
		Summary:     "Run a workload's derived smoke contract against a target and emit one verdict",
		Description: "Derive the smoke contract from the workload's route inventory, execute it against --target (local composes the workload with compose; a URL is any deployed environment), and print one verdict bound to the tested tree or to the deployed sha. Fails closed: unsupported, not_run, timed_out, canceled, target_unreachable, digest_mismatch and composition_failed are distinct from passed and exit non-zero",
		Usage:       "putnami qualify <project> --target <local|url> [--expect-sha <sha>] [--platform-prefix <path>] [--ready-timeout <duration>] [--request-timeout <duration>] [--print-contract] [--output <format>]",
		Flags: []Flag{
			{Long: "--target", Type: FlagValue, ValueName: "<local|url>", Description: "local composes the workload on this machine; an http(s) URL is a running deployment"},
			{Long: "--expect-sha", Type: FlagValue, ValueName: "<sha>", Description: "Required with a URL target: the git sha the deployment must report on /version"},
			{Long: "--platform-prefix", Type: FlagValue, ValueName: "<path>", Description: "Prefix under which /readyz and /version are mounted; default: where the route inventory declares both, else the root"},
			{Long: "--ready-timeout", Type: FlagValue, ValueName: "<duration>", Description: "Deadline for readiness; default 60s"},
			{Long: "--request-timeout", Type: FlagValue, ValueName: "<duration>", Description: "Deadline per smoke request; default 5s"},
			{Long: "--print-contract", Type: FlagBool, Description: "Print the derived contract and exit without a target"},
		},
		Positionals:      []Positional{{Name: "project", Required: true}},
		Examples:         []string{"putnami qualify /go/samples/migrations-feature --target local", "putnami qualify @example/06-database --target https://pr-123.preview.example --expect-sha 3cc91b658", "putnami qualify /go/samples/migrations-feature --print-contract --output=json"},
		Related:          []string{"compose", "tree fingerprint"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "sessions list",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "List recent execution sessions",
		Description: "Show recent execution sessions with timing and status. --revision keeps the sessions whose recorded tree sat on the named commit (a lowercase hex prefix of its id, 7 to 64 characters) and adds the head commit and the actual placement to each row, so a new session can find what judged a given revision; a session that recorded no tree never matches. --output=jsonl rows carry revision and placement when the record states them",
		Usage:       "putnami sessions list [--revision <sha>] [--output <format>]",
		Flags: []Flag{
			{Long: "--revision", Type: FlagValue, ValueName: "<sha>", Description: "Keep only sessions whose recorded tree sat on this commit; a lowercase hex prefix of the commit id, never a ref name"},
		},
		Examples: []string{
			"putnami sessions list",
			"putnami sessions list --revision 7194fc3537",
			"putnami sessions list --output=jsonl",
		},
		Related:          []string{"sessions inspect"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "sessions inspect",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Inspect a specific session",
		Description: "Show full details and event stream for a specific session. --run names a remote attempt instead of a session: the attempt reference or submission key printed when a --where remote run was submitted, resolved through the durable attempt records under .putnami/runner/attempts. An attempt whose session is already imported is shown from the local store; one that is not (the CLI died mid-run, the import failed) is resumed through the workspace's runner provider from the persisted cursor and imported through the same path, and nothing is ever resubmitted. Ctrl-C during a resumed follow cancels the attempt within bounded time",
		Usage:       "putnami sessions inspect [<id>] [--run <ref>] [--output <format>]",
		Positionals: []Positional{{Name: "id"}},
		Flags: []Flag{
			{Long: "--run", Type: FlagValue, ValueName: "<ref>", Description: "Inspect the session of a remote attempt by its attempt reference or submission key, resuming the attempt first when it was never imported"},
		},
		Examples: []string{
			"putnami sessions inspect abc123",
			"putnami sessions inspect latest",
			"putnami sessions inspect --run fixture-attempt-42",
		},
		Related:          []string{"sessions list"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "sessions replay",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Resume delivery of a retained execution session",
		Description: "Replay retained session artifacts through each explicitly selected reporter extension (session-reporter, log-reporter) that has not delivered them. Each resumes from its own acknowledgement cursor with fresh reporter credentials, without running graph work. Incomplete delivery returns an error and keeps the bounded checkpoint for another replay",
		Usage:       "putnami sessions replay --session <id>",
		Flags: []Flag{
			{Long: "--session", Type: FlagValue, ValueName: "<id>", Description: "Exact retained session id; latest is not accepted"},
		},
		Examples:         []string{"putnami sessions replay --session 20260914-120000-abc123"},
		Related:          []string{"sessions inspect", "sessions export"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "sessions export",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Export recorded sessions as JSONL",
		Description: "Stream every session document still present under .putnami/sessions, one per line, oldest first, deduplicated by session id. Each line is the recorded document forwarded verbatim (only whitespace is folded), so every line declares its own protocolVersion and a record written by a pre-v2 CLI keeps the version-1 shape. A session with no session.json, and a document that does not parse, are skipped rather than failing the command. The store is pruned and a worktree is disposable, so this is how per-run CPU, task counts and cache reuse leave a worktree before it disappears",
		Usage:       "putnami sessions export [--since <timestamp>] [--output <format>]",
		Flags: []Flag{
			{Long: "--since", Type: FlagValue, ValueName: "<timestamp>", Description: "Keep only records whose recorded start time is at or after this RFC 3339 timestamp; a record with no readable start time is not emitted"},
		},
		Examples: []string{
			"putnami sessions export > sessions.jsonl",
			"putnami sessions export --since 2026-09-01T00:00:00Z",
		},
		Related:          []string{"sessions list", "sessions inspect", "sessions summary"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "sessions summary",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Summarize recorded sessions, one row per run",
		Description: "Reduce every session this worktree recorded to one row: session id, start time, commands, selection, task total/executed/reused, run.cpu.actualMs, wall and outcome. It shares `sessions export`'s ordering and deduplication, so a store reached through a worktree symlink still counts each run once. A run a task spawned — a validation guard that builds what it checks is the canonical case — carries the session that spawned it and is marked nested, so it is never mistaken for a second gate. --command keeps TOP-LEVEL sessions whose recorded command set is EXACTLY the one named, order-insensitive: a nested run never matches whatever it ran, so a guard that ran the full gate cannot be counted as a second gate. --by-digest regroups the selected sessions' task records by their input digest — the cache key each task was keyed on — with how many records carry it and how many executed, reused or failed, so a reader tells why a task ran without re-running it. With --output=json the rows travel as the shared result envelope's data array at every count — an empty result is an empty array, and a single result is not a bare object",
		Usage:       "putnami sessions summary [--since <timestamp>] [--command <list>] [--by-digest] [--output <format>]",
		Flags: []Flag{
			{Long: "--since", Type: FlagValue, ValueName: "<timestamp>", Description: "Keep only records whose recorded start time is at or after this RFC 3339 timestamp; a record with no readable start time is not listed"},
			{Long: "--command", Type: FlagValue, ValueName: "<list>", Description: "Keep only top-level sessions whose recorded command set is exactly this comma-separated list, in any order; a nested run never matches"},
			{Long: "--by-digest", Type: FlagBool, Description: "Group the selected sessions' task records by input digest: records per digest, and how many executed, reused or failed"},
		},
		Examples: []string{
			"putnami sessions summary",
			"putnami sessions summary --since 2026-09-13T00:00:00Z",
			"putnami sessions summary --command lint,test,build,validate",
			"putnami sessions summary --by-digest",
			"putnami sessions summary --output=json",
		},
		Related:          []string{"sessions list", "sessions export"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Reports ──────────────────────────────────────────────────────────
	{
		Path:        "report",
		Kind:        KindStructured,
		Category:    "Diagnostics",
		Summary:     "Read back a run's report",
		Description: "Print the bounded synthesis a finished run recorded under .putnami/reports: the newest CLI-driven run's by default, or the one named by --session. With --output=json the recorded document is printed as-is, unwrapped, because it is the contract a consumer binds to. An interrupted run records no report, so a consumer that files a report against a commit verifies its git.sha rather than trusting recency",
		Usage:       "putnami report [--session <id>] [--output <format>]",
		Flags: []Flag{
			{Long: "--session", Type: FlagValue, ValueName: "<id>", Description: "Report to read, by session ID (default: the newest CLI-driven run's)"},
		},
		Examples: []string{
			"putnami report",
			"putnami report --session 20260723-002049-9108d9",
			"putnami report --output=json",
		},
		Related:          []string{"sessions list", "sessions inspect"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Migration ────────────────────────────────────────────────────────
	{
		Path:    "migrate",
		Kind:    KindStructured,
		Summary: "Migrate Putnami workspace files forward",
		Workspace: WorkspaceNeed{
			Note: "cmdMigrate falls back to os.Getwd(): vnext migrates the lock file found at the workspace root or the current directory",
		},
	},
	{
		Path:        "migrate vnext",
		Kind:        KindStructured,
		Category:    "Migration",
		Summary:     "Migrate putnami.lock.json to the current format",
		Description: "Check or atomically migrate putnami.lock.json to the current format while preserving pins and recording any missing installed extension task-contract metadata",
		Usage:       "putnami migrate vnext [--check] [--apply] [--output <format>]",
		Flags: []Flag{
			{Long: "--check", Description: "Report the pending migration without writing (default); exits non-zero when a migration is needed"},
			{Long: "--apply", Description: "Perform the migration, atomically and idempotently"},
		},
		Examples: []string{
			"putnami migrate vnext --check",
			"putnami migrate vnext --check --output=json",
			"putnami migrate vnext --apply",
		},
		Related:          []string{"extensions install", "extensions list"},
		StructuredOutput: true,
		Workspace: WorkspaceNeed{
			Note: "cmdMigrate falls back to os.Getwd(): vnext migrates the lock file found at the workspace root or the current directory",
		},
	},
	{
		Path:        "migrate agent-content",
		Kind:        KindStructured,
		Category:    "Migration",
		Summary:     "Move superseded agent artifacts to an extension's agent content",
		Description: "Check, apply or roll back the move of separately declared agent artifacts that an installed extension's agent content supersedes: their declarations, pins and ownership records move to the extension, and every file the content adds, changes or removes is listed before anything is written",
		Usage:       "putnami migrate agent-content <extension> [--check] [--apply] [--rollback] [--output <format>]",
		Positionals: []Positional{{Name: "extension", Required: true}},
		Flags: []Flag{
			{Long: "--check", Description: "Plan the migration without writing (default); exits non-zero when a migration is pending"},
			{Long: "--apply", Description: "Perform the migration, or finish one that stopped"},
			{Long: "--rollback", Description: "Restore the declarations, pins, ownership records and files recorded before the migration"},
		},
		Examples: []string{
			"putnami migrate agent-content @putnami/contributor",
			"putnami migrate agent-content @putnami/contributor --apply",
			"putnami migrate agent-content @putnami/contributor --rollback",
		},
		Related:          []string{"extensions install", "install", "upgrade"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},

	// ── Authoring ────────────────────────────────────────────────────────
	{
		Path:    "dev",
		Kind:    KindStructured,
		Summary: "Author extensions and templates",
		Workspace: WorkspaceNeed{
			Note: "cmdDev validates and packages authored artifacts from a path, with no workspace lookup",
		},
	},
	{
		Path:        "dev extension",
		Kind:        KindStructured,
		Category:    "Authoring",
		Label:       "dev extension validate",
		Summary:     "Validate an extension manifest",
		Description: "Develop extensions: validate manifests against the protocol",
		Usage:       "putnami dev extension validate [path]",
		Positionals: []Positional{{Name: "path"}},
		Examples: []string{
			"putnami dev extension validate",
			"putnami dev extension validate ./my-extension/",
		},
		Related: []string{"dev template", "extensions install"},
		Workspace: WorkspaceNeed{
			Note: "cmdDev validates an authored manifest from a path, with no workspace lookup",
		},
	},
	{
		Path:        "dev template",
		Kind:        KindStructured,
		Category:    "Authoring",
		Label:       "dev template <action>",
		Summary:     "Validate, test, or package a template",
		Description: "Develop and package templates: validate manifests, test rendering output, or package for distribution",
		Usage:       "putnami dev template <validate|test|package> [path] [--skip-build] [--keep] [--version <version>] [--stable]",
		Flags: []Flag{
			{Long: "--skip-build", Description: "With test: render the template without running build and test on it"},
			{Long: "--keep", Description: "With test: keep the rendered temporary workspace for inspection"},
			{Long: "--version", Type: FlagValue, ValueName: "<version>", Description: "With package: version to stamp on the produced archive"},
			{Long: "--stable", Description: "With package: publish under a stable version instead of a prerelease"},
		},
		Positionals: []Positional{
			{Name: "validate|test|package", Required: true},
			{Name: "path"},
		},
		Examples: []string{
			"putnami dev template validate",
			"putnami dev template validate ./my-template/",
			"putnami dev template test",
			"putnami dev template package",
		},
		Related: []string{"dev extension", "templates install"},
		Workspace: WorkspaceNeed{
			Note: "cmdDev renders and packages an authored template from a path, with no workspace lookup",
		},
	},

	// ── Agents ───────────────────────────────────────────────────────────
	{
		Path:        "mcp",
		Kind:        KindStructured,
		Category:    "Agents",
		Summary:     "Run the MCP server over stdio for AI agent harnesses",
		Description: fmt.Sprintf("Run the Model Context Protocol server over stdio so an AI agent harness can discover projects, context, ownership, impact, plans, and diagnostics through %d typed core tools (%s) and the workspace://context resource. All except run_jobs are read-only; run_jobs is the only mutating core tool, supports dry-run planning, and refuses long-lived or externally mutating execution. Spawned by the client; speaks JSON-RPC 2.0 on stdin/stdout and exits with the session. Before it serves, it brings the workspace's agent workflows to the version the lock pins, from the local artifact store only, and reports what it cannot reconcile as a warning on stderr. Optional request provenance is disabled by default; set PUTNAMI_AGENT_IDENTITY=1 and explicitly set PUTNAMI_AGENT_MODEL in the MCP server environment to propagate bounded harness/model headers.", len(CoreMCPToolNames()), strings.Join(CoreMCPToolNames(), ", ")),
		Usage:       "putnami mcp [install]",
		Positionals: []Positional{{Name: "install"}},
		Examples: []string{
			"putnami mcp",
			"putnami mcp install",
		},
		Related:   []string{"mcp install", "projects list"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "mcp install",
		Kind:        KindStructured,
		Category:    "Agents",
		Summary:     "Register the MCP server in .mcp.json for agent IDEs",
		Description: "Write the putnami entry into .mcp.json at the workspace root so agent IDEs (Claude Code, Cursor, VS Code) auto-discover the MCP server. init, install and upgrade already add a missing entry; this explicit write is the only one that also rewrites a diverged putnami entry. Merge-aware: other servers and unknown keys are preserved, and an unparseable file is refused untouched.",
		Usage:       "putnami mcp install",
		Examples: []string{
			"putnami mcp install",
		},
		Related:   []string{"mcp", "context generate"},
		Workspace: requiredWorkspace(),
	},

	// ── Other ────────────────────────────────────────────────────────────
	{
		Path:       "telemetry",
		Kind:       KindStructured,
		Category:   "Other",
		Label:      "telemetry on|off|status|show",
		Summary:    "Manage anonymous telemetry",
		DefaultSub: "status",
		Workspace: WorkspaceNeed{
			Note: "cmdTelemetry reads and writes the machine-global consent state, not the workspace",
		},
	},
	{
		Path:        "telemetry on",
		Kind:        KindStructured,
		Description: "Enable anonymous usage telemetry",
		Usage:       "putnami telemetry on",
		Related:     []string{"telemetry off", "telemetry status"},
		Workspace: WorkspaceNeed{
			Note: "cmdTelemetry writes the machine-global consent state, not the workspace",
		},
	},
	{
		Path:        "telemetry off",
		Kind:        KindStructured,
		Description: "Disable anonymous usage telemetry",
		Usage:       "putnami telemetry off",
		Related:     []string{"telemetry on", "telemetry status"},
		Workspace: WorkspaceNeed{
			Note: "cmdTelemetry writes the machine-global consent state, not the workspace",
		},
	},
	{
		Path:        "telemetry status",
		Kind:        KindStructured,
		Description: "Check current telemetry settings",
		Usage:       "putnami telemetry status",
		Related:     []string{"telemetry on", "telemetry off", "telemetry show"},
		Workspace: WorkspaceNeed{
			Note: "cmdTelemetry reads the machine-global consent state, not the workspace",
		},
	},
	{
		Path:             "telemetry show",
		Kind:             KindStructured,
		Description:      "View buffered telemetry events pending flush",
		Usage:            "putnami telemetry show [--output <format>]",
		Related:          []string{"telemetry status"},
		StructuredOutput: true,
		Workspace: WorkspaceNeed{
			Note: "cmdTelemetry reads the machine-global event buffer, not the workspace",
		},
	},
	{
		Path:        "completion",
		Kind:        KindStructured,
		Category:    "Other",
		Label:       "completion <shell>",
		Summary:     "Generate shell completions (bash, zsh, fish)",
		Description: "Generate a shell completion script for bash, zsh, or fish; source it or install it where the shell looks for completions",
		Usage:       "putnami completion <bash|zsh|fish>",
		Positionals: []Positional{{Name: "bash|zsh|fish", Required: true}},
		Examples: []string{
			"putnami completion bash",
			"putnami completion zsh",
			"putnami completion fish",
		},
		Workspace: WorkspaceNeed{
			Note: "cmdCompletion emits a script; workspace-derived values are added only when a workspace happens to be present",
		},
	},
	// The three shells are dispatcher paths (cmdCompletion reads env.Sub), which
	// is why completion offers them as subcommands. They inherit no help: their
	// only documentation is `completion`'s own, and repeating it three times in
	// the man page and Markdown reference would be noise.
	{
		Path: "completion bash",
		Kind: KindStructured,
		Workspace: WorkspaceNeed{
			Note: "cmdCompletion emits a script; a workspace only enriches it",
		},
	},
	{
		Path: "completion zsh",
		Kind: KindStructured,
		Workspace: WorkspaceNeed{
			Note: "cmdCompletion emits a script; a workspace only enriches it",
		},
	},
	{
		Path: "completion fish",
		Kind: KindStructured,
		Workspace: WorkspaceNeed{
			Note: "cmdCompletion emits a script; a workspace only enriches it",
		},
	},
	{
		Path:        "help",
		Kind:        KindStructured,
		Summary:     "Show help for a command",
		Description: "Show help for putnami, for one command, or for the whole CLI in man or Markdown format",
		Usage:       "putnami help [<command>] [<subcommand>] [--man] [--markdown]",
		Flags: []Flag{
			{Long: "--man", Description: "Render the full reference as a man page"},
			{Long: "--markdown", Description: "Render the full reference as Markdown"},
		},
		Positionals: []Positional{
			{Name: "command"},
			{Name: "subcommand"},
		},
		Examples: []string{
			"putnami help",
			"putnami help projects list",
			"putnami help --man",
			"putnami help --markdown",
		},
		Workspace: WorkspaceNeed{
			Note: "cmdHelp renders static metadata and must work outside a workspace",
		},
		Recovery: true,
	},
}

// requiredWorkspace is the plain "needs a workspace" requirement.
func requiredWorkspace() WorkspaceNeed { return WorkspaceNeed{Required: true} }

// userScopeWorkspace is the requirement of the `extensions` paths that take
// --user: the flag moves the command to the user scope, which exists outside
// any workspace.
func userScopeWorkspace() WorkspaceNeed {
	return WorkspaceNeed{
		Required:    true,
		ExemptFlags: []string{"--user"},
		Note:        "cmdExtensions/cmdExtensionsUserScope: --user manages the user scope under ~/.putnami/user, which exists outside any workspace",
	}
}

func userScopeFlag(description string) Flag {
	return Flag{Long: "--user", Description: description}
}

// globalFlags is the CLI-wide flag vocabulary, in help's display order and
// grouped by the category each flag heads. See catalog.go for the type contract.
//
// Conventions in this table:
//
//   - Declaration order is display order, and a category's flags must be
//     contiguous: GlobalFlagCategories groups by first appearance, exactly like
//     Categories does for commands.
//   - Category "" means accepted and completed but not listed in help.
//   - Values enumerate what completion offers for a FlagValue flag. Leave it nil
//     when the value is unbounded (a path, a project selector, a count): an
//     incomplete candidate list reads as "these are the only values".
//   - Since slice A1c this table IS what the parser accepts: internal/cli's
//     parseGlobalFlags reads Long/Short to recognize a token and Type to decide
//     whether it consumes a value, and keeps only the binding to GlobalFlags
//     fields as code. A flag added to the parser and not here is unparseable,
//     and one added here with no binding is accepted and dropped — which is what
//     internal/cli's TestGlobalFlags_CoverTheParser reads flags.go to catch.
var globalFlags = []GlobalFlag{
	{Long: "--verbose", Short: "-v", Description: "Show job results and diagnostics", Category: "Common"},
	{Long: "--debug", Short: "-d", Description: "Stream all events in real-time", Category: "Common"},
	{Long: "--watch", Short: "-w", Description: "Re-run on file changes", Category: "Common"},
	{Long: "--plan", Description: "Show execution plan without running", Category: "Common"},
	{Long: "--help", Short: "-h", Description: "Show help", Category: "Common"},

	{Long: "--all", Description: "All projects", Category: "Project Selection"},
	{Long: "--impacted", Description: "Projects affected by git changes", Category: "Project Selection"},
	{Long: "--projects", Type: FlagValue, ValueName: "<list>", Description: "Comma-separated project names", Category: "Project Selection"},
	{Long: "--tag", Type: FlagValue, ValueName: "<tags>", Description: "Filter by tags", Category: "Project Selection"},
	{Long: "--exclude-tag", Type: FlagValue, ValueName: "<tags>", Description: "Exclude by tags", Category: "Project Selection"},
	{Long: "--exclude", Type: FlagValue, ValueName: "<names>", Description: "Exclude by name", Category: "Project Selection"},
	{Long: "--baseline", Type: FlagValue, ValueName: "<branch>", Description: "Custom git baseline for --impacted", Category: "Project Selection"},
	{Long: "--impacted-strict", Description: "Fail when baseline can't be resolved", Category: "Project Selection"},

	{Long: "--where", Type: FlagValue, ValueName: "<placement>", Description: "Execution placement: local | remote (local when no runner provider is installed)", Category: "Execution", Values: []string{"local", "remote"}},
	{Long: "--providers", Type: FlagValue, ValueName: "<list>", Description: "Credentials to ask the credential provider for: install, publish (comma list)", Category: "Execution", Values: []string{"install", "publish"}},
	{Long: runcredential.Flag, Type: FlagValue, ValueName: "<n>", Description: runcredential.FlagDescription, Category: "Execution"},
	{Long: "--no-cache", Description: "Force re-execution (skip cache)", Category: "Execution"},
	{Long: "--no-cache-projects", Type: FlagValue, ValueName: "<list>", Description: "Force re-execution of these projects only", Category: "Execution"},
	{Long: "--cache-trust", Type: FlagValue, ValueName: "<policy>", Description: "Remote cache trust: ci | any | none", Category: "Execution", Values: []string{"ci", "any", "none"}},
	{Long: "--continue-on-error", Description: "Don't abort on first failure", Category: "Execution"},
	{Long: "--max-parallel", Type: FlagValue, ValueName: "<mode|n>", Description: "Parallelism: auto | eco | max | number", Category: "Execution", Values: []string{"auto", "eco", "max"}},
	{Long: "--resource", Type: FlagValue, ValueName: "<name=units>", Description: "Budget for a named shared resource (repeatable)", Category: "Execution"},
	{Long: "--cpu-policy", Type: FlagValue, ValueName: "<policy>", Description: "CPU ceiling policy: critical-path | measured", Category: "Execution", Values: []string{"critical-path", "measured"}},
	{Long: "--retry", Type: FlagValue, ValueName: "<n>", Description: "Retry transient failures", Category: "Execution"},
	{Long: "--retry-failed", Description: "Re-run tasks whose identical failure is cached", Category: "Execution"},
	{Long: "--profile", Type: FlagValue, ValueName: "<profile>", Description: "Deployment profile: dev | test | production", Category: "Execution", Values: []string{"dev", "test", "production"}},
	{Long: "--dry-run", Description: "Show changes without applying", Category: "Execution"},

	{Long: "--output", Type: FlagValue, ValueName: "<format>", Description: "Output format: text | json | jsonl | cloud-logging", Category: "Output", Values: []string{"text", "json", "jsonl", "cloud-logging"}},
	{Long: "--json", Description: "Shorthand for --output=json", Category: "Output"},
	{Long: "--quiet", Description: "Suppress non-error output", Category: "Output"},
	{Long: "--color", Description: "Force color output", Category: "Output"},
	{Long: "--no-color", Description: "Disable color output", Category: "Output"},

	{Long: "--trace-profile", Type: FlagValue, ValueName: "<path>", Description: "Collect trace events (Chrome trace format)", Category: "Advanced"},

	// Unlisted: the version path prints the version, so the flag catalog does
	// not carry a row for it (internal/cli's TestHelpCatalogCoversReservedGlobals
	// exempts it by name). Completion still offers it — it is a real flag.
	{Long: "--version", Short: "-V", Description: "Show version"},
}
