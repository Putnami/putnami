// Package authoring proves, through the real CLI, that each authoring
// subcommand reaches its own handler.
package authoring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// TestAuthoringRoutesReachTheirHandlers proves each authoring subcommand
// reaches its own handler. Every case runs the CLI outside a workspace and
// expects a line only that handler prints, so swapping two case labels in
// cmdTemplates fails the test.
func TestAuthoringRoutesReachTheirHandlers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(extension.UserScopeRepairEnv, "")
	t.Setenv(extension.PrivatePutRegistryURLEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(home, "artifacts"))

	templateDir := t.TempDir()
	for name, content := range map[string]string{
		"putnami.template.json": `{"name":"route-template","version":"1.0.0","description":"route proof"}`,
		"putnami.json.template": `{"name":"{{.ProjectName}}"}`,
		"README.md":             "# readme",
		"LICENSE.md":            "MIT",
	} {
		clitest.WriteFile(t, filepath.Join(templateDir, name), content)
	}
	extensionDir := t.TempDir()
	clitest.WriteFile(t, filepath.Join(extensionDir, "putnami.extension.json"), `{
  "name": "@putnami/route-ext",
  "version": "1.0.0",
  "cliContract": 4,
  "commands": {"build": {"run": [{"id": "build", "task": "build-exec"}]}},
  "tasks": {"build-exec": {"kind": "command", "command": "echo", "cache": false}}
}`)

	cases := []struct {
		args    []string
		want    string
		notWant []string
	}{
		{[]string{"extensions", "validate", extensionDir}, "Extension is valid.", nil},
		{[]string{"templates", "validate", templateDir}, "Template is valid.", []string{"Skipping build+test", "Archive:"}},
		{[]string{"templates", "test", templateDir, "--skip-build"}, "Skipping build+test", []string{"Archive:"}},
		{[]string{"templates", "package", templateDir}, "Archive:", []string{"Skipping build+test", "Template is valid."}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[:2], " "), func(t *testing.T) {
			code, out := clitest.RunGateArgs(t, t.TempDir(), tc.args...)
			if code != 0 {
				t.Fatalf("%s: exit %d\noutput:\n%s", strings.Join(tc.args, " "), code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
			for _, absent := range tc.notWant {
				if strings.Contains(out, absent) {
					t.Errorf("output = %q, must not contain %q (another handler ran)", out, absent)
				}
			}
		})
	}
}
