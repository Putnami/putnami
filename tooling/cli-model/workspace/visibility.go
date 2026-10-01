package workspace

import (
	"path"
	"sort"

	wsproto "go.putnami.dev/protocol/workspace"
)

// EffectiveVisibility is a project's import boundary with the default applied.
// A project that declares none is wsproto.DefaultVisibility.
func EffectiveVisibility(p *Project) wsproto.Visibility {
	if p == nil {
		return wsproto.DefaultVisibility
	}
	return wsproto.ResolveVisibility(p.Visibility)
}

// ScopeKeyOf is the identity of the scope a project belongs to: the workspace
// directory of the NEAREST ancestor putnami.json that is a scope config, in
// slash form. A project under no scope config belongs to the workspace root
// scope, whose key is "".
//
// It is read from what discovery already resolved (Scope.ConfigPaths, the
// chain shallowest first), so the boundary is a function of the same files the
// scope's tags and extensions come from, and nothing re-walks the tree to
// answer it. A project's OWN putnami.json is not part of that chain, so an
// activated scope belongs to its parent scope rather than to itself — a scope
// is where a project sits, never the project itself.
func ScopeKeyOf(p *Project) string {
	if p == nil || len(p.Scope.ConfigPaths) == 0 {
		return ""
	}
	nearest := p.Scope.ConfigPaths[len(p.Scope.ConfigPaths)-1]
	dir := path.Dir(nearest)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// SameScope reports whether two projects sit in one scope.
func SameScope(a, b *Project) bool {
	return ScopeKeyOf(a) == ScopeKeyOf(b)
}

// VisibilityViolation is one import edge a project's declared boundary refuses.
type VisibilityViolation struct {
	// Importer is the ID of the project that performs the import.
	Importer string
	// ImporterScope is the importer's scope key.
	ImporterScope string
	// Imported is the ID of the project whose boundary is crossed.
	Imported string
	// ImportedScope is the imported project's scope key.
	ImportedScope string
	// Source names the manifest family the import was derived from.
	Source wsproto.DependencySource
}

// VisibilityViolations returns every import edge of the workspace's resolved
// graph that crosses a scope boundary the imported project did not open.
//
// The rule, in one place:
//
//   - Only REAL imports are judged (wsproto.DependencySource.IsImport): a
//     go.mod require or replace of a workspace module, a package.json
//     dependency on a workspace package. An edge a putnami.json merely
//     declares — and the implicit ordering edge an activated scope adds — is
//     not an import and is ignored, because a boundary enforced on edges the
//     build never reads is a boundary the build never respects.
//   - A contract edge is always allowed. A service's contract IS its public
//     surface; its implementation stays private, and the client reads the
//     contract rather than the provider's sources.
//   - An import of a generated client is allowed from every scope when the
//     client resolves to a provider and declares no visibility. The client is
//     that contract rendered for one language, the service's published way
//     in. A client that declares `scope` keeps it, and a client manifest no
//     provider backs opens nothing.
//   - An import inside one scope is always allowed.
//   - An import that leaves the imported project's scope is allowed only when
//     that project declares wsproto.VisibilityPublic.
//
// The answer is sorted by (importer, imported) so two runs over one workspace
// report the same list in the same order.
func VisibilityViolations(ws *Workspace) []VisibilityViolation {
	if ws == nil || ws.Graph == nil {
		return nil
	}
	var violations []VisibilityViolation
	for _, importer := range ws.Projects {
		for _, dependencyID := range ws.Graph.DependenciesOf(importer.ID) {
			imported := ws.ProjectByID(dependencyID)
			if imported == nil {
				continue
			}
			source := ws.Graph.EdgeSource(importer.ID, dependencyID)
			if !source.IsImport() {
				continue
			}
			if EffectiveVisibility(imported) == wsproto.VisibilityPublic || publishedClient(ws, imported) {
				continue
			}
			if SameScope(importer, imported) {
				continue
			}
			violations = append(violations, VisibilityViolation{
				Importer:      importer.ID,
				ImporterScope: ScopeKeyOf(importer),
				Imported:      imported.ID,
				ImportedScope: ScopeKeyOf(imported),
				Source:        source,
			})
		}
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Importer != violations[j].Importer {
			return violations[i].Importer < violations[j].Importer
		}
		return violations[i].Imported < violations[j].Imported
	})
	return violations
}

// publishedClient reports whether p is a generated client bound to a provider
// that declares no visibility of its own.
func publishedClient(ws *Workspace, p *Project) bool {
	return p.GeneratedClient != nil && p.Visibility == "" && ws.Graph.ContractProviderOf(p.ID) != ""
}

// ScopeLabel renders a scope key for a human: the workspace root scope has no
// directory to name.
func ScopeLabel(scopeKey string) string {
	if scopeKey == "" {
		return "<workspace root>"
	}
	return scopeKey
}
