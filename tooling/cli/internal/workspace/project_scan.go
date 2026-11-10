package workspace

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
)

// The full-tree project scan, after the core marker table was deleted.
//
// C3b widened this scan with the markers extensions declare while keeping core's
// own table ("package.json", "go.mod", "pyproject.toml") unioned into it, on the
// explicit condition that the table would go once all three language probes
// existed. They do, so it is gone: core knows exactly one marker, and it is its
// own — `putnami.json`.
//
// Four rules stay core's alone and no adapter can override them:
//
//   - `.git` and `.putnami` are never scanned. They are repository and
//     orchestrator state, not source.
//   - A directory git ignores entirely is never scanned. Generated output
//     routinely contains a manifest, and discovering one as a project is exactly
//     the failure where a build artifact becomes a build input.
//   - Core assigns canonical paths. The scan returns DIRECTORIES; which of them
//     become projects, and under what ID, stays core's decision.
//   - A scope-only `putnami.json` never marks a project, even for an adapter
//     that lists `putnami.json` among its markers. The adapter's other markers
//     still match in that directory.
//
// Everything else an adapter says about where NOT to look is honored, and it is
// honored as a WALK exclusion rather than only as a match filter: an adapter
// that excludes `node_modules` must stop the walk from descending into it, or
// the cost of not-matching a million files is paid on every `projects sync`.

