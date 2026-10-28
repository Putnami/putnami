package agentctx

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// languageProvider is one local extension of a languages fixture: the language
// its manifest declares (empty for none) and the project directories the
// workspace index records its probe as having claimed.
type languageProvider struct {
	name     string
	language string
	claims   []string
}

// writeLanguagesWorkspace writes a workspace of four go.mod directories, one
// local extension per provider, and a workspace index that records each
// provider's claims. recordedLanguage overrides the Language the index records
// for a provider, so a test can make the record disagree with the manifest.
// It returns the loaded workspace.
func writeLanguagesWorkspace(t *testing.T, providers []languageProvider, recordedLanguage map[string]string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	extensions := make([]string, 0, len(providers))
	records := make([]workspace.SnapshotProvider, 0, len(providers))
	for i, provider := range providers {
		dir := fmt.Sprintf("tools/ext%d", i)
		declared := ""
		if provider.language != "" {
			declared = fmt.Sprintf(`,"language":%q`, provider.language)
		}
		writePathFile(t, root, dir+"/putnami.extension.json", fmt.Sprintf(
			`{"name":%q,"cliContract":%d,"workspace":{"markers":["go.mod"],"inputs":["go.mod"]%s}}`,
			provider.name, protocolcli.AgentContentContract, declared))
		writePathFile(t, root, dir+"/putnami.json", fmt.Sprintf(`{"name":%q}`, provider.name))
		extensions = append(extensions, fmt.Sprintf("%q", "/"+dir))

		result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: provider.name}
		for _, claim := range provider.claims {
			result.Projects = append(result.Projects, wsproto.ProbeProject{Path: claim})
		}
		records = append(records, workspace.SnapshotProvider{
			Extension: provider.name,
			Result:    result,
			Language:  recordedLanguage[provider.name],
		})
	}
	writePathFile(t, root, "putnami.workspace.json", fmt.Sprintf(
		`{"name":"languages","includes":["svc/a","svc/b","svc/c","svc/d"],"extensions":[%s]}`,
		strings.Join(extensions, ",")))
	for _, name := range []string{"a", "b", "c", "d"} {
		writePathFile(t, root, "svc/"+name+"/go.mod", "module example/"+name+"\n\ngo 1.25\n")
		writePathFile(t, root, "svc/"+name+"/putnami.json", fmt.Sprintf(`{"name":"example/%s"}`, name))
	}
	snapshot := &workspace.Snapshot{Version: 4, Providers: records}
	if err := workspace.WriteSnapshot(root, snapshot, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("write the workspace index: %v", err)
	}
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load the workspace: %v", err)
	}
	return ws
}

// documentLanguages builds the document of the project at path and returns the
// languages it reports, resolved the way the command resolves them.
func documentLanguages(t *testing.T, ws *workspace.Workspace, path string) []string {
	t.Helper()
	languages, err := declaredProjectLanguages(ws)
	if err != nil {
		t.Fatalf("declaredProjectLanguages: %v", err)
	}
	project := ws.ProjectByID("/" + path)
	if project == nil {
		t.Fatalf("project %s missing from the workspace", path)
	}
	doc, _, err := BuildDocument(ws, project, languages, fixtureRevision, fixtureVersion)
	if err != nil {
		t.Fatalf("BuildDocument(%s): %v", path, err)
	}
	return doc.Identity.Languages
}

// A project claimed by a provider whose manifest declares go reports go.
func TestContextPackLanguages_ClaimedProjectReportsTheDeclaredLanguage(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "agent-context-languages", "a-claimed-project-reports-the-declared-language")
	ws := writeLanguagesWorkspace(t, []languageProvider{
		{name: "@local/go", language: "go", claims: []string{"svc/a"}},
	}, nil)

	if got := documentLanguages(t, ws, "svc/a"); !slices.Equal(got, []string{"go"}) {
		t.Errorf("languages = %v, want [go]", got)
	}
}

// A project that two providers claim reports both languages, sorted, whatever
// order the providers are recorded in.
func TestContextPackLanguages_TwoProvidersReportBothSorted(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "agent-context-languages", "a-project-claimed-by-two-providers-reports-both-sorted")
	ws := writeLanguagesWorkspace(t, []languageProvider{
		{name: "@local/ts", language: "typescript", claims: []string{"svc/b"}},
		{name: "@local/go", language: "go", claims: []string{"svc/b"}},
	}, nil)

	if got := documentLanguages(t, ws, "svc/b"); !slices.Equal(got, []string{"go", "typescript"}) {
		t.Errorf("languages = %v, want [go typescript]", got)
	}
}

// A provider that declares no language adds none, and it does not change what
// another provider declares for its own projects.
func TestContextPackLanguages_ProviderWithoutLanguageReportsNone(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "agent-context-languages", "a-provider-that-declares-no-language-adds-none")
	ws := writeLanguagesWorkspace(t, []languageProvider{
		{name: "@local/silent", claims: []string{"svc/a"}},
		{name: "@local/go", language: "go", claims: []string{"svc/b"}},
	}, nil)

	if got := documentLanguages(t, ws, "svc/a"); len(got) != 0 {
		t.Errorf("languages = %v, want none from a provider that declares no language", got)
	}
	if got := documentLanguages(t, ws, "svc/b"); !slices.Equal(got, []string{"go"}) {
		t.Errorf("languages = %v, want only the declaring provider's go", got)
	}
}

// A directory with a go.mod that no provider claims reports no language: the
// pack reads no file name.
func TestContextPackLanguages_UnclaimedProjectReportsNone(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "agent-context-languages", "an-unclaimed-project-reports-none-whatever-its-files")
	ws := writeLanguagesWorkspace(t, []languageProvider{
		{name: "@local/go", language: "go", claims: []string{"svc/a"}},
	}, nil)

	if got := documentLanguages(t, ws, "svc/c"); len(got) != 0 {
		t.Errorf("languages = %v, want none for a go.mod directory no provider claims", got)
	}
}

// The language is the manifest's, so a record written before a manifest edit
// cannot name a language the next synchronization would replace.
func TestContextPackLanguages_ManifestDeclarationWinsOverTheRecordedLanguage(t *testing.T) {
	ws := writeLanguagesWorkspace(t, []languageProvider{
		{name: "@local/go", language: "go", claims: []string{"svc/a"}},
	}, map[string]string{"@local/go": "python"})

	if got := documentLanguages(t, ws, "svc/a"); !slices.Equal(got, []string{"go"}) {
		t.Errorf("languages = %v, want the manifest's go over the recorded python", got)
	}
}
