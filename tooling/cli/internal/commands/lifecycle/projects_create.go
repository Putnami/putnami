package lifecycle

import (
	"context"
	"errors"
	"fmt"
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

// ProjectsCreate scaffolds a new project from an extension template, then runs
// the project-setup job of the project's extensions (setUpProject). env runs
// the job, and the workspace installers when a toolchain the job requires does
// not resolve and the lock pins it once the job's prepare stage wrote its
// source. When those installers fail and the setup still succeeds, the project
// is kept, and create exits non-zero naming the command that finishes the
// install, as `init --project` does. When the setup fails, the project is kept
// and create exits non-zero naming the create command to run again with
// --force.
func ProjectsCreate(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, env LifecycleEnv) error {
	created, err := createProjectFiles(ctx, wsRoot, cfg, args, env)
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
func createInitProjectFiles(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, env LifecycleEnv) error {
	_, err := createProjectFiles(ctx, wsRoot, cfg, args, env)
	return err
}

// createdProject is a project createProjectFiles rendered and listed in the
// workspace config.
type createdProject struct {
	name string
	// installErr is the failure of the lock step or the workspace installers
	// the setup ran to install a toolchain it needed, after which the setup
	// succeeded.
	installErr error
}

// createProjectFiles renders a template into a new project directory, lists
// the project and its extension in the workspace config, and runs the
// project-setup job of the project's extensions.
func createProjectFiles(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, env LifecycleEnv) (createdProject, error) {
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
		if installErr := extensions.TemplatesInstall(ctx, wsRoot, cfg, []string{shared.ChannelArtifact(flags.template, env.channel)}, ""); installErr == nil {
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
	// The jobs run from here on (project-setup, the workspace-install it may
	// need, and init's next install) must plan over the new project, or they
	// match nothing.
	workspace.InvalidateLoadCache(wsRoot)

	installErr, err := setUpProject(ctx, wsRoot, projectPath, env)
	if err != nil {
		if installErr != nil {
			err = errors.Join(err, fmt.Errorf("install workspace dependencies: %w", installErr))
		}
		return createdProject{}, protocolcli.WithNext(fmt.Errorf("set up project %s: %w", name, err),
			projectsCreateRetryCommand(name, flags, projectPath))
	}
	created := createdProject{name: name, installErr: installErr}

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
