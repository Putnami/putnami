// Package php is @putnami/php, the PHP stack extension: it runs the components
// of a Composer repository, such as a Symfony application and the packages
// beside it, as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Composer detector of putnami observe.
package php

import (
	_ "embed"
	"fmt"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/composer"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/php"

//go:embed profile.json
var profileJSON []byte

// Stack returns the PHP stack: the embedded profile and the Composer detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the components among the candidate paths with the Composer
// detector. The root manifest, and a package a path repository of the root
// composer.json provides, have the workspace root for stack root: the root's
// composer install puts them, and the tools that run them, in its vendor/.
// Any other component is its own stack root.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	result := composer.Probe(root, request)
	diags := result.Diagnostics
	members, err := composer.PathRepositoryDirs(root, wsproto.ProbeRootPath)
	if err != nil {
		diags = append(diags, diag.Warningf(composer.CodeManifestInvalid, composer.Marker, "%s: %v: its path repositories are not read, so each package is its own stack root", composer.Marker, err))
	}
	out := make([]stacks.Component, 0, len(result.Projects))
	for _, p := range result.Projects {
		stackRoot := p.Path
		if slices.Contains(members, p.Path) {
			stackRoot = wsproto.ProbeRootPath
		}
		out = append(out, stacks.Component{
			Path:           p.Path,
			Name:           p.SourceName,
			SourceFile:     p.SourceFile,
			Root:           stackRoot,
			Dependencies:   p.Dependencies,
			Type:           p.Type,
			ReleaseMembers: imageMember(root, p, stackRoot),
		})
	}
	return out, diags
}
