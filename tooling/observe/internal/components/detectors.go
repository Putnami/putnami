package components

import (
	"sync"

	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/bazel"
	"go.putnami.dev/tooling/observe/detect/cargo"
	"go.putnami.dev/tooling/observe/detect/composer"
	"go.putnami.dev/tooling/observe/detect/dart"
	"go.putnami.dev/tooling/observe/detect/dotnet"
	"go.putnami.dev/tooling/observe/detect/gradle"
	"go.putnami.dev/tooling/observe/detect/maven"
	"go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/observe/detect/nx"
	"go.putnami.dev/tooling/observe/detect/pants"
	"go.putnami.dev/tooling/observe/detect/python"
	"go.putnami.dev/tooling/observe/detect/ruby"
	"go.putnami.dev/tooling/observe/detect/swift"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// Options tune the default detectors.
type Options struct {
	// MoonGraph is the moon graph export to read, absolute or relative to the
	// repository root; empty reads moon-graph.json when it exists.
	MoonGraph string
	// PantsGraph is the Pants export to read, absolute or relative to the
	// repository root; empty reads pants-graph.json at the root.
	PantsGraph string
	// BazelGraph is the Bazel query export to read, absolute or relative to
	// the repository root; empty reads bazel-graph.json at the root.
	BazelGraph string
}

// Default returns the detectors observe runs, Nx, moon, Pants and Rush first.
// Bazel comes last: its export outranks the others by source, and without one
// its packages yield.
func Default(opts Options) []Detector {
	scan := &swiftScan{}
	return []Detector{NxDetector{}, MoonDetector{GraphFile: opts.MoonGraph}, PantsDetector{GraphFile: opts.PantsGraph}, RushDetector{}, NodeDetector{}, ComposerDetector{}, GoDetector{}, PythonDetector{}, GradleDetector{}, MavenDetector{}, SwiftPMDetector{scan: scan}, XcodeDetector{scan: scan}, DartDetector{}, DotnetDetector{}, RubyDetector{}, CargoDetector{}, BazelDetector{GraphFile: opts.BazelGraph}}
}

// fromProbe turns the projects of a probe answer into components.
func fromProbe(projects []wsproto.ProbeProject, describe func(wsproto.ProbeProject) (Component, bool)) []Component {
	out := make([]Component, 0, len(projects))
	for _, p := range projects {
		c, ok := describe(p)
		if !ok {
			continue
		}
		c.ID = p.Path
		if c.Name == "" {
			c.Name = p.SourceName
		}
		c.DependsOn = append([]string(nil), p.Dependencies...)
		out = append(out, c)
	}
	return out
}

// NxDetector adapts the Nx detector: a graph export is an orchestrator
// source, the Nx configuration a workspace one.
type NxDetector struct {
	// GraphFile is the graph export to read; empty reads graph.json.
	GraphFile string
}

// Name implements Detector.
func (NxDetector) Name() string { return "nx" }

// Detect implements Detector.
func (d NxDetector) Detect(root string) Found {
	result := nx.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: nx.Extension}, nx.Options{GraphFile: d.GraphFile})
	return Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(p wsproto.ProbeProject) (Component, bool) {
			meta, ok := nx.ReadMetadata(p)
			if !ok {
				return Component{}, false
			}
			return Component{Name: meta.Name, Ecosystem: nx.Ecosystem, Source: meta.Source}, true
		}),
	}
}

// MoonDetector adapts the moon detector: a graph export is an orchestrator
// source, the moon configuration a workspace one.
type MoonDetector struct {
	// GraphFile is the graph export to read; empty reads moon-graph.json.
	GraphFile string
}

// Name implements Detector.
func (MoonDetector) Name() string { return "moon" }

// Detect implements Detector.
func (d MoonDetector) Detect(root string) Found {
	result := moon.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: moon.Extension}, moon.Options{GraphFile: d.GraphFile})
	return Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(p wsproto.ProbeProject) (Component, bool) {
			meta, ok := moon.ReadMetadata(p)
			if !ok {
				return Component{}, false
			}
			return Component{Name: meta.Name, Ecosystem: moon.Ecosystem, Source: meta.Source}, true
		}),
	}
}

// BazelDetector adapts the Bazel detector: each Bazel package is an
// orchestrator component, with the edges of the query export, or, without an
// export, a manifest one without edges that yields to any other detector's
// component at or above its directory. It also places
// each test log of bazel-testlogs on the package of its test target.
type BazelDetector struct {
	// GraphFile is the query export to read; empty reads bazel-graph.json.
	GraphFile string
}

