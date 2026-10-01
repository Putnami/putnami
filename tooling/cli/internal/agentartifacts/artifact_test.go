package agentartifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// A pin that cannot secure a materialization is refused before the tree is
// read. A digest may become a path component, so a malformed value is a
// traversal payload, not just an unusable hash.
func TestValidatePin_RejectsUnusablePins(t *testing.T) {
	good := strings.Repeat("a", 64)
	cases := map[string]lockfile.AgentArtifactLockEntry{
		"no version":            {Version: "", Integrity: good, ManifestHash: good},
		"sentinel version":      {Version: "0.0.0", Integrity: good, ManifestHash: good},
		"no integrity":          {Version: "1.0.0", Integrity: "", ManifestHash: good},
		"prefixed integrity":    {Version: "1.0.0", Integrity: "sha256:" + good, ManifestHash: good},
		"uppercase integrity":   {Version: "1.0.0", Integrity: strings.ToUpper(good), ManifestHash: good},
		"traversal integrity":   {Version: "1.0.0", Integrity: "../../../../etc/passwd", ManifestHash: good},
		"no manifest hash":      {Version: "1.0.0", Integrity: good, ManifestHash: ""},
		"short manifest hash":   {Version: "1.0.0", Integrity: good, ManifestHash: "abc"},
		"non-hex manifest hash": {Version: "1.0.0", Integrity: good, ManifestHash: strings.Repeat("z", 64)},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePin(testArtifactName, entry); err == nil {
				t.Fatalf("ValidatePin(%+v) = nil, want an error", entry)
			}
			dir, _ := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
			if _, err := LoadArtifact(dir, testArtifactName, entry); err == nil {
				t.Fatal("LoadArtifact accepted an unusable pin")
			}
		})
	}
}

// The manifest hash is the binding between the lock and the file list. It is
// checked BEFORE the manifest is trusted to name a single path.
func TestLoadArtifact_RejectsManifestHashMismatch(t *testing.T) {
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	entry := testEntry(manifestHash)
	entry.ManifestHash = strings.Repeat("c", 64)

	_, err := LoadArtifact(dir, testArtifactName, entry)
	if err == nil || !strings.Contains(err.Error(), "manifest hash mismatch") {
		t.Fatalf("error = %v, want a manifest hash mismatch", err)
	}
}

// Manifest/archive drift in either direction is a hard failure: a declared file
// the archive does not carry, and a declared file whose bytes are not the ones
// the manifest names.
func TestLoadArtifact_RejectsArchiveDrift(t *testing.T) {
	t.Run("declared file missing from the archive", func(t *testing.T) {
		dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
		if err := os.Remove(filepath.Join(dir, ".agents/skills/fix/SKILL.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash)); err == nil {
			t.Fatal("expected a drift error for a missing declared file")
		}
	})

	t.Run("declared file with different bytes", func(t *testing.T) {
		dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
		if err := os.WriteFile(filepath.Join(dir, ".agents/skills/fix/SKILL.md"), []byte("swapped\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
		if err == nil || !strings.Contains(err.Error(), "drifted from its manifest") {
			t.Fatalf("error = %v, want a content drift error", err)
		}
	})
}

// Unsafe paths never reach the filesystem. The manifest parser rejects them and
// LoadArtifact surfaces the protocol's diagnostic codes, so the CLI and the
// protocol cannot disagree about what is safe.
func TestLoadArtifact_RejectsUnsafeManifests(t *testing.T) {
	digest := strings.Repeat("1", 64)
	cases := map[string]struct {
		document string
		want     string
	}{
		"absolute path": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":"/etc/passwd","sha256":"` + digest + `"}]}`,
			"invalid-path",
		},
		"parent traversal": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":"../AGENTS.md","sha256":"` + digest + `"}]}`,
			"invalid-path",
		},
		"inner traversal": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":".agents/../../x","sha256":"` + digest + `"}]}`,
			"invalid-path",
		},
		"backslash path": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":".agents\\skills\\x","sha256":"` + digest + `"}]}`,
			"invalid-path",
		},
		"duplicate path": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":".agents/x","sha256":"` + digest + `"},{"path":".agents/x","sha256":"` + strings.Repeat("2", 64) + `"}]}`,
			"duplicate-path",
		},
		"malformed digest": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":".agents/x","sha256":"nope"}]}`,
			"invalid-integrity",
		},
		"unknown field": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","mode":"copy","files":[{"path":".agents/x","sha256":"` + digest + `"}]}`,
			"parse-error",
		},
		"unsupported protocol version": {
			`{"protocolVersion":99,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":".agents/x","sha256":"` + digest + `"}]}`,
			"invalid-protocol-version",
		},
		"no files": {
			`{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[]}`,
			"required-field",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
			manifestHash := writeRawManifest(t, dir, tc.document)
			_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// A symlink inside the artifact tree is never followed. The store is shared by
// every repo on the machine, so following one would copy whatever it points at
// into the workspace.
func TestLoadArtifact_RejectsSymlinkedMember(t *testing.T) {
	requireSymlinks(t)
	files := defaultWorkflowFiles()
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", files)

	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte(files[".agents/skills/fix/SKILL.md"]), 0o600); err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(dir, ".agents/skills/fix/SKILL.md")
	if err := os.Remove(member); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, member); err != nil {
		t.Fatal(err)
	}

	// The link even resolves to byte-identical content, so only the SHAPE check
	// can catch it.
	_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want a symlink rejection", err)
	}
}

