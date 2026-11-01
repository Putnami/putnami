package components

import (
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/internal/detect/composer"
	"go.putnami.dev/tooling/observe/internal/detectors/nx"
)

// Default returns the detectors observe runs, orchestrators first.
func Default() []Detector {
	return []Detector{NxDetector{}, NodeDetector{}, ComposerDetector{}, GoDetector{}}
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
