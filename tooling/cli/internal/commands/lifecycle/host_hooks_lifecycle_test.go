package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// hostHooksWorkspace writes a workspace that declares a local extension
// with agent content and host hooks, opts into its content and binds the
// memory contract its hooks require.
func hostHooksWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"ext/sessions/putnami.extension.json": fmt.Sprintf(`{
  "name": "@acme/sessions",
  "cliContract": %d,
  "agentContent": {"path": "content", "source": "src"},
  "commandGroups": {"agent": {"subcommands": {"hook": {"command": "agent-hook", "interactive": true}}}},
  "commands": {"agent-hook": {"run": [{"id": "hook", "task": "hook"}]}},
  "tasks": {"hook": {"kind": "command", "command": "echo"}},
  "hostHooks": {
    "requires": {"contract": "memory"},
    "command": ["agent", "hook"],
    "hosts": {
      "claude-code": [{"event": "Stop", "args": ["--engine", "claude-code"], "timeoutSeconds": 30}],
      "codex": [{"event": "Stop", "args": ["--engine", "codex"], "timeoutSeconds": 30}]
    }
  }
}`, protocolcli.HostHooksContract),
		"ext/sessions/putnami.json":                `{"name":"@acme/sessions","options":{"agent-artifact":{"forbiddenContent":[],"requiredSkills":["session"]}}}`,
		"ext/sessions/src/skills/session/SKILL.md": "---\nname: session\ndescription: Report the session\n---\n\nReport the session.\n",
		wsproto.WorkspaceConfigFilename: `{"name":"hooks-ws","extensions":{"/ext/sessions":""},` +
			`"agentArtifacts":["extension:@acme/sessions"],` +
			`"options":{"collaboration":{"memory":{"provider":"@acme/memory","version":1}}}}` + "\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// An explicit install writes the hooks the opted-in extension declares; the
// first-use bootstrap writes none, since it must not change a committed file
// on behalf of another command.
func TestInstall_WritesHostHooksOnlyWhenExplicit(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-written-on-explicit-lifecycle", "an-explicit-install-writes-the-hooks")
	hometest.Temp(t)
	install := func(ctx context.Context, root string) string {
		t.Helper()
		var out strings.Builder
		if _, err := captureStdout(t, func() error {
			return Install(ctx, root, wsproto.Load(root), nil, installTestEnv(&out, WorkspaceJobOK))
		}); err != nil {
			t.Fatalf("Install: %v", err)
		}
		return out.String()
	}

	t.Run("explicit", func(t *testing.T) {
		root := hostHooksWorkspace(t)
		out := install(context.Background(), root)
		claude := readWorkspaceFile(t, root, ".claude/settings.json")
		if !strings.Contains(claude, `"command": "putnami agent hook --engine claude-code || true"`) {
			t.Fatalf("Claude Code hooks:\n%s", claude)
		}
		codex := readWorkspaceFile(t, root, ".codex/hooks.json")
		if !strings.Contains(codex, `"command": "putnami agent hook --engine codex || true"`) {
			t.Fatalf("Codex hooks:\n%s", codex)
		}
		if !strings.Contains(out, "run /hooks") {
			t.Fatalf("install did not report the Codex trust step:\n%s", out)
		}
	})

	t.Run("implicit", func(t *testing.T) {
		spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hooks-written-on-explicit-lifecycle", "the-implicit-bootstrap-writes-no-hooks")
		root := hostHooksWorkspace(t)
		install(shared.WithImplicitInstall(context.Background()), root)
		for _, rel := range []string{".claude/settings.json", ".codex/hooks.json"} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Fatalf("the implicit bootstrap wrote %s (%v)", rel, err)
			}
		}
	})
}