// Identity comes from the lock; a manifest that renames or re-versions itself
// is refused rather than believed.
func TestLoadArtifact_RejectsIdentityDrift(t *testing.T) {
	t.Run("name", func(t *testing.T) {
		dir, manifestHash := buildArtifactTree(t, "@putnami/other-workflows", "1.0.0", defaultWorkflowFiles())
		_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
		if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("error = %v, want an identity mismatch", err)
		}
	})

	t.Run("version", func(t *testing.T) {
		dir, manifestHash := buildArtifactTree(t, testArtifactName, "9.9.9", defaultWorkflowFiles())
		_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
		if err == nil || !strings.Contains(err.Error(), "version mismatch") {
			t.Fatalf("error = %v, want a version mismatch", err)
		}
	})
}

// The manifest describes the artifact; materializing it into the workspace
// would install a file nobody asked for at the workspace root.
func TestLoadArtifact_RejectsSelfReferentialManifest(t *testing.T) {
	dir, _ := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	document := `{"protocolVersion":1,"name":"` + testArtifactName + `","version":"1.0.0","files":[{"path":"` +
		wsproto.AgentArtifactManifestFilename + `","sha256":"` + strings.Repeat("1", 64) + `"}]}`
	manifestHash := writeRawManifest(t, dir, document)

	_, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err == nil || !strings.Contains(err.Error(), "never materialized") {
		t.Fatalf("error = %v, want a self-reference rejection", err)
	}
}

func TestLoadArtifact_RejectsEmptyDirectoryAndBadPin(t *testing.T) {
	if _, err := LoadArtifact("", testArtifactName, testEntry(strings.Repeat("a", 64))); err == nil {
		t.Fatal("expected an error for an empty directory")
	}
	if _, err := LoadArtifact(t.TempDir(), testArtifactName, lockfile.AgentArtifactLockEntry{}); err == nil {
		t.Fatal("expected an error for an invalid pin")
	}
	if _, err := LoadArtifact(t.TempDir(), testArtifactName, testEntry(strings.Repeat("a", 64))); err == nil {
		t.Fatal("expected an error for a directory with no manifest")
	}
}

// Read re-hashes on every read, so a tree mutated between verification and
// write — a GC eviction plus a re-admit, or a hand edit under $HOME — cannot
// slip bytes past the check LoadArtifact performed.
func TestArtifactRead_RehashesAndRefusesUndeclaredPaths(t *testing.T) {
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.Read(".agents/skills/fix/SKILL.md"); err != nil {
		t.Fatalf("verified read failed: %v", err)
	}
	if _, err := artifact.Read(".agents/skills/undeclared.md"); err == nil {
		t.Fatal("expected a rejection for an undeclared path")
	}

	if err := os.WriteFile(filepath.Join(dir, ".agents/skills/fix/SKILL.md"), []byte("swapped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = artifact.Read(".agents/skills/fix/SKILL.md")
	if err == nil || !strings.Contains(err.Error(), "changed under us") {
		t.Fatalf("error = %v, want a re-hash failure", err)
	}
}

func TestArtifact_DigestsCoverEveryDeclaredFile(t *testing.T) {
	files := defaultWorkflowFiles()
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", files)
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	digests := artifact.Digests()
	if len(digests) != len(files) {
		t.Fatalf("digests = %d entries, want %d", len(digests), len(files))
	}
	for path, content := range files {
		if digests[path] != lockfile.HashBytes([]byte(content)) {
			t.Errorf("%s digest = %q", path, digests[path])
		}
	}
	if !sortedFileSlice(artifact.Files) {
		t.Errorf("artifact files are not sorted: %+v", artifact.Files)
	}
}

func sortedFileSlice(files []File) bool {
	for i := 1; i < len(files); i++ {
		if files[i-1].Path > files[i].Path {
			return false
		}
	}
	return true
}
