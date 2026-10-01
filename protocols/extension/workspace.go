// Workspace probe and synchronization: the second of the three lifecycle
// primitives contract v3 adds.
//
// Before it, the CLI knew what a project WAS: a table of marker filenames
// (package.json, go.mod, pyproject.toml), a table of directories to skip, a
// parser per language, and a dependency-derivation pass that read go.mod
// itself. Every one of those is language knowledge inside a core that is
// supposed to be language-neutral, and every new ecosystem is another entry in
// four tables.
//
// `workspace` moves the knowledge to the extension that owns it. The extension
// declares which paths MARK a project it owns, which files are its metadata
// INPUTS, what to EXCLUDE from the scan, and which task synchronizes native
// manifests. Core scans, batches every candidate into one probe request per
// extension, and merges the normalized results.
//
// Two invariants shape the declaration:
//
//   - PATHS, NOT MODULES. Every member here is a PATH pattern relative to a
//     candidate directory. A probe contract must never assume that a project
//     root is also a module root — that a Go project's directory contains the
//     go.mod that declares it, that a package's directory is its npm workspace
//     entry, that a Python project owns its pyproject.toml. Those coincide
//     today in this repository and would stop coinciding the moment one module
//     hosts several projects. Keying on paths keeps that a layout choice
//     instead of a protocol break.
//
//   - THE MARKERS ARE INPUTS. A marker's CONTENT is what the probe reads to
//     answer "what project is this?", so a marker that is not also a metadata
//     input would be hashed by nobody: editing it would leave the snapshot
//     valid and the answer stale. Markers must therefore appear in `inputs`,
//     which is checked here rather than left to a comment.
//
// Core keeps what only core can own: it always excludes AlwaysExcludedDirs and
// gitignored directories, it alone assigns canonical project paths and IDs, and
// explicit putnami.json values always outrank probe-derived ones. The merge
// table lives in doc/07-lifecycles.md.
//
// Everything here is declaration-only. The probe protocol, the snapshot store
// and the merge implementation land in slices C3a/C3b; this file fixes the
// shape they implement and the failure vocabulary they report through (see
// lifecycle.go).

package extension

