package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// bunManager is the package manager of a workspace that declares none.
var bunManager = packageManager{Name: managerBun}

// mockLookPath answers PATH lookups from found, by program name, and fails for
// every other program.
func mockLookPath(t *testing.T, found map[string]string) {
	t.Helper()
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		if bin, ok := found[name]; ok {
			return bin, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

// managerCall is one process the install started.
type managerCall struct {
	bin  string
	args []string
}

// recordCalls records every process the jobs start and answers each with
// success.
func recordCalls(t *testing.T) *[]managerCall {
	t.Helper()
	var calls []managerCall
	mockAllExec(t, func(bin string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls = append(calls, managerCall{bin: bin, args: append([]string(nil), args...)})
		if len(args) == 1 && args[0] == "--version" {
			return &exec.Result{Success: true, Stdout: "1.3.14\n"}, nil
		}
		return &exec.Result{Success: true}, nil
	})
	return &calls
}

// managerWorkspace writes a workspace root with the given root package.json
// and extra root files, an extension root without devDependencies, and the web
// member of membership().
func managerWorkspace(t *testing.T, rootManifest string, files map[string]string) (*pctx.Context, string) {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	writeProbeFile(t, filepath.Join(dir, "putnami.extension.json"), `{}`)
	writeExtensionBiomeDefault(t, dir)
	if rootManifest != "" {
		writeProbeFile(t, filepath.Join(dir, "package.json"), rootManifest)
	}
	for name, content := range files {
		writeProbeFile(t, filepath.Join(dir, name), content)
	}
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web"}`)
	return ctx, dir
}

func TestThePackageManagerFieldNamesTheInstaller(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "the-package-manager-field-names-the-installer")
	for _, tc := range []struct {
		declared string
		want     packageManager
	}{
		{"pnpm@10.26.1", packageManager{Name: managerPNPM, Version: "10.26.1", Source: "package.json#packageManager"}},
		{"npm@10.9.0+sha512.abc", packageManager{Name: managerNPM, Version: "10.9.0", Source: "package.json#packageManager"}},
		{"yarn@1.22.22", packageManager{Name: managerYarn, Version: "1.22.22", Source: "package.json#packageManager"}},
		{"yarn@4.5.0", packageManager{Name: managerYarn, Version: "4.5.0", Berry: true, Source: "package.json#packageManager"}},
		{"bun@1.3.14", packageManager{Name: managerBun, Version: "1.3.14", Source: "package.json#packageManager"}},
	} {
		dir := t.TempDir()
		// A lockfile of another manager does not outrank the declaration.
		writeProbeFile(t, filepath.Join(dir, "bun.lock"), "{}")
		writeProbeFile(t, filepath.Join(dir, "package.json"), `{"packageManager":"`+tc.declared+`"}`)
		got, err := detectPackageManager(dir)
		if err != nil || got != tc.want {
			t.Errorf("detectPackageManager(%s) = %+v, %v; want %+v", tc.declared, got, err, tc.want)
		}
	}
}

func TestARootLockfileNamesTheInstaller(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "a-root-lockfile-names-the-installer")
	for _, tc := range []struct {
		files map[string]string
		want  packageManager
	}{
		{map[string]string{"bun.lockb": ""}, packageManager{Name: managerBun, Source: "bun.lockb"}},
		{map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"}, packageManager{Name: managerPNPM, Source: "pnpm-lock.yaml"}},
		{map[string]string{"yarn.lock": "# yarn lockfile v1\n"}, packageManager{Name: managerYarn, Source: "yarn.lock"}},
		{map[string]string{"yarn.lock": "__metadata:\n  version: 8\n"}, packageManager{Name: managerYarn, Berry: true, Source: "yarn.lock"}},
		{map[string]string{"yarn.lock": "# yarn lockfile v1\n", ".yarnrc.yml": "nodeLinker: node-modules\n"}, packageManager{Name: managerYarn, Berry: true, Source: "yarn.lock"}},
		{map[string]string{"package-lock.json": "{}"}, packageManager{Name: managerNPM, Source: "package-lock.json"}},
		{map[string]string{"npm-shrinkwrap.json": "{}"}, packageManager{Name: managerNPM, Source: "npm-shrinkwrap.json"}},
		// The order of rootLockfiles decides between two lockfiles.
		{map[string]string{"pnpm-lock.yaml": "", "package-lock.json": "{}"}, packageManager{Name: managerPNPM, Source: "pnpm-lock.yaml"}},
		{map[string]string{"pnpm-workspace.yaml": "packages:\n  - apps/*\n", "package-lock.json": "{}"}, packageManager{Name: managerPNPM, Source: "pnpm-workspace.yaml"}},
	} {
		dir := t.TempDir()
		writeProbeFile(t, filepath.Join(dir, "package.json"), `{"name":"repo","private":true}`)
		for name, content := range tc.files {
			writeProbeFile(t, filepath.Join(dir, name), content)
		}
		got, err := detectPackageManager(dir)
		if err != nil || got != tc.want {
			t.Errorf("detectPackageManager(%v) = %+v, %v; want %+v", tc.files, got, err, tc.want)
		}
	}
}

func TestAWorkspaceWithNeitherInstallsWithBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "a-workspace-with-neither-installs-with-bun")
	for _, manifest := range []string{"", `{"name":"fresh","private":true}`} {
		dir := t.TempDir()
		if manifest != "" {
			writeProbeFile(t, filepath.Join(dir, "package.json"), manifest)
		}
		got, err := detectPackageManager(dir)
		if err != nil || got != bunManager {
			t.Errorf("detectPackageManager(%q) = %+v, %v; want bun", manifest, got, err)
		}
	}

	ctx, dir := managerWorkspace(t, `{"name":"fresh","private":true}`, nil)
	ctx.WorkspaceProjects = membership()
	mockBunResolution(t)
	mockLookPath(t, nil)
	calls := recordCalls(t)
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	last := (*calls)[len(*calls)-1]
	if last.bin != "/mock/bun" || !slices.Equal(last.args, []string{"install"}) {
		t.Fatalf("the install ran %s %v, want /mock/bun install", last.bin, last.args)
	}
	if raw := readJSONField(t, filepath.Join(dir, "package.json"), "packageManager"); string(raw) != `"bun@1.3.14"` {
		t.Fatalf("packageManager = %s, want the bun the install ran", raw)
	}
}

