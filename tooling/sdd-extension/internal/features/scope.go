package features

import (
	"sort"

	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// Scope is the resolved selected projection one evaluation runs under.
//
// Selected projects are SEEDS, not a filesystem wall. A scope answers two
// questions and nothing else:
//
//   - SeedRoot: is this discovery root one the caller selected? Seed roots are
//     where authored features and specs are OWNED, so they decide what a scoped
//     run is about.
//   - SelectedFeature: is this authored identity one of the seeds' features?
//     That is what lets discovery follow a record across a project boundary —
//     an evidence document, a spec, or a capability manifest in an unselected
//     project still counts when a selected feature's correctness depends on it.
//
// Two tiers fall out of that split, and both are load-bearing:
//
//   - The IDENTITY tier stays workspace-wide. Durable putnami.features.json
//     manifests and canonical specs/ documents are the only artifacts that mint
//     an identity, so reading them at every root is what keeps global duplicate
//     detection exact under any selection. Each is one bounded JSON document per
//     root; neither walks a source tree or a generated directory.
//   - The EVALUATION tier is scoped. Evidence fragments, capability manifests,
//     and generated design graphs are read for seed roots plus the exact roots a
//     selected feature reaches, never for the rest of the workspace.
//
// A nil *Scope is the historical whole-workspace meaning: every method below is
// nil-safe and answers "yes", so an unscoped run is byte-identical to the
// pre-selection behavior.
type Scope struct {
	roots    map[string]bool
	features map[string]bool
}

// NewScope builds a scope from the workspace-relative roots of the selected
// projects.
//
// The workspace root ("") is always a seed. It is exactly one root — bounded,
// and it does not grow with the repository — it is the root of the selection
// itself rather than an unrelated project, and a workspace-level manifest is
// authority every project's features may relate to. Dropping it would make a
// scoped run silently blind to workspace-level intent.
func NewScope(roots []string) *Scope {
	scope := &Scope{
		roots:    map[string]bool{"": true},
		features: make(map[string]bool),
	}
	for _, root := range roots {
		scope.roots[root] = true
	}
	return scope
}

// NewProjectScope is NewScope over an already-resolved project selection.
func NewProjectScope(projects []*workspace.Project) *Scope {
	roots := make([]string, 0, len(projects))
	for _, project := range projects {
		if project != nil {
			roots = append(roots, project.Path)
		}
	}
	return NewScope(roots)
}

// SelectFeatures records the authored feature identities this selection owns.
// Callers that already resolved the identity tier (the spec surface reads
// manifests before specs) pass them in; Aggregate derives them itself from the
// manifests it discovered.
func (scope *Scope) SelectFeatures(ids []string) {
	if scope == nil {
		return
	}
	for _, id := range ids {
		if id != "" {
			scope.features[id] = true
		}
	}
}

// Active reports whether evaluation is narrowed at all.
func (scope *Scope) Active() bool { return scope != nil }

// SeedRoot reports whether one discovery root is selected. A nil scope selects
// every root.
func (scope *Scope) SeedRoot(root string) bool {
	if scope == nil {
		return true
	}
	return scope.roots[root]
}

// SelectedFeature reports whether one authored feature identity belongs to the
// selection. A nil scope selects every feature.
func (scope *Scope) SelectedFeature(id string) bool {
	if scope == nil {
		return true
	}
	return scope.features[id]
}

// Roots returns the seed roots, sorted, so any surface that reports the
// resolved scope reports the same bytes for the same selection.
func (scope *Scope) Roots() []string {
	if scope == nil {
		return nil
	}
	roots := make([]string, 0, len(scope.roots))
	for root := range scope.roots {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// SelectAuthored is the identity tier for surfaces that read durable manifests
// directly instead of through Aggregate — the feature catalog and the spec
// repository both do. It records the identities the seed roots declare on the
// scope and returns them sorted. An inactive scope owns every authored
// identity, so it returns them all.
func SelectAuthored(scope *Scope, manifests []featureproto.ManifestSource) []string {
	selected := make(map[string]bool)
	for _, source := range manifests {
		if source.Manifest == nil {
			continue
		}
		if !scope.SeedRoot(ManifestRoot(source.Path)) {
			continue
		}
		for _, feature := range featureproto.CanonicalManifest(source.Manifest).Features {
			if feature.ID != "" {
				selected[feature.ID] = true
			}
		}
	}
	ids := sortedKeys(selected)
	scope.SelectFeatures(ids)
	return ids
}

// SeedManifests returns the manifests a selection OWNS, plus the manifests
// outside the seed roots that declare one of the selected identities — the
// cross-boundary declarations that must stay complete.
func SeedManifests(scope *Scope, manifests []featureproto.ManifestSource) []featureproto.ManifestSource {
	if !scope.Active() {
		return manifests
	}
	kept := make([]featureproto.ManifestSource, 0, len(manifests))
	for _, source := range manifests {
		if scope.SeedRoot(ManifestRoot(source.Path)) {
			kept = append(kept, source)
			continue
		}
		if source.Manifest == nil {
			continue
		}
		for _, feature := range featureproto.CanonicalManifest(source.Manifest).Features {
			if scope.SelectedFeature(feature.ID) {
				kept = append(kept, source)
				break
			}
		}
	}
	return kept
}

// ManifestRoot returns the discovery root that owns one durable manifest path.
func ManifestRoot(manifestPath string) string {
	return featureManifestRoot(manifestPath)
}

// Features returns the selected authored identities, sorted.
func (scope *Scope) Features() []string {
	if scope == nil {
		return nil
	}
	features := make([]string, 0, len(scope.features))
	for id := range scope.features {
		features = append(features, id)
	}
	sort.Strings(features)
	return features
}
