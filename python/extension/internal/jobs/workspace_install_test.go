package jobs

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestWorkspaceInstall_SkipWhenNoMembers(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "workspace-scope", "a-workspace-with-no-python-members-reports-skipped")
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": []}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})

	// Should have emitted phase start + phase end (skipped)
	if len(events) < 2 {
		t.Errorf("expected at least 2 events (phase start/end), got %d", len(events))
	}
}

func TestWorkspaceInstall_SkipWhenLockFresh(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg/pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	// Pre-create root pyproject.toml so SyncUVWorkspace reports updated=false
	os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte(`[tool.uv.workspace]
members = ["pkg"]
`), 0644)

	// Create existing uv.lock so the skip-when-fresh path is taken
	os.WriteFile(filepath.Join(tmp, "uv.lock"), []byte("# lock"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Either OK (uv.lock fresh, uv found) or FAILED (uv not in PATH)
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}

func TestWorkspaceInstall_ForceFlagOverridesSkip(t *testing.T) {
	ctx := &pctx.Context{
		WorkspaceRoot: t.TempDir(),
		Params:        pctx.Params{},
	}
	os.WriteFile(filepath.Join(ctx.WorkspaceRoot, ".putnamirc.json"), []byte(`{"projects": []}`), 0644)

	// With no members, it returns SKIP regardless of force
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, []string{"--force"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP (no members), got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}

func TestWorkspaceInstall_NoWorkspaceConfig(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "workspace-scope", "a-missing-workspace-config-is-reported-not-invented")
	// Workspace with no config file — no members discovered → SKIP
	tmp := t.TempDir()

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})

	if len(events) < 2 {
		t.Errorf("expected at least 2 events (phase start/end), got %d", len(events))
	}
}

func TestWorkspaceInstall_UpdatedWorkspace(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg/pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	// No pre-existing root pyproject.toml, so SyncUVWorkspace will create it (updated=true)
	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := WorkspaceInstall(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Either OK (uv lock succeeded) or FAILED (uv not on PATH or lock fails)
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}

	// Verify the root pyproject.toml was created
	if !FileExists(filepath.Join(tmp, "pyproject.toml")) {
		t.Error("expected root pyproject.toml to be created")
	}
}

func TestWorkspaceInstall_ForceWithMembers(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, ".putnamirc.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg/pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	// Pre-create pyproject.toml and uv.lock (normally would skip lock)
	os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte("[tool.uv.workspace]\nmembers = [\"pkg\"]\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "uv.lock"), []byte("# lock"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Params:        pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		// With --force, should proceed to lock phase even when uv.lock exists
		status, _, err := WorkspaceInstall(ctx, emit, []string{"--force"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Either OK (lock succeeded) or FAILED (uv not available or lock error)
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}
