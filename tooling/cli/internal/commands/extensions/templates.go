package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/template"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/template"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TemplatesInstall installs templates from the workspace config and lock file.
// If a specific name is given, only that template is installed/added.
// Pass --latest in args to ignore the lock file and resolve the latest versions.
// outputFormat selects the result rendering ("jsonl" for machine-readable).
func TemplatesInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return templatesInstall(ctx, wsRoot, cfg, args, outputFormat, os.Stdout)
}

// templatesInstall is TemplatesInstall with the human progress stream chosen by
// the caller — see extensionsInstall for why.
func templatesInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, out io.Writer) error {
	return TemplatesInstallWithOptions(ctx, wsRoot, cfg, args, InstallOptions{OutputFormat: outputFormat, Out: out})
}

// TemplatesInstallWithWriter is TemplatesInstall with the human progress
// stream chosen by the caller. It exists for internal/commands' combined
// `install` lifecycle command (install.go), which is outside this vertical and
// threads its own writer through — see ExtensionsInstallWithWriter for why.
func TemplatesInstallWithWriter(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, out io.Writer) error {
	return templatesInstall(ctx, wsRoot, cfg, args, outputFormat, out)
}

// TemplatesInstallWithOptions installs templates and reports typed outcomes in
// the same stable order as extension installs.
func TemplatesInstallWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, opts InstallOptions) error {
	return installArtifactsWithOptions(ctx, wsRoot, cfg, args, templateOps(wsRoot), opts)
}

// TemplatesUpdate updates one or all templates to the latest compatible version.
// outputFormat selects the result rendering ("jsonl" for machine-readable).
func TemplatesUpdate(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return TemplatesUpdateWithOptions(ctx, wsRoot, cfg, args, ArtifactUpdateOptions{}, outputFormat)
}

func TemplatesUpdateWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, opts ArtifactUpdateOptions, outputFormat string) error {
	return updateArtifacts(ctx, wsRoot, cfg, args, templateOps(wsRoot), opts, outputFormat)
}

