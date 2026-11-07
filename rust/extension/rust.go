// Package rust is @putnami/rust, the reference stack extension: it runs the
// crates of a Cargo workspace as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Cargo detector of putnami observe.
package rust

import (
	_ "embed"
	"fmt"

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
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the crates among the candidate paths with the Cargo detector.
// A member of the root's Cargo workspace has the workspace root for stack
// root; any other crate is its own.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	result := cargo.Probe(root, request)
	diags := result.Diagnostics
	var out []stacks.Component
	for _, p := range result.Projects {
		meta, err := cargo.ReadMetadata(p)
		if err != nil {
			diags = append(diags, diag.Warningf(cargo.CodeManifestInvalid, p.Path, "%s: %v", p.Path, err))
			continue
		}
		stackRoot := p.Path
		if meta.Source == cargo.SourceWorkspace {
			stackRoot = wsproto.ProbeRootPath
		}
		out = append(out, stacks.Component{
			Path:         p.Path,
			Name:         p.SourceName,
			SourceFile:   p.SourceFile,
			Root:         stackRoot,
			Dependencies: p.Dependencies,
		})
	}
	return out, diags
}
