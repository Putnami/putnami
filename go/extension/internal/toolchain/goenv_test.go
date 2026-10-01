package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/envkeys"
)

// testModuleOrigin is the vanity module server a test workspace declares. It is
// a fixture, not a built-in: the seam under test resolves whatever host a
// workspace declares, and TestGoCommandEnvHonorsAnyDeclaredVanityHost proves a
// different one gets identical treatment.
const (
	testModuleOrigin  = "go.putnami.dev"
	testModulePattern = testModuleOrigin + "/*"
	testOriginEnv     = goRegistryURLEnvVar + "=https://" + testModuleOrigin
)

func writeGoWork(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "go.work")
	if err := os.WriteFile(path, []byte("go 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	return path
}

func goworkValues(env []string) []string {
	var vals []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GOWORK="); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

func TestFindGoWork_SameDir(t *testing.T) {
	root := t.TempDir()
	want := writeGoWork(t, root)

	if got := FindGoWork(root); got != want {
		t.Errorf("FindGoWork(%q) = %q, want %q", root, got, want)
	}
}

func TestFindGoWork_AncestorDir(t *testing.T) {
	root := t.TempDir()
	want := writeGoWork(t, root)

	proj := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := FindGoWork(proj); got != want {
		t.Errorf("FindGoWork(%q) = %q, want %q (nearest ancestor)", proj, got, want)
	}
}

func TestFindGoWork_NotFound(t *testing.T) {
	// t.TempDir() lives outside any repo go.work (under /tmp or /var/folders),
	// so a project here has no governing workspace file.
	proj := filepath.Join(t.TempDir(), "standalone")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := FindGoWork(proj); got != "" {
		t.Errorf("FindGoWork(%q) = %q, want \"\"", proj, got)
	}
}

// A leaked GOWORK=off must be dropped and replaced with the governing go.work so
// the workload compiles in workspace mode. This is the deploy regression this test guards.
func TestWorkspaceBuildEnv_PointsAtGoWorkAndStripsOff(t *testing.T) {
	root := t.TempDir()
	gowork := writeGoWork(t, root)
	proj := filepath.Join(root, "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	in := []string{"GOWORK=off", "PATH=/usr/bin", "FOO=bar"}
	out := WorkspaceBuildEnv(in, proj, "")

	wantGoWork := resolvedPath(t, gowork)
	if vals := goworkValues(out); len(vals) != 1 || vals[0] != wantGoWork {
		t.Fatalf("GOWORK = %v, want exactly [%q]", vals, wantGoWork)
	}
	if slices.Contains(out, "GOWORK=off") {
		t.Errorf("inherited GOWORK=off survived: %v", out)
	}
	for _, want := range []string{"PATH=/usr/bin", "FOO=bar"} {
		if !slices.Contains(out, want) {
			t.Errorf("unrelated env %q not preserved: %v", want, out)
		}
	}
	// Input must not be mutated.
	if in[0] != "GOWORK=off" {
		t.Errorf("input env was mutated: %v", in)
	}
}

// An explicit but unrelated GOWORK path is still overridden by the project's own
// governing workspace file.
func TestWorkspaceBuildEnv_OverridesInheritedPath(t *testing.T) {
	root := t.TempDir()
	gowork := writeGoWork(t, root)
	proj := filepath.Join(root, "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	out := WorkspaceBuildEnv([]string{"GOWORK=/elsewhere/go.work"}, proj, "")
	wantGoWork := resolvedPath(t, gowork)
	if vals := goworkValues(out); len(vals) != 1 || vals[0] != wantGoWork {
		t.Fatalf("GOWORK = %v, want exactly [%q]", vals, wantGoWork)
	}
}

// With no governing go.work the module is genuinely standalone: an inherited
// GOWORK=off is dropped (not re-asserted) and no GOWORK is added.
func TestWorkspaceBuildEnv_NoGoWorkDropsGoWork(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "standalone")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	out := WorkspaceBuildEnv([]string{"GOWORK=off", "PATH=/usr/bin"}, proj, "")
	if vals := goworkValues(out); len(vals) != 0 {
		t.Errorf("expected no GOWORK entry, got %v", vals)
	}
	if !slices.Contains(out, "PATH=/usr/bin") {
		t.Errorf("PATH not preserved: %v", out)
	}
}

func TestWorkspaceBuildEnv_SymlinkedWorkspaceKeepsGoPathIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a workspace reached through a directory symbolic link: Windows links directories with junctions (D-W6), which the EvalSymlinks audit (F17) covers")
	}

	parent := t.TempDir()
	physicalRoot := filepath.Join(parent, "physical")
	moduleRoot := filepath.Join(physicalRoot, "module")
	if err := os.MkdirAll(moduleRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(physicalRoot, "go.work"): "go 1.24\n\nuse ./module\n",
		filepath.Join(moduleRoot, "go.mod"):    "module example.test/module\n\ngo 1.24\n",
		filepath.Join(moduleRoot, "module.go"): "package module\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	logicalRoot := filepath.Join(parent, "logical")
	if err := os.Symlink(physicalRoot, logicalRoot); err != nil {
		t.Fatal(err)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go binary unavailable: %v", err)
	}

	// This is the prepared-runtime shape: the job context retains Conductor's
	// logical alias, while the runtime process inherited the physical checkout
	// as PWD. Keeping both entries makes cmd/go reject ./module/... even though
	// the module is present in go.work.
	inherited := setEnvValue(os.Environ(), "PWD", physicalRoot)
	env := WorkspaceBuildEnv(inherited, logicalRoot, goBinary)
	if got := envValue(env, "PWD"); got != "" {
		t.Fatalf("PWD = %q, want removed so cmd/go derives the child cwd", got)
	}
	if got, want := envValue(env, "GOWORK"), resolvedPath(t, filepath.Join(physicalRoot, "go.work")); got != want {
		t.Fatalf("GOWORK = %q, want physical workspace path %q", got, want)
	}

	cmd := exec.Command(goBinary, "list", "./module/...")
	cmd.Dir = logicalRoot
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go list through logical workspace alias: %v\n%s", err, output)
	}
}

func TestGoCommandEnvMapsManagedCacheAndCreatesDirectories(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "a-child-go-invocation-runs-against-the-managed-caches")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	env := GoCommandEnv([]string{"PUTNAMI_GO_CACHE_DIR=" + cacheRoot, testOriginEnv}, "")

	buildCache := filepath.Join(cacheRoot, "build")
	moduleCache := filepath.Join(cacheRoot, "mod")
	for _, want := range []string{
		"GOCACHE=" + buildCache,
		"GOMODCACHE=" + moduleCache,
		"GOTOOLCHAIN=local",
		"GOPROXY=" + defaultGoProxy,
		"GONOPROXY=" + testModulePattern,
		"GONOSUMDB=" + testModulePattern,
	} {
		if !slices.Contains(env, want) {
			t.Errorf("GoCommandEnv missing %q: %v", want, env)
		}
	}
	for _, path := range []string{buildCache, moduleCache} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Errorf("cache directory %q was not created: %v", path, err)
		}
	}
}

