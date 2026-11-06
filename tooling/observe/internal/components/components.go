// Package components combines the detectors of @putnami/observe into one set
// of components, and tells which components a change touched and which
// depend on them.
//
// Each detector keeps its own package, API and metadata. A Detector adapts one
// of them to the one shape the change analyses read: components keyed by
// their repository-relative directory, with their edges as directories. When
// two detectors name one directory, the higher source wins: orchestrator,
// then workspace, then manifest; a tie goes to the detector listed first.
package components

import (
	"path"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// FallbackEcosystem is the ecosystem of the fallback component.
const FallbackEcosystem = "unknown"

// Component is one component a detector found.
type Component struct {
	// ID is the component's directory relative to the repository root, or ".".
	ID string
	// Name is the component's own name.
	Name string
	// Ecosystem is the evidence ecosystem slug.
	Ecosystem string
	// Source is an evidence component source.
	Source string
	// DependsOn lists the ids of the components it depends on.
	DependsOn []string
	// Detector names the detector that found it.
	Detector string
}

// Found is one detector's answer.
type Found struct {
	Components  []Component
	Diagnostics []diag.Diagnostic
	// Locate maps a name a report writes, a Go import path, an import path
	// followed by a file name or a cargo-nextest test binary id, to its
	// repository path and component. Nil for a detector that reads none.
	Locate func(importPath string) (file, component string, ok bool)
	// Lockfile returns the ecosystem and the reader of a lockfile only this
	// detector can read, such as a go.mod. Nil for a detector that reads none.
	Lockfile func(file string) (ecosystem string, read LockReader, ok bool)
}

// LockReader reads the versions a lockfile pins per package name.
type LockReader func(data []byte) (map[string][]string, error)

// Detector is what the change analyses need from one detector.
type Detector interface {
	// Name names the detector in diagnostics.
	Name() string
	// Detect reads the repository at root. It never fails: a finding is a
	// diagnostic.
	Detect(root string) Found
}

// Set is the combined components of one repository.
type Set struct {
	// Components are sorted by ID.
	Components  []Component
	Diagnostics []diag.Diagnostic
	locators    []func(string) (string, string, bool)
	lockfiles   []func(string) (string, LockReader, bool)
}

// rank orders the sources: a lower rank wins.
var rank = map[string]int{
	evidence.SourceDeclared:     0,
	evidence.SourceOrchestrator: 1,
	evidence.SourceWorkspace:    2,
	evidence.SourceManifest:     3,
	evidence.SourceFallback:     4,
}

// Detect runs the detectors on root and combines their components. Without
// any component, the repository is one fallback component.
func Detect(root string, detectors []Detector) Set {
	var set Set
	byID := map[string]Component{}
	for _, d := range detectors {
		found := d.Detect(root)
		set.Diagnostics = append(set.Diagnostics, found.Diagnostics...)
		if found.Locate != nil {
			set.locators = append(set.locators, found.Locate)
		}
		if found.Lockfile != nil {
			set.lockfiles = append(set.lockfiles, found.Lockfile)
		}
		for _, c := range found.Components {
			if c.ID == "" {
				c.ID = evidence.RootComponent
			}
			c.Detector = d.Name()
			if _, known := rank[c.Source]; !known || c.Source == evidence.SourceDeclared {
				c.Source = evidence.SourceManifest
			}
			current, taken := byID[c.ID]
			if taken && rank[current.Source] <= rank[c.Source] {
				continue
			}
			byID[c.ID] = c
		}
	}
	if len(byID) == 0 {
		byID[evidence.RootComponent] = Component{
			ID: evidence.RootComponent, Ecosystem: FallbackEcosystem, Source: evidence.SourceFallback, Detector: "fallback",
		}
	}
	for id, c := range byID {
		var edges []string
		for _, target := range c.DependsOn {
			if _, known := byID[target]; known && target != id && !slices.Contains(edges, target) {
				edges = append(edges, target)
			}
		}
		sort.Strings(edges)
		c.DependsOn = edges
		byID[id] = c
	}
	for _, c := range byID {
		set.Components = append(set.Components, c)
	}
	sort.Slice(set.Components, func(i, j int) bool { return set.Components[i].ID < set.Components[j].ID })
	return set
}

// Owner returns the innermost component whose directory holds the
// repository-relative file.
func (s Set) Owner(file string) (string, bool) {
	file = path.Clean(file)
	best, found := "", false
	for _, c := range s.Components {
		if Contains(c.ID, file) && (!found || depth(c.ID) > depth(best)) {
			best, found = c.ID, true
		}
	}
	return best, found
}

// Locate maps a Go import path or a cargo-nextest test binary id to its
// repository path and component, through the detectors that read them.
func (s Set) Locate(importPath string) (file, component string, ok bool) {
	for _, locate := range s.locators {
		if file, component, ok := locate(importPath); ok {
			if _, known := s.find(component); known {
				return file, component, true
			}
		}
	}
	return "", "", false
}

// Lockfile returns the ecosystem and reader a detector offers for a
// lockfile, such as a go.mod.
func (s Set) Lockfile(file string) (string, LockReader, bool) {
	for _, lockfile := range s.lockfiles {
		if ecosystem, read, ok := lockfile(file); ok {
			return ecosystem, read, true
		}
	}
	return "", nil, false
}

func (s Set) find(id string) (Component, bool) {
	i := sort.Search(len(s.Components), func(i int) bool { return s.Components[i].ID >= id })
	if i < len(s.Components) && s.Components[i].ID == id {
		return s.Components[i], true
	}
	return Component{}, false
}

// Impacts returns the impact of every component, given the components that
// hold a changed file: changed, dependent when it reaches a changed one
// through its edges, unaffected otherwise.
func (s Set) Impacts(changed map[string]bool) map[string]string {
	dependents := map[string][]string{}
	for _, c := range s.Components {
		for _, target := range c.DependsOn {
			dependents[target] = append(dependents[target], c.ID)
		}
	}
	reaches := map[string]bool{}
	var queue []string
	for id := range changed {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[id] {
			if !reaches[dependent] {
				reaches[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	out := make(map[string]string, len(s.Components))
	for _, c := range s.Components {
		switch {
		case changed[c.ID]:
			out[c.ID] = evidence.ImpactChanged
		case reaches[c.ID]:
			out[c.ID] = evidence.ImpactDependent
		default:
			out[c.ID] = evidence.ImpactUnaffected
		}
	}
	return out
}

// Contains reports whether the repository-relative path lies inside the
// component directory id. The directory itself, such as a submodule's
// gitlink, is not inside it: this is the rule protocol/evidence checks.
func Contains(id, file string) bool {
	return id == evidence.RootComponent || strings.HasPrefix(file, id+"/")
}

func depth(id string) int {
	if id == evidence.RootComponent {
		return 0
	}
	return strings.Count(id, "/") + 1
}