func TestAManagerMissingFromPathFailsNamingIt(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "a-manager-missing-from-path-fails-naming-it")
	ctx, dir := managerWorkspace(t, `{"name":"repo","private":true,"packageManager":"pnpm@10.26.1"}`, nil)
	ctx.WorkspaceProjects = membership()
	mockLookPath(t, nil)
	calls := recordCalls(t)
	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("runWorkspaceInstall = %q, %v; want FAILED", status, err)
	}
	for _, want := range []string{"pnpm (package.json#packageManager)", "no pnpm is on PATH", "corepack enable", "putnami install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("the failed install started %v", *calls)
	}
	if _, err := os.Stat(pnpmWorkspaceFile(dir)); !os.IsNotExist(err) {
		t.Fatal("a failed resolution wrote pnpm-workspace.yaml")
	}
}

func TestAnUnknownPackageManagerFailsTheInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "an-unknown-package-manager-fails-the-install")
	ctx, _ := managerWorkspace(t, `{"name":"repo","private":true,"packageManager":"deno@2.1.0"}`, nil)
	mockBunResolution(t)
	calls := recordCalls(t)
	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if status != "FAILED" || !errors.Is(err, errUnknownPackageManager) || !strings.Contains(err.Error(), "deno@2.1.0") {
		t.Fatalf("runWorkspaceInstall = %q, %v; want FAILED naming deno@2.1.0", status, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the refused install started %v", *calls)
	}
}