// TestGoCommandEnvNeverLeadsWithTheModuleOrigin pins the checksum-database
// contract. Go asks only the FIRST GOPROXY entry whether it relays the checksum
// database. go.putnami.dev answers 200 on /sumdb/<name>/supported through its
// vanity-import catch-all and then 404s every tile, and Go never falls back to
// the next proxy once it has picked a base. Leading with the origin therefore
// broke every module download, public ones included.
func TestGoCommandEnvNeverLeadsWithTheModuleOrigin(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "the-proxy-chain-never-leads-with-the-module-origin")
	env := GoCommandEnv([]string{testOriginEnv}, "")

	proxy := envValue(env, "GOPROXY")
	leader, _, _ := strings.Cut(proxy, ",")
	leader, _, _ = strings.Cut(leader, "|")
	if strings.Contains(leader, testModuleOrigin) {
		t.Fatalf("GOPROXY leads with the module origin (%q); the sumdb probe would never fall back", proxy)
	}
	if got := envValue(env, "GONOPROXY"); !strings.Contains(got, testModulePattern) {
		t.Fatalf("GONOPROXY = %q, want %q so the origin's modules still resolve from it", got, testModulePattern)
	}
}

func TestGoCommandEnvRepairsInheritedOriginLeadingProxy(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "an-inherited-origin-leading-proxy-chain-is-repaired")
	env := GoCommandEnv([]string{
		testOriginEnv,
		"GOPROXY=https://" + testModuleOrigin + ",https://proxy.golang.org,direct",
		"GONOPROXY=corp.example/*",
	}, "")

	if got, want := envValue(env, "GOPROXY"), defaultGoProxy; got != want {
		t.Fatalf("GOPROXY = %q, want repaired default %q", got, want)
	}
	if got, want := envValue(env, "GONOPROXY"), "corp.example/*,"+testModulePattern; got != want {
		t.Fatalf("GONOPROXY = %q, want %q", got, want)
	}
}

