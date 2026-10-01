package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

func TestSettingsChooseTheBackendExplicitly(t *testing.T) {
	spectest.Proves(t, feature, "explicit-backend", "a-binding-without-a-backend-is-invalid")
	for name, settings := range map[string]string{
		"no settings":         ``,
		"no backend":          `{"path":"memory"}`,
		"an unknown backend":  `{"backend":"s3"}`,
		"an unknown setting":  `{"backend":"file","bucket":"x"}`,
		"a file remote":       `{"backend":"file","remote":"origin"}`,
		"a file branch":       `{"backend":"file","branch":"memory"}`,
		"an option remote":    `{"backend":"git","remote":"--upload-pack=touch /tmp/x"}`,
		"a bad branch":        `{"backend":"git","branch":"a..b"}`,
		"a lock branch":       `{"backend":"git","branch":"memory.lock"}`,
		"a spaced repository": `{"backend":"file","repository":"acme app"}`,
		"not an object":       `["file"]`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, t.TempDir(), settings)
			failure := h.fails(collab.OperationContext, map[string]any{}, collab.OutcomeInvalid)
			if failure.Reason != "settings.invalid" || failure.Retryable {
				t.Fatalf("%s: %+v", settings, failure)
			}
		})
	}
	h := newHarness(t, "", `{"backend":"file"}`)
	if failure := h.fails(collab.OperationMission, map[string]any{"mission": "m"}, collab.OutcomeInvalid); !strings.Contains(failure.Message, "workspace root") {
		t.Fatalf("a relative path without a workspace root: %+v", failure)
	}
	absolute := filepath.Join(t.TempDir(), "elsewhere")
	h = newHarness(t, "", `{"backend":"file","path":`+mustJSON(t, absolute)+`}`)
	h.save(checkpoint("m", "k", "c", ""))
	if _, err := os.Stat(filepath.Join(absolute, store.MetaPath)); err != nil {
		t.Fatalf("an absolute path was not used: %v", err)
	}
}

func TestARemoteCarryingCredentialsIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "explicit-backend", "a-remote-carrying-credentials-is-refused")
	for _, remote := range []string{
		"https://token@example.com/acme/memory.git",
		"https://user:secret@example.com/acme/memory.git",
		"ssh://git:secret@example.com/acme/memory.git",
	} {
		h := newHarness(t, t.TempDir(), `{"backend":"git","remote":`+mustJSON(t, remote)+`}`)
		failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k", "c", ""), collab.OutcomeInvalid)
		if strings.Contains(failure.Message, "secret") || strings.Contains(failure.Message, "token@") {
			t.Fatalf("the refusal quotes the credential: %+v", failure)
		}
	}
	for _, remote := range []string{"origin", "ssh://git@example.com/acme/memory.git", "git@example.com:acme/memory.git", "../memory.git"} {
		if failure := checkRemote(remote); failure != nil {
			t.Errorf("remote %q refused: %+v", remote, failure)
		}
	}
}

func TestARelativeRemotePathIsTheWorkspaces(t *testing.T) {
	isolateGit(t)
	workspace := t.TempDir()
	gitCommand(t, workspace, "init", "--bare", "--quiet", "shared.git")
	h := newHarness(t, workspace, `{"backend":"git","remote":"./shared.git"}`)
	h.save(checkpoint("m", "k", "c", ""))
	gitCommand(t, workspace, "--git-dir=shared.git", "rev-parse", "--verify", "refs/heads/putnami-memory")
}

func TestTheRepositorySettingIsTheDefaultIdentity(t *testing.T) {
	h := newHarness(t, t.TempDir(), `{"backend":"file","repository":"acme/app"}`)
	record := h.save(checkpoint("m", "k", "c", "")).Record
	if record.Identity.Repository != "acme/app" {
		t.Fatalf("record %+v", record)
	}
	other := checkpoint("m", "k2", "other repository", "")
	other["identity"] = map[string]any{"repository": "acme/other"}
	if created := h.save(other); created.Record.Ref == record.Ref {
		t.Fatalf("two repositories share one mission: %+v", created)
	}
}

