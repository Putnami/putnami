package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
	"go.putnami.dev/tooling/cli/internal/template"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

type createFlags struct {
	template string
	path     string
	force    bool
}

// ProjectsCreate scaffolds a new project from an extension template. A template
// that renders the Go framework version gets the one the module proxies name
// (resolveGoFrameworkVersion), and create fails before it writes anything when
// none answers. The create `putnami init` runs asks them for the version of
// the channel init chose (resolveGoFrameworkVersionOn). verbose names the proxy
// that answered. env runs the workspace installers when a Go project needs a go
// command the host does not have yet. When those installers fail but still
// leave a go, the project is set up and kept, and create exits non-zero naming
// the command that finishes the install, as `init --project` does. When go mod
// tidy fails, the project is kept and create exits non-zero quoting go's
// output and naming the create command to run again with --force.
func ProjectsCreate(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, verbose bool, env LifecycleEnv) error {
	created, err := createProjectFiles(ctx, wsRoot, cfg, args, verbose, env)
	if err != nil {
		return err
	}
	if created.installErr != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install workspace dependencies after creating %s: %w", created.name, created.installErr),
			"putnami deps install")
	}
	return nil
}

// createInitProjectFiles is the create `putnami init --project` runs: init
// runs the workspace installers itself and reports their failure
// (initializeProject).
func createInitProjectFiles(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, verbose bool, env LifecycleEnv) error {
	_, err := createProjectFiles(ctx, wsRoot, cfg, args, verbose, env)
	return err
}

// createdProject is a project createProjectFiles rendered and listed in the
// workspace config.
type createdProject struct {
	name string
	// installErr is the failure of the workspace installers the Go setup ran
	// to install a missing go command, which still left a go.
	installErr error
}

// createProjectFiles renders a template into a new project directory, lists
// the project and its extension in the workspace config, and sets a Go
// project up in go.work.
func createProjectFiles(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, verbose bool, env LifecycleEnv) (createdProject, error) {
	name, flags, err := parseProjectsCreateArgs(args)
	if err != nil {
		return createdProject{}, err
	}

	// Discover templates
	projectPaths := discoverProjectPaths(wsRoot)

	templates, err := template.DiscoverTemplates(wsRoot, projectPaths)
	if err != nil {
		return createdProject{}, fmt.Errorf("discover templates: %w", err)
	}

	// Find the template
	foundTemplate := template.FindByName(templates, flags.template)

	// If not found locally, try installing it
	if foundTemplate == nil {
		iox.Fprintf(os.Stdout, "  Template %q not found locally, attempting install…\n", flags.template)
		// The create of an init that chose a channel installs on that channel;
		// any other create installs as the workspace configures the template.
		if installErr := extensions.TemplatesInstall(ctx, wsRoot, cfg, []string{channelArtifact(flags.template, env.channel)}, ""); installErr == nil {
			// Re-discover after install
			templates, _ = template.DiscoverTemplates(wsRoot, projectPaths)
			foundTemplate = template.FindByName(templates, flags.template)
		}
	}

	if foundTemplate == nil {
		iox.Fprintf(os.Stderr, "  Template %q not found.\n\n  Available templates:\n", flags.template)
		for _, tpl := range templates {
			iox.Fprintf(os.Stderr, "    %-25s %s\n", tpl.Name, tpl.Description)
		}
		return createdProject{}, cmderr.NotFoundf("template not found: %s", flags.template)
	}

	// Determine target path
	projectPath, err := resolveProjectPath(name, flags.path)
	if err != nil {
		return createdProject{}, err
	}
	targetDir := filepath.Join(wsRoot, projectPath)
	if _, err := os.Stat(targetDir); err == nil && !flags.force {
		return createdProject{}, fmt.Errorf("directory already exists: %s (use --force to overwrite)", targetDir)
	}

	// Compute template variables
	vars := template.RenderVars{
		ProjectName:           name,
		ProjectPath:           projectPath,
		ProjectModule:         template.NormalizeProjectModule(name),
		PutnamiVersion:        "latest",
		WorkspaceRelativePath: workspaceRelativePath(projectPath),
	}
	// The Go framework version is asked of the module proxies only for a
	// template that renders it, and before anything is written: a failed lookup
	// leaves no project behind.
	usesFramework, err := template.UsesVariable(foundTemplate.Path, template.GoFrameworkVersionVariable)
	if err != nil {
		return createdProject{}, fmt.Errorf("read template %s: %w", flags.template, err)
	}
	if usesFramework {
		if message := refreshGoFrameworkCredential(ctx, wsRoot, os.Getenv); message != "" && verbose {
			iox.Fprintf(os.Stdout, "  %s\n", message)
		}
		version, proxy, err := resolveGoFrameworkVersionOn(ctx, http.DefaultClient, os.Getenv, env.channel)
		if err != nil {
			return createdProject{}, protocolcli.WithNext(err, projectsCreateCommand(name, flags, projectPath))
		}
		if verbose {
			iox.Fprintf(os.Stdout, "  Resolved %s %s from %s\n", goFrameworkModule, version, proxy)
		}
		vars.GoFrameworkVersion = version
	}

	// Copy template files
	if err := template.RenderDir(foundTemplate.Path, targetDir, vars); err != nil {
		return createdProject{}, fmt.Errorf("copy template: %w", err)
	}

	// Ensure the project's config lists the extension that provided the template.
	// Templates may omit this, but the job matcher requires it.
	if err := shared.EnsureProjectExtension(targetDir, foundTemplate.Extension); err != nil {
		return createdProject{}, fmt.Errorf("update project config: %w", err)
	}

	// Register the project in putnami.workspace.json
	if err := addProjectToWorkspaceConfig(wsRoot, projectPath); err != nil {
		iox.Fprintf(os.Stderr, "  Warning: could not update workspace config: %v\n", err)
	}

	// Ensure the workspace config lists the extension (needed for job matching)
	if err := ensureWorkspaceExtension(wsRoot, foundTemplate.Extension); err != nil {
		iox.Fprintf(os.Stderr, "  Warning: could not update workspace extensions: %v\n", err)
	}

	// The memoized workspace predates the project and the config writes above.
	// The jobs run from here on (the workspace-install that installs Go, and
	// init's next install) must plan over the new project, or they match
	// nothing.
	workspace.InvalidateLoadCache(wsRoot)

	// Go-specific post-create: update go.work and run go mod tidy
	created := createdProject{name: name}
	if isGoProject(targetDir) {
		installErr, err := setupGoProject(ctx, wsRoot, projectPath, foundTemplate.Extension, env)
		if err != nil {
			err = fmt.Errorf("go workspace setup: %w", err)
			if errors.Is(err, errGoModTidy) && installErr != nil {
				err = errors.Join(err, fmt.Errorf("install workspace dependencies: %w", installErr))
			}
			if errors.Is(err, errGoUnavailable) || errors.Is(err, errGoModTidy) {
				return createdProject{}, protocolcli.WithNext(err, projectsCreateRetryCommand(name, flags, projectPath))
			}
			return createdProject{}, err
		}
		created.installErr = installErr
	}

	iox.Fprintf(os.Stdout, "  Created project %s from template %s (%s)\n", name, flags.template, foundTemplate.Extension)
	iox.Fprintf(os.Stdout, "  Path: %s\n", targetDir)
	return created, nil
}

