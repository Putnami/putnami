package observe

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

func writeRepoFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAWorkspaceCarriesItsNewestSpecVerificationRecord(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "a-workspace-carries-its-newest-spec-verification-record")
	root := t.TempDir()
	writeRepoFile(t, root, analyze.WorkspaceFile, "{}\n")
	writeRepoFile(t, root, ".putnami/sessions/a/spec-verification.json", `{"protocolVersion": 1, "generatedAt": "2026-11-12T08:00:00Z", "groups": []}`)
	writeRepoFile(t, root, ".putnami/sessions/b/spec-verification.json", `{"protocolVersion": 1, "generatedAt": "2026-11-12T09:00:00+01:00", "groups": [{"project": "older"}]}`)
	writeRepoFile(t, root, ".putnami/sessions/c/spec-verification.json", `{"protocolVersion": 1, "generatedAt": "2026-11-12T08:30:00Z", "groups": [{"project": "pricing"}]}`)
	writeRepoFile(t, root, ".putnami/sessions/d/session.json", `{}`)
	record, diags := readSpecVerification(root)
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v", diags)
	}
	if want := `{"protocolVersion":1,"generatedAt":"2026-11-12T08:30:00Z","groups":[{"project":"pricing"}]}`; string(record) != want {
		t.Fatalf("record = %s, want %s", record, want)
	}

	// A workspace whose CI verified no spec gets the step to add.
	empty := t.TempDir()
	writeRepoFile(t, empty, analyze.WorkspaceFile, "{}\n")
	record, diags = readSpecVerification(empty)
	if record != nil || len(diags) != 1 || diags[0].Code != CodeSpecVerificationMissing {
		t.Fatalf("record = %s, diagnostics = %v, want the missing-record warning", record, diags)
	}

	// An unreadable record is named, never sent.
	broken := t.TempDir()
	writeRepoFile(t, broken, analyze.WorkspaceFile, "{}\n")
	writeRepoFile(t, broken, ".putnami/sessions/a/spec-verification.json", `[1]`)
	record, diags = readSpecVerification(broken)
	if record != nil || len(diags) != 1 || diags[0].Code != CodeSpecVerificationUnread {
		t.Fatalf("record = %s, diagnostics = %v, want the unread warning", record, diags)
	}
}

func TestAnotherRepositoryCarriesNoSpecVerificationRecord(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "another-repository-declares-nothing")
	root := t.TempDir()
	writeRepoFile(t, root, ".putnami/sessions/a/spec-verification.json", `{"generatedAt": "2026-11-12T08:00:00Z"}`)
	if record, diags := readSpecVerification(root); record != nil || diags != nil {
		t.Fatalf("record = %s, diagnostics = %v, want none", record, diags)
	}
}
