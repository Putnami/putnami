// Package rush is @putnami/rush, the extension that adopts a Rush repository
// in the mode its workspace chose (D-064). In delegate mode it is a delegated
// stack on go.putnami.dev/tooling/stacks: each Rush project is a Putnami
// project whose tasks run rush on that project at the workspace root, and
// Rush keeps its build cache. In import mode the sync writes each Rush
// project as a TypeScript project, which Putnami's runner, installer and
// cache serve, and keeps on Rush each script whose tool the TypeScript
// extension does not run (D-070): the probe claims only the projects that
// keep one, whose tasks run rushx in the project's directory. Both read
// rush.json and the projects' package.json files with the Rush detector of
// putnami observe.
package rush

import (
	_ "embed"
	"fmt"
	"path"
	"path/filepath"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/rush"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/rush"

// TypeScriptExtension is the extension an import writes into each project.
const TypeScriptExtension = stacks.TypeScriptExtension

// The adoption modes, as putnami init records them under
// options.@putnami/rush.mode of putnami.workspace.json.
const (
	ModeImport   = stacks.AdoptionImport
	ModeDelegate = stacks.AdoptionDelegate
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
// without one, as putnami init --from imports without a terminal (D-064). A
// value that names no mode is an error.
func Mode(root string) (string, error) {
	return stacks.AdoptionMode(root, ExtensionName)
}

// Detect names the Rush projects among the candidate paths, and only them:
// a candidate rush.json does not list, such as a test fixture that holds a
// package.json, is no project. In delegate mode each project of rush.json is
// a component named by its package name, which rush receives, with the
// workspace root for stack root and an edge to each Rush project its
// package.json depends on. In import mode a project whose putnami.json names
// the extension keeps a target on Rush (ADR 0017 of tooling/stacks) and is a
// component; every other one is bare, a member whose targets the TypeScript
// extension runs, with the edges the sync writes into its putnami.json
// attributed to Rush's graph.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := detect.Detect(root)
	if len(detection.Projects) == 0 && len(detection.Diagnostics) == 0 {
		return nil, nil
	}
	mode, err := Mode(root)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(stacks.CodeAdoptionModeInvalid, "", "%v", err)}
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
			continue
		}
		if mode == ModeImport && !keepsTargets(root, dir) {
			out = append(out, stacks.Component{Path: dir, Bare: true, Dependencies: p.DependsOn, EdgeSource: wsproto.DependencySourceBuildGraph})
			continue
		}
		out = append(out, stacks.Component{
			Path:         p.Folder,
			Name:         p.Name,
			Ref:          p.Name,
			SourceFile:   path.Join(p.Folder, "package.json"),
			Root:         wsproto.ProbeRootPath,
			Dependencies: p.DependsOn,
			EdgeSource:   wsproto.DependencySourceBuildGraph,
		})
	}
	return out, detection.Diagnostics
}

// keepsTargets reports whether the putnami.json of the project at dir names
// the extension: an import keeps one of its targets on Rush.
func keepsTargets(root, dir string) bool {
	cfg := wsproto.LoadProjectConfig(filepath.Join(root, filepath.FromSlash(dir)))
	return cfg != nil && slices.Contains(cfg.Extensions, ExtensionName)
}