// A pnpm workspace installs with the pnpm on PATH, adds to the globs of
// pnpm-workspace.yaml the member they do not name, keeps its lockfile, and
// gets no bun.lock, no packageManager and no root workspaces.
func TestAPNPMInstallKeepsItsLockfileAndWritesNoBunLock(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "a-pnpm-install-keeps-its-lockfile-and-writes-no-bun-lock")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members", "a-pnpm-workspace-lists-its-members-in-pnpm-workspace-yaml")
	lock := "lockfileVersion: '9.0'\n\nimporters:\n\n  .: {}\n\n  web: {}\n"
	manifest := "{\n  \"name\": \"shop\",\n  \"private\": true\n}\n"
	ctx, dir := managerWorkspace(t, manifest, map[string]string{
		"pnpm-lock.yaml":      lock,
		"pnpm-workspace.yaml": "packages:\n  - 'apps/*'\n\nonlyBuiltDependencies:\n  - esbuild\n",
	})
	ctx.WorkspaceProjects = membership()
	mockLookPath(t, map[string]string{"pnpm": "/usr/local/bin/pnpm"})
	var seen string
	mockAllExec(t, func(bin string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if bin != "/usr/local/bin/pnpm" || !slices.Equal(args, []string{"install"}) {
			t.Errorf("the install started %s %v, want /usr/local/bin/pnpm install", bin, args)
		}
		data, _ := os.ReadFile(pnpmWorkspaceFile(dir))
		seen = string(data)
		return &exec.Result{Success: true}, nil
	})
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if want := "packages:\n  - 'apps/*'\n  - web\n\nonlyBuiltDependencies:\n  - esbuild\n"; seen != want {
		t.Fatalf("pnpm-workspace.yaml when pnpm install started =\n%s\nwant\n%s", seen, want)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "pnpm-lock.yaml")); string(got) != lock {
		t.Fatalf("pnpm-lock.yaml changed to %s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(got) != manifest {
		t.Fatalf("package.json changed to %s", got)
	}
	for _, name := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("the pnpm install wrote %s", name)
		}
	}
}

func TestAWorkspaceOnAnotherManagerDeclaresNoBun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-bun-pin", "a-workspace-on-another-manager-declares-no-bun")
	for _, tc := range []struct {
		lock    string
		program string
		want    []string
	}{
		{"package-lock.json", "npm", []string{"install"}},
		{"yarn.lock", "yarn", []string{"install", "--non-interactive"}},
	} {
		manifest := "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"workspaces\": [\n    \"web\"\n  ]\n}\n"
		ctx, dir := managerWorkspace(t, manifest, map[string]string{tc.lock: "{}"})
		ctx.WorkspaceProjects = membership()
		mockBunResolution(t)
		mockLookPath(t, map[string]string{tc.program: "/usr/bin/" + tc.program})
		calls := recordCalls(t)
		if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
			t.Fatalf("%s: runWorkspaceInstall = %q, %v; want OK", tc.program, status, err)
		}
		if len(*calls) != 1 || (*calls)[0].bin != "/usr/bin/"+tc.program || !slices.Equal((*calls)[0].args, tc.want) {
			t.Fatalf("%s: the install started %v, want only %s %v", tc.program, *calls, tc.program, tc.want)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(got) != manifest {
			t.Fatalf("%s: package.json changed to %s", tc.program, got)
		}
	}
}

func TestEachManagerInstallsFrozenAndOfflineOnAHostedRun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "each-manager-installs-frozen-and-offline")
	for _, tc := range []struct {
		manager packageManager
		force   bool
		hosted  bool
		want    []string
		env     map[string]string
	}{
		{bunManager, false, true, []string{"install", "--frozen-lockfile"}, nil},
		{packageManager{Name: managerNPM}, false, true, []string{"ci", "--offline"}, nil},
		{packageManager{Name: managerPNPM}, false, true, []string{"install", "--frozen-lockfile", "--offline"}, nil},
		{packageManager{Name: managerYarn}, false, true, []string{"install", "--non-interactive", "--frozen-lockfile", "--offline"}, nil},
		{packageManager{Name: managerYarn, Berry: true}, false, true, []string{"install", "--immutable"}, map[string]string{"YARN_ENABLE_NETWORK": "0"}},
		{packageManager{Name: managerNPM}, false, false, []string{"install"}, nil},
		{packageManager{Name: managerNPM}, true, false, []string{"install", "--force"}, nil},
		{packageManager{Name: managerPNPM}, true, false, []string{"install", "--force"}, nil},
		{packageManager{Name: managerYarn}, true, false, []string{"install", "--non-interactive", "--force"}, nil},
		{packageManager{Name: managerYarn, Berry: true}, true, false, []string{"install"}, nil},
		{bunManager, true, false, []string{"install", "--force"}, nil},
	} {
		got := workspaceInstallInvocation(tc.manager, tc.force, tc.hosted)
		if !slices.Equal(got.args, tc.want) || len(got.env) != len(tc.env) || got.env["YARN_ENABLE_NETWORK"] != tc.env["YARN_ENABLE_NETWORK"] {
			t.Errorf("%+v force=%t hosted=%t: %v %v, want %v %v", tc.manager, tc.force, tc.hosted, got.args, got.env, tc.want, tc.env)
		}
	}
}