// TemplatesList shows all available templates (configured, workspace, and installed).
func TemplatesList(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	lockFile, lockErr := lockfile.ReadLockFile(wsRoot)
	if lockErr != nil {
		noteUnreadableLock(lockErr) // see ExtensionsList for why
	}

	tplMap := shared.BuildTemplateMap(cfg)

	// Discover workspace templates
	ws, _ := workspace.Load(wsRoot)
	var projectPaths []string
	if ws != nil {
		for _, p := range ws.Projects {
			projectPaths = append(projectPaths, p.Path)
		}
	}
	discovered, _ := template.DiscoverTemplates(wsRoot, projectPaths)

	if outputFormat == "jsonl" {
		return templatesListJSONL(tplMap, lockFile, discovered)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  %-30s %-15s %-15s %s\n", "TEMPLATE", "CONSTRAINT", "INSTALLED", "SOURCE")
	iox.Fprintf(os.Stdout, "  %-30s %-15s %-15s %s\n", "--------", "----------", "---------", "------")

	for name, constraint := range tplMap {
		installed := "-"
		source := "config"
		if lockFile != nil {
			if entry, ok := lockFile.GetTemplate(name); ok {
				installed = entry.Version
			}
		}
		iox.Fprintf(os.Stdout, "  %-30s %-15s %-15s %s\n", name, constraint, installed, source)
	}

	for _, tpl := range discovered {
		if _, inConfig := tplMap[tpl.Name]; inConfig {
			continue
		}
		version := tpl.Version
		if version == "" {
			version = "-"
		}
		iox.Fprintf(os.Stdout, "  %-30s %-15s %-15s %s\n", tpl.Name, "-", version, "workspace")
	}

	iox.Fprintln(os.Stdout)
	return nil
}

// TemplatesRemove removes a template from config and disk.
func TemplatesRemove(wsRoot string, cfg *wsproto.Config, args []string) error {
	return removeArtifact(wsRoot, args, templateOps(wsRoot))
}

// parseTemplateArg parses "name", "name@version", "@scope/name" or
// "@scope/name@version" from args.
func parseTemplateArg(arg string) (string, string) {
	return splitArtifactArg(arg)
}

func templatesListJSONL(tplMap map[string]string, lockFile *lockfile.LockFile, discovered []*template.TemplateDescription) error {
	var entries []artifactListEntry
	for name, constraint := range tplMap {
		entry := artifactListEntry{Name: name, Constraint: constraint, Source: "config"}
		if lockFile != nil {
			if le, ok := lockFile.GetTemplate(name); ok {
				entry.Installed = le.Version
			}
		}
		entries = append(entries, entry)
	}

	for _, tpl := range discovered {
		if _, inConfig := tplMap[tpl.Name]; inConfig {
			continue
		}
		entries = append(entries, artifactListEntry{Name: tpl.Name, Installed: tpl.Version, Source: "workspace"})
	}

	return printArtifactListJSONL(entries)
}

// TemplatesValidate validates a template manifest against the protocol.
// If no path is given, it looks for putnami.template.json in the current directory.
func TemplatesValidate(args []string) error {
	path, err := resolveTemplateManifestPath(args)
	if err != nil {
		return err
	}

	iox.Fprintf(os.Stdout, "  Validating %s\n\n", path)

	// Read and parse manifest via protocol strict parser.
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read template manifest: %w", err)
	}

	manifest, diags := proto.ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		iox.Fprintf(os.Stdout, "  ✗ Validation failed:\n")
		for _, d := range diag.Errors(diags) {
			// Carry the originating manifest path so the diagnostic points at a
			// concrete location, matching the file:line rendering used by
			// `sessions inspect`. The diagnostic protocol models no line/column
			// for manifest validation, so only the file is surfaced.
			if d.Field != "" {
				iox.Fprintf(os.Stdout, "    - %s: [%s] %s: %s\n", path, d.Code, d.Field, d.Message)
			} else {
				iox.Fprintf(os.Stdout, "    - %s: [%s] %s\n", path, d.Code, d.Message)
			}
		}
		iox.Fprintln(os.Stdout)
		return cmderr.Classify(fmt.Errorf("template manifest is invalid"), cmderr.ErrInvalidConfig)
	}
	iox.Fprintf(os.Stdout, "  ✓ Manifest parsed and validated (name: %s)\n", manifest.Name)

	// Check template directory contents (filesystem check, stays in CLI).
	templateDir := filepath.Dir(path)
	templateFiles, err := findTemplateFiles(templateDir)
	if err != nil {
		iox.Fprintf(os.Stdout, "  ✗ Cannot read template directory: %v\n\n", err)
		return err
	}
	if len(templateFiles) > 0 {
		iox.Fprintf(os.Stdout, "  ✓ %d template file(s) found\n", len(templateFiles))
	} else {
		iox.Fprintf(os.Stdout, "  ! No .template files found (optional)\n")
	}

	iox.Fprintf(os.Stdout, "\n  Template is valid.\n")
	return nil
}

