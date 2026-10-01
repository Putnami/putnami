package agentartifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const testArtifactName = "@putnami/agent-workflows"

// defaultWorkflowFiles is the artifact shape the issue fixes: the canonical
// skills, their referenced scripts and references, the openai metadata, and the
// thin Claude adapters that point back at the canonical skills.
func defaultWorkflowFiles() map[string]string {
	return map[string]string{
		".agents/skills/fix/SKILL.md":                "# fix\ncanonical fix workflow\n",
		".agents/skills/fix/references/fix-heavy.md": "# fix-heavy\nworker instructions\n",
		".agents/skills/fix/scripts/finalize-pr.sh":  "#!/usr/bin/env bash\nset -euo pipefail\n",
		".agents/skills/agents/openai.yaml":          "name: fix\nmodel: gpt\n",
		".claude/skills/fix/SKILL.md":                "See ../../../.agents/skills/fix/SKILL.md\n",
	}
}

// buildArtifactTree writes an extracted artifact tree (files plus a canonical
// manifest) and returns its directory and manifest hash.
func buildArtifactTree(t *testing.T, name, version string, files map[string]string) (dir, manifestHash string) {
	t.Helper()
	dir = t.TempDir()
	manifest := &wsproto.AgentArtifactManifest{
		ProtocolVersion: wsproto.AgentArtifactManifestProtocolVersion,
		Name:            name,
		Version:         version,
	}
	for _, path := range sortedKeys(files) {
		content := files[path]
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, wsproto.AgentArtifactFile{
			Path:   path,
			SHA256: lockfile.HashBytes([]byte(content)),
		})
	}
	data, err := wsproto.MarshalAgentArtifactManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsproto.AgentArtifactManifestFilename), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, lockfile.HashBytes(data)
}

// writeRawManifest replaces an artifact tree's manifest with arbitrary bytes
// and returns their hash, so a test can pin what happens when the manifest
// itself is malformed, mis-identified, or drifted from the archive.
func writeRawManifest(t *testing.T, dir, document string) string {
	t.Helper()
	path := filepath.Join(dir, wsproto.AgentArtifactManifestFilename)
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return lockfile.HashBytes([]byte(document))
}

// testEntry is a well-formed pin for a tree whose manifest hashes to
// manifestHash. The identity digest is arbitrary but correctly SHAPED: these
// unit tests exercise the ownership rules, not how a pin is resolved.
func testEntry(manifestHash string) lockfile.AgentArtifactLockEntry {
	return lockfile.AgentArtifactLockEntry{
		Version:      "1.0.0",
		Integrity:    strings.Repeat("a", 64),
		ManifestHash: manifestHash,
	}
}

// fixture is a workspace plus a prepared, pinned content tree. It drives the
// materializer's phases in the order the lifecycle runs them — verify, read
// the ownership record, plan, apply, record — so the ownership rules are
// exercised without any resolution in front of them.
type fixture struct {
	root  string
	dir   string
	entry lockfile.AgentArtifactLockEntry
}

func newFixture(t *testing.T, version string, files map[string]string) *fixture {
	t.Helper()
	root := t.TempDir()
	dir, manifestHash := buildArtifactTree(t, testArtifactName, version, files)
	return &fixture{
		root: root,
		dir:  dir,
		entry: lockfile.AgentArtifactLockEntry{
			Version:      version,
			Integrity:    strings.Repeat("a", 64),
			ManifestHash: manifestHash,
		},
	}
}

// upgrade re-points the fixture at a new content version.
func (f *fixture) upgrade(t *testing.T, version string, files map[string]string) {
	t.Helper()
	dir, manifestHash := buildArtifactTree(t, testArtifactName, version, files)
	f.dir = dir
	f.entry = lockfile.AgentArtifactLockEntry{
		Version:      version,
		Integrity:    strings.Repeat("b", 64),
		ManifestHash: manifestHash,
	}
}

// preflight is everything known before a write: the verified content and the
// complete plan against the ownership record.
func (f *fixture) preflight(t *testing.T) (*Artifact, *Plan, error) {
	t.Helper()
	state, err := LoadState(f.root, testArtifactName)
	if err != nil {
		return nil, nil, err
	}
	artifact, err := LoadArtifact(f.dir, testArtifactName, f.entry)
	if err != nil {
		return nil, nil, err
	}
	plan, err := BuildPlan(f.root, artifact, state)
	if err != nil {
		return nil, nil, err
	}
	return artifact, plan, nil
}

// materialize runs the full sequence. A collision returns its report with an
// error wrapping ErrCollision and writes nothing, including the record, which
// is committed last and only on success.
func (f *fixture) materialize(t *testing.T) (*Report, error) {
	t.Helper()
	artifact, plan, err := f.preflight(t)
	if err != nil {
		return nil, err
	}
	if plan.HasCollisions() {
		report := NewReport(plan, false)
		return report, fmt.Errorf("%w (%d preserved)", ErrCollision, len(report.Collided))
	}
	if err := Apply(f.root, artifact, plan); err != nil {
		return nil, err
	}
	if err := WriteState(f.root, StateFor(artifact)); err != nil {
		return nil, err
	}
	return NewReport(plan, true), nil
}

// mustMaterialize fails the test unless the run applied cleanly.
func (f *fixture) mustMaterialize(t *testing.T) *Report {
	t.Helper()
	report, err := f.materialize(t)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return report
}

func (f *fixture) write(t *testing.T, path, content string) {
	t.Helper()
	writeWorkspaceFile(t, f.root, path, content)
}

func (f *fixture) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeWorkspaceFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree records every path under root outside .putnami/ with its content
// digest (or its link target / mode for anything that is not a regular file),
// so a test can assert that a run mutated NOTHING. .putnami/ is excluded
// because it is the materializer's own gitignored bookkeeping, not a target.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		if slashed == ".putnami" || strings.HasPrefix(slashed, ".putnami/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			out[slashed] = "symlink:" + target
		case info.IsDir():
			out[slashed] = "dir"
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			out[slashed] = "file:" + lockfile.HashBytes(data)
		default:
			out[slashed] = "other:" + info.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func assertSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared", path)
			continue
		}
		if got != want {
			t.Errorf("%s changed: %s -> %s", path, want, got)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s appeared", path)
		}
	}
}

func collisionReason(report *Report, path string) string {
	for _, collision := range report.Collided {
		if collision.Path == path {
			return collision.Reason
		}
	}
	return ""
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