// Name implements Detector.
func (BazelDetector) Name() string { return "bazel" }

// Detect implements Detector.
func (d BazelDetector) Detect(root string) Found {
	detection := bazel.Detect(root, d.GraphFile)
	found := Found{Diagnostics: detection.Diagnostics}
	if len(detection.Packages) == 0 {
		return found
	}
	source := evidence.SourceManifest
	if detection.FromExport {
		source = evidence.SourceOrchestrator
	}
	for _, p := range detection.Packages {
		found.Components = append(found.Components, Component{
			ID: p.Dir, Name: p.Label, Ecosystem: bazel.Ecosystem, Source: source, DependsOn: p.DependsOn,
		})
	}
	found.ReportOwner = detection.TestTarget
	found.Yield = !detection.FromExport
	return found
}

// ComposerDetector adapts the Composer detector: each composer.json is a
// manifest component.
type ComposerDetector struct{}

// Name implements Detector.
func (ComposerDetector) Name() string { return "composer" }

// Detect implements Detector.
func (ComposerDetector) Detect(root string) Found {
	result := composer.Detect(root)
	return Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(wsproto.ProbeProject) (Component, bool) {
			return Component{Ecosystem: composer.Ecosystem, Source: evidence.SourceManifest}, true
		}),
	}
}

// PythonDetector adapts the Python detector: each uv workspace member is a
// workspace component, each other pyproject.toml project a manifest one.
type PythonDetector struct{}

// Name implements Detector.
func (PythonDetector) Name() string { return "python" }

// Detect implements Detector.
func (PythonDetector) Detect(root string) Found {
	result := python.Detect(root)
	return Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(p wsproto.ProbeProject) (Component, bool) {
			source := evidence.SourceManifest
			if meta, err := python.ReadMetadata(p); err == nil && meta.Source == python.SourceWorkspace {
				source = evidence.SourceWorkspace
			}
			return Component{Ecosystem: python.Ecosystem, Source: source}, true
		}),
	}
}

// PantsDetector adapts the Pants detector: each directory of a peek export is
// an orchestrator component, each directory of a BUILD file a workspace one.
// It also reads the lockfiles of the Python resolves.
type PantsDetector struct {
	// GraphFile is the export to read; empty reads pants-graph.json.
	GraphFile string
}

// Name implements Detector.
func (PantsDetector) Name() string { return "pants" }

// Detect implements Detector.
func (d PantsDetector) Detect(root string) Found {
	detection := pants.Detect(root, pants.Options{GraphFile: d.GraphFile})
	found := Found{Diagnostics: detection.Diagnostics}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Ecosystem: pants.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	if len(detection.Lockfiles) > 0 {
		found.Lockfile = func(file string) (string, LockReader, bool) {
			if !detection.IsLockfile(file) {
				return "", nil, false
			}
			return lockdiff.EcosystemPyPI, func(data []byte) (map[string][]string, error) {
				return lockdiff.ReadPEXLock(data)
			}, true
		}
	}
	return found
}

// GradleDetector adapts the Gradle detector: each project the settings
// include is a workspace component.
type GradleDetector struct{}

// Name implements Detector.
func (GradleDetector) Name() string { return "gradle" }

// Detect implements Detector.
func (GradleDetector) Detect(root string) Found {
	detection := gradle.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: gradle.Ecosystem, Source: evidence.SourceWorkspace, DependsOn: c.DependsOn})
	}
	return found
}

// MavenDetector adapts the Maven detector: each module that packages code is
// a workspace component.
type MavenDetector struct{}

// Name implements Detector.
func (MavenDetector) Name() string { return "maven" }

// Detect implements Detector.
func (MavenDetector) Detect(root string) Found {
	detection := maven.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: maven.Ecosystem, Source: evidence.SourceWorkspace, DependsOn: c.DependsOn})
	}
	return found
}

// swiftScan runs the Swift detection of one repository once, for both the
// SwiftPM and the Xcode detector.
type swiftScan struct {
	mu        sync.Mutex
	root      string
	detection swift.Detection
	done      bool
}