// ScanProjectPathsWithProviders walks the workspace root and discovers the
// directories core itself recognizes as projects: those carrying a
// `putnami.json` that is more than a scope declaration. With no scopes, that is
// the whole answer — a language manifest alone marks nothing. A provider's
// markers make its directories discoverable; a provider's excludes stop the
// walk from descending into the directories it names.
func ScanProjectPathsWithProviders(root string, scopes []ProviderScope) ([]string, error) {
	// Core-owned exclusions. They are not configurable, and the protocol reports
	// an adapter that declares them as redundant precisely because core already
	// refuses to descend.
	coreExcluded := map[string]bool{
		".putnami": true,
		".git":     true,
	}

	// Directories git ignores entirely are build artifacts or generated output,
	// not source projects. Skip them so e.g. a generated `clients/ts` is never
	// discovered and registered. Empty when not a git repo — then nothing extra
	// is skipped and behavior matches a plain filesystem walk.
	gitIgnored := git.IgnoredDirs(root)

	var paths []string
	seen := make(map[string]bool)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !d.IsDir() {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if coreExcluded[filepath.Base(path)] || gitIgnored[rel] {
			return filepath.SkipDir
		}
		// THE EXCLUDE UNION, and it is deliberate.
		//
		// A directory ANY adapter refuses to descend into is not descended into
		// by ANYONE, including core's own putnami.json marker. The consequence is
		// real and worth stating plainly: once a Python adapter is installed, no
		// project is discoverable under a directory named `build`; once a Go
		// adapter is, none is discoverable under `testdata` or `vendor` — whatever
		// language that project is written in.
		//
		// An exclude silencing only its own declarer's markers cannot be had
		// without also descending into the install trees and fixture corpora those
		// excludes exist to keep out: `node_modules` is excluded by the TypeScript
		// adapter alone, so a workspace that also has the Go adapter would walk it
		// and an installed @putnami package's putnami.json would become a
		// discovered project. Union-excluding can only make the scan MISS a
		// project, never invent one, and a missed project is visible (`projects
		// sync` does not add it) rather than silent.
		//
		// The one destructive consequence is closed at the other end: `--prune`
		// refuses to remove a member the scan could not have seen, because the
		// scan's silence about an excluded directory is not evidence of absence
		// (see ExcludedByProviderScopes and `projects sync`).
		if ExcludedByProviderScopes(scopes, rel) {
			return filepath.SkipDir
		}

		// A scope-only putnami.json marks nothing, whoever declares it as a
		// marker. An adapter that lists `putnami.json` among its markers still
		// matches the directory's other manifests, never the scope file itself:
		// otherwise every scope directory becomes a project the moment such an
		// adapter is installed.
		var ignored string
		if fileExists(filepath.Join(path, wsproto.ConfigFilename)) {
			if !isScopeOnlyConfig(path) {
				seen[rel] = true
				paths = append(paths, rel)
				return nil
			}
			ignored = wsproto.ConfigFilename
		}
		for _, scope := range scopes {
			if seen[rel] {
				continue
			}
			if markerPresent(path, scope.Markers, ignored) {
				seen[rel] = true
				paths = append(paths, rel)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(paths)
	return paths, nil
}

// DetectMarkers answers, for each scope, the root-relative directory of the
// first directory that holds one of its markers: "." for root itself, "" when
// none does. It walks as the project scan does, in the same order, with root
// included: it never descends into `.git`, `.putnami`, a directory git ignores,
// or a directory any of the scopes excludes. It reads each directory once,
// matches names only and reads no file, and stops once every scope has a match.
func DetectMarkers(root string, scopes []ProviderScope) []string {
	found := make([]string, len(scopes))
	remaining := len(scopes)
	if remaining == 0 {
		return found
	}
	gitIgnored := git.IgnoredDirs(root)
	var walk func(dir, rel string) bool
	walk = func(dir, rel string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		for i, scope := range scopes {
			if found[i] == "" && markerAmong(dir, entries, scope.Markers) {
				found[i] = rel
				remaining--
			}
		}
		if remaining == 0 {
			return false
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			child := name
			if rel != "." {
				child = rel + "/" + name
			}
			if name == ".git" || name == ".putnami" || gitIgnored[child] || ExcludedByProviderScopes(scopes, child) {
				continue
			}
			if !walk(filepath.Join(dir, name), child) {
				return false
			}
		}
		return true
	}
	walk(root, ".")
	return found
}

// markerAmong reports whether the directory dir, whose entries are entries,
// holds one of markers, as markerPresent decides it. A one-segment marker is
// matched against the entries alone; a deeper one globs only below an entry
// that matches its first segment.
func markerAmong(dir string, entries []os.DirEntry, markers []string) bool {
	for _, marker := range markers {
		first, rest, nested := strings.Cut(marker, "/")
		for _, entry := range entries {
			if matched, err := path.Match(first, entry.Name()); err != nil || !matched {
				continue
			}
			candidate := filepath.Join(dir, entry.Name())
			if nested {
				if markerPresent(candidate, []string{rest}, "") {
					return true
				}
				continue
			}
			if entry.Type()&os.ModeSymlink == 0 {
				if !entry.IsDir() {
					return true
				}
				continue
			}
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return true
			}
		}
	}
	return false
}

// ExcludedByProviderScopes reports whether any adapter refuses to descend into a
// workspace-relative directory — the union the scan applies, exported so the
// commands that act on the scan's ANSWER can tell "the scan found nothing there"
// from "the scan never looked".
//
// `projects sync --prune` is the caller that needs the distinction: it deletes
// membership for every declared project the scan did not return, and an excluded
// directory is precisely the one the scan was told not to enter.
func ExcludedByProviderScopes(scopes []ProviderScope, rel string) bool {
	rel = wsproto.CleanWorkspacePath(rel)
	if rel == "" {
		return false
	}
	for _, scope := range scopes {
		if scope.excludesDir(rel) {
			return true
		}
	}
	return false
}

func isScopeOnlyConfig(dir string) bool {
	sc := wsproto.ReadScopeConfig(dir)
	if sc == nil {
		return false
	}
	// An activated scope is also a project — its own dir must be discovered.
	if sc.Activate {
		return false
	}
	projectCfg := wsproto.LoadProjectConfig(dir)
	return !hasProjectSpecificConfig(projectCfg)
}

func hasProjectSpecificConfig(cfg *wsproto.ProjectConfig) bool {
	if cfg == nil {
		return false
	}
	return cfg.Name != "" ||
		cfg.Type != "" ||
		cfg.Description != "" ||
		cfg.Main != "" ||
		cfg.Bin != nil ||
		cfg.Exports != nil ||
		len(cfg.Publish) > 0 ||
		len(cfg.RunsWith) > 0 ||
		cfg.Build != nil ||
		len(cfg.Dependencies) > 0 ||
		cfg.Options != nil ||
		cfg.Jobs != nil ||
		cfg.Disable != nil
}
