package launch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestEnterAgentWorkspaceMovesBareMCPBeforePinResolution(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "launcher-enters-the-host-workspace-before-pin-resolution")
	workspace := t.TempDir()
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	var entered string
	unset := false
	err := enterAgentWorkspace(
		[]string{"mcp"},
		func(name string) string {
			if name == AgentWorkspaceEnv {
				return project
			}
			return ""
		},
		func(name string) error {
			if name != AgentWorkspaceEnv {
				t.Fatalf("unset %q, want %q", name, AgentWorkspaceEnv)
			}
			unset = true
			return nil
		},
		func(path string) error {
			entered = path
			return nil
		},
	)
	if err != nil {
		t.Fatalf("enterAgentWorkspace: %v", err)
	}
	want, _ := filepath.Abs(project)
	if entered != want {
		t.Fatalf("entered %q, want %q", entered, want)
	}
	if !unset {
		t.Fatal("agent workspace environment was not consumed")
	}
}

func TestEnterAgentWorkspaceUsesClaudeSessionRootForPreservedHostDefinition(t *testing.T) {
	project := t.TempDir()
	var entered string
	err := enterAgentWorkspace(
		[]string{"mcp"},
		func(name string) string {
			if name == claudeProjectDirEnv {
				return project
			}
			return ""
		},
		func(string) error {
			t.Fatal("direct Claude environment must remain available to the server")
			return nil
		},
		func(path string) error { entered = path; return nil },
	)
	if err != nil {
		t.Fatalf("enterAgentWorkspace: %v", err)
	}
	if entered != project {
		t.Fatalf("entered %q, want %q", entered, project)
	}
}

func TestEnterAgentWorkspaceLeavesNonHostInvocationsAlone(t *testing.T) {
	for _, args := range [][]string{{"mcp", "install"}, {"build"}, nil} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := false
			err := enterAgentWorkspace(
				args,
				func(string) string { return "/a/session/worktree" },
				func(string) error { called = true; return nil },
				func(string) error { called = true; return nil },
			)
			if err != nil {
				t.Fatalf("enterAgentWorkspace: %v", err)
			}
			if called {
				t.Fatalf("invocation %v consumed the host workspace", args)
			}
		})
	}
}

func TestEnterAgentWorkspaceRejectsUnexpandedOrInvalidRoots(t *testing.T) {
	// A missing directory is matched by its error, not its text, which
	// differs by OS ("no such file" on Unix, "cannot find the file" on
	// Windows).
	tests := []struct {
		name    string
		root    string
		want    string
		wantErr error
	}{
		{"unexpanded Claude variable", "${CLAUDE_PROJECT_DIR:-.}", "was not expanded", nil},
		{"missing directory", filepath.Join(t.TempDir(), "missing"), "resolve agent workspace", fs.ErrNotExist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			err := enterAgentWorkspace(
				[]string{"mcp"},
				func(string) string { return tc.root },
				func(string) error { called = true; return nil },
				func(string) error { called = true; return nil },
			)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want text %q", err, tc.want)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if called {
				t.Fatal("invalid host root changed process state")
			}
		})
	}
}

func TestEnterAgentWorkspaceFailsIfMoveOrConsumeFails(t *testing.T) {
	dir := t.TempDir()
	t.Run("chdir", func(t *testing.T) {
		moveErr := errors.New("move refused")
		err := enterAgentWorkspace(
			[]string{"mcp"},
			func(string) string { return dir },
			func(string) error { t.Fatal("unset called after failed move"); return nil },
			func(string) error { return moveErr },
		)
		if !errors.Is(err, moveErr) {
			t.Fatalf("error = %v, want move failure", err)
		}
	})

	t.Run("unset", func(t *testing.T) {
		unsetErr := errors.New("unset refused")
		err := enterAgentWorkspace(
			[]string{"mcp"},
			func(name string) string {
				if name == AgentWorkspaceEnv {
					return dir
				}
				return ""
			},
			func(string) error { return unsetErr },
			func(string) error { return nil },
		)
		if !errors.Is(err, unsetErr) {
			t.Fatalf("error = %v, want unset failure", err)
		}
	})
}

func TestEnterAgentWorkspaceConsumesTheConfiguredVariableEvenWhenClaudeWins(t *testing.T) {
	// Both are set on every Claude session that uses the .mcp.json entry
	// `putnami mcp install` writes. The direct value decides the directory; the
	// configured one must still be removed so a nested Putnami process cannot
	// reinterpret it.
	claudeRoot := t.TempDir()
	configuredRoot := t.TempDir()
	var entered string
	unset := false
	err := enterAgentWorkspace(
		[]string{"mcp"},
		func(name string) string {
			switch name {
			case claudeProjectDirEnv:
				return claudeRoot
			case AgentWorkspaceEnv:
				return configuredRoot
			}
			return ""
		},
		func(name string) error {
			if name != AgentWorkspaceEnv {
				t.Fatalf("unset %q, want %q", name, AgentWorkspaceEnv)
			}
			unset = true
			return nil
		},
		func(path string) error { entered = path; return nil },
	)
	if err != nil {
		t.Fatalf("enterAgentWorkspace: %v", err)
	}
	if entered != claudeRoot {
		t.Fatalf("entered %q, want the host's direct root %q", entered, claudeRoot)
	}
	if !unset {
		t.Fatalf("%s survived into child processes", AgentWorkspaceEnv)
	}
}

func TestEnterAgentWorkspaceNamesTheVariableThatCarriedTheBadValue(t *testing.T) {
	err := enterAgentWorkspace(
		[]string{"mcp"},
		func(name string) string {
			if name == claudeProjectDirEnv {
				return "${CLAUDE_PROJECT_DIR:-.}"
			}
			return ""
		},
		func(string) error { t.Fatal("unexpanded root changed process state"); return nil },
		func(string) error { t.Fatal("unexpanded root changed process state"); return nil },
	)
	if err == nil || !strings.Contains(err.Error(), claudeProjectDirEnv+" was not expanded") {
		t.Fatalf("error = %v, want it to name %s", err, claudeProjectDirEnv)
	}
}
