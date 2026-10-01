package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// initExtensionConfig maps --extension shorthand to extension package + default
// template. It is the starter definition: everything `putnami init` writes into
// a new workspace that is not derived from the user's flags comes from here.
type initExtensionConfig struct {
	packageName string
	template    string
	// agentContent is the extension whose agent content this starter opts
	// into. It is DATA, not policy: init declares exactly that extension in
	// `extensions`, opts into its content with `extension:<name>` in
	// `agentArtifacts`, and materializes exactly that. Empty opts into nothing.
	//
	// The extension is installed in init's agent step, which reports a failure
	// and goes on: until the extension resolves, init keeps both declarations
	// and names `putnami install`, which installs and materializes it later.
	agentContent string
	// registries is the starter's `registries` entries, one per ecosystem id:
	// the registry its framework dependencies install from. The extension that
	// owns the ecosystem reads the entry; nothing else names the endpoint.
	registries map[string]json.RawMessage
}

// starterAgentContent is the extension every built-in starter opts into.
const starterAgentContent = "@putnami/contributor"

// starterNPMRegistries maps the @putnami npm scope to the Putnami package
// registry, which serves the TypeScript framework packages the TypeScript
// templates depend on.
var starterNPMRegistries = map[string]json.RawMessage{
	"npm": json.RawMessage(`{"scopes":{"@putnami":"https://npm.putnami.dev"}}`),
}

var initExtensions = map[string]initExtensionConfig{
	"ts": {packageName: "@putnami/typescript", template: "typescript-web", agentContent: starterAgentContent, registries: starterNPMRegistries},
	"go": {packageName: "@putnami/go", template: "go-server", agentContent: starterAgentContent},
	"py": {packageName: "@putnami/python", template: "python-server", agentContent: starterAgentContent},
}

// installExtension is swapped in tests so init tests never perform a real
// registry install (slow and environment-dependent).
var installExtension = extensions.ExtensionsInstall

// The remaining required init stages are also seams for failure-path tests.
// Successful tests still exercise their real implementations elsewhere; init
// tests replace them so a registry, package manager, or toolchain is never
// required to prove that a failed stage stops the workflow.
var (
	installInitDependencies = DepsInstall
	installInitTemplate     = extensions.TemplatesInstall
	createInitProject       = createInitProjectFiles
	refreshInitLockMetadata = versioncmd.RefreshLockMetadataWithResult
)

// initFlags holds parsed flags for the init command.
type initFlags struct {
	force       bool
	workspace   string
	project     string
	projectPath string
	extension   string
	// channel is the value of --channel, and channelSet reports that the flag
	// was given: a flag with no value is a usage error, not an absent flag.
	channel    string
	channelSet bool
}

func parseInitFlags(args []string) initFlags {
	f := initFlags{extension: "ts"}
	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], initChannelFlag+"="); ok {
			f.channel, f.channelSet = value, true
			continue
		}
		switch args[i] {
		case initChannelFlag:
			f.channelSet = true
			if i+1 < len(args) {
				f.channel = args[i+1]
				i++
			}
		case "--force":
			f.force = true
		case "--workspace":
			if i+1 < len(args) {
				f.workspace = args[i+1]
				i++
			}
		case "--project":
			if i+1 < len(args) {
				f.project = args[i+1]
				i++
			}
		case "--project-path":
			if i+1 < len(args) {
				f.projectPath = args[i+1]
				i++
			}
		case "--extension":
			if i+1 < len(args) {
				f.extension = args[i+1]
				i++
			}
		}
	}
	return f
}

const defaultGitignore = `node_modules
dist
.putnami
coverage
.gen
.generated
.DS_Store
.env.local.yaml
.env.prod.yaml
`

// workspaceName is the --workspace name, or the directory's name.
func workspaceName(flags initFlags, cwd string) string {
	if flags.workspace != "" {
		return flags.workspace
	}
	return filepath.Base(cwd)
}

// workspaceReadme is the README.md a new workspace starts with: what the
// workspace is, the commands that check a change, and where each project's
// documentation lives.
func workspaceReadme(name string) string {
	return "# " + name + `

A Putnami workspace. Each project lives in its own directory and is listed in
putnami.workspace.json.

## Commands

` + "```bash" + `
putnami lint,test,build --impacted   # check the projects a change touches
putnami projects list                # list the projects
putnami projects create <name> --template <template>
` + "```" + `

## Documentation

Each project documents its purpose and its commands in its own README.md.
AGENTS.md tells coding agents how to work in this workspace.
`
}

