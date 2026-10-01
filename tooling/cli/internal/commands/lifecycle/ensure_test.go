package lifecycle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func TestIsRegistryArtifactRef(t *testing.T) {
	cases := map[string]bool{
		"@putnami/go":   true,
		"@scope/x":      true,
		"/go/extension": false,
		"./local":       false,
		"../up":         false,
		"bare":          false,
	}
	for name, want := range cases {
		if got := shared.IsRegistryArtifactRef(name); got != want {
			t.Errorf("shared.IsRegistryArtifactRef(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestEnsureArtifacts_NoopGuards(t *testing.T) {
	// Empty workspace root and nil config are no-ops (no panic, no error).
	if err := EnsureArtifacts(context.Background(), "", nil); err != nil {
		t.Errorf("empty wsRoot should be a no-op: %v", err)
	}
	if err := EnsureArtifacts(context.Background(), t.TempDir(), nil); err != nil {
		t.Errorf("nil cfg should be a no-op: %v", err)
	}
}

func TestEnsureArtifacts_SkipsUnlockedArtifact(t *testing.T) {
	// A registry extension declared in config but absent from the lock must NOT
	// be fetched implicitly (that would re-attempt a download on every command).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unlocked artifact must not be fetched implicitly")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)
	t.Setenv(artifactsEnsuredEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	ws := t.TempDir() // no putnami.lock.json → no lock entry for the extension
	cfg := &wsproto.Config{}
	cfg.Extensions.List = map[string]string{"@putnami/unlocked": "latest"}

	if err := EnsureArtifacts(context.Background(), ws, cfg); err != nil {
		t.Errorf("unlocked artifact should be skipped, got: %v", err)
	}
}

func TestEnsureArtifacts_MaterializesLockedBareTemplate(t *testing.T) {
	t.Setenv(artifactsEnsuredEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	const manifest = `{"name":"alpha","version":"1.0.0","description":"Alpha template","extension":"@putnami/go"}`
	archive := buildManifestArchive(t, "putnami.template.json", manifest)
	digest := sha256Hex(archive)
	manifestHash := sha256Hex([]byte(manifest))

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-Resolved-Version", "1.0.0")
		w.Write(archive)
	}))
	defer srv.Close()
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)

	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetTemplate("alpha", lockfile.LockEntry{
		Version:      "1.0.0",
		ManifestHash: manifestHash,
		Integrities:  map[string]string{lockfile.PlatformKey("linux", "x64"): digest},
	})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	cfg := &wsproto.Config{Templates: []string{"alpha"}}
	if err := EnsureArtifacts(context.Background(), ws, cfg); err != nil {
		t.Fatalf("ensure artifacts: %v", err)
	}
	if hits == 0 {
		t.Fatal("locked bare template should be fetched and materialized")
	}
	if _, err := os.Stat(layout.StableDir(ws, layout.Templates, "alpha")); err != nil {
		t.Fatalf("template stable link should resolve: %v", err)
	}
}

// TestRunWorkspaceJob_MaterializesArtifactsFirst verifies the B1 fix: a
// structured workspace job (deps install, upgrade) materializes lock-pinned
// artifacts BEFORE the run discovers extensions, so a fresh zero-init worktree
// resolves its providers without an explicit `putnami install`. EnsureArtifacts
// stamps the dedup env on entry, so observing that stamp FROM INSIDE the runner
// proves it ran first — and it keeps proving it now that discovery happens
// behind the injected engine adapter (slice A5a) rather than inline.
func TestRunWorkspaceJob_MaterializesArtifactsFirst(t *testing.T) {
	ws := t.TempDir()
	t.Setenv(artifactsEnsuredEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	cfg := &wsproto.Config{} // no extensions → EnsureArtifacts is a quick no-op pass

	var stampAtRun string
	env := LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		stampAtRun = os.Getenv(artifactsEnsuredEnv)
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}

	if _, err := runWorkspaceJob(context.Background(), env, WorkspaceJobRequest{
		WorkspaceRoot: ws,
		Config:        cfg,
		Job:           "workspace-install",
	}); err != nil {
		t.Fatalf("runWorkspaceJob: %v", err)
	}

	if stampAtRun != ws {
		t.Errorf("runWorkspaceJob must call EnsureArtifacts before the run; env at run = %q, want %q", stampAtRun, ws)
	}
}

// A --no-cache typed on the lifecycle command reaches every job it runs, so
// install, upgrade, and sync cannot each forget to copy it.
func TestRunWorkspaceJob_ForwardsTheEnvNoCache(t *testing.T) {
	t.Setenv(artifactsEnsuredEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	for _, noCache := range []bool{false, true} {
		var got bool
		env := LifecycleEnv{NoCache: noCache, RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
			got = req.NoCache
			return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
		}}
		if _, err := runWorkspaceJob(context.Background(), env, WorkspaceJobRequest{
			WorkspaceRoot: t.TempDir(),
			Config:        &wsproto.Config{},
			Job:           "workspace-install",
		}); err != nil {
			t.Fatalf("runWorkspaceJob: %v", err)
		}
		if got != noCache {
			t.Errorf("env.NoCache = %v: request NoCache = %v, want %v", noCache, got, noCache)
		}
	}
}

// A lifecycle command whose runner was never wired must FAIL, not report a
// silent no-op: "nothing to install" and "nobody could install" look identical
// to a caller otherwise, and the second one bricks a fresh checkout quietly.
func TestRunWorkspaceJob_NilRunnerFailsLoudly(t *testing.T) {
	ws := t.TempDir()
	t.Setenv(artifactsEnsuredEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())

	result, err := runWorkspaceJob(context.Background(), LifecycleEnv{}, WorkspaceJobRequest{
		WorkspaceRoot: ws,
		Config:        &wsproto.Config{},
		Job:           "workspace-install",
	})
	if err == nil {
		t.Fatal("an unwired workspace job runner must return an error")
	}
	if result.Outcome != WorkspaceJobFailed {
		t.Errorf("outcome = %v, want WorkspaceJobFailed", result.Outcome)
	}
}

func TestEnsureArtifacts_EnvGuardSkips(t *testing.T) {
	ws := t.TempDir()
	t.Setenv(artifactsEnsuredEnv, ws)

	// Even with a registry extension configured (which would otherwise try to
	// contact a registry and fail), the parent-ensured env guard short-circuits
	// to a no-op.
	cfg := &wsproto.Config{}
	cfg.Extensions.List = map[string]string{"@putnami/never": "1.0.0"}

	if err := EnsureArtifacts(context.Background(), ws, cfg); err != nil {
		t.Errorf("env-guarded ensure should be a no-op: %v", err)
	}
}

// buildManifestArchive returns a tar.gz holding one manifest file, the minimal
// artifact archive an installer accepts.
func buildManifestArchive(t *testing.T, filename, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     filename,
		Mode:     0o644,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
