// Package dart is @putnami/dart: it runs the packages of a Dart or Flutter
// repository as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Dart detector of putnami observe.
package dart

import (
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/dart"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/dart"

// Profile variants. Every component names one (stacks ADR 0005).
const (
	// VariantDart is the variant of a pure Dart package outside a Flutter
	// workspace: the profile itself.
	VariantDart = "dart"
	// VariantFlutter is the variant of a package built with the Flutter SDK.
	VariantFlutter = "flutter"
	// VariantDartInFlutter is the variant of a pure Dart package whose stack
	// root holds a Flutter package: only flutter resolves that workspace, so
	// the package resolves with flutter and runs dart.
	VariantDartInFlutter = "dart-in-flutter"
)

//go:embed profile.json
var profileJSON []byte

// Stack returns the Dart stack: the embedded profile and the Dart detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect, Provide: Provide, Tools: Tools}, nil
}

// Detect names the packages among the candidate paths with the Dart
// detector. A package that a pub workspace lists has that workspace's root for
// stack root, where pub resolves it and keeps its one pubspec.lock; any other
// package, a melos package included, is its own. The root of a pub workspace
// below the workspace root is an input-only project that carries its files. A
// package whose pubspec names the Flutter SDK runs the flutter variant, and a
// pure Dart package of a pub workspace that holds one the dart-in-flutter
// variant.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	asked := map[string]bool{}
	for _, p := range request.Paths {
		if dir, ok := wsproto.NormalizeProbePath(p); ok {
			asked[dir] = true
		}
	}
	detection := dart.Detect(root)
	rootOf := func(c dart.Component) string {
		if c.PubWorkspace != "" {
			return c.PubWorkspace
		}
		return c.ID
	}
	flutterRoots := map[string]bool{}
	for _, c := range detection.Components {
		if c.Flutter {
			flutterRoots[rootOf(c)] = true
		}
	}
	var out []stacks.Component
	for _, c := range detection.Components {
		if !asked[c.ID] {
			continue
		}
		stackRoot := rootOf(c)
		variant := VariantDart
		switch {
		case c.Flutter:
			variant = VariantFlutter
		case flutterRoots[stackRoot] && (c.PubWorkspace != "" || c.DeclaresWorkspace):
			variant = VariantDartInFlutter
		}
		component := stacks.Component{
			Path:         c.ID,
			Name:         c.Name,
			SourceFile:   path.Join(c.ID, dart.Pubspec),
			Root:         stackRoot,
			Dependencies: c.DependsOn,
			Type:         componentType(root, c.ID),
			Variant:      variant,
		}
		if c.DeclaresWorkspace && c.PubWorkspace == "" {
			component.Name, component.InputOnly, component.Type = "", true, ""
		}
		// A pure Dart application may publish an image of its executable.
		if component.Type == "application" && variant != VariantFlutter && len(executables(filepath.Join(root, filepath.FromSlash(c.ID)))) > 0 {
			component.ReleaseMembers = stacks.ImageMember(root, c.ID, c.Name)
		}
		out = append(out, component)
	}
	return out, detection.Diagnostics
}

// componentType classifies a package: an application when it holds an
// executable (bin/*.dart) or a Flutter entrypoint (lib/main.dart), a library
// otherwise. The planner gives a library no runnable command.
func componentType(root, dir string) string {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	if _, err := os.Stat(filepath.Join(abs, "lib", "main.dart")); err == nil {
		return "application"
	}
	if executables, _ := filepath.Glob(filepath.Join(abs, "bin", "*.dart")); len(executables) > 0 {
		return "application"
	}
	return "library"
}
