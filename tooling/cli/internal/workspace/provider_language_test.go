package workspace

import (
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

func claimingProvider(extension string, paths ...string) SnapshotProvider {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: extension}
	for _, path := range paths {
		result.Projects = append(result.Projects, wsproto.ProbeProject{Path: path})
	}
	return SnapshotProvider{Extension: extension, Result: result}
}

func TestDeclaredLanguages_JoinsDeclarationsWithRecordedClaims(t *testing.T) {
	t.Parallel()
	snapshot := &Snapshot{Providers: []SnapshotProvider{
		claimingProvider("@x/ts", "web", "shared", "."),
		claimingProvider("@x/go", "api", "shared"),
		claimingProvider("@x/silent", "docs"),
		claimingProvider("@x/missing", "legacy"),
		// A recorded Language is never read: the declaration decides.
		{Extension: "@x/py", Language: "stale", Result: claimingProvider("@x/py", "tools").Result},
	}}
	scopes := []ProviderScope{
		{Extension: "@x/ts", Language: "typescript"},
		{Extension: "@x/go", Language: "go"},
		{Extension: "@x/silent"},
		{Extension: "@x/py", Language: "python"},
	}

	languages := DeclaredLanguages(snapshot, scopes)

	want := map[string][]string{
		"web":    {"typescript"},
		"api":    {"go"},
		"shared": {"go", "typescript"},
		".":      {"typescript"},
		"tools":  {"python"},
	}
	for path, wantLanguages := range want {
		if got := languages.Of(path); !slices.Equal(got, wantLanguages) {
			t.Errorf("languages of %q = %v, want %v", path, got, wantLanguages)
		}
	}
	for _, path := range []string{"docs", "legacy", "unclaimed"} {
		if got := languages.Of(path); len(got) != 0 {
			t.Errorf("languages of %q = %v, want none", path, got)
		}
	}
	if got := languages.Of(""); !slices.Equal(got, []string{"typescript"}) {
		t.Errorf("languages of the workspace root = %v, want [typescript]", got)
	}
}

func TestDeclaredLanguages_NoSnapshotClaimsNothing(t *testing.T) {
	t.Parallel()
	languages := DeclaredLanguages(nil, []ProviderScope{{Extension: "@x/go", Language: "go"}})
	if got := languages.Of("api"); len(got) != 0 {
		t.Errorf("languages = %v, want none without a workspace index", got)
	}
}

func TestProjectLanguagesOf_ReturnsACopy(t *testing.T) {
	t.Parallel()
	languages := ProjectLanguages{"api": {"go"}}
	languages.Of("api")[0] = "changed"
	if got := languages.Of("api"); !slices.Equal(got, []string{"go"}) {
		t.Errorf("languages = %v, want the caller's edit not to reach the map", got)
	}
}