import (
	"fmt"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// AlwaysExcludedDirs are the directories core excludes from every workspace
// scan regardless of what any extension declares, in canonical (sorted) order.
//
// They are core-owned because they are workspace mechanics, not language
// knowledge: `.git` is the repository's own state and `.putnami` is the
// orchestrator's scratch, caches and staging trees. An extension that listed
// them in `excludes` would be restating a rule it cannot change, which is why
// doing so is reported as redundant rather than honored as meaningful.
// Gitignored directories are excluded too, but they are discovered rather than
// named, so they are not part of this list.
var AlwaysExcludedDirs = []string{".git", ".putnami"}

// WorkspaceAdapter is the manifest's `workspace` section: how an extension
// participates in project discovery and workspace synchronization.
type WorkspaceAdapter struct {
	// Markers are the candidate-directory-relative path patterns whose presence
	// makes a directory a project this extension owns ("package.json",
	// "go.mod", "*.csproj"). At least one is required: an adapter that marks
	// nothing can never be asked anything.
	Markers []string `json:"markers"`
	// Inputs are the candidate-directory-relative path patterns whose CONTENT
	// determines the probe's answer — the marker itself plus lockfiles and
	// compiler configuration. They are the bounded set core hashes on ordinary
	// loads to decide whether the stored snapshot is still valid, so the digest
	// (never a size or an mtime) is the validity oracle. Every marker must
	// appear here.
	Inputs []string `json:"inputs"`
	// Excludes are directory patterns this extension's scan must not descend
	// into ("node_modules", "dist"). They are additive to AlwaysExcludedDirs and
	// to gitignored directories, which core excludes unconditionally.
	Excludes []string `json:"excludes,omitempty"`
	// SyncTask names a task in this manifest's `tasks` map that performs the
	// extension's own workspace mutations — package-workspace membership,
	// module discovery and replace closure, native project-name alignment.
	// Absent means the extension reads the workspace but never writes it.
	SyncTask string `json:"syncTask,omitempty"`
	// DependencySources declares that this extension's probe attributes every
	// edge it reports to the manifest family it came from — an import, or a
	// declaration nothing imports. Core asks for the attribution
	// (workspace ProbeRequest.DependencySources) only from an adapter that
	// declares it, because a provider built before the request member would
	// reject a request carrying it.
	DependencySources bool `json:"dependencySources,omitempty"`
}

// DeclaresWorkspaceAdapter reports whether the manifest contributes to project
// discovery.
func (m *Manifest) DeclaresWorkspaceAdapter() bool {
	return m != nil && m.Workspace != nil
}

// IsAlwaysExcludedDir reports whether a directory name is excluded by core
// regardless of any adapter declaration.
func IsAlwaysExcludedDir(name string) bool {
	for _, candidate := range AlwaysExcludedDirs {
		if candidate == name {
			return true
		}
	}
	return false
}

// ValidateWorkspaceAdapter checks the manifest's `workspace` section. It
// returns nothing for a manifest without one, so wiring it into the strict path
// cannot change any existing manifest's verdict.
//
// Diagnostics come out in a fixed order — markers, inputs, excludes, then the
// cross-member rules — so two runs over one manifest are byte-identical.
func ValidateWorkspaceAdapter(m *Manifest) []diag.Diagnostic {
	if m == nil || m.Workspace == nil {
		return nil
	}
	adapter := m.Workspace
	var diags []diag.Diagnostic

	if len(adapter.Markers) == 0 {
		diags = append(diags, diag.Errorf("required-field", "workspace.markers",
			"at least one marker is required; an adapter that marks no directory is never asked anything"))
	}
	_, markerDiags := validatePatternSet("workspace.markers", "marker", adapter.Markers)
	diags = append(diags, markerDiags...)

	if len(adapter.Inputs) == 0 {
		diags = append(diags, diag.Errorf("required-field", "workspace.inputs",
			"at least one metadata input is required; inputs are what core hashes to decide whether the "+
				"stored workspace snapshot is still valid"))
	}
	inputs, inputDiags := validatePatternSet("workspace.inputs", "metadata input", adapter.Inputs)
	diags = append(diags, inputDiags...)

	_, excludeDiags := validatePatternSet("workspace.excludes", "exclude", adapter.Excludes)
	diags = append(diags, excludeDiags...)
	for i, exclude := range adapter.Excludes {
		if IsAlwaysExcludedDir(strings.TrimSpace(exclude)) {
			diags = append(diags, diag.Warningf("redundant-exclude",
				fmt.Sprintf("workspace.excludes[%d]", i),
				"%q is excluded by core for every extension; declaring it here has no effect", exclude))
		}
	}

	// A marker is read by the probe, so its content must invalidate the
	// snapshot. Reported per marker, in declaration order.
	inputSet := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		inputSet[input] = true
	}
	for i, marker := range adapter.Markers {
		normalized, err := NormalizeInputPattern(marker)
		if err != nil || inputSet[normalized] {
			continue
		}
		diags = append(diags, diag.Errorf("marker-not-input",
			fmt.Sprintf("workspace.markers[%d]", i),
			"marker %q is not declared in workspace.inputs; the probe reads the marker, so editing it must "+
				"invalidate the workspace snapshot — add it to inputs", marker))
	}

	if adapter.SyncTask != "" {
		if _, ok := m.Tasks[adapter.SyncTask]; !ok {
			diags = append(diags, diag.Errorf("unresolved-sync-task", "workspace.syncTask",
				"workspace syncTask %q is not defined in this manifest's tasks", adapter.SyncTask))
		}
	}

	return diags
}

// validatePatternSet checks one pattern list and returns its normalized
// members. Invalid patterns are reported and omitted from the result, so a
// cross-member rule never compares against a value the author never wrote.
func validatePatternSet(field, noun string, patterns []string) ([]string, []diag.Diagnostic) {
	var diags []diag.Diagnostic
	normalized := make([]string, 0, len(patterns))
	seen := make(map[string]bool, len(patterns))
	for i, pattern := range patterns {
		entry := fmt.Sprintf("%s[%d]", field, i)
		clean, err := NormalizeInputPattern(pattern)
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-workspace-path", entry,
				"invalid %s %q: %v", noun, pattern, err))
			continue
		}
		if seen[clean] {
			diags = append(diags, diag.Errorf("duplicate-value", entry,
				"%s %q is declared more than once", noun, pattern))
			continue
		}
		seen[clean] = true
		normalized = append(normalized, clean)
	}
	return normalized, diags
}

// NormalizeWorkspaceAdapter canonicalizes the adapter in place: every pattern
// set is cleaned and sorted. Pattern sets are SETS — their order carries no
// meaning — so sorting them is what makes the normalized probe declaration, and
// therefore the workspace snapshot digest that includes it, independent of
// authoring order.
func NormalizeWorkspaceAdapter(m *Manifest) {
	if m == nil || m.Workspace == nil {
		return
	}
	m.Workspace.Markers = normalizePatternList(m.Workspace.Markers)
	m.Workspace.Inputs = normalizePatternList(m.Workspace.Inputs)
	m.Workspace.Excludes = normalizePatternList(m.Workspace.Excludes)
}