func parseProjectsCreateArgs(args []string) (string, createFlags, error) {
	if len(args) < 1 {
		return "", createFlags{}, fmt.Errorf("usage: projects create <name> --template <template>")
	}

	name := args[0]
	flags := createFlags{}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--template":
			if i+1 < len(args) {
				flags.template = args[i+1]
				i++
			}
		case "--path":
			if i+1 < len(args) {
				flags.path = args[i+1]
				i++
			}
		case "--force":
			flags.force = true
		}
	}

	if flags.template == "" {
		return "", createFlags{}, cmderr.Usagef("--template flag is required")
	}

	return name, flags, nil
}

// projectsCreateRetryCommand is the command that creates the project again
// over the directory a failed create left behind.
func projectsCreateRetryCommand(name string, flags createFlags, projectPath string) string {
	flags.force = true
	return projectsCreateCommand(name, flags, projectPath)
}

// projectsCreateCommand is the create command flags describe.
func projectsCreateCommand(name string, flags createFlags, projectPath string) string {
	command := "putnami projects create " + name + " --template " + flags.template
	if flags.path != "" {
		command += " --path " + projectPath
	}
	if flags.force {
		command += " --force"
	}
	return command
}

func defaultProjectPath(name string) string {
	return strings.ReplaceAll(name, "@", "")
}

// resolveProjectPath returns the workspace-relative project path in slash form.
// The path is written to putnami.workspace.json and go.work, which every host
// of the workspace reads, so a --path typed on Windows records no backslash.
func resolveProjectPath(name, customPath string) (string, error) {
	if customPath == "" {
		return defaultProjectPath(name), nil
	}
	// On Windows, a path rooted without a drive (\apps\api) and a path relative
	// to a drive (C:api) are not absolute, and neither is inside the workspace.
	if filepath.IsAbs(customPath) || filepath.VolumeName(customPath) != "" || strings.HasPrefix(filepath.ToSlash(customPath), "/") {
		return "", fmt.Errorf("--path must be relative to the workspace")
	}

	projectPath := filepath.ToSlash(filepath.Clean(customPath))
	if projectPath == "." || projectPath == "" || projectPath == ".." || strings.HasPrefix(projectPath, "../") {
		return "", fmt.Errorf("--path must stay within the workspace")
	}
	return projectPath, nil
}

func workspaceRelativePath(projectPath string) string {
	depth := len(strings.Split(filepath.Clean(projectPath), string(filepath.Separator)))
	return strings.TrimRight(strings.Repeat("../", depth), "/")
}

// discoverProjectPaths returns the paths of all projects in the workspace.
func discoverProjectPaths(wsRoot string) []string {
	ws, _ := workspace.Load(wsRoot)
	if ws == nil {
		return nil
	}
	paths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		paths[i] = p.Path
	}
	return paths
}

