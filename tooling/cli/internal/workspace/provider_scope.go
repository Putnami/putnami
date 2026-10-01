package workspace

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
)

// The core-owned half of the workspace adapter.
//
// An extension declares WHAT marks a project it owns and WHICH files decide its
// answer; this file is everything core does with that declaration, and nothing
// here interprets a marker's contents. Four rules are core's alone and cannot
// be overridden by any adapter:
//
//   - `.git` and `.putnami` are never scanned. They are repository and
//     orchestrator state, not source. Declaring them in `excludes` is reported
//     by the protocol as redundant precisely because core already refuses.
//   - Gitignored directories are never scanned. Generated output routinely
//     contains a `package.json` or a `go.mod`; discovering those as projects is
//     exactly the failure where a build artifact becomes a build input.
//   - Core assigns canonical paths. A scan returns DIRECTORIES; which of them
//     become projects, and under what ID, stays core's decision.
//   - A scope-only `putnami.json` never marks a project, even for an adapter
//     that lists `putnami.json` among its markers. The adapter's other markers
//     still match in that directory.

// ProviderScope is one extension's declared discovery surface, normalized for
// core's use.
type ProviderScope struct {
	// Extension is the declaring extension's name.
	Extension string
	// Markers are candidate-directory-relative patterns whose presence makes a
	// directory a project this extension owns.
	Markers []string
	// Inputs are candidate-directory-relative patterns whose content decides
	// this extension's probe answer. Every marker appears here (the protocol
	// rejects a manifest where it does not).
	Inputs []string
	// Excludes are directory names this extension's scan must not descend into,
	// additive to the core-owned exclusions.
	Excludes []string
	// SyncTask is the extension's own workspace-mutation task, when declared.
	SyncTask string
	// DependencySources reports that the adapter declares edge attribution, so
	// core may ask for it (wsproto.ProbeRequest.DependencySources).
	DependencySources bool
}

// NewProviderScope normalizes one declared adapter into a scope. It sorts and
// deduplicates every pattern set so two runs over one manifest produce the same
// scan, the same recorded inputs, and therefore the same snapshot.
func NewProviderScope(extension string, adapter *extproto.WorkspaceAdapter) (ProviderScope, bool) {
	if adapter == nil || strings.TrimSpace(extension) == "" {
		return ProviderScope{}, false
	}
	scope := ProviderScope{
		Extension: strings.TrimSpace(extension),
		Markers:   normalizePatternSet(adapter.Markers),
		Inputs:    normalizePatternSet(adapter.Inputs),
		Excludes:  normalizePatternSet(adapter.Excludes),
		SyncTask:  strings.TrimSpace(adapter.SyncTask),

		DependencySources: adapter.DependencySources,
	}
	if len(scope.Markers) == 0 || len(scope.Inputs) == 0 {
		// An adapter that marks nothing is never asked anything, and an adapter
		// with no inputs has no validity oracle. The protocol rejects both; a
		// manifest that reached here anyway contributes nothing rather than
		// contributing an unbounded scan.
		return ProviderScope{}, false
	}
	return scope, true
}

func normalizePatternSet(patterns []string) []string {
	seen := make(map[string]bool, len(patterns))
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		clean, err := extproto.NormalizeInputPattern(pattern)
		if err != nil || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	sort.Strings(out)
	return out
}

// excludesDir reports whether this scope refuses to scan a directory whose
// workspace-relative path is rel.
//
// An exclude means "do not DESCEND into", so it applies to the named directory
// AND to everything beneath it: `node_modules` must exclude
// `web/node_modules/dep`, not only `web/node_modules`. Matching is therefore
// per SEGMENT — a directory nested anywhere under the tree is excluded exactly
// as its top-level sibling is — plus the whole relative path, so an adapter can
// exclude one specific location.
func (s ProviderScope) excludesDir(rel string) bool {
	if len(s.Excludes) == 0 {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, exclude := range s.Excludes {
		if exclude == rel {
			return true
		}
		if matched, err := path.Match(exclude, rel); err == nil && matched {
			return true
		}
		for _, segment := range segments {
			if exclude == segment {
				return true
			}
			if matched, err := path.Match(exclude, segment); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// MatchesInput reports whether a workspace-relative file path is one of this
// scope's declared metadata inputs, relative to some candidate directory.
//
// This is the ATTRIBUTION rule: it decides which provider owns a changed file,
// and therefore which single provider a metadata edit re-probes. Patterns are
// candidate-directory-relative, so a pattern of N segments is compared against
// the LAST N segments of the changed path — which is what lets one declaration
// of "package.json" claim every project's package.json without the adapter
// enumerating directories it cannot know.
//
// Markers need no separate predicate: the protocol requires every marker to
// appear in `inputs` (a marker nobody hashes would leave the snapshot valid and
// the answer stale), so creating or removing a marker is already an input
// change this claims.
func (s ProviderScope) MatchesInput(rel string) bool {
	rel = filepath.ToSlash(strings.TrimPrefix(path.Clean(rel), "./"))
	if rel == "" || rel == "." {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, pattern := range s.Inputs {
		if strings.Contains(pattern, "**") {
			// The candidate root is not available during attribution, so try
			// each suffix. One of them is the path relative to the candidate
			// whose recursive input produced this changed snapshot entry.
			for start := range segments {
				if matchWorkspaceGlob(strings.Join(segments[start:], "/"), pattern) {
					return true
				}
			}
			continue
		}
		depth := strings.Count(pattern, "/") + 1
		if depth > len(segments) {
			continue
		}
		tail := strings.Join(segments[len(segments)-depth:], "/")
		if matched, err := path.Match(pattern, tail); err == nil && matched {
			return true
		}
	}
	return false
}

// markerPresent reports whether a directory carries one of the marker patterns.
// Directories never count as markers: a marker is a manifest core hashes, and a
// directory has no content digest. A file directly in dir named ignored never
// counts; an empty ignored ignores nothing.
func markerPresent(dir string, markers []string, ignored string) bool {
	for _, marker := range markers {
		matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(marker)))
		if err != nil {
			continue
		}
		for _, match := range matches {
			if ignored != "" && match == filepath.Join(dir, ignored) {
				continue
			}
			if info, statErr := os.Stat(match); statErr == nil && !info.IsDir() {
				return true
			}
		}
	}
	return false
}

// providerInputs resolves one scope's declared input patterns over a set of
// candidate directories and digests each resolved input.
//
// The result is BOUNDED by construction — one entry per (directory, pattern) —
// which is the property that lets an ordinary load decide snapshot validity
// with a fixed number of reads instead of a tree walk. A literal pattern
// records the file (present or absent, so its later creation invalidates); a
// glob pattern records the whole matched set (see globInput).
func providerInputs(root string, dirs []string, patterns []string) []SnapshotInput {
	inputs := make([]SnapshotInput, 0, len(dirs)*len(patterns))
	seen := make(map[string]bool, len(dirs)*len(patterns))
	for _, dir := range dirs {
		base := cleanWorkspacePath(dir)
		for _, pattern := range patterns {
			rel := pattern
			if base != "" {
				rel = base + "/" + pattern
			}
			if seen[rel] {
				continue
			}
			seen[rel] = true
			if hasGlobMeta(pattern) {
				inputs = append(inputs, globInput(root, rel))
				continue
			}
			inputs = append(inputs, digestInput(root, rel))
		}
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	return inputs
}