// TestGoCommandEnvLeavesCallerProxyRoutingIntact pins the other half of that
// contract. GONOPROXY ships with our default GOPROXY and only with it: a caller
// that names a proxy — the publish smoke test against a token-authenticated
// registry, a job pinned to a local fixture — expects it to serve
// go.putnami.dev/* as well.
func TestGoCommandEnvLeavesCallerProxyRoutingIntact(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "caller-supplied-proxy-routing-is-left-intact")
	env := GoCommandEnv([]string{testOriginEnv, "GOPROXY=https://registry.example.test,off"}, "")

	// "none", not "": Go falls GONOPROXY back to GOPRIVATE whenever GONOPROXY is
	// empty, and GOPRIVATE can come from Go's own env file, which this process
	// environment cannot see. An empty value would let a developer's
	// `go env -w GOPRIVATE=<origin>/*` bypass the proxy the caller just chose.
	if got, want := envValue(env, "GONOPROXY"), "none"; got != want {
		t.Fatalf("GONOPROXY = %q, want %q: the caller's proxy must serve the origin's modules", got, want)
	}
}

// TestGoCommandEnvOverridesFileLevelPrivateRouting is the regression proof for
// that fallback. A developer machine carrying `GOPRIVATE=<origin>/*` in
// `go env -w` sent a job whose caller named a fixture proxy straight to the real
// origin, which answered 401. The process environment cannot see or edit Go's
// env file, so the only fix is emitting a non-empty GONOPROXY.
func TestGoCommandEnvOverridesFileLevelPrivateRouting(t *testing.T) {
	env := GoCommandEnv([]string{testOriginEnv, "GOPROXY=file:///tmp/fixture-proxy"}, "")

	if got := envValue(env, "GONOPROXY"); got == "" {
		t.Fatal("GONOPROXY is empty; Go would fall back to a file-level GOPRIVATE and bypass the fixture proxy")
	}
	if got := envValue(env, "GONOPROXY"); strings.Contains(got, testModuleOrigin) {
		t.Fatalf("GONOPROXY = %q, want the origin routed through the caller's proxy", got)
	}
}

// TestGoCommandEnvDropsInheritedNoProxyWhenCallerNamesAProxy covers the pairing
// surviving process inheritance. An outer job exports the companion GONOPROXY;
// a descendant that sets only GOPROXY inherits it and would keep routing
// go.putnami.dev/* past the very proxy it just chose. Patterns the caller owns
// stay put.
func TestGoCommandEnvDropsInheritedNoProxyWhenCallerNamesAProxy(t *testing.T) {
	env := GoCommandEnv([]string{
		testOriginEnv,
		"GOPROXY=http://127.0.0.1:8080",
		"GONOPROXY=corp.example/*," + testModulePattern + ",other.example/*",
	}, "")

	if got, want := envValue(env, "GONOPROXY"), "corp.example/*,other.example/*"; got != want {
		t.Fatalf("GONOPROXY = %q, want %q", got, want)
	}
}

func TestGoCommandEnvPreservesExplicitCredentialAndProxySettings(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "explicit-credential-and-proxy-settings-are-preserved")
	env := GoCommandEnv([]string{
		"HOME=/home/ignored",
		"PUTNAMI_HOME=/putnami/ignored",
		"NETRC=/caller/.netrc",
		testOriginEnv,
		"GOPROXY=https://proxy.example.test,direct",
		"GONOPROXY=corp.example/*",
		"GONOSUMDB=corp.example/*,go.putnami.dev/*",
		"GOTOOLCHAIN=auto",
	}, "")

	// GONOPROXY stays untouched: the caller named a proxy and expects it to
	// serve go.putnami.dev/* too. Injecting our pattern would route those
	// modules past the very proxy the caller selected.
	for _, want := range []string{
		"NETRC=/caller/.netrc",
		"GOPROXY=https://proxy.example.test,direct",
		"GONOPROXY=corp.example/*",
		"GONOSUMDB=corp.example/*,go.putnami.dev/*",
		"GOTOOLCHAIN=local",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("GoCommandEnv missing preserved/normalized %q: %v", want, env)
		}
	}
	for _, key := range []string{"NETRC", "GOPROXY", "GONOPROXY", "GONOSUMDB", "GOTOOLCHAIN"} {
		if got := envKeyCount(env, key); got != 1 {
			t.Errorf("%s entries = %d, want exactly 1: %v", key, got, env)
		}
	}
}