// TemplatesTest tests a template in a temporary workspace.
//
// Phases 1 and 2 render the template into a scratch directory outside the
// project tree and check the result, so a broken template fails before any
// install. Phase 3 does what a user does: it installs the template's
// extension, runs `projects create` from a copy of the template that the
// workspace discovers by convention, then runs the build and test the
// extension declares on the created project. The workspace config lists the
// extension only; `projects create` adds the project and runs the extension's
// project-setup job. channel is the release channel the extension installs on,
// or "" for the workspace default.
func TemplatesTest(ctx context.Context, args []string, channel string) error {
	var skipBuild, keepDir bool
	var filteredArgs []string
	for _, a := range args {
		switch a {
		case "--skip-build":
			skipBuild = true
		case "--keep":
			keepDir = true
		default:
			filteredArgs = append(filteredArgs, a)
		}
	}

	path, err := resolveTemplateManifestPath(filteredArgs)
	if err != nil {
		return err
	}

	templateDir := filepath.Dir(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read template manifest: %w", err)
	}

	var manifest template.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return cmderr.Classify(fmt.Errorf("template manifest is invalid: %w", err), cmderr.ErrInvalidConfig)
	}

	iox.Fprintf(os.Stdout, "  Testing template: %s\n", manifest.Name)
	iox.Fprintf(os.Stdout, "  Source: %s\n\n", templateDir)

	// Build render variables, applying testVariables overrides from manifest.
	vars := template.DefaultTestVars()
	if manifest.TestVariables != nil {
		if v, ok := manifest.TestVariables["projectName"]; ok {
			vars.ProjectName = v
		}
		if v, ok := manifest.TestVariables["projectModule"]; ok {
			vars.ProjectModule = v
		}
	}

	// Create temp workspace
	tmpDir, err := os.MkdirTemp("", "putnami-tpl-test-")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	if !keepDir {
		defer os.RemoveAll(tmpDir)
	} else {
		iox.Fprintf(os.Stdout, "  Temp workspace: %s\n\n", tmpDir)
	}

	if err := writeTemplateTestWorkspace(tmpDir, manifest.Extension); err != nil {
		return err
	}

	// Render template into a scratch directory the workspace does not list.
	projectDir := filepath.Join(tmpDir, templateRenderDir, vars.ProjectPath)
	if err := os.MkdirAll(filepath.Dir(projectDir), 0o755); err != nil {
		return fmt.Errorf("create project parent: %w", err)
	}

	iox.Fprintf(os.Stdout, "  Phase 1: Render template\n")
	if err := template.RenderDir(templateDir, projectDir, vars); err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	iox.Fprintf(os.Stdout, "  ✓ Template rendered to %s\n\n", vars.ProjectPath)

	// Validate project structure
	iox.Fprintf(os.Stdout, "  Phase 2: Validate project structure\n")
	projectCfg := filepath.Join(projectDir, "putnami.json")
	if _, err := os.Stat(projectCfg); os.IsNotExist(err) {
		return fmt.Errorf("rendered template has no putnami.json — template may be broken")
	}
	iox.Fprintf(os.Stdout, "  ✓ putnami.json present\n\n")

	if skipBuild {
		iox.Fprintf(os.Stdout, "  Skipping build+test (--skip-build)\n\n")
		iox.Fprintf(os.Stdout, "  Template %s is valid.\n", manifest.Name)
		return nil
	}

	// Run build+test using the CLI binary
	iox.Fprintf(os.Stdout, "  Phase 3: Build and test rendered project\n")

	cliPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate CLI executable: %w", err)
	}

	// Make the template discoverable the way a workspace template is, so
	// `projects create` finds it by name.
	templateName := manifest.Name
	if templateName == "" {
		templateName = filepath.Base(templateDir)
	}
	if err := stageTemplateUnderTest(tmpDir, templateDir); err != nil {
		return err
	}

	// These nested CLI invocations can run for a long time and fan out to
	// grandchildren (job runner subprocesses), so spawn them in their own
	// process group under ctx — a canceled `templates test` then tears down
	// the whole tree instead of orphaning it.
	env := append(os.Environ(), "PUTNAMI_NO_PROGRESS=true")

	// Install the template's extension by name, on the channel the run chose.
	// The workspace config keeps the bare name, as init leaves it.
	if manifest.Extension != "" {
		artifact := manifest.Extension
		if channel != "" {
			artifact += "@" + channel
			iox.Fprintf(os.Stdout, "  Channel: %s\n", channel)
		}
		iox.Fprintf(os.Stdout, "  Running: putnami extensions install %s\n", artifact)
		if err := shared.RunGroupStreaming(ctx, tmpDir, env, cliPath, "extensions", "install", artifact); err != nil {
			return fmt.Errorf("extension install failed: %w", err)
		}
	}

	// Create the project from the template, as a user does. Create renders it,
	// lists it in the workspace config and runs the extension's project-setup
	// job, which pins the toolchains and the module versions.
	iox.Fprintf(os.Stdout, "\n  Running: putnami projects create %s --template %s\n", vars.ProjectName, templateName)
	if err := shared.RunGroupStreaming(ctx, tmpDir, env, cliPath, "projects", "create", vars.ProjectName, "--template", templateName); err != nil {
		return fmt.Errorf("projects create failed for template %s: %w", manifest.Name, err)
	}

	// Run the verbs the extension declares on the created project. An
	// extension that declares no build, such as Python's, is tested without it.
	verbs, err := declaredCheckVerbs(tmpDir)
	if err != nil {
		return fmt.Errorf("read the installed extensions: %w", err)
	}
	if len(verbs) == 0 {
		return fmt.Errorf("extension %s declares neither build nor test: nothing to run on template %s", manifest.Extension, manifest.Name)
	}
	command := strings.Join(verbs, ",")
	iox.Fprintf(os.Stdout, "\n  Running: putnami %s --projects %s\n", command, vars.ProjectName)
	if err := shared.RunGroupStreaming(ctx, tmpDir, env, cliPath, command, "--projects", vars.ProjectName, "--no-cache", "--verbose"); err != nil {
		return fmt.Errorf("%s failed for template %s: %w", command, manifest.Name, err)
	}

	iox.Fprintf(os.Stdout, "\n  ✓ Template %s passes %s.\n", manifest.Name, strings.Join(verbs, " and "))
	return nil
}

