package workspace

import (
	"sort"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ProjectLanguages maps a project's probe path to the languages that the
// workspace providers declare for it. It is keyed by the recorded claim, never
// by a file name: a project no declaring provider claims has no entry.
type ProjectLanguages map[string][]string

// DeclaredLanguages joins the languages the given adapters declare with the
// projects the snapshot records each provider as having claimed.
//
// The language comes from the live declaration (scopes) and the claim from the
// record (snapshot). That is the one pairing that cannot disagree with the next
// synchronization: a sync records the declared language beside the claims it
// asks for, so a manifest edit changes the language here before the record
// catches up, and the language the sync records is the one already used. A
// recorded Language would lag behind an edit until that sync.
//
// A provider that is recorded but has no scope (its extension is not installed
// or declares no adapter) contributes nothing, as does a scope with no declared
// language. The result lists each project's languages sorted and once.
func DeclaredLanguages(snapshot *Snapshot, scopes []ProviderScope) ProjectLanguages {
	if snapshot == nil {
		return nil
	}
	declared := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		if scope.Language != "" {
			declared[scope.Extension] = scope.Language
		}
	}
	sets := make(map[string]map[string]bool)
	for _, provider := range snapshot.Providers {
		language, ok := declared[provider.Extension]
		if !ok {
			continue
		}
		for _, project := range provider.Result.Projects {
			path, valid := wsproto.NormalizeProbePath(project.Path)
			if !valid {
				continue
			}
			if sets[path] == nil {
				sets[path] = make(map[string]bool)
			}
			sets[path][language] = true
		}
	}
	languages := make(ProjectLanguages, len(sets))
	for path, set := range sets {
		list := make([]string, 0, len(set))
		for language := range set {
			list = append(list, language)
		}
		sort.Strings(list)
		languages[path] = list
	}
	return languages
}

// Of returns the languages declared for the project at a workspace-relative
// path, or nil when no declaring provider claims it. The result is a copy.
func (l ProjectLanguages) Of(projectPath string) []string {
	return append([]string(nil), l[probePathOf(projectPath)]...)
}
