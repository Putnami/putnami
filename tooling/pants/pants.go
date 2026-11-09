// Package pants is @putnami/pants, the first delegated stack extension: it
// runs the packages of a Pants repository as Putnami projects, each task
// running pants on one package directory, with the stack core of
// go.putnami.dev/tooling/stacks and the Pants detector of putnami observe.
// Pants keeps its own cache: the manifest disables Putnami's.
package pants

import (
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/pants"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/pants"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Pants stack: the embedded profile and the Pants detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the components among the candidate paths with the Pants
// detector: each directory whose BUILD file declares a first-party target,
// named by its directory, with the workspace root for stack root and the
// edges of the export pants-graph.json when the root holds one. Every other
// candidate that holds a BUILD file, a directory whose BUILD file declares
// only third-party targets or BUILD file symbols and the repository root, is
// bare: it is named by its directory, the root by the workspace, and runs no
// task.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := detect.Detect(root, detect.Options{})
	components := make(map[string]detect.Component, len(detection.Components))
	for _, c := range detection.Components {
		components[c.ID] = c
	}
	var out []stacks.Component
	for _, p := range request.Paths {
		c, ok := components[p]
		if !ok || p == wsproto.ProbeRootPath {
			file := buildFile(root, p)
			if file == "" {
				continue
			}
			name := p
			if p == wsproto.ProbeRootPath {
				name = ""
			}
			out = append(out, stacks.Component{Path: p, Name: name, SourceFile: file, Bare: true})
			continue
		}
		out = append(out, stacks.Component{
			Path:         c.ID,
			Name:         c.ID,
			SourceFile:   buildFile(root, c.ID),
			Root:         wsproto.ProbeRootPath,
			Dependencies: withoutRoot(c.DependsOn),
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

func withoutRoot(dirs []string) []string {
	return slices.DeleteFunc(slices.Clone(dirs), func(d string) bool { return d == wsproto.ProbeRootPath })
}

// buildFile is the workspace-relative BUILD file of a component directory:
// BUILD, BUILD.pants, or the first other BUILD.* file.
func buildFile(root, dir string) string {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	for _, name := range []string{"BUILD", "BUILD.pants"} {
		if info, err := os.Stat(filepath.Join(abs, name)); err == nil && info.Mode().IsRegular() {
			return path.Join(dir, name)
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "BUILD.") && e.Type().IsRegular() {
			return path.Join(dir, e.Name())
		}
	}
	return ""
}
