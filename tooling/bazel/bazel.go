// Package bazel is @putnami/bazel, a delegated stack extension: it runs the
// packages of a Bazel repository as Putnami projects, each task running bazel
// on one package, with the stack core of go.putnami.dev/tooling/stacks and
// the Bazel detector of putnami observe. Bazel keeps its own cache and remote
// execution: the manifest disables Putnami's cache.
package bazel

import (
	_ "embed"
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/bazel"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/bazel"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Bazel stack: the embedded profile, the Bazel detector,
// and the runtime command that collects the reports of a test run.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{
		Extension: ExtensionName,
		Profile:   profile,
		Detect:    Detect,
		Commands:  map[string]stacks.Command{CollectReportsCommand: CollectReports},
	}, nil
}

// Detect names the Bazel packages among the candidate paths with the Bazel
// detector: each directory that holds a BUILD or BUILD.bazel file and a rule
// of the export bazel-graph.json, or without the export each directory that
// holds a BUILD file, named by its directory, with the workspace root for
// stack root and the export's edges. Every other candidate that holds a BUILD
// file, the root package included, is bare: it is named by its directory, the
// root by the workspace, and runs no task.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	if !detect.IsWorkspace(root) {
		return nil, nil
	}
	detection := detect.Detect(root, "")
	packages := make(map[string]detect.Package, len(detection.Packages))
	for _, p := range detection.Packages {
		packages[p.Dir] = p
	}
	var out []stacks.Component
	for _, dir := range request.Paths {
		p, ok := packages[dir]
		if !ok || dir == wsproto.ProbeRootPath {
			file := detect.BuildFile(root, dir)
			if file == "" {
				continue
			}
			name := dir
			if dir == wsproto.ProbeRootPath {
				name = ""
			}
			out = append(out, stacks.Component{Path: dir, Name: name, SourceFile: file, Bare: true})
			continue
		}
		var deps []string
		for _, d := range p.DependsOn {
			if d != wsproto.ProbeRootPath {
				deps = append(deps, d)
			}
		}
		out = append(out, stacks.Component{
			Path:         p.Dir,
			Name:         p.Dir,
			SourceFile:   detect.BuildFile(root, p.Dir),
			Root:         wsproto.ProbeRootPath,
			Dependencies: deps,
		})
	}
	// Without an export the edges come from each package's putnami.json,
	// which the sync task writes: a missing export is no finding here.
	var diags []diag.Diagnostic
	for _, d := range detection.Diagnostics {
		if d.Code != detect.CodeGraphMissing {
			diags = append(diags, d)
		}
	}
	return out, diags
}
