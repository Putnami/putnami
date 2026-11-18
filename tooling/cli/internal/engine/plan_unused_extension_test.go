package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// webExtension is a loaded extension that declares one project job.
func webExtension(command string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/web",
		Jobs: map[string]*extension.JobDefinition{
			command: {Name: command, ExtensionName: "@putnami/web"},
		},
	}
}

// A declared command that plans no job because a selected project does not
// use the extension serving it fails with the usual line, then one line that
// names the extension, the project and the fix.
func TestAnEmptyPlanNamesTheExtensionItsProjectsDoNotUse(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "no-core-development-verb", "an-empty-plan-names-the-extension-its-projects-do-not-use")

	root := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"hint-fixture","includes":["site"]}`)
	write("site/putnami.json", `{"name":"site","extensions":["@putnami/typescript"]}`)
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"", "json"} {
		t.Run("output="+mode, func(t *testing.T) {
			req := &Request{
				WorkspaceRoot: root,
				Config:        wsproto.Load(root),
				Commands:      []string{"web-i18n-sync"},
				Stdout:        &strings.Builder{},
			}
			req.Global.Output = mode
			code := ExitSuccess
			stderr := captureStderr(t, func() {
				_, code = buildPlan(req, ws, ws.Projects, []*extension.ExtensionDescription{webExtension("web-i18n-sync")}, nil)
			})
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d", code, ExitUsage)
			}
			const want = `putnami: @putnami/web serves web-i18n-sync and the project /site does not use it: add "@putnami/web" to "extensions" in its putnami.json`
			if !strings.Contains(stderr, "no job matched command(s) web-i18n-sync") || !strings.Contains(stderr, want+"\n") {
				t.Fatalf("stderr = %q, want the no-match line and %q", stderr, want)
			}
		})
	}
}
