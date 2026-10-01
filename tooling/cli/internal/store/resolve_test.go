package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workspace "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

func i64(n int64) *int64                 { return &n }
func dur(d time.Duration) *time.Duration { return &d }

func TestResolveMaxBytes_Precedence(t *testing.T) {
	t.Setenv(storeMaxBytesEnv, "")
	if got := ResolveMaxBytes(Config{}); got != defaultStoreMaxBytes {
		t.Errorf("default = %d, want %d", got, defaultStoreMaxBytes)
	}
	if got := ResolveMaxBytes(Config{MaxBytes: i64(123)}); got != 123 {
		t.Errorf("config = %d, want 123", got)
	}
	t.Setenv(storeMaxBytesEnv, "456")
	if got := ResolveMaxBytes(Config{MaxBytes: i64(123)}); got != 456 {
		t.Errorf("env should beat config: got %d, want 456", got)
	}
}

func TestResolveGCGrace_Precedence(t *testing.T) {
	t.Setenv(gcGraceEnv, "")
	if got := ResolveGCGrace(Config{}); got != defaultGCGrace {
		t.Errorf("default = %v, want %v", got, defaultGCGrace)
	}
	if got := ResolveGCGrace(Config{GCGrace: dur(30 * time.Minute)}); got != 30*time.Minute {
		t.Errorf("config = %v, want 30m", got)
	}
	if got := ResolveGCGrace(Config{GCGrace: dur(0)}); got != 0 {
		t.Errorf("config 0 (disable) should be honored, got %v", got)
	}
	t.Setenv(gcGraceEnv, "2h")
	if got := ResolveGCGrace(Config{GCGrace: dur(30 * time.Minute)}); got != 2*time.Hour {
		t.Errorf("env should beat config: got %v, want 2h", got)
	}
}

func TestResolveMaxIdleBuilds_Precedence(t *testing.T) {
	t.Setenv(maxIdleBuildsEnv, "")
	if got := ResolveMaxIdleBuilds(Config{}); got != defaultMaxIdleBuilds {
		t.Errorf("default = %d, want %d", got, defaultMaxIdleBuilds)
	}
	if got := ResolveMaxIdleBuilds(Config{MaxIdleBuilds: i64(7)}); got != 7 {
		t.Errorf("config = %d, want 7", got)
	}
	if got := ResolveMaxIdleBuilds(Config{MaxIdleBuilds: i64(0)}); got != 0 {
		t.Errorf("config 0 (disable) should be honored, got %d", got)
	}
	t.Setenv(maxIdleBuildsEnv, "9")
	if got := ResolveMaxIdleBuilds(Config{MaxIdleBuilds: i64(7)}); got != 9 {
		t.Errorf("env should beat config: got %d, want 9", got)
	}
}

func TestConfigFromWorkspace(t *testing.T) {
	if c := ConfigFromWorkspace(nil); c.MaxBytes != nil || c.GCGrace != nil || c.MaxIdleBuilds != nil {
		t.Error("nil workspace config should yield an all-unset Config")
	}
	wc := &workspace.Config{Store: &workspace.StoreConfig{MaxBytes: i64(5), GCGrace: "45m", MaxIdleBuilds: i64(3)}}
	c := ConfigFromWorkspace(wc)
	if c.MaxBytes == nil || *c.MaxBytes != 5 {
		t.Errorf("MaxBytes = %v, want 5", c.MaxBytes)
	}
	if c.GCGrace == nil || *c.GCGrace != 45*time.Minute {
		t.Errorf("GCGrace = %v, want 45m", c.GCGrace)
	}
	if c.MaxIdleBuilds == nil || *c.MaxIdleBuilds != 3 {
		t.Errorf("MaxIdleBuilds = %v, want 3", c.MaxIdleBuilds)
	}
	// An invalid duration string is treated as unset (falls back to env/default).
	if c2 := ConfigFromWorkspace(&workspace.Config{Store: &workspace.StoreConfig{GCGrace: "bogus"}}); c2.GCGrace != nil {
		t.Errorf("invalid GCGrace should be unset, got %v", c2.GCGrace)
	}
}