// resolveInitRequest validates what an init run was asked for before anything
// is written: the starter extension, the project placement and the channel.
func resolveInitRequest(flags initFlags) (initExtensionConfig, initChannel, error) {
	extConfig, ok := initExtensions[flags.extension]
	if !ok {
		return initExtensionConfig{}, initChannel{}, cmderr.Usagef("unknown extension %q (choose: ts, go, py)", flags.extension)
	}
	if flags.projectPath != "" && flags.project == "" {
		return initExtensionConfig{}, initChannel{}, cmderr.Usagef("--project-path requires --project")
	}
	channel, err := resolveInitChannel(flags, os.Getenv, runningCLIName())
	if err != nil {
		return initExtensionConfig{}, initChannel{}, err
	}
	return extConfig, channel, nil
}

// printInitChannel names the channel an init run resolves on and what chose
// it. A run on latest prints nothing, as a run without a channel choice does.
func printInitChannel(channel initChannel) {
	if name := channel.selected(); name != "" {
		iox.Fprintf(os.Stdout, "  Channel: %s (%s)\n", name, channel.origin)
	}
}

// WorkspaceInit creates a new workspace, installs an extension, and optionally
// scaffolds a project. It mirrors the onboarding DX of the TypeScript CLI.
//
// One channel drives every resolution of the run: the extensions, the template
// and the starter's dependencies (resolveInitChannel). The channel is a target
// of this run only. The workspace config keeps bare artifact names, the lock
// records the exact versions the channel resolved, and no later command reads
// the channel back.
func WorkspaceInit(ctx context.Context, wsRoot string, args []string, env LifecycleEnv) error {
	flags := parseInitFlags(args)

	extConfig, channel, err := resolveInitRequest(flags)
	if err != nil {
		return err
	}
	// Every installer and the project create of this run read the channel from
	// env, so no step resolves on another one.
	env.channel = channel.selected()

	projectPath := ""
	if flags.project != "" {
		resolvedProjectPath, err := resolveProjectPath(flags.project, flags.projectPath)
		if err != nil {
			return err
		}
		projectPath = resolvedProjectPath
	}

	// Check existing workspace
	if wsRoot != "" && !flags.force {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  Good news: this place is already a Putnami workspace\n")
		iox.Fprintf(os.Stdout, "    %s\n", wsRoot)
		iox.Fprintln(os.Stdout)
		iox.Fprintln(os.Stdout, "  Initialization skipped.")
		iox.Fprintln(os.Stdout)
		iox.Fprintln(os.Stdout, "  Want to rebuild it from scratch? (may overwrite files)")
		iox.Fprintln(os.Stdout, "    putnami init --force")
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	// --- Phase 1: Initialize workspace ---
	iox.Fprintf(os.Stdout, "  → Initializing workspace…\n")
	printInitChannel(channel)

	// Init git repo if git is available and no .git exists
	if _, err := exec.LookPath("git"); err == nil {
		gitDir := filepath.Join(cwd, ".git")
		if _, err := os.Stat(gitDir); os.IsNotExist(err) {
			cmd := exec.CommandContext(ctx, "git", "init")
			cmd.Dir = cwd
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Run(); err == nil {
				iox.Fprintln(os.Stdout, "  ✓ Git repository initialized")
			}
		}
	}

	// Create .gitignore if it doesn't exist
	gitignorePath := filepath.Join(cwd, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitignorePath, []byte(defaultGitignore), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ .gitignore created")
		}
	}

	// Create README.md if it doesn't exist, so the workspace documents what it
	// is and how to check a change from its first commit. An existing file is
	// the team's and stays untouched.
	readmePath := filepath.Join(cwd, "README.md")
	if _, err := os.Stat(readmePath); os.IsNotExist(err) {
		if err := os.WriteFile(readmePath, []byte(workspaceReadme(workspaceName(flags, cwd))), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ README.md created")
		}
	}

	// Create .gitattributes with the LF policy (D-W4) if it doesn't exist, so
	// every checkout, Windows included, hashes the committed bytes. An existing
	// file is the team's and stays untouched; `putnami doctor` reports a
	// checkout that converts line endings without the policy.
	gitattributesPath := filepath.Join(cwd, ".gitattributes")
	if _, err := os.Stat(gitattributesPath); os.IsNotExist(err) {
		if err := os.WriteFile(gitattributesPath, []byte(git.LFPolicyAttributes+"\n"), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ .gitattributes created (LF line endings)")
		}
	}

	// Place the Putnami guidance block in the assistant entrypoints, and
	// register the putnami MCP server so the first agent session in the new
	// workspace reaches it with no manual step (ADR 0040). Nothing else is
	// written for agents (ADR 0055).
	if err := agentctx.WriteAgentEntrypoints(cwd); err == nil {
		iox.Fprintln(os.Stdout, "  ✓ Assistant guidance written (CLAUDE.md, AGENTS.md)")
	}
	if agentctx.RegisterMCPServer(cwd, os.Stderr) {
		iox.Fprintln(os.Stdout, "  ✓ MCP server registered for agent sessions (.mcp.json)")
	}

	name := workspaceName(flags, cwd)

	// Write putnami.workspace.json with the extension and template pre-configured
	var templates []string
	if flags.project != "" {
		templates = []string{extConfig.template}
	}
	cfg := initConfig{
		Schema:         "https://putnami.dev/schemas/putnami-workspace.json",
		Name:           name,
		Extensions:     initDeclaredExtensions(extConfig),
		Templates:      templates,
		AgentArtifacts: initAgentArtifacts(cwd, extConfig),
		Registries:     initRegistries(cwd, extConfig),
		Includes:       []string{},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	cfgPath := filepath.Join(cwd, wsproto.WorkspaceConfigFilename)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	iox.Fprintln(os.Stdout, "  ✓ Workspace config written (putnami.workspace.json)")

	// Create package.json if it doesn't exist (needed for workspace-install)
	pkgJSONPath := filepath.Join(cwd, "package.json")
	if _, err := os.Stat(pkgJSONPath); os.IsNotExist(err) {
		pkgJSON := fmt.Sprintf("{\n  \"name\": %q,\n  \"private\": true\n}\n", name)
		if err := os.WriteFile(pkgJSONPath, []byte(pkgJSON), 0o644); err != nil {
			return fmt.Errorf("write package.json: %w", err)
		}
	}

	// --- Phase 2: Install extension ---
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Adding extension…\n")

	// Reload config from disk (now that putnami.workspace.json exists)
	wsCfg := wsproto.Load(cwd)

	if err := installExtension(ctx, cwd, wsCfg, []string{channelArtifact(extConfig.packageName, env.channel)}, ""); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install extension %s: %w", extConfig.packageName, err),
			"putnami extensions install",
		)
	}

	// Regenerate shared AI context now that the extension is installed.
	wsCfg = wsproto.Load(cwd)
	if err := agentctx.ContextGenerate(cwd, wsCfg, nil); err != nil {
		iox.Fprintf(os.Stderr, "  AI context generation failed: %v\n", err)
	}

	// Materialize the agent content the starter opted into, if it opted into
	// any. The extension that ships it is installed first, which pins it; its
	// content then follows that exact release. A starter that opts into
	// nothing skips the phase entirely — init never adds agent content a
	// workspace did not ask for.
	if len(cfg.AgentArtifacts) > 0 {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  → Installing agent content…\n")
		if err := installInitAgentContent(ctx, cwd, wsCfg, extConfig, env.channel); err != nil {
			// Reported, not fatal. The declarations are written and the
			// content's extension is either unpinned or its content unwritten,
			// so nothing is half-installed; `putnami install` installs and
			// materializes both later. Failing here would skip the stages a
			// workspace actually needs — dependencies and the project itself —
			// over workflow files it can get later.
			iox.Fprintf(os.Stdout, "  ✗ Agent content: %v\n", err)
			iox.Fprintln(os.Stdout, "    The declarations are kept; run `putnami install` to install it.")
		}
	}

	// Run workspace-install to set up deps.
	if err := installInitDependencies(ctx, cwd, wsCfg, "", "", env); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install workspace dependencies: %w", err),
			"putnami deps install",
		)
	}

	if flags.project != "" {
		if err := initializeProject(ctx, cwd, flags, extConfig, projectPath, env); err != nil {
			return err
		}
	}

	// Pin the toolchains the installed dependencies declare, with the refresh
	// `putnami install` ends with. It runs after the last dependency install,
	// which writes the files the pins derive from (go.work for a Go project).
	// Without it the lock pins no toolchain, and the commands printed below stop
	// on the missing pin.
	if _, err := refreshInitLockMetadata(ctx, cwd, env.CLIVersion); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("refresh lock metadata: %w", err),
			"putnami install",
		)
	}

	// --- Summary ---
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  ✓ Workspace ready: %s\n", name)
	iox.Fprintf(os.Stdout, "    Path: %s\n", cwd)
	iox.Fprintf(os.Stdout, "    Extension: %s\n", extConfig.packageName)
	iox.Fprintln(os.Stdout)

	iox.Fprintln(os.Stdout, "  Next:")
	if flags.project != "" {
		iox.Fprintf(os.Stdout, "    putnami lint,test,build %s\n", flags.project)
		iox.Fprintf(os.Stdout, "    putnami serve %s\n", flags.project)
	} else {
		iox.Fprintf(os.Stdout, "    putnami projects create my-app --template %s\n", extConfig.template)
		iox.Fprintln(os.Stdout, "    putnami build --all")
	}

	return nil
}

