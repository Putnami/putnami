package agentartifacts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing marker is a fresh checkout, not an error — the same rule the
// workspace install marker uses, and the reason a new worktree can materialize
// without a repair step.
func TestLoadState_MissingIsFreshNotAnError(t *testing.T) {
	state, err := LoadState(t.TempDir(), testArtifactName)
	if err != nil {
		t.Fatalf("a missing state must not be an error: %v", err)
	}
	if state != nil {
		t.Fatalf("state = %+v, want nil", state)
	}
	if len(state.ManagedDigests()) != 0 {
		t.Fatal("a nil state must report nothing as managed")
	}
}

// Corrupt state is a hard, actionable failure. Reading it as "nothing is
// managed" would be quieter and would resolve every subsequent question in the
// direction of touching user files.
func TestLoadState_CorruptionIsActionable(t *testing.T) {
	good := strings.Repeat("a", 64)
	valid := `{"version":1,"name":"` + testArtifactName + `","artifactVersion":"1.0.0",` +
		`"archiveDigest":"` + good + `","manifestHash":"` + good + `",` +
		`"files":[{"path":".agents/skills/fix/SKILL.md","sha256":"` + good + `"}]}`

	cases := map[string]string{
		"truncated json":     `{"version":1,`,
		"not an object":      `[]`,
		"unknown field":      strings.Replace(valid, `"version":1,`, `"version":1,"force":true,`, 1),
		"trailing document":  valid + ` {}`,
		"wrong schema":       strings.Replace(valid, `"version":1`, `"version":99`, 1),
		"wrong artifact":     strings.Replace(valid, testArtifactName, "@putnami/other", 1),
		"no version":         strings.Replace(valid, `"artifactVersion":"1.0.0"`, `"artifactVersion":""`, 1),
		"bad archive digest": strings.Replace(valid, `"archiveDigest":"`+good+`"`, `"archiveDigest":"nope"`, 1),
		"bad manifest hash":  strings.Replace(valid, `"manifestHash":"`+good+`"`, `"manifestHash":"nope"`, 1),
		"no files":           strings.Replace(valid, `"files":[{"path":".agents/skills/fix/SKILL.md","sha256":"`+good+`"}]`, `"files":[]`, 1),
		"escaping path":      strings.Replace(valid, ".agents/skills/fix/SKILL.md", "../../../etc/passwd", 1),
		"absolute path":      strings.Replace(valid, ".agents/skills/fix/SKILL.md", "/etc/passwd", 1),
		"bad file digest":    strings.Replace(valid, `"sha256":"`+good+`"}]`, `"sha256":"nope"}]`, 1),
		"duplicate path": strings.Replace(valid,
			`"files":[{"path":".agents/skills/fix/SKILL.md","sha256":"`+good+`"}]`,
			`"files":[{"path":".agents/x","sha256":"`+good+`"},{"path":".agents/x","sha256":"`+good+`"}]`, 1),
	}

	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := StatePath(root, testArtifactName)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadState(root, testArtifactName)
			var corrupt *CorruptStateError
			if !errors.As(err, &corrupt) {
				t.Fatalf("error = %v, want a *CorruptStateError", err)
			}
			if corrupt.Path != path {
				t.Errorf("error path = %q, want %q", corrupt.Path, path)
			}
			// The diagnostic must name the file, say nothing changed, and state
			// the remedy — otherwise it is a dead end for whoever hits it.
			for _, want := range []string{path, "Nothing was changed", "Remove the ownership state file", "collision"} {
				if !strings.Contains(corrupt.Error(), want) {
					t.Errorf("diagnostic %q does not mention %q", corrupt.Error(), want)
				}
			}
		})
	}
}

// A symlinked state file is never followed: the state authorizes deletions, so
// whoever chooses the file chooses what this package believes it owns.
func TestLoadState_RejectsNonRegularFile(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	path := StatePath(root, testArtifactName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(other, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(root, testArtifactName); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want a symlink rejection", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(root, testArtifactName); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want a non-regular-file rejection", err)
	}
}

