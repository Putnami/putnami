package workspaceclient

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
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

// The TypeScript emitter reads the workspace root package.json to decide how a
// client depends on @putnami/client. The emitter double below copies that file
// into its output, so the fresh render matches the build's render only when
// the mirror holds the root bytes the workspace holds.
func TestFreshRenderWritesTheBytesTheBuildWrites(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "drift-closure", "the-fresh-render-reads-the-workspace-root-inputs-the-build-reads")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	cases := map[string]string{
		"a catalog entry":      `{"name":"consumer","catalog":{"@putnami/client":"0.4.0"}}`,
		"a pinned dependency":  `{"name":"consumer","dependencies":{"@putnami/client":"0.4.0"}}`,
		"no root package.json": "",
	}
	for name, rootPackage := range cases {
		t.Run(name, func(t *testing.T) {
			const runtimeEnv = "PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"
			t.Setenv(runtimeEnv, "")
			if err := os.Unsetenv(runtimeEnv); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeWorkspaceFile(t, root, "putnami.workspace.json", `{"name":"consumer"}`)
			if rootPackage != "" {
				writeWorkspaceFile(t, root, "package.json", rootPackage)
			}
			writeWorkspaceFile(t, root, "tsconfig.base.json", `{}`)
			writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
			writeWorkspaceFile(t, root, "services/catalog/.gen/clientgen/config.json", `{"targets":["ts"],"ts":{"output":"clients/ts"}}`)
			writeWorkspaceFile(t, root, "services/catalog/.gen/schema/openapi.json", string(catalogContractForFixture(t)))
			emitter := filepath.Join(root, "node_modules", ".bin", "putnami-client-generate")
			writeWorkspaceFile(t, root, "node_modules/.bin/putnami-client-generate", rootReadingEmitter)
			if err := os.Chmod(emitter, 0o755); err != nil {
				t.Fatal(err)
			}

			provider := filepath.Join(root, "services", "catalog")
			output, generated, err := SynchronizeTarget(root, provider, clientcontract.GeneratedLanguageTypeScript)
			if err != nil || !generated || output != "clients/ts" {
				t.Fatalf("build render = (%q, %v, %v), want clients/ts", output, generated, err)
			}
			mirror, cleanup, err := RenderExpected(root)
			if err != nil {
				t.Fatalf("fresh render: %v", err)
			}
			defer cleanup()

			built := readTree(t, filepath.Join(provider, "clients", "ts"))
			fresh := readTree(t, filepath.Join(mirror, "services", "catalog", "clients", "ts"))
			if len(built) == 0 {
				t.Fatal("the build render wrote no file")
			}
			if !reflect.DeepEqual(built, fresh) {
				t.Fatalf("fresh render differs from the build render\n  build: %q\n  fresh: %q", built, fresh)
			}
		})
	}
}

// rootReadingEmitter writes the workspace root package.json it reads, or a
// marker when there is none, into the TypeScript target.
const rootReadingEmitter = `#!/bin/sh
set -eu
project=$2
mkdir -p "$project/clients/ts"
if [ -f "$PUTNAMI_WORKSPACE_ROOT/package.json" ]; then
  cp "$PUTNAMI_WORKSPACE_ROOT/package.json" "$project/clients/ts/root-package.json"
else
  printf 'no root package.json\n' > "$project/clients/ts/root-package.json"
fi
`

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path) //nolint:gosec // test-owned temporary tree
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
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
