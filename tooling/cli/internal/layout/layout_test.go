package layout

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

func TestEncodeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"@putnami/go", "putnami-go"},
		{"@putnami/typescript", "putnami-typescript"},
		{"@putnami/python", "putnami-python"},
		{"simple-ext", "simple-ext"},
		{"@scope/name", "scope-name"},
	}
	for _, tt := range tests {
		got := EncodeName(tt.input)
		if got != tt.want {
			t.Errorf("EncodeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestArtifactDir(t *testing.T) {
	got := ArtifactDir("/ws", Extensions, "@putnami/go", "1.0.0")
	want := filepath.Join("/ws", ".putnami", "bin", "artifacts", "extensions", "putnami-go@1.0.0")
	if got != want {
		t.Errorf("ArtifactDir = %q, want %q", got, want)
	}
}

func TestStableDir(t *testing.T) {
	got := StableDir("/ws", Templates, "go-server")
	want := filepath.Join("/ws", ".putnami", "bin", "templates", "go-server")
	if got != want {
		t.Errorf("StableDir = %q, want %q", got, want)
	}
}

func TestAgentArtifactRootsAreIsolated(t *testing.T) {
	stable := StableDir("/ws", AgentArtifacts, "@putnami/agent-workflows")
	wantStable := filepath.Join("/ws", ".putnami", "bin", "agent-artifacts", "putnami-agent-workflows")
	if stable != wantStable {
		t.Errorf("StableDir(agent artifact) = %q, want %q", stable, wantStable)
	}

	artifact := ArtifactDir("/ws", AgentArtifacts, "@putnami/agent-workflows", "1.0.0")
	wantArtifact := filepath.Join("/ws", ".putnami", "bin", "artifacts", "agent-artifacts", "putnami-agent-workflows@1.0.0")
	if artifact != wantArtifact {
		t.Errorf("ArtifactDir(agent artifact) = %q, want %q", artifact, wantArtifact)
	}
	if stable == StableDir("/ws", Templates, "@putnami/agent-workflows") || artifact == ArtifactDir("/ws", Templates, "@putnami/agent-workflows", "1.0.0") {
		t.Fatal("agent artifacts must not share template roots")
	}
}

func TestLinkArtifact(t *testing.T) {
	wsRoot := t.TempDir()

	// Create the artifact directory
	artifactDir := ArtifactDir(wsRoot, Extensions, "@putnami/go", "1.0.0")
	os.MkdirAll(artifactDir, 0o755)
	os.WriteFile(filepath.Join(artifactDir, "manifest.json"), []byte("{}"), 0o644)

	// Create symlink
	if err := LinkArtifact(wsRoot, Extensions, "@putnami/go", "1.0.0"); err != nil {
		t.Fatalf("LinkArtifact: %v", err)
	}

	// Verify symlink exists and resolves
	stableDir := StableDir(wsRoot, Extensions, "@putnami/go")
	info, err := os.Lstat(stableDir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !dirlink.IsLink(stableDir, info) {
		t.Error("expected symlink")
	}

	// Verify file accessible through symlink
	data, err := os.ReadFile(filepath.Join(stableDir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadFile through symlink: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("unexpected content: %s", data)
	}
}

func TestLinkArtifactAtomicSwap(t *testing.T) {
	wsRoot := t.TempDir()

	// Create two versions
	v1 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "1.0.0")
	v2 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "2.0.0")
	os.MkdirAll(v1, 0o755)
	os.MkdirAll(v2, 0o755)
	os.WriteFile(filepath.Join(v1, "version"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(v2, "version"), []byte("2"), 0o644)

	// Link to v1
	if err := LinkArtifact(wsRoot, Extensions, "@putnami/go", "1.0.0"); err != nil {
		t.Fatalf("LinkArtifact v1: %v", err)
	}

	// Swap to v2
	if err := LinkArtifact(wsRoot, Extensions, "@putnami/go", "2.0.0"); err != nil {
		t.Fatalf("LinkArtifact v2: %v", err)
	}

	// Verify it points to v2
	stableDir := StableDir(wsRoot, Extensions, "@putnami/go")
	data, err := os.ReadFile(filepath.Join(stableDir, "version"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "2" {
		t.Errorf("expected version 2, got %s", data)
	}
}

func TestRemoveArtifact(t *testing.T) {
	wsRoot := t.TempDir()

	// Create artifact and symlink
	artifactDir := ArtifactDir(wsRoot, Extensions, "@putnami/go", "1.0.0")
	os.MkdirAll(artifactDir, 0o755)
	LinkArtifact(wsRoot, Extensions, "@putnami/go", "1.0.0")

	// Remove specific version
	if err := RemoveArtifact(wsRoot, Extensions, "@putnami/go", "1.0.0"); err != nil {
		t.Fatalf("RemoveArtifact: %v", err)
	}

	if _, err := os.Stat(artifactDir); !os.IsNotExist(err) {
		t.Error("artifact dir should have been removed")
	}
	if _, err := os.Lstat(StableDir(wsRoot, Extensions, "@putnami/go")); !os.IsNotExist(err) {
		t.Error("symlink should have been removed")
	}
}

func TestRemoveArtifactAllVersions(t *testing.T) {
	wsRoot := t.TempDir()

	// Create two versions
	v1 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "1.0.0")
	v2 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "2.0.0")
	os.MkdirAll(v1, 0o755)
	os.MkdirAll(v2, 0o755)
	LinkArtifact(wsRoot, Extensions, "@putnami/go", "2.0.0")

	// Remove all versions
	if err := RemoveArtifact(wsRoot, Extensions, "@putnami/go", ""); err != nil {
		t.Fatalf("RemoveArtifact: %v", err)
	}

	if _, err := os.Stat(v1); !os.IsNotExist(err) {
		t.Error("v1 should have been removed")
	}
	if _, err := os.Stat(v2); !os.IsNotExist(err) {
		t.Error("v2 should have been removed")
	}
}

func TestListVersions(t *testing.T) {
	wsRoot := t.TempDir()

	v1 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "1.0.0")
	v2 := ArtifactDir(wsRoot, Extensions, "@putnami/go", "2.0.0")
	os.MkdirAll(v1, 0o755)
	os.MkdirAll(v2, 0o755)

	versions, err := ListVersions(wsRoot, Extensions, "@putnami/go")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
}