// declaredCheckVerbs returns the verbs of build and test, in that order, that
// an extension installed in the workspace at workspaceDir declares. It reads
// the extensions in process, as the planner does.
func declaredCheckVerbs(workspaceDir string) ([]string, error) {
	found, err := extension.DiscoverExtensions(workspaceDir, wsproto.Load(workspaceDir), nil)
	if err != nil {
		return nil, err
	}
	declarers := extension.PublicDeclarers([]string{"build", "test"}, found)
	var verbs []string
	for _, verb := range []string{"build", "test"} {
		if len(declarers[verb]) > 0 {
			verbs = append(verbs, verb)
		}
	}
	return verbs, nil
}

// templateRenderDir is the scratch directory of phases 1 and 2. It starts with
// a dot, so template discovery skips it.
const templateRenderDir = ".render"

// templateDomainDir is the directory of the test workspace that holds the
// template under test, at templateDomainDir/templates/<dir>: the layout
// template discovery scans.
const templateDomainDir = "under-test"

// stageTemplateUnderTest copies templateDir into the test workspace at
// templateDomainDir/templates/<dir>, where template discovery finds it by
// convention. The copy is no project: the workspace config lists none.
func stageTemplateUnderTest(workspaceDir, templateDir string) error {
	absTemplateDir, err := filepath.Abs(templateDir)
	if err != nil {
		return fmt.Errorf("resolve template directory: %w", err)
	}
	copyDir := filepath.Join(workspaceDir, templateDomainDir, "templates", filepath.Base(absTemplateDir))
	if err := os.CopyFS(copyDir, os.DirFS(absTemplateDir)); err != nil {
		return fmt.Errorf("copy template into the workspace: %w", err)
	}
	return nil
}

// writeTemplateTestWorkspace writes the putnami.workspace.json of a template
// test workspace into dir. It names the workspace and, when the template names
// one, activates the template's extension. It lists no project and holds no
// language manifest: `projects create` adds the project, and the project's own
// files carry its language manifests.
func writeTemplateTestWorkspace(dir, extension string) error {
	cfg := wsproto.Config{Name: "template-test-workspace"}
	if extension != "" {
		cfg.Extensions = wsproto.ExtensionsConfig{List: map[string]string{extension: ""}}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write workspace config: %w", err)
	}
	return nil
}

// TemplatesPackage packages a template into a distributable archive.
func TemplatesPackage(ctx context.Context, args []string) error {
	var opts template.PackageOptions
	var filteredArgs []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			if i+1 < len(args) {
				opts.Version = args[i+1]
				i++
			}
		case "--stable":
			opts.Stable = true
		default:
			filteredArgs = append(filteredArgs, args[i])
		}
	}

	path, err := resolveTemplateManifestPath(filteredArgs)
	if err != nil {
		return err
	}

	templateDir := filepath.Dir(path)
	iox.Fprintf(os.Stdout, "  Packaging template at %s\n\n", templateDir)

	result, err := template.Package(ctx, templateDir, opts)
	if err != nil {
		return fmt.Errorf("package template: %w", err)
	}

	iox.Fprintf(os.Stdout, "  ✓ Archive: %s\n", result.ArchivePath)
	iox.Fprintf(os.Stdout, "  ✓ Version: %s\n", result.Version)
	iox.Fprintf(os.Stdout, "  ✓ Integrity: %s\n", result.Integrity)
	iox.Fprintf(os.Stdout, "  ✓ Metadata: %s\n\n", result.MetadataPath)

	return nil
}

// resolveTemplateManifestPath finds the putnami.template.json path from args or cwd.
func resolveTemplateManifestPath(args []string) (string, error) {
	if len(args) > 0 {
		path := args[0]
		info, err := os.Stat(path)
		if err != nil {
			return "", cmderr.NotFoundf("path not found: %s", path)
		}
		if info.IsDir() {
			path = filepath.Join(path, "putnami.template.json")
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("no putnami.template.json found at %s", path)
		}
		return path, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	path := filepath.Join(cwd, "putnami.template.json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no putnami.template.json in current directory")
	}
	return path, nil
}

// findTemplateFiles walks a directory and returns paths to .template files.
func findTemplateFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".template" {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}
