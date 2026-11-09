// Package dotnet is @putnami/dotnet, the .NET stack extension: it runs the C#,
// F# and Visual Basic projects of a .NET repository as Putnami projects, with
// the stack core of go.putnami.dev/tooling/stacks and the .NET detector of
// putnami observe.
package dotnet

import (
	"context"
	_ "embed"
	"fmt"
	"path"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/dotnet"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/dotnet"

//go:embed profile.json
var profileJSON []byte

// Stack returns the .NET stack: the embedded profile, the .NET detector and
// the SDK roll-forward rule.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	// A test project measures the projects it references, which run no test
	// of their own: their coverage is the test project's.
	return stacks.Stack{
		Extension:              ExtensionName,
		Profile:                profile,
		Detect:                 Detect,
		MatchVersion:           MatchSDK,
		CrossComponentCoverage: true,
		Provide: func(ctx context.Context, _ stacks.Toolchain, expected []stacks.Expected, info func(string)) (stacks.Provided, error) {
			return Provide(ctx, expected[0], info)
		},
	}, nil
}

// Detect names the projects among the candidate paths with the .NET
// detector. MSBuild finds global.json, Directory.Build.props and NuGet.config
// up to the repository root, and two projects build their references into
// one obj/: every project has the workspace root for stack root. A project
// that is no test project runs no test task.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := dotnet.Detect(root)
	var out []stacks.Component
	for _, c := range detection.Components {
		if !slices.Contains(request.Paths, c.ID) {
			continue
		}
		tasks := []string{stacks.TaskLint, stacks.TaskBuild}
		if c.Test {
			tasks = append(tasks, stacks.TaskTest)
		}
		out = append(out, stacks.Component{
			Path:           c.ID,
			Name:           c.Name,
			SourceFile:     c.File,
			Root:           wsproto.ProbeRootPath,
			Dependencies:   slices.Clone(c.DependsOn),
			Type:           c.Type,
			ReleaseMembers: imageMember(root, wsproto.ProbeProject{Path: c.ID, SourceName: c.Name, Type: c.Type}),
			Tasks:          tasks,
			Watched:        buildProps(c.ID),
		})
	}
	return out, detection.Diagnostics
}

// buildProps are the Directory.Build.props paths from the project directory
// dir up to the workspace root, existing or not: the nearest one decides
// whether the project is a test project. The probe reports them among its
// watched files, and each task's key covers them.
func buildProps(dir string) []string {
	var out []string
	for d := path.Clean(dir); ; d = path.Dir(d) {
		out = append(out, path.Join(d, "Directory.Build.props"))
		if d == "." {
			return out
		}
	}
}
