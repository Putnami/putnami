package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/agentartifacts"
)

func digestOf(fill string) string { return strings.Repeat(fill, 64) }

// The extension's own record is a third owner beside the superseded ones: a
// path it agrees on merges, and a path it records with other bytes is a
// duplicate owner named in the refusal.
func TestMergeOwnershipHistoriesIncludesTheExtensionRecord(t *testing.T) {
	journal := &agentContentJournal{
		Extension: "@acme/contributor",
		Superseded: []journalArtifact{
			{Name: "@acme/workflows", History: []agentartifacts.File{{Path: "a", SHA256: digestOf("1")}, {Path: "b", SHA256: digestOf("2")}}},
		},
		Target: &agentartifacts.State{Name: "@acme/contributor", Files: []agentartifacts.File{{Path: "b", SHA256: digestOf("2")}, {Path: "c", SHA256: digestOf("3")}}},
	}
	merged, err := mergeOwnershipHistories(journal)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 3 || merged[0].Path != "a" || merged[1].Path != "b" || merged[2].Path != "c" {
		t.Fatalf("merged = %+v, want a, b, c once each in path order", merged)
	}

	journal.Target.Files[0].SHA256 = digestOf("9")
	_, err = mergeOwnershipHistories(journal)
	if err == nil || !strings.Contains(err.Error(), "duplicate ownership: b [@acme/contributor 999999999999, @acme/workflows 222222222222]") {
		t.Fatalf("error = %v, want both owners of b named", err)
	}
}

// The opt-in takes the place of the first superseded entry, keeps every other
// entry in order, and is never written twice.
func TestMigratedDeclarationsKeepOrderAndOneOptIn(t *testing.T) {
	root := t.TempDir()
	supersedes := map[string]bool{"@acme/workflows": true, "@acme/maintainer": true}
	got := migratedDeclarations(root, []string{"@other/tools", "@acme/workflows:stable", "@x/y", "@acme/maintainer"}, supersedes, "@acme/contributor", false)
	if strings.Join(got, ",") != "@other/tools,extension:@acme/contributor,@x/y" {
		t.Fatalf("declarations = %v", got)
	}
	got = migratedDeclarations(root, []string{"extension:@acme/contributor", "@acme/workflows"}, supersedes, "@acme/contributor", true)
	if strings.Join(got, ",") != "extension:@acme/contributor" {
		t.Fatalf("an existing opt-in was duplicated or moved: %v", got)
	}
	got = migratedDeclarations(root, nil, supersedes, "@acme/contributor", false)
	if strings.Join(got, ",") != "extension:@acme/contributor" {
		t.Fatalf("a records-only migration must still opt in: %v", got)
	}
}

func validJournal() *agentContentJournal {
	state := &agentartifacts.State{
		Version: agentartifacts.StateVersion, Name: "@acme/contributor", ArtifactVersion: "1.0.0",
		ArchiveDigest: digestOf("a"), ManifestHash: digestOf("b"),
		Files: []agentartifacts.File{{Path: ".agents/skills/x/SKILL.md", SHA256: digestOf("c")}},
	}
	return &agentContentJournal{
		Version:      agentContentJournalVersion,
		Extension:    "@acme/contributor",
		Phase:        journalApplying,
		Content:      agentContentIdentity{Version: "1.0.0", ArchiveDigest: digestOf("a"), ManifestHash: digestOf("b")},
		Declarations: AgentContentMigrationDeclarations{Before: []string{"@acme/workflows"}, After: []string{"extension:@acme/contributor"}},
		Config:       journalConfig{Before: digestOf("d"), After: digestOf("e")},
		Superseded:   []journalArtifact{{Name: "@acme/workflows", Ownership: AgentContentOwnershipNone}},
		After:        state,
		Files:        []journalFile{{Path: ".agents/skills/x/SKILL.md", After: digestOf("c")}},
	}
}

