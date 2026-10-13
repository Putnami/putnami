package workspace_state

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The recorded declaration digest.
//
// Two recorded sessions of one branch must tell a code change from a
// declaration change. These pin the digest to the declaration files alone, to
// every project rather than the selection, and to the start of the run.

func declarationWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for file, content := range map[string]string{
		"decisions.json":                             `{"decisions":[]}`,
		"tooling/putnami.features.json":              `{"features":[]}`,
		"tooling/cli/putnami.features.json":          `{"features":[{"id":"cli/x"}]}`,
		"tooling/cli/specs/x.json":                   `{"feature":"cli/x"}`,
		"tooling/cli/specs.baseline.json":            `{"executableRequirements":[]}`,
		"tooling/cli/putnami.architecture.json":      `{"rules":[]}`,
		"tooling/cli/main.go":                        "package main\n",
		"tooling/cli/internal/testdata/specs/y.json": `{"fixture":true}`,
		"go/lib/lib.go":                              "package lib\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func declarationDigestOf(t *testing.T, root string) (string, int) {
	t.Helper()
	declarations, err := DeclarationDigest(root, []string{"tooling/cli", "go/lib"})
	if err != nil {
		t.Fatalf("DeclarationDigest: %v", err)
	}
	return declarations.Digest, declarations.Files
}

func TestDeclarationDigestMovesWithDeclarationsOnly(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "recorded-run-context",
		"the-declaration-digest-moves-with-declarations-only")
	root := declarationWorkspace(t)
	before, files := declarationDigestOf(t, root)
	if files != 6 {
		t.Fatalf("files = %d, want the 6 declaration files and no code or test fixture", files)
	}

	for _, code := range []string{"tooling/cli/main.go", "go/lib/lib.go", "tooling/cli/internal/testdata/specs/y.json"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(code)), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _ := declarationDigestOf(t, root); got != before {
			t.Errorf("editing %s moved the declaration digest", code)
		}
	}

	for _, declaration := range []string{
		"decisions.json", "tooling/putnami.features.json", "tooling/cli/specs/x.json",
		"tooling/cli/specs.baseline.json", "tooling/cli/putnami.architecture.json",
	} {
		previous, _ := declarationDigestOf(t, root)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(declaration)), []byte(`{"changed":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _ := declarationDigestOf(t, root); got == previous {
			t.Errorf("editing %s left the declaration digest unchanged", declaration)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "tooling", "cli", "specs", "z.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, got := declarationDigestOf(t, root); got != 7 {
		t.Errorf("files = %d after adding a spec, want 7", got)
	}
}

func TestDeclarationDigestOfAnEmptyWorkspaceIsAMeasurement(t *testing.T) {
	declarations, err := DeclarationDigest(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("DeclarationDigest: %v", err)
	}
	if declarations.Files != 0 || len(declarations.Digest) != 64 {
		t.Errorf("declarations = %+v, want zero files and a full digest", declarations)
	}
}

func TestSessionRecordsTheDeclarationsItOpenedOn(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "recorded-run-context",
		"the-declaration-digest-moves-with-declarations-only")
	root := declarationWorkspace(t)
	session, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.CaptureDeclarations(root, []string{"tooling/cli", "go/lib"})
	atStart, _ := declarationDigestOf(t, root)

	if err := os.WriteFile(filepath.Join(root, "decisions.json"), []byte(`{"rewritten":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := session.FinalizeV2(minimalSessionFile(), "", ""); err != nil {
		t.Fatalf("FinalizeV2: %v", err)
	}
	parsed := recordedSession(t, session)
	if parsed.Declarations == nil || parsed.Declarations.Digest != atStart {
		t.Errorf("recorded declarations = %+v, want the start-of-run digest %s", parsed.Declarations, atStart)
	}
	if parsed.Agent != nil {
		t.Errorf("a session nobody named an agent for recorded %+v", parsed.Agent)
	}
}
