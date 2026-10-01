package workspaceclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestResolvedGeneratorsRunWithoutFrameworkSourceInConsumerWorkspace(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "an-external-workspace-needs-no-framework-source-checkout")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	root := t.TempDir()
	project := filepath.Join(root, "provider")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "calls")
	t.Setenv("CLIENTGEN_CAPTURE", capture)
	goEmitter := writeExecutableFixture(t, root, "putnami-client-generate-go", "go")
	tsEmitter := writeExecutableFixture(t, root, "putnami-client-generate", "ts")

	if err := runResolvedGenerators(root, project, project, goEmitter, tsEmitter); err != nil {
		t.Fatalf("run packaged emitters: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "go --project "+project || lines[1] != "ts --project "+project+" --format-project "+project {
		t.Fatalf("resolved emitter calls = %q", data)
	}
	if _, err := os.Stat(filepath.Join(root, "typescript", "framework", "client")); !os.IsNotExist(err) {
		t.Fatalf("test workspace unexpectedly contains framework source: %v", err)
	}
}

func TestTypeScriptEmitterRequiresPutnamiInstalledPackageShim(t *testing.T) {
	root := t.TempDir()
	if _, err := typescriptGeneratorExecutable(root, filepath.Join(root, "provider"), &clientGenConfig{}); err == nil || !strings.Contains(err.Error(), "putnami deps install") {
		t.Fatalf("missing package shim error = %v", err)
	}
}

func TestTypeScriptEmitterResolvesGeneratedTargetPackageShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	root := t.TempDir()
	project := filepath.Join(root, "provider")
	config := &clientGenConfig{}
	config.TS.Output = "clients/ts"
	want := writeExecutableFixture(t, filepath.Join(project, "clients", "ts", "node_modules", ".bin"), "putnami-client-generate", "ts")
	got, err := typescriptGeneratorExecutable(root, project, config)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("typescript emitter = %q, want generated target shim %q", got, want)
	}
}

func TestMirrorRenderUsesInstalledShimFromSourceWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	workspaceRoot := t.TempDir()
	commandRoot := t.TempDir()
	sourceProject := filepath.Join(workspaceRoot, "provider")
	mirrorProject := filepath.Join(commandRoot, "provider")
	if err := os.MkdirAll(mirrorProject, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(workspaceRoot, "calls")
	t.Setenv("CLIENTGEN_CAPTURE", capture)
	writeExecutableFixture(t, filepath.Join(sourceProject, "node_modules", ".bin"), "putnami-client-generate", "ts")
	config := &clientGenConfig{Targets: []string{"ts"}}
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspaceRoot)

	if err := runConfiguredGenerators(workspaceRoot, commandRoot, mirrorProject, config); err != nil {
		t.Fatalf("render mirror with source-installed shim: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "ts --project "+mirrorProject+" --format-project "+sourceProject; got != want {
		t.Fatalf("generator call = %q, want %q", got, want)
	}
	rootData, err := os.ReadFile(capture + ".root")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(rootData)), commandRoot; got != want {
		t.Fatalf("generator workspace root = %q, want mirror root %q", got, want)
	}
}