func TestProjectsSyncWritesThePNPMMembersToo(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-lists-the-members", "projects-sync-writes-the-pnpm-members-too")
	installCtx, installRoot := managerWorkspace(t, `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`, nil)
	installCtx.WorkspaceProjects = membership()
	_, syncRoot := managerWorkspace(t, `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`, nil)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	recordCalls(t)

	if status, _, err := runWorkspaceInstall(installCtx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	changes, err := syncTypeScriptWorkspace(syncRoot, membership(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "pnpm-workspace.yaml" || changes[0].Field != "packages" {
		t.Fatalf("sync changes = %+v, want the pnpm-workspace.yaml packages", changes)
	}
	installed, _ := os.ReadFile(pnpmWorkspaceFile(installRoot))
	synced, _ := os.ReadFile(pnpmWorkspaceFile(syncRoot))
	if string(installed) != string(synced) || string(synced) != "packages:\n  - web\n" {
		t.Fatalf("install wrote %q, sync wrote %q; want both packages: [web]", installed, synced)
	}
	if raw := readJSONField(t, filepath.Join(syncRoot, "package.json"), "workspaces"); len(raw) != 0 {
		t.Fatalf("sync wrote package.json workspaces %s in a pnpm workspace", raw)
	}
	if again, err := syncTypeScriptWorkspace(syncRoot, membership(), false); err != nil || len(again) != 0 {
		t.Fatalf("a second sync = %+v, %v; want no change", again, err)
	}
}

func TestAPNPMRootManifestGetsNoWorkspaces(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-creates-the-root-manifest", "a-pnpm-root-manifest-gets-no-workspaces")
	ctx, dir := managerWorkspace(t, "", map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"})
	ctx.Workspace.Name = "shop"
	ctx.WorkspaceProjects = membership()
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	recordCalls(t)
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if got, want := readFile(t, filepath.Join(dir, "package.json")), "{\n  \"name\": \"shop\",\n  \"private\": true\n}\n"; got != want {
		t.Fatalf("package.json =\n%s\nwant\n%s", got, want)
	}
	if got := readFile(t, pnpmWorkspaceFile(dir)); got != "packages:\n  - web\n" {
		t.Fatalf("pnpm-workspace.yaml = %q", got)
	}
}

func TestAPNPMCatalogEntryIsSeededInPNPMWorkspaceYAML(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-pnpm-catalog-entry-is-seeded-in-pnpm-workspace-yaml")
	ctx, dir := managerWorkspace(t, "{\n  \"name\": \"shop\",\n  \"private\": true\n}\n", map[string]string{
		"pnpm-workspace.yaml": "packages:\n  - web\n\ncatalog:\n  react: ^19.0.0\n  '@putnami/runtime': 1.0.0\n",
	})
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"),
		`{"name":"web","dependencies":{"@putnami/web":"catalog:","@putnami/runtime":"catalog:","@putnami/application":"catalog:framework","react":"catalog:"}}`)
	manager := packageManager{Name: managerPNPM}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New(), manager); err != nil {
		t.Fatal(err)
	}
	want := "packages:\n  - web\n\ncatalog:\n  react: ^19.0.0\n  '@putnami/runtime': 1.0.0\n  '@putnami/web': latest\ncatalogs:\n  framework:\n    '@putnami/application': latest\n"
	if got := readFile(t, pnpmWorkspaceFile(dir)); got != want {
		t.Fatalf("pnpm-workspace.yaml =\n%s\nwant\n%s", got, want)
	}
	if raw := readJSONField(t, filepath.Join(dir, "package.json"), "catalog"); len(raw) != 0 {
		t.Fatalf("the pnpm seed wrote a package.json catalog %s", raw)
	}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New(), manager); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, pnpmWorkspaceFile(dir)); got != want {
		t.Fatalf("a second seed rewrote pnpm-workspace.yaml:\n%s", got)
	}
}

