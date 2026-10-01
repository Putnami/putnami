package cli

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"

	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// userScopeWorkspaceName names the synthetic workspace a user-scope job runs
// in, and its single project.
const userScopeWorkspaceName = "user"

// runUserScopeSubcommand runs `putnami <group> <sub>` when no workspace
// contains the current directory. The extensions are the user scope's, and
// only a subcommand that is interactive and declares `workspace: optional`
// runs; every other one fails with the message a job command gives outside a
// workspace.
//
// The job runs in a synthetic workspace rooted at the user scope
// (~/.putnami/user): its scratch lease, context file, output directory and
// caches live there or in the machine-global caches. The process itself runs
// in the caller's directory, and its context carries that directory as
// userScope.callerDir (mirrored in PUTNAMI_CALLER_DIR), which is how it knows
// there is no workspace. Nothing is written to the caller's directory.
func (a *App) runUserScopeSubcommand(
	ctx context.Context,
	parsed *ParsedArgs,
	cfg *wsproto.Config,
	extensions []*extension.ExtensionDescription,
	group, sub string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	resolved, _ := extension.ResolveSubcommand(extensions, group, sub)
	if resolved == nil || !resolved.Subdef.RunsWithoutWorkspace() {
		return noWorkspaceFound(stderr)
	}
	if owners := extension.CommandSubcommandOwners(extensions, group, sub); len(owners) > 1 {
		iox.Fprintf(stderr, "putnami: command %q is claimed by %s\n", group+" "+sub, strings.Join(owners, " and "))
		iox.Fprintf(stderr, "  Remove one of them from the user scope with `putnami extensions remove --user <name>`.\n")
		return ExitUsage
	}
	if flags := userScopeSelectionFlags(parsed.Global); len(flags) > 0 {
		iox.Fprintf(stderr, "putnami: %s selects workspace projects, and `putnami %s %s` runs outside any workspace\n",
			strings.Join(flags, ", "), group, sub)
		return ExitUsage
	}
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitError
	}
	callerDir, err := os.Getwd()
	if err != nil {
		iox.Fprintf(stderr, "putnami: resolve the current directory: %v\n", err)
		return ExitError
	}

	engine.ApplyEnvOverrides(&parsed.Global, cfg)
	effectiveFlags := resolved.EffectiveFlags()
	printParseWarnings(stderr, parsed.Global,
		undeclaredFlagWarnings(group+" "+sub, parsed.RawJobArgs, effectiveFlags))
	commandParams := buildExtensionCommandParams(parsed, effectiveFlags)
	if parsed.Global.MaxParallel > 0 {
		commandParams["max-parallel"] = strconv.Itoa(parsed.Global.MaxParallel)
	} else if parsed.Global.MaxParallelMode != "" {
		commandParams["max-parallel"] = parsed.Global.MaxParallelMode
	}
	if parsed.Global.NoCache {
		commandParams["no-cache"] = true
	}
	for _, name := range negatedGlobalFlagNames(parsed.OriginalArgs, effectiveFlags) {
		commandParams[name] = false
	}

	ws := workspace.NewWorkspace(userRoot, &wsproto.Config{Name: userScopeWorkspaceName}, nil)
	if resolved.Extension.Runtime != nil && resolved.Extension.RuntimeExecutable == "" {
		if err := jobs.SynchronizeExtensionRuntimes(
			ctx, ws, []*extension.ExtensionDescription{resolved.Extension}, nil); err != nil {
			iox.Fprintf(stderr, "putnami: prepare the %s runtime: %v\n", resolved.Extension.Name, err)
			return ExitError
		}
	}
	jobDef := *resolved.JobDefinition
	jobDef.Flags = effectiveFlags
	jobDef.Args = append(append([]string{}, jobDef.Args...), parsed.RawJobArgs...)
	job := &jobs.ScheduledJob{
		Project:   workspaceOnceProject(ws),
		Extension: resolved.Extension,
		JobDef:    &jobDef,
		UserScope: &protocoljob.UserScope{CallerDir: callerDir},
	}
	// No version snapshot: the user scope is not a repository, and the caller's
	// directory is none of the CLI's business beyond being where the job runs.
	return runInteractiveExtensionCommand(ctx, ws, job, commandParams, nil, stdin, stdout, stderr)
}

// userScopeSelectionFlags names the project-selection flags present in global,
// in a fixed order. Outside a workspace there are no projects to select, so a
// user-scope subcommand refuses them instead of dropping them.
func userScopeSelectionFlags(global GlobalFlags) []string {
	var flags []string
	switch {
	case global.Impacted:
		flags = append(flags, "--impacted")
	case global.All:
		flags = append(flags, "--all")
	case global.Projects != "":
		flags = append(flags, "--projects")
	}
	for _, flag := range []struct {
		name string
		set  bool
	}{
		{"--impacted-strict", global.ImpactedStrict},
		{"--baseline", global.Baseline != ""},
		{"--tag", global.FilterTag != ""},
		{"--exclude-tag", global.ExcludeTag != ""},
		{"--exclude", global.Exclude != ""},
		{"--no-cache-projects", global.NoCacheProjects != ""},
	} {
		if flag.set {
			flags = append(flags, flag.name)
		}
	}
	return flags
}