func TestCopyProviderInputsPreservesGoGeneratorMetadataWithoutGeneratedBytes(t *testing.T) {
	workspaceRoot := t.TempDir()
	mirrorRoot := t.TempDir()
	projectRel := "services/catalog"
	projectRoot := filepath.Join(workspaceRoot, filepath.FromSlash(projectRel))
	config := &clientGenConfig{Targets: []string{"go"}}
	config.Go.Output = "clients/go"
	provider := provider{rel: projectRel, specPath: projectRel + "/.gen/schema/openapi.json", config: config}

	files := map[string]string{
		".gen/clientgen/config.json":     `{"targets":["go"]}`,
		".gen/schema/openapi.json":       `{"openapi":"3.1.0"}`,
		"go.mod":                         "module example.dev/catalog\n",
		"clients/go/go.mod":              "module example.dev/catalog-client\n",
		"clients/go/putnami.json":        `{"name":"example.dev/catalog-client"}`,
		"clients/go/client.gen.go":       "must not enter expected render\n",
		"clients/go/client.putnami.json": `{"must":"not enter expected render"}`,
	}
	for rel, body := range files {
		path := filepath.Join(projectRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := copyProviderInputs(workspaceRoot, mirrorRoot, provider); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".gen/clientgen/config.json", ".gen/schema/openapi.json", "go.mod", "clients/go/go.mod", "clients/go/putnami.json"} {
		got, err := os.ReadFile(filepath.Join(mirrorRoot, filepath.FromSlash(projectRel), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read copied %s: %v", rel, err)
		}
		if string(got) != files[rel] {
			t.Fatalf("copied %s = %q, want %q", rel, got, files[rel])
		}
	}
	for _, rel := range []string{"clients/go/client.gen.go", "clients/go/client.putnami.json"} {
		if _, err := os.Stat(filepath.Join(mirrorRoot, filepath.FromSlash(projectRel), filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("generated byte %s leaked into fresh render: %v", rel, err)
		}
	}
}

func TestCopyProviderInputsRejectsEscapingGeneratedTarget(t *testing.T) {
	config := &clientGenConfig{Targets: []string{"go"}}
	config.Go.Output = "../../outside"
	err := copyProviderInputs(t.TempDir(), t.TempDir(), provider{rel: "provider", config: config})
	if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("escaping output error = %v", err)
	}
}

func TestTypeScriptGeneratorCommandRequiresResolvedRuntimeWhenDeclared(t *testing.T) {
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	if _, err := typescriptGeneratorCommand("shim", "project", "source-project"); err == nil || !strings.Contains(err.Error(), "exact runtime pinned") {
		t.Fatalf("unavailable declared runtime error = %v", err)
	}
}

func TestResolvedGeneratorsUseDeclaredTypeScriptRuntimeAndGoOnlyNeedsNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	root := t.TempDir()
	project := filepath.Join(root, "provider")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "calls")
	t.Setenv("CLIENTGEN_CAPTURE", capture)
	runtimeExecutable := writeExecutableFixture(t, root, "runtime", "runtime")
	shim := filepath.Join(root, "generator.ts")
	if err := os.WriteFile(shim, []byte("not executable; the runtime owns invocation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", runtimeExecutable)
	if err := runResolvedGenerators(root, project, project, "", shim); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "runtime "+shim+" --project "+project+" --format-project "+project; got != want {
		t.Fatalf("declared runtime call = %q, want %q", got, want)
	}

	if err := os.Remove(capture); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	goEmitter := writeExecutableFixture(t, root, "go-emitter", "go")
	if err := runResolvedGenerators(root, project, "", goEmitter, ""); err != nil {
		t.Fatalf("Go-only generation required unavailable TypeScript runtime: %v", err)
	}
}

// The emitter's Bun keeps its transpiler cache under its install when Putnami
// installed it, so that such a Bun writes nothing under the user's home
// directory. A Bun the host holds, and an explicit setting, are left alone.
func TestTypeScriptEmitterKeepsTheTranspilerCacheOfAnInstalledBunUnderItsInstall(t *testing.T) {
	const cacheEnv = "BUN_RUNTIME_TRANSPILER_CACHE_PATH"
	install := filepath.Join(t.TempDir(), ".putnami", "toolchains", "bun", "bun-1.4.0")
	hostInstall := filepath.Join(t.TempDir(), ".bun")

	got := typescriptEmitterEnvironment([]string{"PATH=/usr/bin", "BUN_INSTALL=" + install})
	if value, want := environmentValue(got, cacheEnv), filepath.Join(install, "install", "cache", "@t@"); value != want {
		t.Errorf("transpiler cache of an installed Bun = %q, want %q", value, want)
	}
	if value := environmentValue(got, "BUN_INSTALL"); value != install {
		t.Errorf("BUN_INSTALL = %q, want it unchanged at %q", value, install)
	}

	for name, environment := range map[string][]string{
		"a host Bun":          {"PATH=/usr/bin", "BUN_INSTALL=" + hostInstall},
		"no BUN_INSTALL":      {"PATH=/usr/bin"},
		"an explicit setting": {"BUN_INSTALL=" + install, cacheEnv + "=/explicit/cache"},
	} {
		before := environmentValue(environment, cacheEnv)
		got := typescriptEmitterEnvironment(environment)
		if value := environmentValue(got, cacheEnv); value != before {
			t.Errorf("%s: transpiler cache = %q, want it left at %q", name, value, before)
		}
		if len(got) != len(environment) {
			t.Errorf("%s: environment grew from %d to %d entries", name, len(environment), len(got))
		}
	}
}

func writeExecutableFixture(t *testing.T, root, name, label string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	body := "#!/bin/sh\nprintf '%s %s\\n' '" + label + "' \"$*\" >> \"$CLIENTGEN_CAPTURE\"\nprintf '%s\\n' \"$PUTNAMI_WORKSPACE_ROOT\" >> \"$CLIENTGEN_CAPTURE.root\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
