package analyze

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

func declaredRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, WorkspaceFile), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAWorkspaceChangeCarriesItsDeclarations(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "a-workspace-change-carries-its-declarations")
	base := map[string]string{
		"pricing/specs/discount.json": `{"requirements":[{"id":"a"}]}`,
		"pricing/specs/old.json":      `{"old":true}`,
		"pricing/specs/legacy.json":   `{"legacy":true}`,
	}
	head := map[string]string{
		"pricing/specs/discount.json": `{"requirements":[]}`,
		"specs/moved.json":            `{"old":true}`,
		"CODEOWNERS":                  "* @acme/pricing\n",
	}
	in := Input{
		Root: declaredRoot(t),
		ChangedFiles: []gitbase.ChangedFile{
			{Path: "CODEOWNERS", Status: gitbase.StatusAdded},
			{Path: "pricing/discount_test.go", Status: gitbase.StatusDeleted},
			{Path: "pricing/specs/discount.json", Status: gitbase.StatusModified},
			{Path: "pricing/specs/legacy.json", Status: gitbase.StatusDeleted},
			{Path: "specs/moved.json", Status: gitbase.StatusRenamed, PreviousPath: "pricing/specs/old.json"},
			{Path: "putnami.workspace.json", Status: gitbase.StatusModified},
		},
		Show: func(side Revision, file string) ([]byte, bool, error) {
			sides := head
			if side == Base {
				sides = base
			}
			content, ok := sides[file]
			return []byte(content), ok, nil
		},
	}
	var r Result
	r.declarations(in)
	text := func(s string) *string { return &s }
	want := []evidence.Declaration{
		{Path: "CODEOWNERS", Status: evidence.FileAdded, After: text("* @acme/pricing\n")},
		{Path: "pricing/specs/discount.json", Status: evidence.FileModified, Before: text(`{"requirements":[{"id":"a"}]}`), After: text(`{"requirements":[]}`)},
		{Path: "pricing/specs/legacy.json", Status: evidence.FileDeleted, Before: text(`{"legacy":true}`)},
		{Path: "specs/moved.json", Status: evidence.FileRenamed, PreviousPath: "pricing/specs/old.json", Before: text(`{"old":true}`), After: text(`{"old":true}`)},
	}
	if !reflect.DeepEqual(r.Declarations, want) {
		t.Fatalf("declarations = %+v, want %+v", r.Declarations, want)
	}
	if len(r.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", r.Diagnostics)
	}
}

func TestADeclarationOverItsBoundKeepsItsEntry(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "a-workspace-change-carries-its-declarations")
	large := make([]byte, evidence.MaxDeclarationBytes+1)
	in := Input{
		Root:         declaredRoot(t),
		ChangedFiles: []gitbase.ChangedFile{{Path: "specs/large.json", Status: gitbase.StatusAdded}},
		Show:         func(Revision, string) ([]byte, bool, error) { return large, true, nil },
	}
	var r Result
	r.declarations(in)
	if len(r.Declarations) != 1 || r.Declarations[0].After != nil {
		t.Fatalf("declarations = %+v, want the entry without its document", r.Declarations)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeDeclarationUnread {
		t.Fatalf("diagnostics = %v", r.Diagnostics)
	}
}

func TestAnotherRepositoryDeclaresNothing(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "declared-repository-evidence", "another-repository-declares-nothing")
	in := Input{
		Root:         t.TempDir(),
		ChangedFiles: []gitbase.ChangedFile{{Path: "specs/pricing.json", Status: gitbase.StatusAdded}},
		Show: func(Revision, string) ([]byte, bool, error) {
			t.Error("a declaration was read")
			return nil, false, nil
		},
	}
	var r Result
	r.declarations(in)
	if r.Declarations != nil {
		t.Fatalf("declarations = %+v, want none", r.Declarations)
	}
}