func TestNotesAddedToTheStoreAreContext(t *testing.T) {
	workspace := t.TempDir()
	h := newHarness(t, workspace, `{"backend":"file"}`)
	h.save(checkpoint("m", "k", "mission state", ""))
	note := &store.Document{
		Format: store.FormatVersion, ID: "release-policy", Kind: collab.MemoryKindNote,
		Identity: collab.MemoryIdentity{Scope: "/tooling"}, Title: "Release policy", Content: "Tag from main only.",
		Provenance: collab.Provenance{RecordedAt: "2026-09-01T00:00:00Z", RecordedBy: "maintainers"}, UpdatedAt: "2026-09-01T00:00:00Z",
	}
	records := filepath.Join(workspace, DefaultFilePath, store.RecordsDir)
	mustDo(t, os.WriteFile(filepath.Join(records, "release-policy.json"), stored(t, note), 0o644))
	mustDo(t, os.WriteFile(filepath.Join(records, "README.md"), []byte("notes for people"), 0o644))
	mustDo(t, os.WriteFile(filepath.Join(records, ".m.json.123.tmp"), []byte("{torn"), 0o644))

	var notes collab.MemoryListResult
	h.ok(collab.OperationContext, map[string]any{"kinds": []string{"note"}}, &notes)
	if len(notes.Items) != 1 || notes.Items[0].Ref.ID != "release-policy" || notes.Items[0].Revision != note.Revision() {
		t.Fatalf("notes %+v", notes)
	}
	var all collab.MemoryListResult
	h.ok(collab.OperationContext, map[string]any{}, &all)
	if len(all.Items) != 2 || all.Items[0].Kind != collab.MemoryKindMission || all.Items[1].Kind != collab.MemoryKindNote {
		t.Fatalf("missions come first, then notes: %+v", all)
	}
}

func TestAnInvalidRecordIsNamed(t *testing.T) {
	workspace := t.TempDir()
	h := newHarness(t, workspace, `{"backend":"file"}`)
	record := h.save(checkpoint("m", "k", "c", "")).Record
	records := filepath.Join(workspace, DefaultFilePath, store.RecordsDir)

	misplaced := &store.Document{
		Format: store.FormatVersion, ID: "m-0000", Kind: collab.MemoryKindMission, Identity: collab.MemoryIdentity{Mission: "x"},
		Content: "x", Provenance: collab.Provenance{RecordedAt: "2026-09-01T00:00:00Z"}, UpdatedAt: "2026-09-01T00:00:00Z",
	}
	mustDo(t, os.WriteFile(filepath.Join(records, "m-0000.json"), stored(t, misplaced), 0o644))
	failure := h.fails(collab.OperationContext, map[string]any{}, collab.OutcomeUnavailable)
	if failure.Reason != "store.invalid" || !strings.Contains(failure.Message, "m-0000") {
		t.Fatalf("a misplaced mission answered %+v", failure)
	}
	mustDo(t, os.Remove(filepath.Join(records, "m-0000.json")))

	undated := &store.Document{Format: store.FormatVersion, ID: "undated", Kind: collab.MemoryKindNote, Content: "x"}
	mustDo(t, os.WriteFile(filepath.Join(records, "undated.json"), stored(t, undated), 0o644))
	failure = h.fails(collab.OperationSearch, map[string]any{"query": "x"}, collab.OutcomeUnavailable)
	if failure.Reason != "store.invalid" || !strings.Contains(failure.Message, "undated") {
		t.Fatalf("an undated note answered %+v", failure)
	}
	mustDo(t, os.Remove(filepath.Join(records, "undated.json")))

	path := filepath.Join(records, record.Ref.ID+".json")
	mustDo(t, os.WriteFile(path, []byte(`{"format":2}`), 0o644))
	h.fails(collab.OperationMission, map[string]any{"mission": "m"}, collab.OutcomeUnavailable)
	h.fails(collab.OperationCheckpoint, checkpoint("m", "k2", "c", record.Revision), collab.OutcomeUnavailable)

	mustDo(t, os.Remove(path))
	mustDo(t, os.WriteFile(filepath.Join(workspace, DefaultFilePath, store.MetaPath), []byte(`{"format":1,"id":"nope"}`), 0o644))
	h.fails(collab.OperationContext, map[string]any{}, collab.OutcomeUnavailable)

	mustDo(t, os.Remove(filepath.Join(workspace, DefaultFilePath, store.MetaPath)))
	note := &store.Document{Format: store.FormatVersion, ID: "orphan", Kind: collab.MemoryKindNote, Content: "x",
		Provenance: collab.Provenance{RecordedAt: "2026-09-01T00:00:00Z"}, UpdatedAt: "2026-09-01T00:00:00Z"}
	mustDo(t, os.WriteFile(filepath.Join(records, "orphan.json"), stored(t, note), 0o644))
	if failure := h.fails(collab.OperationContext, map[string]any{}, collab.OutcomeUnavailable); !strings.Contains(failure.Message, store.MetaPath) {
		t.Fatalf("records without a store identity answered %+v", failure)
	}
}