// initializeProject installs and renders the starter explicitly requested by
// --project, registers it, and reconciles dependencies. Every step is required:
// returning immediately prevents WorkspaceInit from printing its ready summary.
func initializeProject(
	ctx context.Context,
	cwd string,
	flags initFlags,
	extConfig initExtensionConfig,
	projectPath string,
	env LifecycleEnv,
) error {
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Installing template…\n")

	wsCfg := wsproto.Load(cwd)
	if err := installInitTemplate(ctx, cwd, wsCfg, []string{channelArtifact(extConfig.template, env.channel)}, ""); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install template %s: %w", extConfig.template, err),
			"putnami templates install",
		)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Creating project…\n")

	// Reload config to pick up installed extensions and templates.
	wsCfg = wsproto.Load(cwd)
	createArgs := []string{flags.project, "--template", extConfig.template}
	if flags.projectPath != "" {
		createArgs = append(createArgs, "--path", projectPath)
	}

	// verbose=false: init has its own scaffolding output; naming the module
	// proxy that answered the framework-version lookup is a `projects create`
	// concern and would need plumbing a verbose flag through WorkspaceInit that
	// init does not otherwise use. A failed lookup still fails the create.
	if err := createInitProject(ctx, cwd, wsCfg, createArgs, false, env); err != nil {
		// A failed create may leave the project directory behind, which only
		// --force lets the retry render over.
		return protocolcli.WithNext(
			fmt.Errorf("create project %s: %w", flags.project, err),
			projectsCreateRetryCommand(flags.project, createFlags{template: extConfig.template, path: flags.projectPath}, projectPath),
		)
	}

	wsCfg = wsproto.Load(cwd)
	projects := append(wsCfg.Includes, projectPath) //nolint:gocritic // intentionally creating new slice
	sort.Strings(projects)
	if err := updateConfigMembership(cwd, nil, projects); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("register project %s in workspace config: %w", flags.project, err),
			"putnami projects sync",
		)
	}
	if err := scaffoldPackageJSONWorkspaces(cwd, projects); err != nil {
		return fmt.Errorf("update package.json workspaces for project %s: %w", flags.project, err)
	}

	wsCfg = wsproto.Load(cwd)
	if err := installInitDependencies(ctx, cwd, wsCfg, "", "", env); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install workspace dependencies after creating %s: %w", flags.project, err),
			"putnami deps install",
		)
	}
	return nil
}

