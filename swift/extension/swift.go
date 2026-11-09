// Package swift is @putnami/swift: it runs the Swift packages and the Xcode
// projects of a repository as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Swift detectors of putnami observe.
//
// A package runs the profile's swiftpm variant (swift build, swift test); an
// Xcode project runs its xcode variant (xcodebuild). The runtime commands of
// commands.go do what an argument vector cannot: export the LCOV of a
// package, pick an xcodebuild destination, and convert a result bundle into
// JUnit and Cobertura.
package swift

import (
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/swift"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/swift"

// Profile variants: a package runs swift, an Xcode project xcodebuild.
const (
	VariantSwiftPM = "swiftpm"
	VariantXcode   = "xcode"
)

//go:embed profile.json
var profileJSON []byte

// Stack returns the Swift stack: the embedded profile, the Swift detectors,
// the install of a package's Swift and the runtime commands.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect, Provide: Provide, Tools: Tools, Commands: Commands()}, nil
}

// Detect names the packages and the Xcode projects among the candidate
// paths with the observe Swift detectors. Each is its own stack root. A
// package comes before an Xcode project of the same directory, so the core
// keeps the package.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	candidates := map[string]bool{}
	for _, p := range request.Paths {
		if clean, ok := wsproto.NormalizeProbePath(p); ok {
			candidates[clean] = true
		}
	}
	detection := swift.Detect(root)
	var out []stacks.Component
	for _, p := range detection.Packages {
		if !candidates[p.Dir] {
			continue
		}
		name := p.Name
		if name == "" {
			name = filepath.Base(root)
		}
		// A package with an executable product is an application: serve and
		// run start it. Any other package is a library.
		kind := "library"
		manifest, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.Dir), swift.Manifest)) //nolint:gosec // a manifest of the workspace
		if len(ExecutableProducts(manifest)) > 0 {
			kind = "application"
		}
		out = append(out, stacks.Component{
			Path:         p.Dir,
			Name:         name,
			SourceFile:   path.Join(p.Dir, swift.Manifest),
			Root:         p.Dir,
			Dependencies: slices.Clone(p.LocalPackages),
			Type:         kind,
			Variant:      VariantSwiftPM,
		})
	}
	for _, p := range detection.Projects {
		if !candidates[p.Dir] {
			continue
		}
		out = append(out, stacks.Component{
			Path:         p.Dir,
			Name:         p.Name,
			SourceFile:   path.Join(p.Dir, p.Name+".xcodeproj", swift.PBXProject),
			Root:         p.Dir,
			Dependencies: slices.Clone(p.LocalPackages),
			Variant:      VariantXcode,
		})
	}
	// The static-read note is observe's advice; a probe reports only what
	// leaves a component out or reads it wrong.
	var diags []diag.Diagnostic
	for _, d := range detection.Diagnostics {
		if d.Severity != diag.Info {
			diags = append(diags, d)
		}
	}
	return out, diags
}