// Ownership state authorizes deletions, so a symlink anywhere on the path to
// the state is rejected. In particular, WriteState must not turn a malicious
// state-parent link into an out-of-workspace write when no state exists yet.
func TestStateParentSymlinksAreNeverFollowed(t *testing.T) {
	requireSymlinks(t)
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	state := StateFor(artifact)

	cases := map[string]func(root, outside string) error{
		"putnami directory": func(root, outside string) error {
			return os.Symlink(outside, filepath.Join(root, ".putnami"))
		},
		"state directory": func(root, outside string) error {
			if err := os.Mkdir(filepath.Join(root, ".putnami"), 0o755); err != nil {
				return err
			}
			return os.Symlink(outside, filepath.Join(root, ".putnami", StateDirName))
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			if err := arrange(root, outside); err != nil {
				t.Fatal(err)
			}

			if _, err := LoadState(root, testArtifactName); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("LoadState error = %v, want a symlink rejection", err)
			}
			if err := WriteState(root, state); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("WriteState error = %v, want a symlink rejection", err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("state operation followed its parent symlink and wrote %d external entries", len(entries))
			}
		})
	}
}

// The state location is validated before anything is planned or written: a
// symlinked .putnami parent would otherwise send the ownership record outside
// the workspace.
func TestMaterialize_RejectsStateParentSymlinkBeforeWriting(t *testing.T) {
	requireSymlinks(t)
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(fixture.root, ".putnami")); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.materialize(t); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("materialize error = %v, want a symlink rejection", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialization created a workflow directory: %v", err)
	}
}

// The state lives under the already-gitignored .putnami/ tree, so it never
// enters a commit and a fresh checkout legitimately starts without it.
func TestStatePath_IsUnderTheGitignoredPutnamiTree(t *testing.T) {
	root := t.TempDir()
	got := StatePath(root, testArtifactName)
	want := filepath.Join(root, ".putnami", "agent-artifacts", "putnami-agent-workflows.json")
	if got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
}

// Canonical bytes: sorted, indented, one trailing newline, and stable across
// repeated marshals of a differently-ordered input.
func TestMarshalState_IsCanonicalAndNonMutating(t *testing.T) {
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	state := StateFor(artifact)

	shuffled := *state
	shuffled.Files = append([]File(nil), state.Files...)
	for i, j := 0, len(shuffled.Files)-1; i < j; i, j = i+1, j-1 {
		shuffled.Files[i], shuffled.Files[j] = shuffled.Files[j], shuffled.Files[i]
	}
	authored := append([]File(nil), shuffled.Files...)

	first, err := MarshalState(&shuffled)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := MarshalState(&shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("iteration %d produced different bytes", i)
		}
	}
	for i := range authored {
		if shuffled.Files[i] != authored[i] {
			t.Fatalf("MarshalState mutated the caller's slice at %d", i)
		}
	}
	if !strings.HasSuffix(string(first), "\n") || strings.HasSuffix(string(first), "\n\n") {
		t.Fatalf("canonical state needs exactly one trailing newline: %q", first)
	}

	canonical, err := MarshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(first) {
		t.Fatalf("authored order leaked into the canonical bytes:\n%s\n%s", canonical, first)
	}

	parsed, err := ParseState(first, testArtifactName)
	if err != nil {
		t.Fatal(err)
	}
	round, err := MarshalState(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(round) != string(first) {
		t.Fatalf("state did not round-trip:\n%s\n%s", first, round)
	}
}

// WriteState publishes atomically and leaves no staging file behind.
func TestWriteState_IsAtomicAndReadable(t *testing.T) {
	root := t.TempDir()
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	state := StateFor(artifact)
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	// A second write over an existing state must succeed too (rename over).
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(StatePath(root, testArtifactName)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir holds %d entries, want exactly the published state", len(entries))
	}

	loaded, err := LoadState(root, testArtifactName)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ArchiveDigest != artifact.ArchiveDigest || loaded.ManifestHash != artifact.ManifestHash {
		t.Fatalf("loaded state lost its digests: %+v", loaded)
	}
	if len(loaded.Files) != len(artifact.Files) {
		t.Fatalf("loaded %d managed files, want %d", len(loaded.Files), len(artifact.Files))
	}
}