// detect returns the detection of root, from the scan when it already read
// that root. A nil scan detects every time.
func (s *swiftScan) detect(root string) swift.Detection {
	if s == nil {
		return swift.Detect(root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done || s.root != root {
		s.root, s.detection, s.done = root, swift.Detect(root), true
	}
	return s.detection
}

// SwiftPMDetector adapts the Swift detector to its Swift packages: each
// package and each of its targets is a workspace component. It also places
// the JUnit cases of the packages' test targets by their class names.
type SwiftPMDetector struct {
	scan *swiftScan
}

// Name implements Detector.
func (SwiftPMDetector) Name() string { return "swiftpm" }

// Detect implements Detector.
func (d SwiftPMDetector) Detect(root string) Found {
	detection := d.scan.detect(root)
	found := Found{}
	for _, d := range detection.Diagnostics {
		if d.Code != swift.CodeProjectUnreadable {
			found.Diagnostics = append(found.Diagnostics, d)
		}
	}
	for _, c := range detection.PackageComponents() {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: c.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	if len(detection.Packages) > 0 {
		found.Locate = detection.Locate
	}
	return found
}

// XcodeDetector adapts the Swift detector to its Xcode projects: each
// directory that holds an .xcodeproj is a manifest component, with an edge to
// each local package the project references.
type XcodeDetector struct {
	scan *swiftScan
}

// Name implements Detector.
func (XcodeDetector) Name() string { return "xcode" }

// Detect implements Detector.
func (d XcodeDetector) Detect(root string) Found {
	detection := d.scan.detect(root)
	found := Found{}
	for _, d := range detection.Diagnostics {
		if d.Code == swift.CodeProjectUnreadable {
			found.Diagnostics = append(found.Diagnostics, d)
		}
	}
	for _, c := range detection.ProjectComponents() {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: c.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	if len(detection.Projects) > 0 && len(detection.Packages) == 0 {
		found.Locate = detection.Locate
	}
	return found
}

// DartDetector adapts the Dart detector: each pub workspace member and melos
// package is a workspace component, each other pubspec.yaml package a
// manifest one.
type DartDetector struct{}

// Name implements Detector.
func (DartDetector) Name() string { return "dart" }

// Detect implements Detector.
func (DartDetector) Detect(root string) Found {
	detection := dart.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: dart.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	return found
}

// DotnetDetector adapts the .NET detector: each project a root solution lists
// is a workspace component, each other project file a manifest one. It also
// maps a test assembly to its project, and reads Directory.Packages.props in a
// repository without NuGet lock files.
type DotnetDetector struct{}

// Name implements Detector.
func (DotnetDetector) Name() string { return "dotnet" }

// Detect implements Detector.
func (DotnetDetector) Detect(root string) Found {
	detection := dotnet.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	if len(detection.Components) == 0 {
		return found
	}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: dotnet.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	found.Locate = detection.Locate
	found.Lockfile = func(file string) (string, LockReader, bool) {
		if !detection.IsCentralPackages(file) {
			return "", nil, false
		}
		return lockdiff.EcosystemNuGet, func(data []byte) (map[string][]string, error) {
			return lockdiff.ReadCentralPackages(data)
		}, true
	}
	return found
}

// RubyDetector adapts the Ruby detector: each packwerk pack is a workspace
// component, each other directory with a gemspec or a Gemfile a manifest one.
type RubyDetector struct{}

// Name implements Detector.
func (RubyDetector) Name() string { return "ruby" }

// Detect implements Detector.
func (RubyDetector) Detect(root string) Found {
	detection := ruby.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	for _, c := range detection.Components {
		found.Components = append(found.Components, Component{ID: c.ID, Name: c.Name, Ecosystem: ruby.Ecosystem, Source: c.Source, DependsOn: c.DependsOn})
	}
	return found
}

// CargoDetector adapts the Cargo detector: each Cargo workspace member is a
// workspace component, each other Cargo.toml package a manifest one. It also
// maps the test binary ids of cargo-nextest reports to their crates.
type CargoDetector struct{}

// Name implements Detector.
func (CargoDetector) Name() string { return "cargo" }

// Detect implements Detector.
func (CargoDetector) Detect(root string) Found {
	result := cargo.Detect(root)
	found := Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(p wsproto.ProbeProject) (Component, bool) {
			source := evidence.SourceManifest
			if meta, err := cargo.ReadMetadata(p); err == nil && meta.Source == cargo.SourceWorkspace {
				source = evidence.SourceWorkspace
			}
			return Component{Ecosystem: cargo.Ecosystem, Source: source}, true
		}),
	}
	if len(result.Projects) > 0 {
		found.LocateTestBinary = cargo.Locator(result)
	}
	return found
}