// Every path a journal names authorizes a write or a removal, so a journal
// that names an unsafe path, a malformed digest or an unknown state is refused
// as a whole.
func TestValidateAgentContentJournalRefusesWhatItCannotTrust(t *testing.T) {
	if reason := validateAgentContentJournal(validJournal(), "@acme/contributor"); reason != "" {
		t.Fatalf("a valid journal was refused: %s", reason)
	}
	unsafeHistory := []agentartifacts.File{{Path: "a/../../b", SHA256: digestOf("f")}}
	foreign := &agentartifacts.State{Name: "@acme/other"}
	for name, mutate := range map[string]func(*agentContentJournal){
		"another version":      func(j *agentContentJournal) { j.Version = 2 },
		"another extension":    func(j *agentContentJournal) { j.Extension = "@acme/other" },
		"an unknown phase":     func(j *agentContentJournal) { j.Phase = "done" },
		"no content identity":  func(j *agentContentJournal) { j.Content.ManifestHash = "x" },
		"no config digests":    func(j *agentContentJournal) { j.Config.After = "" },
		"no superseded":        func(j *agentContentJournal) { j.Superseded = nil },
		"no after record":      func(j *agentContentJournal) { j.After = nil },
		"no declarations":      func(j *agentContentJournal) { j.Declarations.After = nil },
		"a self-supersession":  func(j *agentContentJournal) { j.Superseded[0].Name = "@acme/contributor" },
		"an escaping path":     func(j *agentContentJournal) { j.Files[0].Path = "../outside" },
		"a digestless path":    func(j *agentContentJournal) { j.Files[0].After = "" },
		"an unsafe release":    func(j *agentContentJournal) { j.Released = []string{"/etc/passwd"} },
		"an unsafe history":    func(j *agentContentJournal) { j.Superseded[0].History = unsafeHistory },
		"a foreign record":     func(j *agentContentJournal) { j.Superseded[0].Record = foreign },
		"a foreign target":     func(j *agentContentJournal) { j.Target = foreign },
		"an empty after state": func(j *agentContentJournal) { j.After.Files = nil },
	} {
		t.Run(name, func(t *testing.T) {
			journal := validJournal()
			mutate(journal)
			if reason := validateAgentContentJournal(journal, "@acme/contributor"); reason == "" {
				t.Fatal("the journal was accepted")
			}
		})
	}
}

// Rewriting the declarations keeps every other member in its order, and an
// empty array removes the member rather than leaving an empty opt-in behind.
func TestRenderWorkspaceDeclarationsTouchesOnlyAgentArtifacts(t *testing.T) {
	config := []byte(`{"name":"ws","agentArtifacts":["@acme/workflows"],"extensions":["@acme/contributor"]}`)
	rendered, err := renderWorkspaceDeclarations(config, []string{"extension:@acme/contributor"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	name, artifacts, extensions := strings.Index(text, `"name"`), strings.Index(text, `"agentArtifacts"`), strings.Index(text, `"extensions"`)
	if name >= artifacts || artifacts >= extensions || !strings.Contains(text, `"extension:@acme/contributor"`) {
		t.Fatalf("member order or value changed:\n%s", text)
	}
	emptied, err := renderWorkspaceDeclarations(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(emptied), "agentArtifacts") {
		t.Fatalf("an empty declaration left the member:\n%s", emptied)
	}
	if _, err := renderWorkspaceDeclarations([]byte("{"), nil); err == nil {
		t.Fatal("an unparseable config was rewritten")
	}
}

// A journal directory is never followed through a symlink, and one without a
// journal document is a start that never wrote one.
func TestAgentContentJournalStorageRefusesSymlinksAndIgnoresUnstartedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agentContentMigrationsDir(root), "acme-contributor", agentContentJournalBeforeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	journals, err := listAgentContentJournals(root)
	if err != nil || len(journals) != 0 {
		t.Fatalf("an unstarted directory was read as a journal: %v, %v", journals, err)
	}

	linked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linked, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, ".putnami", agentContentMigrationDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := listAgentContentJournals(linked); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want the symlinked journal directory refused", err)
	}
	if err := requireNoUnfinishedAgentContentMigration(linked); err == nil {
		t.Fatal("an ordinary pass read through a symlinked journal directory")
	}

	journal := validJournal()
	if err := saveAgentContentJournal(root, journal); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := requireNoUnfinishedAgentContentMigration(root); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("error = %v, want the unfinished migration named", err)
	}
	journal.Phase = journalApplied
	if err := saveAgentContentJournal(root, journal); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := requireNoUnfinishedAgentContentMigration(root); err != nil {
		t.Fatalf("a complete journal must be inert: %v", err)
	}
	if err := removeAgentContentJournal(root, journal.Extension); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentContentMigrationsDir(root)); !os.IsNotExist(err) {
		t.Fatalf("the emptied journal root was left behind: %v", err)
	}
}
