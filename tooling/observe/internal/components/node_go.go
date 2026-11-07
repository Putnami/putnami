package components

import (
	"path"

	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/nodeworkspace"
	"go.putnami.dev/tooling/observe/detect/rush"
	"go.putnami.dev/tooling/observe/internal/detectors/golang"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// NodeDetector adapts the Node workspaces detector: each npm, pnpm or yarn
// workspace member is a workspace component.
type NodeDetector struct{}

// Name implements Detector.
func (NodeDetector) Name() string { return "node-workspaces" }

// Detect implements Detector.
func (NodeDetector) Detect(root string) Found {
	result, ok := nodeworkspace.Detect(root)
	if !ok {
		return Found{Diagnostics: result.Diagnostics}
	}
	return Found{
		Diagnostics: result.Diagnostics,
		Components: fromProbe(result.Projects, func(wsproto.ProbeProject) (Component, bool) {
			return Component{Ecosystem: nodeworkspace.Ecosystem, Source: evidence.SourceWorkspace}, true
		}),
	}
}

// GoDetector adapts the Go detector: each go.mod is a component, a workspace
// one when go.work uses it. It also maps the import paths of Go reports and
// reads the versions a go.mod locks.
type GoDetector struct{}

// Name implements Detector.
func (GoDetector) Name() string { return "go" }

// Detect implements Detector.
func (GoDetector) Detect(root string) Found {
	detection := golang.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	if len(detection.Modules) == 0 {
		return found
	}
	for _, c := range detection.Components() {
		found.Components = append(found.Components, Component{
			ID: c.ID, Name: c.Name, Ecosystem: c.Ecosystem, Source: c.Source, DependsOn: c.DependsOn,
		})
	}
	found.Locate = detection.Locate
	found.Lockfile = func(file string) (string, LockReader, bool) {
		if path.Base(file) != golang.GoMod {
			return "", nil, false
		}
		return golang.Ecosystem, func(data []byte) (map[string][]string, error) {
			locked, err := detection.ReadLock(file, data, nil)
			if err != nil {
				return nil, err
			}
			out := make(map[string][]string, len(locked))
			for _, l := range locked {
				out[l.Name] = []string{l.Version}
			}
			return out, nil
		}, true
	}
	return found
}

// RushDetector adapts the Rush detector: each Rush project is an
// orchestrator component. It also reads the lockfiles Rush keeps under
// common/config.
type RushDetector struct{}

// Name implements Detector.
func (RushDetector) Name() string { return "rush" }

// Detect implements Detector.
func (RushDetector) Detect(root string) Found {
	detection := rush.Detect(root)
	found := Found{Diagnostics: detection.Diagnostics}
	if len(detection.Projects) == 0 {
		return found
	}
	for _, p := range detection.Projects {
		found.Components = append(found.Components, Component{
			ID: p.Folder, Name: p.Name, Ecosystem: rush.Ecosystem, Source: evidence.SourceOrchestrator, DependsOn: p.DependsOn,
		})
	}
	found.Lockfile = func(file string) (string, LockReader, bool) {
		read, ok := detection.LockReader(file)
		if !ok {
			return "", nil, false
		}
		return lockdiff.EcosystemNPM, func(data []byte) (map[string][]string, error) {
			return read(data)
		}, true
	}
	return found
}
