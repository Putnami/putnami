// Package rush is @putnami/rush, the extension that adopts a Rush repository
// in the mode its workspace chose (D-064). In delegate mode it is a delegated
// stack on go.putnami.dev/tooling/stacks: each Rush project is a Putnami
// project whose tasks run rush on that project at the workspace root, and
// Rush keeps its build cache. In import mode its probe claims no project: the
// sync writes each Rush project as a TypeScript project, which Putnami's
// runner, installer and cache serve. Both read rush.json and the projects'
// package.json files with the Rush detector of putnami observe.
package rush

import (
	_ "embed"
	"fmt"
	"path"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/rush"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/rush"

// TypeScriptExtension is the extension an import writes into each project.
const TypeScriptExtension = "@putnami/typescript"

// The adoption modes, as putnami init records them under
// options.@putnami/rush.mode of putnami.workspace.json.
const (
	ModeImport   = "import"
	ModeDelegate = "delegate"
)

//go:embed profile.json
var profileJSON []byte

// Stack returns the Rush stack: the embedded profile and the Rush detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{
		Extension: ExtensionName,
		Profile:   profile,
		Detect:    Detect,
	}, nil
}

// Mode answers the adoption mode of the workspace at root: the mode
// options.@putnami/rush.mode of putnami.workspace.json names, and import
// without one, as putnami init --from imports without a terminal (D-064).
func Mode(root string) string {
	cfg := wsproto.Load(root)
	if cfg != nil {
		if mode, _ := cfg.Options[ExtensionName]["mode"].(string); mode == ModeDelegate {
			return ModeDelegate
		}
	}
	return ModeImport
}

// Detect names the Rush projects among the candidate paths. In delegate mode
// each project of rush.json is a component named by its package name, which
// rush receives, with the workspace root for stack root and an edge to each
// Rush project its package.json depends on; any other candidate that holds a
// package.json, such as an autoinstaller, is bare and runs no task. In import
// mode it names none, so the TypeScript extension runs the projects.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := detect.Detect(root)
	if len(detection.Projects) == 0 && len(detection.Diagnostics) == 0 {
		return nil, nil
	}
	if Mode(root) == ModeImport {
		return nil, nil
	}
	projects := make(map[string]detect.Project, len(detection.Projects))
	for _, p := range detection.Projects {
		projects[p.Folder] = p
	}
	var out []stacks.Component
	for _, dir := range request.Paths {
		if dir == wsproto.ProbeRootPath {
			continue
		}
		p, ok := projects[dir]
		if !ok {
			out = append(out, stacks.Component{Path: dir, Name: dir, SourceFile: path.Join(dir, "package.json"), Bare: true})
			continue
		}
		out = append(out, stacks.Component{
			Path:         p.Folder,
			Name:         p.Name,
			Ref:          p.Name,
			SourceFile:   path.Join(p.Folder, "package.json"),
			Root:         wsproto.ProbeRootPath,
			Dependencies: p.DependsOn,
		})
	}
	return out, detection.Diagnostics
}