// installInitAgentContent installs the extension the starter opts into, on
// channel when init chose one, then materializes the agent content every
// opt-in selects from the release the lock pins.
func installInitAgentContent(ctx context.Context, cwd string, wsCfg *wsproto.Config, extConfig initExtensionConfig, channel string) error {
	if extConfig.agentContent != "" {
		if err := installExtension(ctx, cwd, wsCfg, []string{channelArtifact(extConfig.agentContent, channel)}, ""); err != nil {
			return fmt.Errorf("install extension %s: %w", extConfig.agentContent, err)
		}
	}
	return agentctx.AdoptAgentWorkflows(ctx, cwd, wsCfg, os.Stdout)
}

// initDeclaredExtensions is the new workspace's `extensions`: the language
// extension, and the extension whose agent content the starter opts into.
func initDeclaredExtensions(extConfig initExtensionConfig) []string {
	extensions := []string{extConfig.packageName}
	if extConfig.agentContent != "" && extConfig.agentContent != extConfig.packageName {
		extensions = append(extensions, extConfig.agentContent)
	}
	return extensions
}

type initConfig struct {
	Schema         string                     `json:"$schema"`
	Name           string                     `json:"name"`
	Extensions     []string                   `json:"extensions"`
	Templates      []string                   `json:"templates,omitempty"`
	AgentArtifacts []string                   `json:"agentArtifacts,omitempty"`
	Registries     map[string]json.RawMessage `json:"registries,omitempty"`
	Includes       []string                   `json:"includes"`
}