// ensureWorkspaceExtension adds an extension to the workspace config's extensions array if not already present.
func ensureWorkspaceExtension(wsRoot, extName string) error {
	cfgPath := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	if exts, ok := raw.Get("extensions"); ok {
		if arr, ok := exts.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok && s == extName {
					return nil // already present
				}
			}
		}
	}

	existing, _ := raw.Get("extensions")
	arr, _ := existing.([]any)
	raw.Set("extensions", append(arr, extName))
	return jsonutil.WriteFile(cfgPath, raw)
}

// addProjectToWorkspaceConfig adds a project path to the workspace config.
func addProjectToWorkspaceConfig(wsRoot, projectPath string) error {
	cfgPath := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	field := "includes"
	var includes []any
	if existing, ok := raw.Get("includes"); ok {
		if arr, ok := existing.([]any); ok {
			for _, p := range arr {
				if s, ok := p.(string); ok && s == projectPath {
					return nil // already listed
				}
			}
			includes = arr
		}
	} else if existing, ok := raw.Get("projects"); ok {
		if arr, ok := existing.([]any); ok {
			field = "projects"
			for _, p := range arr {
				if s, ok := p.(string); ok && s == projectPath {
					return nil // already listed
				}
			}
			includes = arr
		}
	}
	raw.Set(field, append(includes, projectPath))
	return jsonutil.WriteFile(cfgPath, raw)
}

// isGoProject returns true if the directory contains a go.mod file.
func isGoProject(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// setupGoProject adds the project to go.work (creating it if needed) and runs
// go mod tidy, with the go command resolveGoCommand finds. On a host without
// one, the Go the workspace pins is installed first (provisionGoCommand).
// installErr is the failure of the workspace installers that installed it; err
// is what stopped the setup, errGoModTidy when tidy failed.
func setupGoProject(ctx context.Context, wsRoot, projectPath, extensionName string, env LifecycleEnv) (installErr, err error) {
	goWork := filepath.Join(wsRoot, "go.work")
	projectDir := filepath.Join(wsRoot, projectPath)
	workspaceGoVersion := readGoDirective(goWork)
	if workspaceGoVersion == "" {
		workspaceGoVersion = readGoDirective(filepath.Join(projectDir, "go.mod"))
	}

	goCmd, err := ensureGoCommand(ctx, wsRoot, projectPath, workspaceGoVersion, extensionName, env)
	if err != nil {
		return nil, err
	}

	// Init go.work if it doesn't exist
	if _, err := os.Stat(goWork); os.IsNotExist(err) {
		if out, err := goCmd.run(ctx, wsRoot, "work", "init"); err != nil {
			return nil, fmt.Errorf("go work init: %s", out)
		}
		iox.Fprintf(os.Stdout, "  Initialized go.work\n")
	}

	// Add the project to go.work
	if out, err := goCmd.run(ctx, wsRoot, "work", "use", "./"+projectPath); err != nil {
		return nil, fmt.Errorf("go work use: %s", out)
	}
	if workspaceGoVersion != "" {
		if out, err := goCmd.run(ctx, wsRoot, "work", "edit", "-go="+workspaceGoVersion); err != nil {
			return nil, fmt.Errorf("go work edit: %s", out)
		}
		if out, err := goCmd.run(ctx, projectDir, "mod", "edit", "-go="+workspaceGoVersion); err != nil {
			return nil, fmt.Errorf("go mod edit: %s", out)
		}
	}
	iox.Fprintf(os.Stdout, "  Added %s to go.work\n", projectPath)

	// A project tidy cannot resolve does not build, so its failure fails the
	// setup and quotes what go said.
	if out, err := goCmd.run(ctx, projectDir, "mod", "tidy"); err != nil {
		return goCmd.installErr, fmt.Errorf("%w in %s: %w%s", errGoModTidy, projectPath, err, goOutputExcerpt(out))
	}

	return goCmd.installErr, nil
}

// errGoModTidy marks a Go project setup whose go mod tidy failed.
var errGoModTidy = errors.New("go mod tidy failed")

// goOutputMaxLines and goOutputMaxBytes bound the go command output an error
// quotes. go names the module it could not resolve last, so the end is kept.
const (
	goOutputMaxLines = 20
	goOutputMaxBytes = 2048
)

// goOutputExcerpt is the end of a go command's output, one indented line per
// line, after a newline; "" when the output is empty. URL credentials, which
// a GOPROXY entry can carry, are redacted before the output is cut.
func goOutputExcerpt(out []byte) string {
	text := strings.ReplaceAll(shared.RedactURLCredentials(string(out)), "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > goOutputMaxLines {
		lines = lines[len(lines)-goOutputMaxLines:]
	}
	text = strings.Join(lines, "\n")
	if len(text) > goOutputMaxBytes {
		text = strings.ToValidUTF8(text[len(text)-goOutputMaxBytes:], "")
	}
	if text == "" {
		return ""
	}
	return "\n    " + strings.ReplaceAll(text, "\n", "\n    ")
}

func readGoDirective(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	return ""
}
