package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// An install names each retired extension the workspace binds, before it
// installs anything, and goes on.
func TestInstall_NamesARetiredExtensionTheWorkspaceBinds(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-retired-extension-is-named", "install-names-a-retired-extension-the-workspace-binds")
	hometest.Temp(t)
	notice := "This checkout binds @putnami/intelligence, which is retired; rebase on the default branch or bind @putnami/cloud."
	for name, manifest := range map[string]string{
		"retired": `{"name":"install-ws","options":{"collaboration":{"missions":{"provider":"@putnami/intelligence","version":1}}}}`,
		"current": `{"name":"install-ws","options":{"collaboration":{"missions":{"provider":"@putnami/cloud","version":2}}}}`,
		"unbound": `{"name":"install-ws"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if _, err := captureStdout(t, func() error {
				return Install(context.Background(), dir, wsproto.Load(dir), nil, installTestEnv(&out, WorkspaceJobOK))
			}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			named := strings.Contains(out.String(), "  warning: "+notice+"\n")
			if named != (name == "retired") {
				t.Fatalf("the install output names the retired extension: %v\n%s", named, out.String())
			}
		})
	}
}
