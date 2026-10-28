package commandmeta

import (
	"fmt"
	"strings"

	cache "go.putnami.dev/protocol/cache"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
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
	// ── CI ───────────────────────────────────────────────────────────────
	{
		Path:        "impact-plan",
		Kind:        KindStructured,
		Category:    "CI",
		Summary:     "Emit the impacted plan of a change for a command list",
		Description: "Emit the impacted plan of the exact commit delta from --base to the checked-out HEAD for a comma-separated command list: the changed files, the impacted projects, and every task the engine plans for those commands. An extension reads this ImpactPlan instead of importing the CLI, and derives its own document from it, such as a ChangePlan. The worktree must be clean and --base must be a HEAD ancestor. Source contents, environment values, secrets, command arguments, and output paths are never emitted",
		Usage:       "putnami impact-plan <commands> --base <commit> [--head <commit>] [--no-cache] [--output <format>]",
		Positionals: []Positional{{Name: "commands", Required: true, Description: "Comma-separated command list, such as test,build; aliases resolve as they do for a run"}},
		Flags: []Flag{
			{Long: "--base", Type: FlagValue, ValueName: "<commit>", Description: "Required base commit or ref; emitted as its full immutable commit SHA"},
			{Long: "--head", Type: FlagValue, ValueName: "<commit>", Description: "Optional head commit or ref; it must resolve to the checked-out HEAD"},
		},
		Examples: []string{
			"putnami impact-plan test,build --base origin/main --output=json",
			"putnami impact-plan lint --base 0123456789abcdef0123456789abcdef01234567 --no-cache --output=jsonl",
		},
		Related:          []string{"projects list", "sessions inspect"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
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
		Usage:       "putnami projects create <name> --template <template> [--path <path>] [--channel <channel>] [--force]",
		Flags: []Flag{
			{Long: "--template", Type: FlagValue, ValueName: "<template>", Description: "Template to scaffold from"},
			{Long: "--path", Type: FlagValue, ValueName: "<path>", Description: "Workspace-relative project path"},
			{Long: "--channel", Type: FlagValue, ValueName: "<channel>", Description: "Resolve the template, the project setup and the dependency install on one release channel (default: PUTNAMI_CHANNEL, then the channel the CLI was installed from, then latest)"},
			{Long: "--force", Description: "Overwrite an existing project directory"},
		},
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami projects create my-app --template typescript-web",
			"putnami projects create my-app --template typescript-web --path apps/my-app",
			"putnami projects create my-api --template go-server --channel canary",
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
			{Long: "--dry-run", Description: "Show the changes without applying them"},
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
		Usage:       "putnami upgrade [--global] [--cli] [--extensions] [--deps] [--channel <stable|canary>] [--release <rs_id> --namespace <namespace>] [--version <version>] [--from-source] [--dry-run] [--continue-on-error]",
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
			{Long: "--continue-on-error", Description: "Run the remaining upgrade phases after one fails"},
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
		Related:   []string{"upgrade"},
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
		Path:        "cli",
		Kind:        KindStructured,
		Category:    "Workspace",
		Label:       "cli list|use",
		Summary:     "Installed CLI binaries",
		Description: "List the installed putnami CLI binaries (list) and switch the active one (use)",
		Usage:       "putnami cli <list|use>",
		Positionals: []Positional{{Name: "list|use", Required: true}},
		Examples: []string{
			"putnami cli list --global",
			"putnami cli use go-dev",
		},
		Workspace: WorkspaceNeed{
			Note: "a bare `cli` runs nothing: cmdCLI dispatches list and use, each of which states its own workspace requirement",
		},
		Recovery: true,
	},
	{
		Path:        "cli list",
		Kind:        KindStructured,
		Description: "List the installed putnami CLI binaries in .putnami/bin (or ~/.putnami/bin with --global) and mark the active one",
		Usage:       "putnami cli list [--global]",
		Flags: []Flag{
			{Long: "--global", Short: "-g", Description: "List binaries in ~/.putnami/bin instead of the workspace (alias: -g)"},
		},
		Examples: []string{
			"putnami cli list",
			"putnami cli list --global",
		},
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--global", "-g"},
			Note:        "cmdCLI/cliBinDir: --global lists ~/.putnami/bin, which exists outside any workspace",
		},
	},
	{
		Path:        "cli use",
		Kind:        KindStructured,
		Description: "Switch the active putnami CLI binary to an installed putnami-<name> (e.g. one built by `upgrade --from-source`)",
		Usage:       "putnami cli use <name> [--global]",
		Flags: []Flag{
			{Long: "--global", Short: "-g", Description: "Switch the binary in ~/.putnami/bin instead of the workspace (alias: -g)"},
		},
		Positionals: []Positional{{Name: "name", Required: true}},
		Examples: []string{
			"putnami cli use go-dev",
			"putnami cli use go-source-1a2b3c4 --global",
		},
		Workspace: WorkspaceNeed{
			Required:    true,
			ExemptFlags: []string{"--global", "-g"},
			Note:        "cmdCLI/cliBinDir: --global switches the binary in ~/.putnami/bin, which exists outside any workspace",
		},
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
		Summary:     "List extensions",
		Description: "List declared and discovered extensions with their versions and source",
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
		Path:        "extensions prepare",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Prepare the provider runtimes of a source workspace",
		Description: "Prepare into the artifact store the runtime of each path extension that serves a provider in a workspace that builds its own CLI. A hosted run of the same tree starts these runtimes and prepares none itself. ./putnamiw --print-engine runs this command after it builds the CLI",
		Usage:       "putnami extensions prepare [--output <format>]",
		Examples: []string{
			"putnami extensions prepare",
			"putnami extensions prepare --output=jsonl",
		},
		Related:          []string{"extensions list", "extensions install"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "extensions validate",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Validate an extension manifest",
		Description: "Develop extensions: validate manifests against the protocol",
		Usage:       "putnami extensions validate [path]",
		Positionals: []Positional{{Name: "path"}},
		Examples: []string{
			"putnami extensions validate",
			"putnami extensions validate ./my-extension/",
		},
		Related: []string{"templates validate", "extensions install"},
		Workspace: WorkspaceNeed{
			Note: "cmdExtensions validates an authored manifest from a path, with no workspace lookup",
		},
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
	{
		Path:        "templates validate",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Validate a template manifest",
		Description: "Validate a template manifest against the protocol",
		Usage:       "putnami templates validate [path]",
		Positionals: []Positional{{Name: "path"}},
		Examples: []string{
			"putnami templates validate",
			"putnami templates validate ./my-template/",
		},
		Related: []string{"templates test", "templates package", "extensions validate", "templates install"},
		Workspace: WorkspaceNeed{
			Note: "cmdTemplates validates an authored template from a path, with no workspace lookup",
		},
	},
	{
		Path:        "templates test",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Test a template in a temporary workspace",
		Description: "Test a template by rendering it, then creating a project from it in a temporary workspace and running the build and test its extension declares",
		Usage:       "putnami templates test [path] [--skip-build] [--keep] [--channel <channel>]",
		Flags: []Flag{
			{Long: "--skip-build", Description: "Render the template without running build and test on it"},
			{Long: "--keep", Description: "Keep the rendered temporary workspace for inspection"},
			{Long: "--channel", Type: FlagValue, ValueName: "<channel>", Description: "Install the template's extension and create the project on one release channel (default: PUTNAMI_CHANNEL, then the channel the CLI was installed from, then latest)"},
		},
		Positionals: []Positional{{Name: "path"}},
		Examples: []string{
			"putnami templates test",
			"putnami templates test ./my-template/ --skip-build",
		},
		Related: []string{"templates validate", "templates package"},
		Workspace: WorkspaceNeed{
			Note: "cmdTemplates renders an authored template from a path into a temporary workspace, with no workspace lookup",
		},
	},
	{
		Path:        "templates package",
		Kind:        KindStructured,
		Category:    "Extensions & Templates",
		Summary:     "Package a template into an archive",
		Description: "Package a template into a distributable archive",
		Usage:       "putnami templates package [path] [--version <version>] [--stable]",
		Flags: []Flag{
			{Long: "--version", Type: FlagValue, ValueName: "<version>", Description: "Version to stamp on the produced archive"},
			{Long: "--stable", Description: "Publish under a stable version instead of a prerelease"},
		},
		Positionals: []Positional{{Name: "path"}},
		Examples: []string{
			"putnami templates package",
			"putnami templates package ./my-template/ --version 1.2.3 --stable",
		},
		Related: []string{"templates validate", "templates test"},
		Workspace: WorkspaceNeed{
			Note: "cmdTemplates packages an authored template from a path, with no workspace lookup",
		},
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
	{
		Path:        "deps add",
		Kind:        KindStructured,
		Summary:     "Add a dependency to a project",
		Description: "Add a dependency to one project's language manifest through the deps-edit job of the project's extension",
		Usage:       "putnami deps add <module>[@version] [--projects <name>] [--dry-run]",
		Flags: []Flag{
			{Long: "--dry-run", Description: "Print the planned edit and change nothing"},
		},
		Examples: []string{
			"putnami deps add golang.org/x/text@latest",
			"putnami deps add golang.org/x/text@latest --projects api --dry-run",
		},
		Related:   []string{"deps install"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "deps remove",
		Kind:        KindStructured,
		Summary:     "Remove a dependency from a project",
		Description: "Remove a dependency from one project's language manifest through the deps-edit job of the project's extension",
		Usage:       "putnami deps remove <module> [--projects <name>] [--dry-run]",
		Flags: []Flag{
			{Long: "--dry-run", Description: "Print the planned edit and change nothing"},
		},
		Examples: []string{
			"putnami deps remove golang.org/x/text",
			"putnami deps remove golang.org/x/text --projects api --dry-run",
		},
		Related:   []string{"deps install"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:        "deps prune",
		Kind:        KindStructured,
		Summary:     "Remove declared dependency edges no import backs",
		Description: "Remove the dependency edges a project declares and none of its imports uses, from the provider view the last install or build recorded",
		Usage:       "putnami deps prune [--projects <name>] [--dry-run]",
		Flags: []Flag{
			{Long: "--dry-run", Description: "List the edges to remove and change nothing"},
		},
		Examples: []string{
			"putnami deps prune",
			"putnami deps prune --projects api --dry-run",
		},
		Related:   []string{"deps install"},
		Workspace: requiredWorkspace(),
	},
	{
		Path:       "deps rm",
		Kind:       KindStructured,
		Summary:    "Same as deps remove",
		DetailFrom: "deps remove",
		Workspace:  requiredWorkspace(),
	},
	{
		Path:    "cache",
		Kind:    KindStructured,
		Summary: "Manage cache",
		Workspace: WorkspaceNeed{
			Note: "a bare `cache` runs nothing: it prints its subcommands, so a reader who types it cannot wipe the shared store",
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
		Description: "Run the workspace gate tasks (the blocking commands of putnami.ci.json; lint, test and build without one) twice in isolated worktrees, compare cache keys and captured artifacts, restore the first run into a pristine tree, and fail on undeclared writes or live-versus-hit differences; declared ambient key inputs are reported without failing",
		Usage:       "putnami cache verify [--projects <selector> | --impacted | --all] [--exclude <names>] [--impacted-strict] [--profile <profile>] [--retry <n>] [--cpu-policy <policy>] [--trace-profile <path>] [--continue-on-error] [--output <format>]",
		Flags: []Flag{
			{Long: "--exclude", Type: FlagValue, ValueName: "<names>", Description: "Leave these projects out of the verified runs"},
			{Long: "--impacted-strict", Description: "Fail when the --impacted baseline can't be resolved"},
			{Long: "--profile", Type: FlagValue, ValueName: "<profile>", Description: "Deployment profile the verified runs use: dev | test | production", Values: []string{"dev", "test", "production"}},
			{Long: "--retry", Type: FlagValue, ValueName: "<n>", Description: "Retry transient failures in the verified runs"},
			{Long: "--cpu-policy", Type: FlagValue, ValueName: "<policy>", Description: "CPU ceiling policy: critical-path | measured", Values: []string{"critical-path", "measured"}},
			{Long: "--trace-profile", Type: FlagValue, ValueName: "<path>", Description: "Write the trace events of the last verified run (Chrome trace format)"},
			{Long: "--continue-on-error", Description: "Don't abort a verified run on the first failure"},
		},
		Examples: []string{
			"putnami cache verify --projects /my/project",
			"putnami cache verify --all --output=jsonl",
		},
		Related:          []string{"cache clean"},
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
		Summary:     "Print the workspace orientation map",
		Description: "Print the workspace orientation map, built in memory from committed artifacts (project manifests, schema/openapi.json, schema/config.jsonschema.json, READMEs, and the workspace docs tree): Markdown by default, and with --output=json the map document as data, the same document the MCP workspace_map tool serves. --projects and --impacted narrow the printed projects. Byte-deterministic on an unchanged tree. It writes no file, and a build writes no map",
		Usage:       "putnami context map [--projects <list>] [--impacted] [--output <format>]",
		Examples: []string{
			"putnami context map",
			"putnami context map --output=json",
			"putnami context map --projects <project-id>",
			"putnami context map --impacted",
		},
		Related:          []string{"context generate", "workspace describe"},
		StructuredOutput: true,
		Workspace:        requiredWorkspace(),
	},
	{
		Path:        "context pack",
		Kind:        KindStructured,
		Category:    "Configuration",
		Summary:     "Print each project's agent-context document",
		Description: "Print one deterministic agent-context document per project: its identity and graph, composition roots, commands, config and contracts. --output=json returns the documents as data, and --project narrows them to one project. It writes no file. The MCP agent_context tool builds the same document in memory",
		Usage:       "putnami context pack [--project <selector>] [--output <format>]",
		Flags: []Flag{
			{Long: "--project", Type: FlagValue, ValueName: "<selector>", Description: "Project ID, path, or name; default is every workspace project"},
		},
		Examples: []string{
			"putnami context pack",
			"putnami context pack --project <project-id>",
			"putnami context pack --output=json",
		},
		Related:          []string{"context map", "mcp"},
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
		Usage:       "putnami doctor [--profile <dev|test|production>] [--project <selector>] [--machine] [--output <format>]",
		Flags: []Flag{
			{Long: "--profile", Type: FlagValue, ValueName: "<profile>", Description: "Deployment profile to evaluate under (dev|test|production); production is strictest"},
			{Long: "--project", Type: FlagValue, ValueName: "<selector>", Description: "Project ID, path, or name (comma-separated); default is every workspace project"},
			{Long: "--machine", Type: FlagBool, Description: "Report the one-minute load per logical CPU, or unknown, instead of the readiness preflight; takes neither --profile nor --project"},
		},
		Examples: []string{
			"putnami doctor",
			"putnami doctor --machine",
			"putnami doctor --profile production",
			"putnami doctor --profile production --project /tooling/cli",
			"putnami doctor --profile production --output=jsonl",
		},
		Related:          []string{"config show", "workspace describe"},
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
		Summary: "Identify the worktree the CLI runs in",
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
		Description: "Show full details and event stream for a specific session. --run names a remote attempt instead of a session: the attempt reference or submission key a remote run printed when it was submitted, resolved through the durable attempt records under .putnami/runner/attempts. An attempt whose session is already imported is shown from the local store; one that is not (the CLI died mid-run, the import failed) is resumed through the workspace's runner provider from the persisted cursor and imported through the same path, and nothing is ever resubmitted. Ctrl-C during a resumed follow cancels the attempt within bounded time",
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

	// ── Agents ───────────────────────────────────────────────────────────
	{
		Path:        "mcp",
		Kind:        KindStructured,
		Category:    "Agents",
		Summary:     "Run the MCP server over stdio for AI agent harnesses",
		Description: fmt.Sprintf("Run the Model Context Protocol server over stdio so an AI agent harness can discover projects, context, ownership, impact, plans, and diagnostics through %d typed core tools (%s) and resources for the workspace context, the Putnami guidance, the workspace map, and each project's agent context. All except run_jobs are read-only; run_jobs is the only mutating core tool, supports dry-run planning, and refuses long-lived or externally mutating execution. Spawned by the client; speaks JSON-RPC 2.0 on stdin/stdout and exits with the session. Before it serves, it brings the workspace's agent workflows to the version the lock pins, from the local artifact store only, and reports what it cannot reconcile as a warning on stderr. Optional request provenance is disabled by default; set PUTNAMI_AGENT_IDENTITY=1 and explicitly set PUTNAMI_AGENT_MODEL in the MCP server environment to propagate bounded harness/model headers.", len(CoreMCPToolNames()), strings.Join(CoreMCPToolNames(), ", ")),
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
		Summary:     "Register the MCP server in .mcp.json for Claude Code",
		Description: "Write the putnami entry into .mcp.json at the workspace root, where Claude Code discovers the MCP server. Codex, Cursor and VS Code read their own files and do not see this entry. init, install and upgrade already add a missing entry; this explicit write is the only one that also rewrites a diverged putnami entry. Merge-aware: other servers and unknown keys are preserved, and an unparseable file is refused untouched.",
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

// helpRenderFlags are taken by the parser beside any command, like a global
// flag, but cataloged as flags of the `help` command (putnami help --man):
// listing them as globals would offer them after every command.
var helpRenderFlags = []GlobalFlag{
	{Long: "--man", Description: "Render the full reference as a man page"},
	{Long: "--markdown", Description: "Render the full reference as Markdown"},
}

// HelpRenderFlags returns the flags the parser takes beside any command that
// the global-flag table does not carry (see helpRenderFlags).
func HelpRenderFlags() []GlobalFlag {
	return append([]GlobalFlag(nil), helpRenderFlags...)
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
//   - Providers bind a flag to the reserved commands that give it meaning.
//     --credential-fd declares none although only a hosted provider uses it:
//     a hosted launcher reads it from the pinned CLI's --help wherever that
//     runs (launch.credentialProbe), so help must always list it.
//   - JobOnly marks a flag that only extension commands read (job commands and
//     the subcommands of command groups). A core command with a known catalog
//     path refuses a JobOnly flag unless its own entry declares it (doctor
//     --profile, projects sync --dry-run, cache verify --exclude); a command
//     that declares the flag does not make it shared. A flag is not JobOnly
//     when most core commands read it, directly or through the lifecycle jobs
//     they run.
//   - Values enumerate what completion offers for a FlagValue flag. Leave it nil
//     when the value is unbounded (a path, a project selector, a count): an
//     incomplete candidate list reads as "these are the only values".
//   - Since slice A1c this table IS what the parser accepts: internal/cli's
//     parseGlobalFlags reads Long/Short to recognize a token and Type to decide
//     whether it consumes a value, and keeps only the binding to GlobalFlags
//     fields as code. A flag added to the parser and not here is unparseable,
//     and one added here with no binding is accepted and dropped — which is what
//     internal/cli's TestGlobalFlags_CoverTheParser reads flags.go to catch.
//   - Delivery names the extension commands that receive a global they declare
//     under its name (see GlobalFlag.Delivery). Set it only with the code that
//     delivers the flag: `putnami extensions validate` refuses a manifest flag
//     named like a global its command does not receive.
var globalFlags = []GlobalFlag{
	{Long: "--verbose", Short: "-v", Description: "Show job results and diagnostics", Category: "Common"},
	{Long: "--debug", Short: "-d", Description: "Stream all events in real-time", Category: "Common"},
	{Long: "--watch", Short: "-w", Description: "Re-run on file changes", Category: "Common", JobOnly: true, Delivery: DeliveredToServeWorkload},
	{Long: "--plan", Description: "Show execution plan without running", Category: "Common", JobOnly: true},
	{Long: "--help", Short: "-h", Description: "Show help", Category: "Common"},

	{Long: "--all", Description: "All projects", Category: "Project Selection", Delivery: DeliveredToInteractive},
	{Long: "--impacted", Description: "Projects affected by git changes", Category: "Project Selection"},
	{Long: "--projects", Type: FlagValue, ValueName: "<list>", Description: "Comma-separated project names", Category: "Project Selection", Delivery: DeliveredToInteractive},
	{Long: "--tag", Type: FlagValue, ValueName: "<tags>", Description: "Filter by tags", Category: "Project Selection"},
	{Long: "--exclude-tag", Type: FlagValue, ValueName: "<tags>", Description: "Exclude by tags", Category: "Project Selection"},
	{Long: "--exclude", Type: FlagValue, ValueName: "<names>", Description: "Exclude by name", Category: "Project Selection", JobOnly: true},
	{Long: "--baseline", Type: FlagValue, ValueName: "<branch>", Description: "Custom git baseline for --impacted; a job command with no target or --all implies --impacted", Category: "Project Selection", Delivery: DeliveredToInteractive},
	{Long: "--impacted-strict", Description: "Fail when baseline can't be resolved", Category: "Project Selection", JobOnly: true},

	{Long: "--where", Type: FlagValue, ValueName: "<placement>", Description: "Execution placement: local | remote (local when no runner provider is installed)", Category: "Execution", Values: []string{"local", "remote"}, Providers: []string{runner.ProviderCommandName}, JobOnly: true},
	{Long: "--providers", Type: FlagValue, ValueName: "<list>", Description: "Credentials to ask the credential provider for: install, publish (comma list)", Category: "Execution", Values: []string{"install", "publish"}, Providers: []string{registry.CredentialProviderCommand}},
	{Long: runcredential.Flag, Type: FlagValue, ValueName: "<n>", Description: runcredential.FlagDescription, Category: "Execution"},
	{Long: "--no-cache", Description: "Force re-execution (skip cache)", Category: "Execution", Delivery: DeliveredToEvery},
	{Long: "--no-cache-projects", Type: FlagValue, ValueName: "<list>", Description: "Force re-execution of these projects only", Category: "Execution", JobOnly: true},
	{Long: "--cache-trust", Type: FlagValue, ValueName: "<policy>", Description: "Remote cache trust: ci | any | none", Category: "Execution", Values: []string{"ci", "any", "none"}, Providers: []string{cache.ProviderCommandName}, JobOnly: true},
	{Long: "--continue-on-error", Description: "Don't abort on first failure", Category: "Execution", JobOnly: true},
	{Long: "--max-parallel", Type: FlagValue, ValueName: "<mode|n>", Description: "Parallelism: auto | eco | max | number", Category: "Execution", Values: []string{"auto", "eco", "max"}, JobOnly: true, Delivery: DeliveredToEvery},
	{Long: "--resource", Type: FlagValue, ValueName: "<name=units>", Description: "Budget for a named shared resource (repeatable)", Category: "Execution", JobOnly: true},
	{Long: "--cpu-policy", Type: FlagValue, ValueName: "<policy>", Description: "CPU ceiling policy: critical-path | measured", Category: "Execution", Values: []string{"critical-path", "measured"}, JobOnly: true},
	{Long: "--retry", Type: FlagValue, ValueName: "<n>", Description: "Retry transient failures", Category: "Execution", JobOnly: true},
	{Long: "--retry-failed", Description: "Re-run tasks whose identical failure is cached", Category: "Execution", JobOnly: true},
	{Long: "--profile", Type: FlagValue, ValueName: "<profile>", Description: "Deployment profile: dev | test | production", Category: "Execution", Values: []string{"dev", "test", "production"}, JobOnly: true},
	{Long: "--serve-env", Type: FlagValue, ValueName: "<->", Description: "Read the serve steps' process environment from stdin: one JSON object of string variables", Category: "Execution", Values: []string{"-"}, JobOnly: true},
	{Long: "--dry-run", Description: "Show changes without applying", Category: "Execution", JobOnly: true, Delivery: DeliveredToEvery},

	{Long: "--output", Type: FlagValue, ValueName: "<format>", Description: "Output format: text | json | jsonl | cloud-logging", Category: "Output", Values: []string{"text", "json", "jsonl", "cloud-logging"}},
	{Long: "--json", Description: "Shorthand for --output=json", Category: "Output"},
	{Long: "--quiet", Description: "Suppress non-error output", Category: "Output"},
	{Long: "--color", Description: "Force color output", Category: "Output"},
	{Long: "--no-color", Description: "Disable color output", Category: "Output"},

	{Long: "--trace-profile", Type: FlagValue, ValueName: "<path>", Description: "Collect trace events (Chrome trace format)", Category: "Advanced", JobOnly: true},

	// Unlisted: the version path prints the version, so the flag catalog does
	// not carry a row for it (internal/cli's TestHelpCatalogCoversReservedGlobals
	// exempts it by name). Completion still offers it — it is a real flag.
	{Long: "--version", Short: "-V", Description: "Show version"},
}
