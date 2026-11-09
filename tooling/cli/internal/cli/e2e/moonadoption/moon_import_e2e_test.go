package moonadoption

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// languageExtension writes at dir a local stand-in for a language extension
// under its name: its lint, test and build log the command and the project,
// and pass. The stand-in keeps the test off the registry and the language
// toolchains: what it proves is which extension each project runs.
func languageExtension(t *testing.T, root, dir, name, log string) {
	t.Helper()
	ext := filepath.Join(root, dir)
	task := func(command string) string {
		return `"` + command + `-run": {"kind": "command", "command": "{extensionRoot}/run.sh", "args": ["` + command + `"], "cwd": "{projectRoot}", "cache": false, "timeoutMs": -1}`
	}
	command := func(command string) string {
		return `"` + command + `": {"description": "` + command + ` with the stand-in", "run": [{"id": "` + command + `", "task": "` + command + `-run"}]}`
	}
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), `{
  "name": "`+name+`",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {`+command("lint")+`, `+command("test")+`, `+command("build")+`},
  "tasks": {`+task("lint")+`, `+task("test")+`, `+task("build")+`}
}`)
	script := filepath.Join(ext, "run.sh")
	clitest.WriteFile(t, script, "#!/bin/sh\necho \""+name+" $1 $(basename \"$(pwd)\")\" >> '"+log+"'\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInitFromMoonInImportModeMapsTheProjectsAndPassesLintTestBuild(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "moon-import-runs-the-language-extensions", "init-from-moon-in-import-mode-maps-the-projects-and-passes-lint-test-build")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := moonWorkspace(t, demo, buildRuntime(t), "")
	moonLog := stubMoon(t)
	langLog := filepath.Join(t.TempDir(), "languages.log")
	languageExtension(t, root, "typescript-extension", "@putnami/typescript", langLog)
	languageExtension(t, root, "rust-extension", "@putnami/rust", langLog)
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/moon-extension", "/typescript-extension", "/rust-extension"], "includes": []}`)

	code, out := clitest.RunGateArgs(t, root, "init", "--from", "moon", "--mode", "import", "--apply")
	if code != 0 {
		t.Fatalf("init --apply exit %d:\n%s", code, out)
	}
	// The unmapped task is listed, and keeps running through moon.
	if !strings.Contains(out, "api:dev") {
		t.Errorf("the sync lists no unmapped task:\n%s", out)
	}
	// The project graph is the one observe reads from the demo: api depends
	// on pricing, web stands alone.
	for dir, want := range map[string]struct {
		extension string
		deps      []string
		disabled  []string
	}{
		"apps/api":         {"@putnami/typescript", []string{"/packages/pricing"}, nil},
		"packages/pricing": {"@putnami/rust", nil, []string{"lint"}},
		"apps/web":         {"@putnami/moon", nil, []string{"build", "test"}},
	} {
		p := readProject(t, root, dir)
		if !slices.Equal(p.Extensions, []string{want.extension}) || !slices.Equal(p.Dependencies, want.deps) || !slices.Equal(p.Disable.Jobs, want.disabled) || !slices.Equal(p.Tags, []string{"moon"}) {
			t.Errorf("%s putnami.json = %+v, want %+v", dir, p, want)
		}
	}
	// The language extensions the workspace loads from its tree are
	// declared: the sync adds no registry entry for them.
	config, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if err != nil || strings.Contains(string(config), `"@putnami/typescript"`) {
		t.Errorf("workspace config = %s (%v)", config, err)
	}

	code, out = clitest.RunGateArgs(t, root, "lint,test,build", "--all")
	if code != 0 {
		t.Fatalf("lint,test,build exit %d:\n%s", code, out)
	}
	data, err := os.ReadFile(langLog)
	if err != nil {
		t.Fatal(err)
	}
	ran := strings.Split(strings.TrimSpace(string(data)), "\n")
	slices.Sort(ran)
	want := []string{
		"@putnami/rust build pricing", "@putnami/rust test pricing",
		"@putnami/typescript build api", "@putnami/typescript lint api", "@putnami/typescript test api",
	}
	if !slices.Equal(ran, want) {
		t.Errorf("the language extensions ran %v, want %v\n%s", ran, want, out)
	}
	// web, in JavaScript, has no Putnami extension: it stays on moon, and
	// runs its one task, lint, through it.
	data, err = os.ReadFile(moonLog)
	if err != nil {
		t.Fatalf("moon ran nothing: %v\n%s", err, out)
	}
	var runs []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if _, call, ok := strings.Cut(line, " moon run "); ok {
			runs = append(runs, call)
		}
	}
	if !slices.Equal(runs, []string{"web:lint"}) {
		t.Errorf("moon ran %v, want web:lint alone\n%s", runs, out)
	}
}
