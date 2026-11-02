package golang

import (
	"encoding/json"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProbeExtension is the extension the probe answer names: the detector runs
// inside @putnami/observe until it moves into its own extension.
const ProbeExtension = "@putnami/observe"

// probeMetadata is the provider-owned metadata of one probe project.
type probeMetadata struct {
	Ecosystem string `json:"ecosystem"`
	Source    string `json:"source"`
}

// Probe returns the detection as a workspace probe answer: one project per
// component, its edges as component paths attributed to go-module.
func (d Detection) Probe() wsproto.ProbeResult {
	result := wsproto.ProbeResult{
		Version:     wsproto.ProbeProtocolVersion,
		Extension:   ProbeExtension,
		Diagnostics: d.Diagnostics,
	}
	if d.Workspace != "" {
		result.WatchedFiles = []string{d.Workspace}
	}
	for _, m := range d.Modules {
		metadata, _ := json.Marshal(probeMetadata{Ecosystem: Ecosystem, Source: m.Source})
		project := wsproto.ProbeProject{
			Path:         m.Dir,
			SourceName:   m.Path,
			SourceFile:   m.GoMod,
			Dependencies: append([]string(nil), m.DependsOn...),
			WatchedFiles: []string{m.GoMod},
			Metadata:     metadata,
		}
		if m.GoSum != "" {
			project.WatchedFiles = append(project.WatchedFiles, m.GoSum)
		}
		if len(m.DependsOn) > 0 {
			project.DependencySources = make(map[string]wsproto.DependencySource, len(m.DependsOn))
			for _, dir := range m.DependsOn {
				project.DependencySources[dir] = wsproto.DependencySourceGoModule
			}
		}
		result.Projects = append(result.Projects, project)
	}
	return result
}