func TestResolveStoreRoot_EnvOverride(t *testing.T) {
	t.Setenv("PUTNAMI_STORE_DIR", "/custom/store")
	if got := ResolveStoreRoot("/anything"); got != "/custom/store" {
		t.Errorf("ResolveStoreRoot = %q, want /custom/store", got)
	}
}

func TestResolveStoreRoot_PerRepo(t *testing.T) {
	t.Setenv("PUTNAMI_STORE_DIR", "") // ensure no override
	home := hometest.Temp(t)

	repo := initStoreGitRepo(t)
	got := ResolveStoreRoot(repo)

	wantPrefix := filepath.Join(home, ".putnami", "store") + string(os.PathSeparator)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("ResolveStoreRoot = %q, want under %q", got, wantPrefix)
	}
	id := filepath.Base(got)
	if len(id) != repoIDLen {
		t.Errorf("repo-id %q length = %d, want %d", id, len(id), repoIDLen)
	}
}

func TestResolveStoreRoot_WorktreesShareStore(t *testing.T) {
	t.Setenv("PUTNAMI_STORE_DIR", "")
	hometest.Temp(t)

	repo := initStoreGitRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	run := exec.Command("git", "worktree", "add", "-b", "wt", wt)
	run.Dir = repo
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	// Every worktree of a repo must resolve to the same store so siblings share.
	if a, b := ResolveStoreRoot(repo), ResolveStoreRoot(wt); a != b {
		t.Errorf("worktrees resolved different stores:\n  main=%q\n  linked=%q", a, b)
	}
}

func TestResolveStoreRoot_NonGitFallback(t *testing.T) {
	t.Setenv("PUTNAMI_STORE_DIR", "")
	home := hometest.Temp(t)

	// A non-git workspace still gets a global location, keyed by a hash of the
	// workspace root, so two distinct non-git checkouts get distinct stores.
	wsA := t.TempDir()
	wsB := t.TempDir()
	gotA := ResolveStoreRoot(wsA)
	gotB := ResolveStoreRoot(wsB)

	wantPrefix := filepath.Join(home, ".putnami", "store") + string(os.PathSeparator)
	if !strings.HasPrefix(gotA, wantPrefix) {
		t.Errorf("fallback %q not under %q", gotA, wantPrefix)
	}
	if gotA == gotB {
		t.Error("distinct non-git workspaces should resolve to distinct stores")
	}
	// Deterministic for the same workspace.
	if gotA != ResolveStoreRoot(wsA) {
		t.Error("ResolveStoreRoot not deterministic for the same workspace")
	}
}

func TestResolveScratchRoot_NeverTheStore(t *testing.T) {
	ws := "/work/space"
	scratch := ResolveScratchRoot(ws)
	if scratch != filepath.Join(ws, ".putnami", "cache") {
		t.Errorf("ResolveScratchRoot = %q, want %q", scratch, filepath.Join(ws, ".putnami", "cache"))
	}
	// The split is the whole point: scratch must never equal the CAS root.
	t.Setenv("PUTNAMI_STORE_DIR", "")
	hometest.Temp(t)
	if scratch == ResolveStoreRoot(ws) {
		t.Error("scratch root must differ from the CAS store root")
	}
}

func TestResolveArtifactStoreRoot_EnvOverride(t *testing.T) {
	t.Setenv(artifactDirEnv, "/custom/artifacts")
	if got := ResolveArtifactStoreRoot("/anything"); got != "/custom/artifacts" {
		t.Errorf("ResolveArtifactStoreRoot = %q, want /custom/artifacts", got)
	}
}

func TestResolveArtifactStoreRoot_FlatUnderHome(t *testing.T) {
	t.Setenv(artifactDirEnv, "")
	home := hometest.Temp(t)

	want := filepath.Join(home, ".putnami", "artifacts")
	if got := ResolveArtifactStoreRoot("/some/workspace"); got != want {
		t.Errorf("ResolveArtifactStoreRoot = %q, want %q (flat, no repo-id)", got, want)
	}
	// Flat means workspace-independent: a different workspace resolves identically.
	if a, b := ResolveArtifactStoreRoot("/ws/a"), ResolveArtifactStoreRoot("/ws/b"); a != b {
		t.Errorf("flat artifact root must be workspace-independent: %q != %q", a, b)
	}
}