// initRegistries is the new workspace's `registries`: the starter's entries,
// and every entry a config already at this path declares, which wins. An
// entry is kept or replaced whole, never merged key by key: its keys belong to
// the profile of the extension that owns the ecosystem.
func initRegistries(cwd string, extConfig initExtensionConfig) map[string]json.RawMessage {
	registries := make(map[string]json.RawMessage, len(extConfig.registries))
	for ecosystem, entry := range extConfig.registries {
		registries[ecosystem] = entry
	}
	if existing := readWorkspaceConfigAt(cwd); existing != nil {
		for ecosystem, entry := range existing.Registries {
			registries[ecosystem] = entry
		}
	}
	if len(registries) == 0 {
		return nil
	}
	return registries
}

// initAgentArtifacts is the new workspace's agent-content opt-in: what the
// selected starter opts into, merged with what a config already at this path
// declared.
//
// The merge exists because `putnami init --force` REWRITES the workspace config
// of a workspace that may already have opted in. Dropping that array would
// silently un-declare content whose files are already installed and recorded
// — the next `install` would then report every one of them as an unmanaged
// collision. Re-initializing must not be able to do that.
//
// It merges by what each entry NAMES, not by how it is spelled, and the
// existing declaration wins. An entry in a form this CLI no longer installs is
// kept as well: dropping it would orphan its files and records, while keeping
// it makes every later command name the migration that moves it.
func initAgentArtifacts(cwd string, extConfig initExtensionConfig) []string {
	byName := make(map[string]string)
	record := func(references []string, overwrite bool) {
		for _, reference := range references {
			reference = strings.TrimSpace(reference)
			if reference == "" {
				continue
			}
			name := agentctx.AgentArtifactDeclarationName(cwd, reference)
			if name == "" {
				// Unreadable, but still the user's declaration: keep it under
				// its own spelling rather than drop it.
				name = reference
			}
			if _, ok := byName[name]; ok && !overwrite {
				continue
			}
			byName[name] = reference
		}
	}
	var starter []string
	if extConfig.agentContent != "" {
		starter = []string{"extension:" + extConfig.agentContent}
	}
	record(starter, false)
	if existing := readWorkspaceConfigAt(cwd); existing != nil {
		// Last writer wins, and it is deliberately the committed config: what
		// the user pinned outranks what the starter suggests.
		record(existing.AgentArtifacts, true)
	}
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

// readWorkspaceConfigAt reads the workspace config at EXACTLY this directory.
// wsproto.Load merges the user's global config and resolves upward, which is
// the wrong question here: init is deciding what THIS directory already
// declared, and a parent workspace's opt-in is not this workspace's.
func readWorkspaceConfigAt(dir string) *wsproto.Config {
	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return nil
	}
	var cfg wsproto.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// scaffoldPackageJSONWorkspaces writes the root package.json "workspaces" array
// for a workspace `putnami init` just created.
//
// This is a SCAFFOLDING write, and it is the only npm-shaped manifest edit left
// in core. It survives slice C4b's deletion of the workspaces-array writer from
// `projects sync` for one reason: `init` runs before any extension exists in the
// new workspace, so there is no TypeScript adapter to hand the write to and no
// workspace probe to ask. From the first `putnami projects sync` onward the
// array belongs to the TypeScript extension's own sync task, which rewrites it
// from the resolved selection.
func scaffoldPackageJSONWorkspaces(wsRoot string, projects []string) error {
	pkgPath := filepath.Join(wsRoot, "package.json")
	raw, err := jsonutil.ReadFile(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no package.json — nothing to update
		}
		return err
	}

	// Filter to projects that have a package.json (TS/JS projects).
	var workspaces []string
	for _, p := range projects {
		projPkgPath := filepath.Join(wsRoot, p, "package.json")
		if _, err := os.Stat(projPkgPath); err == nil {
			workspaces = append(workspaces, p)
		}
	}
	sort.Strings(workspaces)

	raw.Set("workspaces", workspaces)
	return jsonutil.WriteFile(pkgPath, raw)
}