func TestNPMAndYarnSeedNoCatalog(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "npm-and-yarn-seed-no-catalog")
	for _, manager := range []packageManager{{Name: managerNPM}, {Name: managerYarn}, {Name: managerYarn, Berry: true}} {
		manifest := "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"workspaces\": [\n    \"web\"\n  ]\n}\n"
		ctx, dir := managerWorkspace(t, manifest, nil)
		writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:"}}`)
		if err := ensureWorkspaceCatalog(ctx, jsonl.New(), manager); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(dir, "package.json")); got != manifest {
			t.Fatalf("%+v: package.json changed to %s", manager, got)
		}
		if _, err := os.Stat(pnpmWorkspaceFile(dir)); !os.IsNotExist(err) {
			t.Fatalf("%+v: the seed wrote pnpm-workspace.yaml", manager)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A root pnpm-workspace.yaml names pnpm when no lockfile does, as it does for
// the Node-workspace detector the monorepo probes read, and a Bun lock still
// outranks it.
func TestAPNPMWorkspaceFileNamesPNPM(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-keeps-the-package-manager", "a-pnpm-workspace-file-names-pnpm")
	dir := t.TempDir()
	writeProbeFile(t, filepath.Join(dir, "package.json"), `{"name":"repo","private":true}`)
	writeProbeFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - apps/*\n")
	if got, err := detectPackageManager(dir); err != nil || got != (packageManager{Name: managerPNPM, Source: "pnpm-workspace.yaml"}) {
		t.Fatalf("detectPackageManager = %+v, %v; want pnpm from pnpm-workspace.yaml", got, err)
	}
	writeProbeFile(t, filepath.Join(dir, "bun.lock"), "{}")
	if got, err := detectPackageManager(dir); err != nil || got.Name != managerBun {
		t.Fatalf("detectPackageManager = %+v, %v; want the Bun lock first", got, err)
	}
}

// pnpm reads its members from pnpm-workspace.yaml, whatever the root
// package.json lists.
func TestWorkspaceMemberDirsPrefersThePNPMWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	writeProbeFile(t, filepath.Join(dir, "package.json"), `{"name":"repo","private":true,"workspaces":["old"]}`)
	writeProbeFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - apps/*\n  - '!apps/skip'\n")
	writeProbeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{"name":"web"}`)
	got := workspaceMemberDirs(dir)
	if len(got) != 1 || got[0] != filepath.Join(dir, "apps", "web") {
		t.Fatalf("workspaceMemberDirs = %v, want apps/web from pnpm-workspace.yaml", got)
	}
}

// putnami upgrade --deps in a pnpm workspace pins the catalog
// pnpm-workspace.yaml declares, in place, removes the redundant root pins,
// and reinstalls with pnpm's own forced install; it writes no bun.lock.
func TestAPNPMUpgradePinsThePNPMWorkspaceCatalogAndInstallsWithPNPM(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "upgrade-installs-with-the-package-manager", "a-pnpm-upgrade-pins-the-pnpm-workspace-catalog-and-installs-with-pnpm")
	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"1.4.2"`)
	writeProbeFile(t, filepath.Join(dir, "package.json"), "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"packageManager\": \"pnpm@10.26.1\",\n  \"dependencies\": {\n    \"@putnami/web\": \"1.0.0\"\n  }\n}\n")
	writeProbeFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - apps/web\n\ncatalog:\n  react: ^19.0.0\n  '@putnami/web': latest # seeded\n")
	writeProbeFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	writeProbeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:","@putnami/runtime":"catalog:"}}`)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	calls := recordCalls(t)

	if status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runDepsUpgrade = %q, %v; want OK", status, err)
	}
	if len(*calls) != 1 || (*calls)[0].bin != "/usr/bin/pnpm" || !slices.Equal((*calls)[0].args, []string{"install", "--force"}) {
		t.Fatalf("the upgrade ran %v, want only pnpm install --force", *calls)
	}
	if got, want := readFile(t, filepath.Join(dir, "pnpm-workspace.yaml")), "packages:\n  - apps/web\n\ncatalog:\n  react: ^19.0.0\n  '@putnami/web': 1.4.2 # seeded\n  '@putnami/runtime': 1.4.2\n"; got != want {
		t.Fatalf("pnpm-workspace.yaml =\n%s\nwant\n%s", got, want)
	}
	if got, want := readFile(t, filepath.Join(dir, "package.json")), "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"packageManager\": \"pnpm@10.26.1\"\n}\n"; got != want {
		t.Fatalf("package.json =\n%s\nwant\n%s", got, want)
	}
	for _, name := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("the upgrade wrote %s", name)
		}
	}
}

// A failed reinstall restores the root package.json, pnpm-workspace.yaml and
// the manager's lockfile, and removes a lockfile the failed install created.
func TestAFailedUpgradeRestoresTheManagersFiles(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "upgrade-installs-with-the-package-manager", "a-failed-upgrade-restores-the-managers-files")
	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"1.4.2"`)
	files := map[string]string{
		"package.json":        `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`,
		"pnpm-workspace.yaml": "packages:\n  - apps/web\ncatalog:\n  '@putnami/web': latest\n",
		"pnpm-lock.yaml":      "lockfileVersion: '9.0'\n",
	}
	for name, content := range files {
		writeProbeFile(t, filepath.Join(dir, name), content)
	}
	writeProbeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:"}}`)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		writeProbeFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\nbroken: true\n")
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "ERR_PNPM_FETCH_404"}, nil
	})
	if status, _, _ := runDepsUpgrade(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("runDepsUpgrade = %q, want FAILED", status)
	}
	for name, content := range files {
		if got := readFile(t, filepath.Join(dir, name)); got != content {
			t.Errorf("%s = %q after the failed upgrade, want it restored", name, got)
		}
	}
}

// A Bun or npm workspace reads its members from the root package.json, a
// stale pnpm-workspace.yaml notwithstanding.
func TestWorkspaceMemberDirsOfAnotherManagerReadsThePackageJSON(t *testing.T) {
	dir := t.TempDir()
	writeProbeFile(t, filepath.Join(dir, "package.json"), `{"name":"repo","private":true,"packageManager":"bun@1.4.0","workspaces":["apps/*"]}`)
	writeProbeFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - stale/*\n")
	writeProbeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{"name":"web"}`)
	writeProbeFile(t, filepath.Join(dir, "stale", "old", "package.json"), `{"name":"old"}`)
	got := workspaceMemberDirs(dir)
	if len(got) != 1 || got[0] != filepath.Join(dir, "apps", "web") {
		t.Fatalf("workspaceMemberDirs = %v, want apps/web from package.json", got)
	}
}

// An imported pnpm repository whose member keeps every task on the
// repository's tool and whose globs list it: the install writes no file pnpm
// reads, so the manifest, pnpm-workspace.yaml and the lockfile stay
// byte-identical.
func TestAnImportWhoseTasksNeedNoPackageKeepsTheLockfile(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-adds-what-a-task-needs",
		"a-workspace-whose-tasks-need-no-package-keeps-its-manifest")
	lock := "lockfileVersion: '9.0'\n\nimporters:\n\n  .: {}\n\n  apps/web: {}\n"
	manifest := "{\n    \"name\": \"shop\",\n    \"private\": true,\n    \"packageManager\": \"pnpm@10.26.1\",\n    \"devDependencies\": {\n        \"turbo\": \"^2.5.0\"\n    }\n}\n"
	workspace := "packages:\n  - 'apps/*'\n  - '!apps/legacy'\n"
	ctx, dir := managerWorkspace(t, manifest, map[string]string{
		"pnpm-lock.yaml":         lock,
		"pnpm-workspace.yaml":    workspace,
		"apps/web/package.json":  `{"name":"web"}`,
		"apps/web/putnami.json":  `{"extensions":["@putnami/turbo"]}`,
		"putnami.extension.json": `{"workspaceDevDependencies":{"typescript":"^6.0.2","@types/bun":"^1.4.0","@biomejs/biome":"2.5.15"}}`,
	})
	ctx.WorkspaceProjects = []pctx.ProjectRef{{Name: "shop", Path: "."}, {Name: "web", Path: "apps/web"}}
	mockLookPath(t, map[string]string{"pnpm": "/usr/local/bin/pnpm"})
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	})
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	for name, want := range map[string]string{"package.json": manifest, "pnpm-workspace.yaml": workspace, "pnpm-lock.yaml": lock} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s =\n%s\nwant it unchanged:\n%s", name, got, want)
		}
	}
}

// A failed install reports what the manager printed: pnpm prints the error of
// a lifecycle script on stdout, and Yarn can print a whole bundle on one line.
func TestAFailedInstallReportsTheManagersError(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-reports-the-managers-error", "a-failed-install-reports-the-managers-error")
	ctx, _ := managerWorkspace(t, `{"name":"repo","private":true,"packageManager":"pnpm@10.19.0"}`, map[string]string{
		"pnpm-workspace.yaml": "packages:\n  - web\n",
	})
	ctx.WorkspaceProjects = membership()
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	bundle := strings.Repeat("x", 48*1024)
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{ExitCode: 1, Stdout: "Scope: all 2 workspace projects\n" + bundle + "\n. postinstall: ⨯ error devDependencies should be ordered alphabetically\nELIFECYCLE Command failed with exit code 1.\n"}, nil
	})
	emit := jsonl.New()
	var logged []string
	emit.Observe(func(message string) { logged = append(logged, message) })
	if status, _, err := runWorkspaceInstall(ctx, emit, nil); status != "FAILED" || err != nil {
		t.Fatalf("runWorkspaceInstall = %q, %v; want FAILED", status, err)
	}
	var failure string
	for _, message := range logged {
		if strings.HasPrefix(message, "pnpm install failed: ") {
			failure = message
		}
	}
	if !strings.Contains(failure, "devDependencies should be ordered alphabetically") || !strings.HasSuffix(failure, "ELIFECYCLE Command failed with exit code 1.") {
		t.Fatalf("the failure %q does not report pnpm's error", failure)
	}
	if len(failure) > managerOutputBudget+len("pnpm install failed: ") {
		t.Fatalf("the failure reports %d bytes, want at most %d", len(failure), managerOutputBudget)
	}
}

// A Rush repository installs with rush install, and gets no root package.json
// and no lockfile of another manager: Rush installs the projects rush.json
// lists, and the npm install-run-rush.js runs fails on a root manifest.
func TestARushRepositoryInstallsWithRushAndGetsNoRootManifest(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-runs-rush-install", "a-rush-repository-installs-with-rush-install")
	ctx, dir := managerWorkspace(t, "", map[string]string{
		"rush.json":                         `{"rushVersion": "5.140.0", "pnpmVersion": "9.15.0", "projects": [{"packageName": "web", "projectFolder": "web"}]}`,
		"common/config/rush/pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})
	ctx.WorkspaceProjects = membership()
	mockLookPath(t, map[string]string{"rush": "/usr/local/bin/rush"})
	calls := recordCalls(t)
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if len(*calls) != 1 || (*calls)[0].bin != "/usr/local/bin/rush" || !slices.Equal((*calls)[0].args, []string{"install"}) {
		t.Fatalf("the install started %v, want only rush install", *calls)
	}
	for _, name := range []string{"package.json", "pnpm-workspace.yaml", "bun.lock", "pnpm-lock.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("the Rush install wrote %s", name)
		}
	}
	changes, err := syncTypeScriptWorkspace(dir, ctx.WorkspaceProjects, false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("the workspace sync of a Rush repository = %v, %v; want no change", changes, err)
	}
}

// A hosted run starts no rush install: it runs the release rush.json pins,
// which it downloads first, and that is repository code.
func TestAHostedRushInstallIsRefused(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-install-runs-rush-install", "a-rush-repository-installs-with-rush-install")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	ctx, _ := managerWorkspace(t, "", map[string]string{"rush.json": `{"rushVersion": "5.140.0", "projects": []}`})
	mockLookPath(t, map[string]string{"rush": "/usr/local/bin/rush"})
	calls := recordCalls(t)
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); !errors.Is(err, errHostedRush) || status != "FAILED" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want the hosted Rush refusal", status, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the hosted install started %v, want nothing", *calls)
	}
}
