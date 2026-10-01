package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// toolchainReportWorkspace writes a workspace whose extension runs every task
// with a required toolchain, "compiler", that the lock pins at 1.2.3 for this
// host, while the only compiler on PATH is 0.9.0. When usedByItself is true the
// extension project uses its own extension, so a workspace-install run plans
// one job; otherwise no project uses it and the run plans none.
func toolchainReportWorkspace(t *testing.T, usedByItself bool) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	uses := ""
	if usedByItself {
		uses = `,"extensions":["/extension"]`
	}
	write(filepath.Join(root, "putnami.workspace.json"), `{"name":"toolchain-report","includes":["app","extension"]}`)
	write(filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	write(filepath.Join(root, "extension", "putnami.json"), `{"name":"@putnami/toolchain-report"`+uses+`}`)
	write(filepath.Join(root, "extension", "putnami.extension.json"), `{
		"name": "@putnami/toolchain-report",
		"version": "1.0.0",
		"cliContract": `+fmt.Sprint(protocolcli.CurrentContract)+`,
		"runtime": {
			"executable": "compiled/missing",
			"toolchains": {
				"compiler": {
					"lock": "compiler",
					"candidates": [{"from": "path", "path": "compiler"}],
					"probe": {"args": ["--version"], "expect": "{version}"}
				}
			},
			"runToolchains": ["compiler"]
		},
		"commands": {
			"workspace-install": {"run": [{"id": "install", "task": "workspace-install-exec"}]}
		},
		"tasks": {
			"workspace-install-exec": {
				"kind": "command",
				"command": `+taskCommand(t, fixtureproc.Program{})+`,
				"cache": false
			}
		}
	}`)
	locked := lockfile.NewLockFile()
	locked.SetToolchain("compiler", lockfile.LockEntry{
		Version:     "1.2.3",
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): "integrity-a"},
	})
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fixtureproc.Write(t, filepath.Join(bin, "compiler"), fixtureproc.Program{Stdout: "0.9.0\n"})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	workspace.InvalidateLoadCache(root)
	return root
}

// A workspace-install run logs a missing pinned toolchain at debug level when
// it starts, since installing it can be what the run does. However the run
// ends, it then warns once about a toolchain that is still missing, including
// a run that plans no job and so returns before any job executes.
func TestLifecycleRunWarnsOnceAboutAStillMissingToolchain(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioning-reports-the-installed-toolchain",
		"a-run-that-plans-no-job-keeps-the-warning")
	for _, tc := range []struct {
		name         string
		usedByItself bool
		jobs         int
	}{
		{name: "no job planned", usedByItself: false, jobs: 0},
		{name: "one job planned", usedByItself: true, jobs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := toolchainReportWorkspace(t, tc.usedByItself)
			t.Setenv("PUTNAMI_HOME", t.TempDir())
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			var result SessionResult
			var err error
			stderr := captureStderr(t, func() {
				result, err = New().Run(context.Background(), Request{
					WorkspaceRoot:      root,
					Config:             &wsproto.Config{},
					Commands:           []string{"workspace-install"},
					WorkspaceLifecycle: true,
					Global:             GlobalFlags{Projects: "*"},
					Stdout:             io.Discard,
				}, nil)
			})
			if err != nil || result.ExitCode != ExitSuccess {
				t.Fatalf("workspace-install: exit %d, %v\n%s", result.ExitCode, err, stderr)
			}
			if len(result.Plan) != tc.jobs {
				t.Fatalf("plan = %d jobs, want %d", len(result.Plan), tc.jobs)
			}
			var warnings []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "still unavailable after workspace-install") {
					warnings = append(warnings, line)
				}
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "level=WARN") ||
				!strings.Contains(warnings[0], "toolchain=compiler") {
				t.Fatalf("toolchain warnings = %q, want one warning for the still-missing compiler\nlogs:\n%s",
					warnings, logs.String())
			}
		})
	}
}