func TestResolveArtifactStoreRoot_SharedAcrossRepos(t *testing.T) {
	t.Setenv(artifactDirEnv, "")
	t.Setenv("PUTNAMI_STORE_DIR", "")
	hometest.Temp(t)

	repoA := initStoreGitRepo(t)
	repoB := initStoreGitRepo(t)

	// The whole point of "flat": distinct repos share ONE artifact store...
	if a, b := ResolveArtifactStoreRoot(repoA), ResolveArtifactStoreRoot(repoB); a != b {
		t.Errorf("distinct repos must share one artifact store: %q != %q", a, b)
	}
	// ...whereas the build store is deliberately per-repo. Asserting the contrast
	// proves the artifact store does NOT inherit the repo-id sub-level.
	if a, b := ResolveStoreRoot(repoA), ResolveStoreRoot(repoB); a == b {
		t.Errorf("build store should differ per repo, both = %q", a)
	}
}

func TestResolveArtifactStoreRoot_HomeUnavailableFallback(t *testing.T) {
	t.Setenv(artifactDirEnv, "")
	hometest.Set(t, "")

	ws := "/work/space"
	want := filepath.Join(ws, ".putnami", "artifacts")
	if got := ResolveArtifactStoreRoot(ws); got != want {
		t.Errorf("HOME-unavailable fallback = %q, want %q", got, want)
	}
	// With no HOME and no override, GC cannot resolve a global root on its own.
	if _, ok := GlobalArtifactRoot(); ok {
		t.Error("GlobalArtifactRoot should be unresolved with no HOME and no override")
	}
}

func TestArtifactRootsForGC(t *testing.T) {
	// Override → exactly one root (never a per-repo enumeration).
	t.Setenv(artifactDirEnv, "/custom/artifacts")
	if got := ArtifactRootsForGC(); len(got) != 1 || got[0] != "/custom/artifacts" {
		t.Errorf("ArtifactRootsForGC under override = %v, want [/custom/artifacts]", got)
	}
	if got := ArtifactRootsForGCIncluding("/ws"); len(got) != 1 || got[0] != "/custom/artifacts" {
		t.Errorf("ArtifactRootsForGCIncluding under override = %v, want [/custom/artifacts]", got)
	}

	// Under HOME → single flat root; Including a workspace that resolves to the
	// same flat root adds nothing (dedup).
	t.Setenv(artifactDirEnv, "")
	home := hometest.Temp(t)
	flat := filepath.Join(home, ".putnami", "artifacts")
	if got := ArtifactRootsForGC(); len(got) != 1 || got[0] != flat {
		t.Errorf("ArtifactRootsForGC under HOME = %v, want [%s]", got, flat)
	}
	if got := ArtifactRootsForGCIncluding("/some/ws"); len(got) != 1 || got[0] != flat {
		t.Errorf("ArtifactRootsForGCIncluding (flat resolves to global) = %v, want [%s]", got, flat)
	}
}

func TestResolveArtifactStoreRoot_DistinctFromStoreAndScratch(t *testing.T) {
	t.Setenv(artifactDirEnv, "")
	t.Setenv("PUTNAMI_STORE_DIR", "")
	hometest.Temp(t)

	ws := initStoreGitRepo(t)
	artifacts := ResolveArtifactStoreRoot(ws)
	if artifacts == ResolveStoreRoot(ws) {
		t.Error("artifact store root must differ from the build store root")
	}
	if artifacts == ResolveScratchRoot(ws) {
		t.Error("artifact store root must differ from the per-worktree scratch root")
	}
}

// initStoreGitRepo creates a throwaway git repo for resolver tests.
func initStoreGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@t.com"},
		{"config", "user.name", "T"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-b", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}