func TestGoCommandEnvUsesManagedToolchainForChild(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-toolchain", "a-child-go-invocation-runs-against-the-managed-toolchain")
	if os.PathSeparator == '\\' {
		t.Skip("the legacy managed layout reaches go through a symbolic link, which Windows does not use; TestResolveGoOnWindowsFindsTheManagedInstallsWithoutALink covers Windows")
	}

	workspace := t.TempDir()
	goRoot := filepath.Join(workspace, ".putnami", "managed", "go-1.25", "go")
	actualBinary := filepath.Join(goRoot, "bin", goBinaryName())
	capture := filepath.Join(t.TempDir(), "child-env")
	if err := os.MkdirAll(filepath.Dir(actualBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$GOROOT\" \"$PATH\" > \"$CAPTURE\"\n"
	if err := os.WriteFile(actualBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	managedBinary := legacyManagedGoPath(workspace)
	if err := os.MkdirAll(filepath.Dir(managedBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actualBinary, managedBinary); err != nil {
		t.Fatal(err)
	}
	resolvedGoRoot, err := filepath.EvalSymlinks(goRoot)
	if err != nil {
		t.Fatal(err)
	}

	clearManagedGoResolutionEnv(t)
	t.Setenv("PUTNAMI_WORKSPACE_ROOT", workspace)
	t.Setenv("PATH", "")
	t.Setenv("CAPTURE", capture)
	resolvedBinary, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo with PATH-less managed workspace: %v", err)
	}
	if resolvedBinary != managedBinary {
		t.Fatalf("ResolveGo = %q, want managed binary %q", resolvedBinary, managedBinary)
	}

	env := GoCommandEnv(os.Environ(), resolvedBinary)
	cmd := exec.Command(resolvedBinary)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("managed child: %v: %s", err, out)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("captured child env = %q, want GOROOT and PATH", data)
	}
	if lines[0] != resolvedGoRoot {
		t.Errorf("child GOROOT = %q, want %q", lines[0], resolvedGoRoot)
	}
	pathEntries := filepath.SplitList(lines[1])
	if len(pathEntries) == 0 || pathEntries[0] != filepath.Join(resolvedGoRoot, "bin") {
		t.Errorf("child PATH = %q, want managed bin first", lines[1])
	}
}

// On Windows the system block spells the variable "Path". Aligning PATH with a
// managed GOROOT must prepend its bin directory to the caller's entries and
// leave one PATH entry, not replace the caller's PATH with the bin directory.
func TestEnvKeysFoldKeepsTheCallerPathBehindTheManagedGoRoot(t *testing.T) {
	sep := string(os.PathListSeparator)
	system := filepath.Join(string(filepath.Separator)+"Windows", "system32")
	user := filepath.Join(string(filepath.Separator)+"Users", "dev", "bin")
	goBin := filepath.Join(string(filepath.Separator)+"Users", "dev", "go", "bin")
	keys := envkeys.Keys{Fold: true}
	env := []string{"Path=" + system + sep + user, "PATHEXT=.COM;.EXE", "USERPROFILE=" + user}

	env = keys.Set(env, "PATH", prependPath(keys.Last(env, "PATH"), goBin))

	want := []string{"PATHEXT=.COM;.EXE", "USERPROFILE=" + user, "PATH=" + goBin + sep + system + sep + user}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	if got := (envkeys.Keys{}).Last([]string{"Path=" + system}, "PATH"); got != "" {
		t.Fatalf("exact matching read %q from a Path entry", got)
	}
}

// TestGoCommandEnvLeavesNetrcToTheUser pins the credential target. An unset
// NETRC makes Go read the standard ~/.netrc — the file the cloud writes with
// `registry-token --host <host> --materialize`, and the file every other Go tool
// on the machine reads. The previous default pointed Go at ~/.putnami/.netrc, a
// path nothing has ever written, so a private module fetch presented nothing.
func TestGoCommandEnvLeavesNetrcToTheUser(t *testing.T) {
	env := GoCommandEnv([]string{
		"PUTNAMI_HOME=/managed/home",
		"HOME=/caller/home",
	}, "")
	if envKeyCount(env, "NETRC") != 0 {
		t.Errorf("GoCommandEnv set NETRC; want it left unset so Go reads ~/.netrc: %v", env)
	}
}

// TestRegistryCredentialHost pins the host an install asks the cloud about: the
// DECLARED module origin, never GOPROXY's leading entry (which is the public
// mirror by construction).
func TestRegistryCredentialHost(t *testing.T) {
	if got := RegistryCredentialHost([]string{testOriginEnv}, ""); got != "go.putnami.dev" {
		t.Errorf("declared origin host = %q, want go.putnami.dev", got)
	}
	if got := RegistryCredentialHost([]string{"HOME=/x"}, ""); got != "" {
		t.Errorf("undeclared origin host = %q, want empty", got)
	}
	// A bare host (the shape a human writes) and an https URL agree.
	if got := RegistryCredentialHost([]string{goRegistryURLEnvVar + "=Vanity.Example.Test"}, ""); got != "vanity.example.test" {
		t.Errorf("bare declared host = %q, want vanity.example.test", got)
	}
	// The environment wins over the workspace file, exactly as moduleOriginHost
	// resolves it inside GoCommandEnv.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile),
		[]byte(`{"registries":{"go":{"origin":"https://modules.example.test"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RegistryCredentialHost(nil, root); got != "modules.example.test" {
		t.Errorf("workspace-declared host = %q, want modules.example.test", got)
	}
	// GOPROXY is not the credential host: it leads with the public mirror.
	if got := RegistryCredentialHost([]string{"GOPROXY=https://proxy.golang.org,direct"}, ""); got != "" {
		t.Errorf("GOPROXY-only env host = %q, want empty", got)
	}
}

func envKeyCount(env []string, key string) int {
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			count++
		}
	}
	return count
}

// TestGoCommandEnvHonorsAnyDeclaredVanityHost is the generalization proof. The
// framework ships to consumers who run their own vanity module server, so the
// protection must follow the host a workspace DECLARES rather than a host built
// into the tooling. A consumer's origin gets byte-identical treatment to
// Putnami's: repaired proxy chain, companion GONOPROXY, checksum exclusion.
func TestGoCommandEnvHonorsAnyDeclaredVanityHost(t *testing.T) {
	const host = "go.acme.test"
	env := GoCommandEnv([]string{
		goRegistryURLEnvVar + "=https://" + host,
		"GOPROXY=https://" + host + ",https://proxy.golang.org,direct",
	}, "")

	if got, want := envValue(env, "GOPROXY"), defaultGoProxy; got != want {
		t.Errorf("GOPROXY = %q, want repaired default %q", got, want)
	}
	for _, key := range []string{"GONOPROXY", "GONOSUMDB"} {
		if got, want := envValue(env, key), host+"/*"; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// Putnami's own host is not special-cased anywhere in the result.
	for _, entry := range env {
		if strings.Contains(entry, "putnami.dev") {
			t.Errorf("consumer environment leaked a Putnami host: %q", entry)
		}
	}
}

// TestGoCommandEnvInjectsNothingWithoutADeclaredOrigin covers the workspace that
// has no vanity server at all. Injecting a pattern there would exclude modules
// from the checksum database that genuinely belong in it, so the correct
// behavior is stock Go: no GONOPROXY, no GONOSUMDB, caller proxy untouched.
func TestGoCommandEnvInjectsNothingWithoutADeclaredOrigin(t *testing.T) {
	env := GoCommandEnv([]string{
		workspaceRootEnvVar + "=" + t.TempDir(),
		"GOPROXY=https://proxy.example.test,direct",
	}, "")

	for _, key := range []string{"GONOPROXY", "GONOSUMDB"} {
		if got := envValue(env, key); got != "" {
			t.Errorf("%s = %q, want empty with no declared origin", key, got)
		}
	}
	if got, want := envValue(env, "GOPROXY"), "https://proxy.example.test,direct"; got != want {
		t.Errorf("GOPROXY = %q, want the caller's value %q", got, want)
	}
}

// TestGoCommandEnvReadsTheOriginFromTheWorkspaceFile pins the declaration a
// consumer actually writes. GO_REGISTRY_URL is the publish-time form; a build
// job has only the workspace root, so the workspace file must carry the same
// declaration or every non-publish invocation loses the protection.
func TestGoCommandEnvReadsTheOriginFromTheWorkspaceFile(t *testing.T) {
	root := t.TempDir()
	config := `{"registries":{"go":{"origin":"https://go.acme.test"}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	env := GoCommandEnv([]string{workspaceRootEnvVar + "=" + root}, "")

	if got, want := envValue(env, "GONOPROXY"), "go.acme.test/*"; got != want {
		t.Errorf("GONOPROXY = %q, want %q from the workspace declaration", got, want)
	}
	if got, want := envValue(env, "GONOSUMDB"), "go.acme.test/*"; got != want {
		t.Errorf("GONOSUMDB = %q, want %q from the workspace declaration", got, want)
	}
}

// TestGoCommandEnvPrefersTheEnvironmentDeclaration keeps the publish path
// authoritative: a job that names its registry explicitly must not be
// overridden by the workspace default.
func TestGoCommandEnvPrefersTheEnvironmentDeclaration(t *testing.T) {
	root := t.TempDir()
	config := `{"registries":{"go":{"origin":"https://go.workspace.test"}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	env := GoCommandEnv([]string{
		workspaceRootEnvVar + "=" + root,
		goRegistryURLEnvVar + "=https://go.job.test",
	}, "")

	if got, want := envValue(env, "GONOPROXY"), "go.job.test/*"; got != want {
		t.Errorf("GONOPROXY = %q, want the job's declaration %q", got, want)
	}
}

// TestGoCommandEnvTakesOriginAndProxyFromTheRegistriesEntry is the distribution
// v2 seam: every registry endpoint the extension needs comes from the workspace
// `registries` section, keyed by ecosystem, whose shape this extension's own
// profile declares. Nothing is hard-coded and nothing is read from a publish
// option any more.
//
// The declared chain is what GOPROXY becomes, and the origin never leads it:
// Go asks only the first proxy whether it relays the checksum database, so an
// origin in that position pins a dead sumdb base for the whole run.
func TestGoCommandEnvTakesOriginAndProxyFromTheRegistriesEntry(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "registries-drive-go-origin", "origin-comes-from-registries")

	root := t.TempDir()
	config := `{"registries":{"go":{` +
		`"origin":"https://go.acme.test",` +
		`"proxy":["https://go.acme.test","https://mirror.acme.test","direct"]}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	env := GoCommandEnv([]string{workspaceRootEnvVar + "=" + root}, "")

	if got, want := envValue(env, "GOPROXY"), "https://mirror.acme.test,direct"; got != want {
		t.Errorf("GOPROXY = %q, want the declared chain without the origin %q", got, want)
	}
	for _, key := range []string{"GONOPROXY", "GONOSUMDB"} {
		if got, want := envValue(env, key), "go.acme.test/*"; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := RegistryCredentialHost(nil, root); got != "go.acme.test" {
		t.Errorf("credential host = %q, want the declared origin", got)
	}
}

// A workspace that declares an origin but no proxy list keeps the public
// default chain: `registries.go.proxy` configures the chain, it does not gate it.
func TestGoCommandEnvFallsBackToThePublicProxyChain(t *testing.T) {
	root := t.TempDir()
	config := `{"registries":{"go":{"origin":"https://go.acme.test"}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	env := GoCommandEnv([]string{workspaceRootEnvVar + "=" + root}, "")

	if got, want := envValue(env, "GOPROXY"), defaultGoProxy; got != want {
		t.Errorf("GOPROXY = %q, want the public default %q", got, want)
	}
}

// A job context carries the PROJECT's effective registries, which a project may
// override; the workspace document is the fallback, not the authority.
func TestContextRegistriesOverrideTheWorkspaceDocument(t *testing.T) {
	root := t.TempDir()
	config := `{"registries":{"go":{"origin":"https://go.workspace.test"}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	useContextRegistriesForTest(t, []byte(`{"go":{"origin":"https://go.project.test","proxy":["https://mirror.project.test"]}}`))

	env := GoCommandEnv([]string{workspaceRootEnvVar + "=" + root}, "")

	if got, want := envValue(env, "GONOPROXY"), "go.project.test/*"; got != want {
		t.Errorf("GONOPROXY = %q, want the context declaration %q", got, want)
	}
	if got, want := envValue(env, "GOPROXY"), "https://mirror.project.test"; got != want {
		t.Errorf("GOPROXY = %q, want the context chain %q", got, want)
	}
}

// A context with no `registries` member, or none carrying a `go` entry, leaves
// the workspace document in charge rather than blanking the declaration.
func TestContextWithoutAGoEntryKeepsTheWorkspaceDeclaration(t *testing.T) {
	root := t.TempDir()
	config := `{"registries":{"go":{"origin":"https://go.workspace.test"}}}`
	if err := os.WriteFile(filepath.Join(root, workspaceConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	useContextRegistriesForTest(t, []byte(`{"npm":{"publish":"https://npm.example.test"}}`))

	env := GoCommandEnv([]string{workspaceRootEnvVar + "=" + root}, "")

	if got, want := envValue(env, "GONOPROXY"), "go.workspace.test/*"; got != want {
		t.Errorf("GONOPROXY = %q, want the workspace declaration %q", got, want)
	}
}

// useContextRegistriesForTest records a job context's registries member and
// restores the process default afterwards. The recording is deliberately
// process-global — one extension process runs one job — so a test that sets it
// must also clear it.
func useContextRegistriesForTest(t *testing.T, registries []byte) {
	t.Helper()
	UseContextRegistries(registries)
	t.Cleanup(func() {
		declaredGoRegistryMu.Lock()
		defer declaredGoRegistryMu.Unlock()
		contextGoRegistries = nil
	})
}

// TestModuleDownloadsDisabledReadsTheLeadingProxyEntry pins the predicate the
// tidy phase decides on. Go stops at the `off` sentinel wherever it sits in the
// list, so only a list that LEADS with it forbids every download; anything else
// can still reach a proxy or a direct origin and must be attempted.
func TestModuleDownloadsDisabledReadsTheLeadingProxyEntry(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "offline-tidy-is-a-cacheable-no-op",
		"only-a-proxy-list-leading-with-off-disables-downloads")
	tests := []struct {
		proxy string
		want  bool
	}{
		{"off", true},
		{" off ", true},
		{"off,direct", true},
		{"off|https://proxy.example.test", true},
		{"", false},
		{"https://proxy.golang.org,direct", false},
		{"https://proxy.example.test,off", false},
		{"direct", false},
		{"file:///tmp/fixture-proxy", false},
		{"OFF", false},
	}
	for _, tt := range tests {
		env := []string{"GOPROXY=" + tt.proxy}
		if got := ModuleDownloadsDisabled(env); got != tt.want {
			t.Errorf("ModuleDownloadsDisabled(GOPROXY=%q) = %t, want %t", tt.proxy, got, tt.want)
		}
	}
	if ModuleDownloadsDisabled(nil) {
		t.Error("ModuleDownloadsDisabled(no GOPROXY) = true, want false")
	}
}

// TestGoCommandEnvPreservesDisabledModuleDownloads pins the agreement between
// the value a `go` command observes and the value a cache key hashes. GOPROXY is
// a declared cache-key input of build-tidy, and the key hashes the INHERITED
// value; the tidy phase reads the resolved one. An inherited `off` must survive
// resolution, whether or not the workspace declares a module origin, or the key
// and the behavior it describes are computed from two different strings.
func TestGoCommandEnvPreservesDisabledModuleDownloads(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "offline-tidy-is-a-cacheable-no-op",
		"an-inherited-off-survives-environment-resolution")
	for _, env := range [][]string{
		{"GOPROXY=off"},
		{testOriginEnv, "GOPROXY=off"},
	} {
		resolved := GoCommandEnv(env, "")
		if got := envValue(resolved, "GOPROXY"); got != "off" {
			t.Errorf("GoCommandEnv(%v) GOPROXY = %q, want the inherited \"off\"", env, got)
		}
		if !ModuleDownloadsDisabled(resolved) {
			t.Errorf("GoCommandEnv(%v) no longer disables module downloads", env)
		}
	}
}
