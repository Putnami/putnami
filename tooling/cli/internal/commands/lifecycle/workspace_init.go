package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// initExtensionConfig is what init declares for one extension: the extension,
// and, for a language that ships a starter (initStarters), the template
// --project renders, the agent content it opts into and its registries.
// Everything `putnami init` writes into a new workspace that is not derived
// from the user's flags comes from here.
type initExtensionConfig struct {
	packageName string
	template    string
	// agentContent is the extension whose agent content this starter opts
	// into. No built-in starter names one: a workspace asks for
	// @putnami/contributor with `putnami extensions install @putnami/contributor`. It is DATA,
	// not policy: init declares exactly that extension in
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

// initSource is a monorepo tool `putnami init --from` adopts: the starter
// that declares the tool's stack extension, the files, any one of which marks
// a repository the tool describes, and the modes its extension adopts in. The
// extension owns everything else: its probe names the packages and its
// workspace sync task writes their putnami.json in the mode the workspace
// records, so init only declares it, records the mode and runs projects sync.
type initSource struct {
	starter initExtensionConfig
	markers []string
	// modes are the adoption modes the extension offers, the default first:
	// import, delegate, or both, which makes init ask (D-064).
	modes []string
	// importList reports that the extension reads options import, the
	// projects a delegated workspace imports, which --import adds to.
	importList bool
	// syncExtensions are the extensions the tool's sync declares for the
	// projects it imports, each only when a target moves to it (ADR 0017
	// of tooling/stacks): init installs them after the sync, so an import
	// that moves no target installs nothing.
	syncExtensions []string
}

// initSources are the tools --from accepts, by name.
var initSources = map[string]initSource{
	"bazel": {starter: initExtensionConfig{packageName: "@putnami/bazel"}, markers: []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"}, modes: []string{initModeDelegate}},
	"nx": {
		starter:        initExtensionConfig{packageName: "@putnami/nx"},
		markers:        []string{"nx.json"},
		modes:          []string{initModeImport, initModeDelegate},
		syncExtensions: []string{"@putnami/typescript"},
		importList:     true,
	},
	// moon's import mode maps each project to its language's extension: the
	// sync declares the ones the projects need, and init installs them after
	// it.
	"moon": {
		starter:        initExtensionConfig{packageName: "@putnami/moon"},
		markers:        []string{".moon/workspace.yml"},
		modes:          []string{initModeImport, initModeDelegate},
		syncExtensions: []string{"@putnami/go", "@putnami/php", "@putnami/python", "@putnami/ruby", "@putnami/rust", "@putnami/typescript"},
	},
	"pants": {starter: initExtensionConfig{packageName: "@putnami/pants"}, markers: []string{"pants.toml"}, modes: []string{initModeDelegate}},
	// Turborepo's import mode moves a TypeScript package's task to
	// @putnami/typescript when its script already runs what that extension's
	// task runs; every other task stays on Turborepo.
	"turbo": {
		starter:        initExtensionConfig{packageName: "@putnami/turbo"},
		markers:        []string{"turbo.json"},
		modes:          []string{initModeImport, initModeDelegate},
		syncExtensions: []string{"@putnami/typescript"},
		importList:     true,
	},
	"rush": {
		starter:        initExtensionConfig{packageName: "@putnami/rush"},
		markers:        []string{"rush.json"},
		modes:          []string{initModeImport, initModeDelegate},
		syncExtensions: []string{"@putnami/typescript"},
	},
}

// holdsAny reports whether dir holds one of the files.
func holdsAny(dir string, files []string) bool {
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// initSourceNames lists the tools --from accepts, sorted.
func initSourceNames() string {
	names := make([]string, 0, len(initSources))
	for name := range initSources {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// removeInitExtension removes from the lock and the disk an extension init
// removed from the workspace config; cfg is the config as it now stands.
// Swapped in tests.
var removeInitExtension = extensions.ExtensionsRemoveUndeclared

// installExtension is swapped in tests so init tests never perform a real
// registry install (slow and environment-dependent). Its progress lines go to
// stdout.
var installExtension = func(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return extensions.ExtensionsInstallWithOptions(ctx, wsRoot, cfg, args, extensions.InstallOptions{OutputFormat: outputFormat, Out: os.Stdout})
}

// The remaining required init stages are also seams for failure-path tests.
// Successful tests still exercise their real implementations elsewhere; init
// tests replace them so a registry, package manager, or toolchain is never
// required to prove that a failed stage stops the workflow.
var (
	installInitDependencies = DepsInstall
	installInitTemplate     = extensions.TemplatesInstall
	createInitProject       = createInitProjectFiles
	pinInitToolchains       = resolveToolchainPins
	refreshInitLockMetadata = versioncmd.RefreshLockMetadataWithResult
	syncInitProjects        = func(ctx context.Context, root string, cfg *wsproto.Config, env LifecycleEnv) error {
		return ProjectsSync(ctx, root, cfg, nil, false, env)
	}
)

// initFlags holds parsed flags for the init command.
type initFlags struct {
	force       bool
	workspace   string
	project     string
	projectPath string
	extension   string
	// extensionSet reports that --extension was given.
	extensionSet bool
	// from is the monorepo tool of --from, and fromSet reports that the flag
	// was given: a flag with no value is a usage error, not an absent flag.
	from    string
	fromSet bool
	// mode is the adoption mode of --mode, and modeSet reports that the flag
	// was given.
	mode    string
	modeSet bool
	// apply lets a rerun of init --from write what it previews.
	apply bool
	// imports are the project paths of --import, and importSet reports that
	// the flag was given.
	imports   []string
	importSet bool
	// channel is the value of --channel, and channelSet reports that the flag
	// was given: a flag with no value is a usage error, not an absent flag.
	channel    string
	channelSet bool
}

func parseInitFlags(args []string) initFlags {
	var f initFlags
	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], initChannelFlag+"="); ok {
			f.channel, f.channelSet = value, true
			continue
		}
		if value, ok := strings.CutPrefix(args[i], "--from="); ok {
			f.from, f.fromSet = value, true
			continue
		}
		if value, ok := strings.CutPrefix(args[i], "--mode="); ok {
			f.mode, f.modeSet = value, true
			continue
		}
		if value, ok := strings.CutPrefix(args[i], "--import="); ok {
			f.imports, f.importSet = append(f.imports, splitImportFlag(value)...), true
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
			f.extensionSet = true
			if i+1 < len(args) {
				f.extension = args[i+1]
				i++
			}
		case "--from":
			f.fromSet = true
			if i+1 < len(args) {
				f.from = args[i+1]
				i++
			}
		case "--mode":
			f.modeSet = true
			if i+1 < len(args) {
				f.mode = args[i+1]
				i++
			}
		case "--apply":
			f.apply = true
		case "--import":
			f.importSet = true
			// The next flag is no project path: --import then has none.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				f.imports = append(f.imports, splitImportFlag(args[i+1])...)
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

// putnamiGitignoreEntries are the directories every Putnami run writes: the
// generated tree and build stamp of each project (.gen/), and the sessions,
// caches and command output of the workspace (.putnami/). Neither is ever
// committed, and Git reports both as changes until .gitignore lists them.
var putnamiGitignoreEntries = []struct {
	entry string
	// covering are the spellings of an existing line that already ignore the
	// directory everywhere it appears.
	covering []string
}{
	{".putnami/", []string{".putnami", ".putnami/", "/.putnami", "/.putnami/", "**/.putnami", "**/.putnami/"}},
	{".gen/", []string{".gen", ".gen/", "**/.gen", "**/.gen/"}},
}

// writeInitGitignore creates the workspace .gitignore, or appends to the
// repository's own the putnamiGitignoreEntries it does not list yet. Every
// other line of an existing file is the team's and stays untouched.
func writeInitGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(defaultGitignore), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ .gitignore created")
		}
		return
	}
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	missing := missingGitignoreEntries(string(data))
	if len(missing) == 0 {
		return
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text != "" {
		text += "\n"
	}
	text += "# Putnami\n" + strings.Join(missing, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err == nil {
		iox.Fprintf(os.Stdout, "  ✓ .gitignore lists %s\n", strings.Join(missing, " and "))
	}
}

// missingGitignoreEntries returns the putnamiGitignoreEntries no line of a
// .gitignore covers, in their order.
func missingGitignoreEntries(gitignore string) []string {
	lines := make(map[string]bool)
	for _, line := range strings.Split(gitignore, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, e := range putnamiGitignoreEntries {
		if !slices.ContainsFunc(e.covering, func(spelling string) bool { return lines[spelling] }) {
			missing = append(missing, e.entry)
		}
	}
	return missing
}

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

// initPlan is what an init run declares: the languages, in the order
// --extension names them, and one config per language. An init --from run
// declares no language, and its one config is the tool's.
type initPlan struct {
	languages []initLanguage
	configs   []initExtensionConfig
}

// initPlanFor is the plan that declares languages.
func initPlanFor(languages []initLanguage) initPlan {
	plan := initPlan{languages: languages}
	for _, language := range languages {
		plan.configs = append(plan.configs, initConfigFor(language))
	}
	return plan
}

// starter is the config whose template --project renders: the first one's.
func (p initPlan) starter() initExtensionConfig {
	return p.configs[0]
}

// packageNames are the extensions of the plan's configs, in order.
func (p initPlan) packageNames() []string {
	names := make([]string, len(p.configs))
	for i, config := range p.configs {
		names[i] = config.packageName
	}
	return names
}

// resolveInitRequest validates what an init run was asked for before anything
// is written: the languages, the starter, the project placement and the
// channel. --from names the starter of a monorepo tool, which takes no project
// and no --extension: the tool's packages are the projects.
func resolveInitRequest(flags initFlags) (initPlan, initChannel, error) {
	var plan initPlan
	if flags.fromSet {
		if flags.from == "" {
			return initPlan{}, initChannel{}, cmderr.Usagef("--from needs a tool (choose: %s)", initSourceNames())
		}
		source, known := initSources[flags.from]
		if !known {
			return initPlan{}, initChannel{}, cmderr.Usagef("unknown --from tool %q (choose: %s)", flags.from, initSourceNames())
		}
		if flags.project != "" || flags.projectPath != "" || flags.extensionSet {
			return initPlan{}, initChannel{}, cmderr.Usagef("--from %s does not combine with --project, --project-path or --extension: the %s packages become the projects", flags.from, flags.from)
		}
		if err := validateInitMode(flags, source); err != nil {
			return initPlan{}, initChannel{}, err
		}
		if err := validateInitImport(flags, source); err != nil {
			return initPlan{}, initChannel{}, err
		}
		plan = initPlan{configs: []initExtensionConfig{source.starter}}
	} else {
		if flags.modeSet || flags.apply || flags.importSet {
			return initPlan{}, initChannel{}, cmderr.Usagef("--mode, --import and --apply adopt a monorepo tool: give --from <tool> (choose: %s)", initSourceNames())
		}
		value := flags.extension
		if !flags.extensionSet {
			value = initDefaultLanguage
		}
		languages, err := parseInitLanguages(value)
		if err != nil {
			return initPlan{}, initChannel{}, err
		}
		plan = initPlanFor(languages)
		if flags.project != "" && plan.starter().template == "" {
			return initPlan{}, initChannel{}, cmderr.Usagef("--project renders the starter of the first language of --extension, and %s ships none (languages with a starter: %s)",
				plan.languages[0].Language, initStarterLanguages())
		}
	}
	if flags.projectPath != "" && flags.project == "" {
		return initPlan{}, initChannel{}, cmderr.Usagef("--project-path requires --project")
	}
	channel, err := resolveInitChannel(flags, os.Getenv, runningCLIName())
	if err != nil {
		return initPlan{}, initChannel{}, err
	}
	return plan, channel, nil
}

// initProposes reports whether init chooses the languages itself: no
// --extension, --from or --project names what to declare (D-067).
func initProposes(flags initFlags) bool {
	return !flags.extensionSet && !flags.fromSet && flags.project == ""
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

	plan, channel, err := resolveInitRequest(flags)
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

	// A rerun of init --from previews the adoption, and writes only with
	// --apply.
	if source, ok := initSources[flags.from]; ok && wsRoot != "" && !flags.force {
		if !holdsAny(wsRoot, source.markers) {
			return cmderr.Usagef("putnami init --from %s runs at the root of a %s repository, and %s holds no %s", flags.from, flags.from, wsRoot, strings.Join(source.markers, ", "))
		}
		return rerunInitFrom(ctx, wsRoot, flags, source, env)
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
	if initProposes(flags) {
		languages, err := proposeInitLanguages(cwd)
		if err != nil {
			return err
		}
		if len(languages) > 0 {
			plan = initPlanFor(languages)
		}
	}
	// The repository as init found it decides what it adopts: the markers the
	// starter project writes are not a repository to adopt.
	adopted := detectInitLanguages(cwd, plan.languages)
	extConfig := plan.starter()
	declared := initDeclaredExtensions(plan.configs...)
	var options map[string]initToolOptions
	// fromMode and fromImports are the mode and import list of a tool's
	// adoption, which the note after its sync reads.
	var fromMode string
	var fromImports []string
	if source, ok := initSources[flags.from]; ok {
		if !holdsAny(cwd, source.markers) {
			return cmderr.Usagef("putnami init --from %s runs at the root of a %s repository, and %s holds no %s", flags.from, flags.from, cwd, strings.Join(source.markers, ", "))
		}
		mode, err := resolveInitMode(flags, source, "")
		if err != nil {
			return err
		}
		imports, _, err := initImportList(flags, mode, nil)
		if err != nil {
			return err
		}
		fromMode, fromImports = mode, imports
		declared = []string{source.starter.packageName}
		options = initOptions(source, mode, imports)
		if source.offersBothModes() {
			iox.Fprintf(os.Stdout, "  Mode: %s\n", mode)
		}
	}

	// --- Phase 1: Initialize workspace ---
	iox.Fprintf(os.Stdout, "  → Initializing workspace…\n")
	printInitChannel(channel)

	// Init git repo if git is available and no .git exists
	if _, err := exec.LookPath("git"); err == nil {
		gitDir := filepath.Join(cwd, ".git")
		if _, err := os.Stat(gitDir); os.IsNotExist(err) {
			cmd := exec.CommandContext(ctx, gitread.Executable(), "init")
			cmd.Dir = cwd
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Run(); err == nil {
				iox.Fprintln(os.Stdout, "  ✓ Git repository initialized")
			}
		}
	}

	// Create .gitignore, or add the directories Putnami writes to the one the
	// repository has. Here and for README.md and .gitattributes below, a
	// symbolic link is an existing file, even one whose target does not
	// resolve, and a write never goes through it.
	writeInitGitignore(cwd)

	// Create README.md if it doesn't exist, so the workspace documents what it
	// is and how to check a change from its first commit. An existing file is
	// the team's and stays untouched.
	readmePath := filepath.Join(cwd, "README.md")
	if _, err := os.Lstat(readmePath); os.IsNotExist(err) {
		if err := os.WriteFile(readmePath, []byte(workspaceReadme(workspaceName(flags, cwd))), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ README.md created")
		}
	}

	// Create .gitattributes with the LF policy (D-W4) if it doesn't exist, so
	// every checkout, Windows included, hashes the committed bytes. An existing
	// file is the team's and stays untouched; `putnami doctor` reports a
	// checkout that converts line endings without the policy.
	gitattributesPath := filepath.Join(cwd, ".gitattributes")
	if _, err := os.Lstat(gitattributesPath); os.IsNotExist(err) {
		if err := os.WriteFile(gitattributesPath, []byte(git.LFPolicyAttributes+"\n"), 0o644); err == nil {
			iox.Fprintln(os.Stdout, "  ✓ .gitattributes created (LF line endings)")
		}
	}

	// Place the Putnami guidance block in the assistant entrypoints. Nothing
	// else is written for agents (ADR 0055).
	if err := agentctx.WriteAgentEntrypoints(cwd); err == nil {
		iox.Fprintln(os.Stdout, "  ✓ Assistant guidance written (CLAUDE.md, AGENTS.md)")
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
		Extensions:     declared,
		Templates:      templates,
		AgentArtifacts: initAgentArtifacts(cwd, plan.configs...),
		Registries:     initRegistries(cwd, plan.configs...),
		Includes:       []string{},
		Options:        options,
	}

	cfgPath := filepath.Join(cwd, wsproto.WorkspaceConfigFilename)
	data, err := wsproto.RenderConfigFileAt(cfgPath, cfg, nil)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	iox.Fprintln(os.Stdout, "  ✓ Workspace config written (putnami.workspace.json)")

	// --- Phase 2: Install extension ---
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Adding extension…\n")

	// Reload config from disk (now that putnami.workspace.json exists)
	wsCfg := wsproto.Load(cwd)

	installs := plan.packageNames()
	if flags.from != "" {
		// A tool's import mode hands targets to other extensions, which
		// install beside the tool's.
		installs = declared
	}
	for _, name := range installs {
		if err := installExtension(ctx, cwd, wsCfg, []string{shared.ChannelArtifact(name, env.channel)}, ""); err != nil {
			return protocolcli.WithNext(
				fmt.Errorf("install extension %s: %w", name, err),
				"putnami extensions install",
			)
		}
		wsCfg = wsproto.Load(cwd)
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
		if err := installInitAgentContent(ctx, cwd, wsCfg, env.channel, plan.configs...); err != nil {
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

	// Write the host hooks the opted-in extensions declare (ADR 0081). It
	// runs here, not beside the guidance block, because the workspace
	// config and the extensions exist only now. A new workspace binds no
	// collaboration contract, so this writes nothing until one is bound.
	for _, change := range agentctx.RegisterHostHooks(cwd, wsCfg, os.Stderr) {
		iox.Fprintf(os.Stdout, "  ✓ %s\n", change.Description())
	}

	// Pin what the extensions answer, then run workspace-install, which runs
	// the toolchains the lock pins.
	if err := installInitWorkspace(ctx, cwd, wsCfg, env); err != nil {
		return err
	}

	if flags.project != "" {
		if err := initializeProject(ctx, cwd, flags, extConfig, projectPath, env); err != nil {
			return err
		}
	}

	// A monorepo tool's packages become the projects: the sync records the
	// membership the extension's probe answers, then runs its sync task,
	// which writes each package's putnami.json.
	if flags.from != "" {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  → Synchronizing the %s packages…\n", flags.from)
		before := initDeclaredNames(cwd, wsproto.Load(cwd))
		if err := syncInitProjects(ctx, cwd, wsproto.Load(cwd), env); err != nil {
			return protocolcli.WithNext(fmt.Errorf("synchronize the %s packages: %w", flags.from, err), "putnami projects sync")
		}
		if err := installSyncDeclaredExtensions(ctx, cwd, before, env); err != nil {
			return err
		}
		installs = initDeclaredNames(cwd, wsproto.Load(cwd))
		noteNothingMoves(cwd, flags.from, initSources[flags.from], fromMode, fromImports)
	}

	// A language whose markers the repository held is adopted: the sync
	// records the projects its extension's probe answers.
	if len(adopted) > 0 {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  → Synchronizing the %s projects…\n", initLanguageNamesOf(detectedLanguages(adopted)))
		before := initDeclaredNames(cwd, wsproto.Load(cwd))
		if err := syncInitProjects(ctx, cwd, wsproto.Load(cwd), env); err != nil {
			return protocolcli.WithNext(fmt.Errorf("synchronize the %s projects: %w", initLanguageNamesOf(detectedLanguages(adopted)), err), "putnami projects sync")
		}
		if err := installSyncDeclaredExtensions(ctx, cwd, before, env); err != nil {
			return err
		}
	}

	// End with the lock metadata refresh `putnami install` ends with.
	if _, err := refreshInitLockMetadata(ctx, cwd, env.CLIVersion); err != nil {
		return protocolcli.WithNext(fmt.Errorf("refresh lock metadata: %w", err), "putnami install")
	}

	// --- Summary ---
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  ✓ Workspace ready: %s\n", name)
	iox.Fprintf(os.Stdout, "    Path: %s\n", cwd)
	iox.Fprintf(os.Stdout, "    Extension: %s\n", strings.Join(installs, ", "))
	iox.Fprintln(os.Stdout, "    Agent skills: not installed. To add them, run putnami extensions install @putnami/contributor")
	iox.Fprintln(os.Stdout)

	iox.Fprintln(os.Stdout, "  Next:")
	switch {
	case flags.project != "":
		iox.Fprintf(os.Stdout, "    putnami lint,test,build %s\n", flags.project)
		iox.Fprintf(os.Stdout, "    putnami serve %s\n", flags.project)
	case flags.from != "" || len(adopted) > 0:
		iox.Fprintln(os.Stdout, "    putnami projects list")
		iox.Fprintln(os.Stdout, "    putnami lint,test,build --impacted")
	case extConfig.template != "":
		iox.Fprintf(os.Stdout, "    putnami projects create my-app --template %s\n", extConfig.template)
		iox.Fprintln(os.Stdout, "    putnami build --all")
	default:
		iox.Fprintf(os.Stdout, "    putnami projects sync    # once the repository holds a %s\n", plan.languages[0].Markers[0])
		iox.Fprintln(os.Stdout, "    putnami lint,test,build --impacted")
	}

	return nil
}

// installInitWorkspace is init's first install: the toolchain pins phase,
// then the workspace installers, which run the toolchains the lock pins. A
// failed phase names its own next step, or `putnami install`, and runs no
// installer.
func installInitWorkspace(ctx context.Context, cwd string, cfg *wsproto.Config, env LifecycleEnv) error {
	pins, err := pinInitToolchains(ctx, cwd, env)
	if err != nil {
		err = fmt.Errorf("pin toolchains: %w", err)
		if protocolcli.SuggestedNext(err) != "" {
			return err
		}
		return protocolcli.WithNext(err, "putnami install")
	}
	if pins.Written {
		iox.Fprintln(os.Stdout, "  ✓ Toolchain pins recorded")
	}
	if err := installInitDependencies(ctx, cwd, cfg, "", "", env); err != nil {
		return protocolcli.WithNext(fmt.Errorf("install workspace dependencies: %w", err), "putnami deps install")
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
	if err := installInitTemplate(ctx, cwd, wsCfg, []string{shared.ChannelArtifact(extConfig.template, env.channel)}, ""); err != nil {
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

	if err := createInitProject(ctx, cwd, wsCfg, createArgs, env); err != nil {
		// A failed create may leave the project directory behind, which only
		// --force lets the retry render over.
		return protocolcli.WithNext(
			fmt.Errorf("create project %s: %w", flags.project, err),
			projectsCreateRetryCommand(flags.project, createFlags{template: extConfig.template, path: flags.projectPath}, projectPath),
		)
	}

	wsCfg = wsproto.Load(cwd)
	projects := membersWith(wsCfg.Includes, projectPath)
	if err := updateConfigMembership(cwd, nil, projects); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("register project %s in workspace config: %w", flags.project, err),
			"putnami projects sync",
		)
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

// membersWith returns the sorted members of a workspace with projectPath among
// them, each listed once. The project create that precedes it already lists the
// project in the workspace config, so projectPath is usually a member already.
func membersWith(members []string, projectPath string) []string {
	projects := append(slices.Clone(members), projectPath)
	slices.Sort(projects)
	return slices.Compact(projects)
}

// installInitAgentContent installs each extension the configs opt into, on
// channel when init chose one, then materializes the agent content every
// opt-in selects from the release the lock pins.
func installInitAgentContent(ctx context.Context, cwd string, wsCfg *wsproto.Config, channel string, configs ...initExtensionConfig) error {
	var installed []string
	for _, config := range configs {
		if config.agentContent == "" || slices.Contains(installed, config.agentContent) {
			continue
		}
		installed = append(installed, config.agentContent)
		if err := installExtension(ctx, cwd, wsCfg, []string{shared.ChannelArtifact(config.agentContent, channel)}, ""); err != nil {
			return fmt.Errorf("install extension %s: %w", config.agentContent, err)
		}
	}
	return agentctx.AdoptAgentWorkflows(ctx, cwd, wsCfg, os.Stdout)
}

// initDeclaredExtensions is the new workspace's `extensions`: each config's
// extension, then the extension whose agent content it opts into, each once.
func initDeclaredExtensions(configs ...initExtensionConfig) []string {
	var extensions []string
	for _, config := range configs {
		for _, name := range []string{config.packageName, config.agentContent} {
			if name != "" && !slices.Contains(extensions, name) {
				extensions = append(extensions, name)
			}
		}
	}
	return extensions
}

// initLanguageNamesOf joins the ids of languages for a sentence: "rust",
// "rust and java", "rust, java and go".
func initLanguageNamesOf(languages []initLanguage) string {
	names := make([]string, len(languages))
	for i, language := range languages {
		names[i] = language.Language
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

type initConfig struct {
	Schema         string                     `json:"$schema"`
	Name           string                     `json:"name"`
	Extensions     []string                   `json:"extensions"`
	Templates      []string                   `json:"templates,omitempty"`
	AgentArtifacts []string                   `json:"agentArtifacts,omitempty"`
	Registries     map[string]json.RawMessage `json:"registries,omitempty"`
	Includes       []string                   `json:"includes"`
	Options        map[string]initToolOptions `json:"options,omitempty"`
}

// initRegistries is the new workspace's `registries`: the entries of each
// config's starter, and every entry a config already at this path declares,
// which wins. An entry is kept or replaced whole, never merged key by key: its
// keys belong to the profile of the extension that owns the ecosystem.
func initRegistries(cwd string, configs ...initExtensionConfig) map[string]json.RawMessage {
	registries := make(map[string]json.RawMessage)
	for _, config := range configs {
		for ecosystem, entry := range config.registries {
			registries[ecosystem] = entry
		}
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
// configs opt into, merged with what a config already at this path declared.
//
// The merge exists because `putnami init --force` REWRITES the workspace config
// of a workspace that may already have opted in. Dropping that array would
// silently un-declare content whose files are already installed and recorded
// — the next `install` would then report every one of them as an unmanaged
// collision. Re-initializing must not be able to do that.
//
// It merges each opt-in by the extension it NAMES, not by how it is spelled,
// and the existing declaration wins. An entry in a form this CLI no longer
// installs is kept under its own spelling: dropping it would orphan its files
// and records, while keeping it makes every later command refuse it and say
// how to replace it.
func initAgentArtifacts(cwd string, configs ...initExtensionConfig) []string {
	byName := make(map[string]string)
	record := func(references []string, overwrite bool) {
		for _, reference := range references {
			reference = strings.TrimSpace(reference)
			if reference == "" {
				continue
			}
			name := agentctx.AgentArtifactDeclarationName(reference)
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
	for _, config := range configs {
		if config.agentContent != "" {
			starter = append(starter, "extension:"+config.agentContent)
		}
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
