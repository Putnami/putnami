// Package turbo is @putnami/turbo: it adopts a Turborepo repository. Each
// member of the npm, pnpm or yarn workspace becomes a Putnami project with
// its edges. In delegate mode, each task runs turbo run on its package with
// the stack core of go.putnami.dev/tooling/stacks, and Turborepo keeps its
// cache: the manifest disables Putnami's. In import mode, a TypeScript
// package's task moves to the TypeScript extension when its script already
// runs what that extension's task runs; every other task stays on Turborepo.
package turbo

import (
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/nodeworkspace"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/turbo"

// Marker is the file at the root of a Turborepo repository.
const Marker = "turbo.json"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Turborepo stack: the embedded profile and Detect.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the components among the candidate paths: at a root that
// holds turbo.json, each member of the Node workspace, named by its package
// name, with the workspace root for stack root and the members it depends on
// as edges. Each component watches the root files of the Node workspace,
// its lockfiles, pnpm-workspace.yaml and .npmrc, which Turborepo hashes into
// every package, so a change to one selects every package. A member whose
// putnami.json names extensions without this one is not answered, and
// neither is the root or any other candidate.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	if !isRepository(root) {
		return nil, nil
	}
	result, ok := nodeworkspace.Detect(root)
	if !ok {
		return nil, result.Diagnostics
	}
	members := make(map[string]wsproto.ProbeProject, len(result.Projects))
	for _, p := range result.Projects {
		members[p.Path] = p
	}
	var out []stacks.Component
	for _, candidate := range request.Paths {
		p, ok := members[candidate]
		if !ok || p.Path == wsproto.ProbeRootPath {
			continue
		}
		if extensions := declaredExtensions(root, p.Path); len(extensions) > 0 && !slices.Contains(extensions, ExtensionName) {
			continue
		}
		out = append(out, stacks.Component{
			Path:         p.Path,
			Name:         p.SourceName,
			SourceFile:   p.SourceFile,
			Root:         wsproto.ProbeRootPath,
			Dependencies: slices.DeleteFunc(slices.Clone(p.Dependencies), func(d string) bool { return d == wsproto.ProbeRootPath }),
			Watched:      append([]string{Marker, path.Join(p.Path, Marker), path.Join(p.Path, wsproto.ConfigFilename)}, result.WatchedFiles...),
		})
	}
	return out, result.Diagnostics
}

// isRepository reports whether root holds turbo.json.
func isRepository(root string) bool {
	info, err := os.Stat(filepath.Join(root, Marker))
	return err == nil && info.Mode().IsRegular()
}

// declaredExtensions reads the extensions the putnami.json of dir names.
func declaredExtensions(root, dir string) []string {
	doc, _, err := readProject(filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename))
	if err != nil {
		return nil
	}
	return stringList(doc["extensions"])
}
