package workspaceclient

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The TypeScript emitter is the package the workspace installed, never the
// framework source: a consumer workspace holds no typescript/framework/client.
func TestTypeScriptGenerationRunsTheInstalledEmitterWithoutFrameworkSource(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "an-external-workspace-needs-no-framework-source-checkout")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	root := t.TempDir()
	client := filepath.Join(root, "clients", "items")
	if err := os.MkdirAll(client, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "calls")
	t.Setenv("CLIENTGEN_CAPTURE", capture)
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	if err := os.Unsetenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"); err != nil {
		t.Fatal(err)
	}
	writeExecutableFixture(t, filepath.Join(root, "node_modules", ".bin"), "putnami-client-generate", "ts")

	generation := TypeScriptGeneration{Contract: "/contract.json", Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err != nil {
		t.Fatalf("run the installed emitter: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "ts --contract /contract.json --output " + client + " --package-name @example/items --format-project " + client
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("emitter call = %q, want %q", got, want)
	}
	rootData, err := os.ReadFile(capture + ".root")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(rootData)); got != root {
		t.Fatalf("emitter workspace root = %q, want %q", got, root)
	}
	if _, err := os.Stat(filepath.Join(root, "typescript", "framework", "client")); !os.IsNotExist(err) {
		t.Fatalf("test workspace unexpectedly contains framework source: %v", err)
	}
}

func TestTypeScriptEmitterRequiresPutnamiInstalledPackageShim(t *testing.T) {
	root := t.TempDir()
	if _, err := typescriptGeneratorExecutable(root, filepath.Join(root, "client")); err == nil || !strings.Contains(err.Error(), "putnami deps install") {
		t.Fatalf("missing package shim error = %v", err)
	}
}

func TestTypeScriptEmitterPrefersTheClientProjectsOwnInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	root := t.TempDir()
	project := filepath.Join(root, "client")
	writeExecutableFixture(t, filepath.Join(root, "node_modules", ".bin"), "putnami-client-generate", "root")
	want := writeExecutableFixture(t, filepath.Join(project, "node_modules", ".bin"), "putnami-client-generate", "ts")
	got, err := typescriptGeneratorExecutable(root, project)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("typescript emitter = %q, want the client's own shim %q", got, want)
	}
}

func TestTypeScriptGeneratorCommandRequiresResolvedRuntimeWhenDeclared(t *testing.T) {
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	if _, err := typescriptGeneratorCommand("shim", TypeScriptGeneration{}); err == nil || !strings.Contains(err.Error(), "exact runtime pinned") {
		t.Fatalf("unavailable declared runtime error = %v", err)
	}
}

func TestTypeScriptGeneratorCommandRunsTheShimUnderTheDeclaredRuntime(t *testing.T) {
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "/toolchains/bun")
	command, err := typescriptGeneratorCommand("/shim", TypeScriptGeneration{Contract: "/c.json", Project: "/p", PackageName: "p"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/toolchains/bun", "/shim", "--contract", "/c.json", "--output", "/p", "--package-name", "p", "--format-project", "/p"}
	if !slices.Equal(command.Args, want) {
		t.Fatalf("command = %q, want %q", command.Args, want)
	}
}

// The Go emitter takes only the package and client names and the omitted
// operations; the omissions are sorted and deduplicated so their order in
// putnami.json does not change the run.
func TestGoGeneratorArgumentsCarryOnlyTheGoOptions(t *testing.T) {
	got := goGeneratorArguments(GoGeneration{Contract: "/c.json", Output: "/out", PackageName: "itemsclient",
		ClientName: "ItemsClient", OmitOperations: []string{"b", "a", "b"}})
	want := []string{"--contract", "/c.json", "--output", "/out", "--package", "itemsclient", "--client", "ItemsClient",
		"--omit", "a", "--omit", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	if got := goGeneratorArguments(GoGeneration{Contract: "/c.json", Output: "/out"}); !slices.Equal(got, want[:4]) {
		t.Fatalf("default arguments = %q, want %q", got, want[:4])
	}
}

func TestReadModuleTargetReadsTheCommittedGoTarget(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "clientgen.json",
		`{"targets":["go"],"go":{"output":"clients/go/","modulePath":"","packageName":"p","clientName":"C","omitOperations":["x"]}}`)
	target, err := ReadModuleTarget(root)
	if err != nil {
		t.Fatal(err)
	}
	if target.Output != "clients/go" || target.PackageName != "p" || target.ClientName != "C" || !slices.Equal(target.OmitOperations, []string{"x"}) {
		t.Fatalf("target = %+v", target)
	}
	if _, err := ReadModuleTarget(t.TempDir()); err == nil {
		t.Fatal("a project without clientgen.json read a target")
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
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test-owned executable fixture
		t.Fatal(err)
	}
	return path
}
