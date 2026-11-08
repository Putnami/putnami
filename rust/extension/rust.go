// Package rust is @putnami/rust, the reference stack extension: it runs the
// crates of a Cargo workspace as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Cargo detector of putnami observe.
package rust

import (
	_ "embed"
	"fmt"
	"path"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/cargo"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/rust"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Rust stack: the embedded profile and the Cargo detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{
		Extension:     ExtensionName,
		Profile:       profile,
		Detect:        Detect,
		CaseComponent: CaseComponent,
		ComponentKey:  cargo.PackageKey,
	}, nil
}

// CaseComponent names the crate of a nextest JUnit case: the package of the
// binary id its class name states, else its suite name.
func CaseComponent(classname, suite string) (string, bool) {
	if pkg, ok := cargo.BinaryPackage(classname); ok {
		return pkg, true
	}
	return cargo.BinaryPackage(suite)
}

// Detect names the crates among the candidate paths with the Cargo detector,
// each with the directory of the Cargo workspace that holds it for stack
// root. A candidate that holds a virtual Cargo workspace below the workspace
// root is a stack root with no crate.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	result := cargo.Probe(root, request)
	diags := result.Diagnostics
	var out []stacks.Component
	answered := map[string]bool{}
	for _, p := range result.Projects {
		answered[p.Path] = true
		meta, err := cargo.ReadMetadata(p)
		if err != nil {
			diags = append(diags, diag.Warningf(cargo.CodeManifestInvalid, p.Path, "%s: %v", p.Path, err))
			continue
		}
		stackRoot := meta.Workspace
		if stackRoot == "" {
			stackRoot = p.Path
		}
		out = append(out, stacks.Component{
			Path:         p.Path,
			Name:         p.SourceName,
			SourceFile:   p.SourceFile,
			Root:         stackRoot,
			Dependencies: p.Dependencies,
		})
	}
	for _, candidate := range request.Paths {
		dir, ok := wsproto.NormalizeProbePath(candidate)
		if !ok || answered[dir] || !cargo.VirtualWorkspace(root, dir) {
			continue
		}
		answered[dir] = true
		out = append(out, stacks.Component{Path: dir, SourceFile: path.Join(dir, cargo.Marker), Root: dir, InputOnly: true})
	}
	return out, diags
}
