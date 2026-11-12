// Package nx is @putnami/nx, the stack extension that adopts a Nx
// repository: each Nx project is a Putnami project with its edges. In
// delegate mode each build, test and lint task runs nx run <project>:<target>
// at the workspace root and Nx keeps its cache; in import mode a TypeScript
// project's targets run as @putnami/typescript tasks and every other target
// keeps running through Nx. The stack core is go.putnami.dev/tooling/stacks
// and the Nx detector is the one of putnami observe.
package nx

import (
	_ "embed"
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/nx"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/nx"

// TypeScriptExtension is the extension import mode hands a TypeScript
// project's targets to.
const TypeScriptExtension = "@putnami/typescript"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Nx stack: the embedded profile and the Nx detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the Nx projects among the candidate paths with the Nx
// detector: each is named by its Nx project name, which nx run receives, with
// the workspace root for stack root and the projects it depends on. A project
// the TypeScript extension names carries no name of its own: the package name
// wins, and nx run still receives the Nx name. Any other candidate is no
// member of the extension.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := detect.Detect(root, detect.Options{})
	projects := make(map[string]detect.Project, len(detection.Projects))
	for _, p := range detection.Projects {
		projects[p.Path] = p
	}
	typeScript := stacks.DeclaresExtension(root, TypeScriptExtension)
	var out []stacks.Component
	for _, path := range request.Paths {
		p, ok := projects[path]
		if !ok {
			continue
		}
		// The TypeScript extension names a project with a named package.json;
		// nx run still receives the Nx name.
		name := p.Name
		if typeScript && stacks.NamedByTypeScript(root, p.Path) {
			name = ""
		}
		out = append(out, stacks.Component{
			Path:         p.Path,
			Name:         name,
			Ref:          p.Name,
			SourceFile:   p.SourceFile,
			Root:         wsproto.ProbeRootPath,
			Dependencies: p.Dependencies,
			// Nx's project graph states each edge, and the sync writes it.
			EdgeSource: wsproto.DependencySourceBuildGraph,
		})
	}
	return out, detection.Diagnostics
}
