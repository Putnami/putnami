package components

import (
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/internal/detect/bazel"
	"go.putnami.dev/tooling/observe/internal/detect/composer"
	"go.putnami.dev/tooling/observe/internal/detect/pants"
	"go.putnami.dev/tooling/observe/internal/detect/python"
	"go.putnami.dev/tooling/observe/internal/detectors/nx"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// Options tune the default detectors.
type Options struct {
	// PantsGraph is the Pants export to read, absolute or relative to the
	// repository root; empty reads pants-graph.json at the root.
	PantsGraph string
	// BazelGraph is the Bazel query export to read, absolute or relative to
	// the repository root; empty reads bazel-graph.json at the root.
	BazelGraph string
}

// Default returns the detectors observe runs, orchestrators first.
func Default(opts Options) []Detector {
	return []Detector{NxDetector{}, PantsDetector{GraphFile: opts.PantsGraph}, BazelDetector{GraphFile: opts.BazelGraph}, NodeDetector{}, ComposerDetector{}, GoDetector{}, PythonDetector{}}
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

// BazelDetector adapts the Bazel detector: each Bazel package is an
// orchestrator component, with the edges of the query export. It also places
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
	for _, p := range detection.Packages {
		found.Components = append(found.Components, Component{
			ID: p.Dir, Name: p.Label, Ecosystem: bazel.Ecosystem, Source: evidence.SourceOrchestrator, DependsOn: p.DependsOn,
		})
	}
	found.ReportOwner = detection.TestTarget
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
