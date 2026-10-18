package lifecycle

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// placeholderClientVersion is the only go.putnami.dev/client release the
// fixture module proxy serves. The generated client requires v0.0.1, which no
// proxy serves.
const placeholderClientVersion = "v0.9.3"

// requireRealGo skips a test when no go command is on PATH, and points the go
// commands the test runs at the fixture module proxy at proxy, or at no proxy
// when proxy is "". GOENV=off keeps the machine's go env file, such as a
// GOPRIVATE that sends go.putnami.dev to its origin, out of the test.
func requireRealGo(t *testing.T, proxy string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command on PATH")
	}
	if proxy == "" {
		proxy = "off"
	}
	for key, value := range map[string]string{
		"GOPROXY": proxy, "GOENV": "off", "GOFLAGS": "-modcacherw", "GOMODCACHE": t.TempDir(),
		"GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "",
	} {
		t.Setenv(key, value)
	}
}

// writeClientModuleProxy writes a file module proxy that serves
// go.putnami.dev/client at placeholderClientVersion only, and returns its URL.
func writeClientModuleProxy(t *testing.T) string {
	t.Helper()
	return writeModuleProxy(t, "go.putnami.dev/client", placeholderClientVersion, "package client\n")
}

// writeModuleProxy writes a file module proxy that serves the module path at
// version only, as one package file, and returns its URL.
func writeModuleProxy(t *testing.T, path, version, source string) string {
	t.Helper()
	root := t.TempDir()
	versions := filepath.Join(root, filepath.FromSlash(path), "@v")
	goMod := "module " + path + "\n\ngo 1.22\n"
	writeRawFile(t, filepath.Join(versions, "list"), version+"\n")
	writeRawFile(t, filepath.Join(versions, version+".info"),
		`{"Version":"`+version+`","Time":"2026-01-01T00:00:00Z"}`)
	writeRawFile(t, filepath.Join(versions, version+".mod"), goMod)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	prefix := path + "@" + version + "/"
	for name, content := range map[string]string{"go.mod": goMod, "lib.go": source} {
		entry, err := writer.Create(prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writeRawFile(t, filepath.Join(versions, version+".zip"), archive.String())
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(root), "/")
}

// placeholderClientWorkspace writes the workspace of the deps tidy failure:
// apps/c imports the generated client of apps/b, whose go.mod requires
// go.putnami.dev/client at the v0.0.1 placeholder the client generator writes,
// and it requires libs/a, which it does not import. libs/a's putnami.json name
// is not its module path. clientReplace is the go.work replace directive that
// supplies go.putnami.dev/client, or "" for none.
//
// The workspace model is the one the declared-edge check reads after a
// provider view: apps/c requires libs/a in go.mod and declares libs/d in
// putnami.json, and imports neither.
func placeholderClientWorkspace(t *testing.T, clientReplace string) (root string, ws *workspace.Workspace) {
	t.Helper()
	root = t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename),
		`{"name":"client-ws","includes":["apps/b/clients/go","apps/c","libs/a"]}`)
	writeRawFile(t, filepath.Join(root, "libs", "a", "putnami.json"), `{"name":"libs/a"}`)
	writeRawFile(t, filepath.Join(root, "libs", "a", "go.mod"), "module example.com/a\n\ngo 1.22\n")
	writeRawFile(t, filepath.Join(root, "libs", "a", "a.go"), "package a\n")
	writeRawFile(t, filepath.Join(root, "apps", "b", "clients", "go", "putnami.json"), `{"name":"apps/b-client"}`)
	writeRawFile(t, filepath.Join(root, "apps", "b", "clients", "go", "go.mod"),
		"module example.com/b/clients/go\n\ngo 1.22\n\nrequire go.putnami.dev/client v0.0.1\n")
	writeRawFile(t, filepath.Join(root, "apps", "b", "clients", "go", "client.gen.go"),
		"package client\n\nimport _ \"go.putnami.dev/client\"\n")
	writeRawFile(t, filepath.Join(root, "apps", "c", "putnami.json"),
		"{\n  \"name\": \"apps/c\",\n  \"dependencies\": [\"libs/d\"]\n}\n")
	writeRawFile(t, filepath.Join(root, "apps", "c", "go.mod"), `module example.com/c

go 1.22

require (
	example.com/a v0.0.0
	example.com/b/clients/go v0.0.0
)

replace example.com/a => ../../libs/a

replace example.com/b/clients/go => ../b/clients/go
`)
	writeRawFile(t, filepath.Join(root, "apps", "c", "main.go"),
		"package main\n\nimport _ \"example.com/b/clients/go\"\n\nfunc main() {}\n")
	goWork := "go 1.22\n\nuse (\n\t./apps/b/clients/go\n\t./apps/c\n\t./libs/a\n)\n"
	if clientReplace != "" {
		goWork += "\nreplace " + clientReplace + "\n"
	}
	writeRawFile(t, filepath.Join(root, "go.work"), goWork)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	libA := &workspace.Project{ID: "/libs/a", Name: "libs/a", SourceName: "libs/a", Path: "libs/a"}
	libD := &workspace.Project{ID: "/libs/d", Name: "libs/d", SourceName: "libs/d", Path: "libs/d"}
	client := &workspace.Project{ID: "/apps/b/clients/go", Name: "apps/b-client", SourceName: "apps/b-client", Path: "apps/b/clients/go"}
	appC := &workspace.Project{
		ID: "/apps/c", Name: "apps/c", SourceName: "apps/c", Path: "apps/c",
		Config:       &wsproto.ProjectConfig{Name: "apps/c", Dependencies: []string{"libs/d"}},
		Dependencies: []string{"apps/b-client", "libs/a", "libs/d"},
		DependencySources: map[string]wsproto.DependencySource{
			"apps/b-client": wsproto.DependencySourceGoModule,
			"libs/a":        wsproto.DependencySourceDeclared,
		},
	}
	return root, workspace.NewWorkspace(root, nil, []*workspace.Project{libA, libD, client, appC})
}

// readModFile parses the go.mod at path.
func readModFile(t *testing.T, path string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return file
}

func requires(file *modfile.File, path string) bool {
	for _, require := range file.Require {
		if require.Mod.Path == path {
			return true
		}
	}
	return false
}

func replaces(file *modfile.File, path string) bool {
	for _, replace := range file.Replace {
		if replace.Old.Path == path {
			return true
		}
	}
	return false
}

// `go mod tidy` reads no go.work, so it asks the module proxy for the v0.0.1
// placeholder of a generated client, which no proxy serves. deps prune tidies
// apps/c as go.work resolves it, with the release go.work pins, and writes
// none of go.work's directives into apps/c's go.mod. The requirement it drops
// is named by the module path libs/a's go.mod declares, not by its putnami.json
// name.
func TestDepsPruneTidiesAModuleThatReachesAGeneratedClientPlaceholder(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-tidy-resolves-as-the-workspace-does",
		"deps-tidy-reads-the-go-work-replace-directives")
	requireRealGo(t, writeClientModuleProxy(t))
	root, ws := placeholderClientWorkspace(t,
		"go.putnami.dev/client => go.putnami.dev/client "+placeholderClientVersion)

	var out bytes.Buffer
	output, err := captureStdout(t, func() error {
		return prunePlan(context.Background(), root, ws, "", false, &out, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("deps prune: %v\n%s%s", err, out.String(), output)
	}
	if !strings.Contains(output, "Removing example.com/a in apps/c") {
		t.Errorf("prune output = %q, want it to remove the module path example.com/a", output)
	}
	goMod := readModFile(t, filepath.Join(root, "apps", "c", "go.mod"))
	if requires(goMod, "example.com/a") {
		t.Errorf("apps/c/go.mod still requires example.com/a")
	}
	if !requires(goMod, "example.com/b/clients/go") {
		t.Errorf("apps/c/go.mod lost the requirement its import backs")
	}
	if replaces(goMod, "go.putnami.dev/client") {
		t.Errorf("deps prune wrote go.work's replace directive into apps/c/go.mod")
	}
	goSum, err := os.ReadFile(filepath.Join(root, "apps", "c", "go.sum"))
	if err != nil || !strings.Contains(string(goSum), "go.putnami.dev/client "+placeholderClientVersion) {
		t.Errorf("apps/c/go.sum = %q, %v; want the checksum of the release go.work pins", goSum, err)
	}
	if config := readFile(t, filepath.Join(root, "apps", "c", "putnami.json")); strings.Contains(config, "libs/d") {
		t.Errorf("apps/c/putnami.json still declares libs/d:\n%s", config)
	}
}

// A Go step that fails leaves every file deps prune would edit as it was: the
// putnami.json entry it already dropped comes back, and go.mod and go.sum keep
// their bytes. Here no go.work directive supplies the generated client's
// placeholder, so the tidy fails.
func TestDepsPruneLeavesEveryFileAsItWasWhenTheGoStepFails(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-tidy-resolves-as-the-workspace-does",
		"a-failed-go-step-leaves-every-file-as-it-was")
	requireRealGo(t, writeClientModuleProxy(t))
	root, ws := placeholderClientWorkspace(t, "")
	files := []string{
		filepath.Join("apps", "c", "putnami.json"),
		filepath.Join("apps", "c", "go.mod"),
	}
	before := make(map[string]string, len(files))
	for _, file := range files {
		before[file] = readFile(t, filepath.Join(root, file))
	}

	var out bytes.Buffer
	output, err := captureStdout(t, func() error {
		return prunePlan(context.Background(), root, ws, "", false, &out, LifecycleEnv{})
	})
	if err == nil {
		t.Fatalf("deps prune succeeded although go mod tidy cannot resolve the placeholder:\n%s%s", out.String(), output)
	}
	for _, want := range []string{"go mod tidy", "back as it was"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to state %q", err, want)
		}
	}
	for _, file := range files {
		if after := readFile(t, filepath.Join(root, file)); after != before[file] {
			t.Errorf("a failed deps prune changed %s:\n%s", file, after)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "c", "go.sum")); !os.IsNotExist(err) {
		t.Errorf("a failed deps prune left apps/c/go.sum behind: %v", err)
	}
}

// deps remove tidies through the same step. A go.work replace directive that
// names a directory is relative to go.work, so the tidy states it from the
// module's own directory, and resolves with no module proxy at all.
func TestDepsRemoveTidiesThroughAGoWorkDirectoryReplace(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-tidy-resolves-as-the-workspace-does",
		"deps-tidy-reads-the-go-work-replace-directives")
	requireRealGo(t, "")
	root, _ := placeholderClientWorkspace(t, "go.putnami.dev/client => ./third_party/client")
	writeRawFile(t, filepath.Join(root, "third_party", "client", "go.mod"), "module go.putnami.dev/client\n\ngo 1.22\n")
	writeRawFile(t, filepath.Join(root, "third_party", "client", "client.go"), "package client\n")

	output, err := captureStdout(t, func() error {
		return DepsRemove(context.Background(), root, []string{"example.com/a"}, "apps/c", LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("deps remove: %v\n%s", err, output)
	}
	goMod := readModFile(t, filepath.Join(root, "apps", "c", "go.mod"))
	if requires(goMod, "example.com/a") {
		t.Errorf("apps/c/go.mod still requires example.com/a")
	}
	if replaces(goMod, "go.putnami.dev/client") {
		t.Errorf("deps remove wrote go.work's replace directive into apps/c/go.mod")
	}
}

// `go get` loads the module graph, so it resolves a go.mod requirement of
// `example.com/lib v0.0.0` through the module proxy and fails when none serves
// it: the placeholder a workspace module keeps after its local replace is gone.
// deps add resolves the module with `go list -m`, which loads no graph, and
// deps remove drops the requirement without resolving it. The proxy here serves
// example.com/lib v0.3.0 only, and the workspace has a go.work, where an
// @upgrade query would otherwise read the build list.
func TestDepsAddAndRemoveEditAModuleThatRequiresAnUnservedVersion(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "deps-edit-reads-no-replaced-version",
		"deps-add-and-remove-edit-a-module-that-requires-an-unserved-version")
	requireRealGo(t, writeModuleProxy(t, "example.com/lib", "v0.3.0", "package lib\n\nconst Name = \"lib\"\n")+","+
		writeModuleProxy(t, "example.com/dep", "v1.0.0", "package dep\n\nconst Name = \"dep\"\n"))
	root := t.TempDir()
	moduleDir := filepath.Join(root, "api")
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"api-ws","includes":["api"]}`)
	writeRawFile(t, filepath.Join(moduleDir, "putnami.json"), `{"name":"api"}`)
	writeRawFile(t, filepath.Join(root, "go.work"), "go 1.22\n\nuse ./api\n")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	unserved := "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n"
	reset := func(source string) {
		writeRawFile(t, filepath.Join(moduleDir, "go.mod"), unserved)
		writeRawFile(t, filepath.Join(moduleDir, "main.go"), source)
		if err := os.Remove(filepath.Join(moduleDir, "go.sum")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	importsLib := "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Name }\n"
	goMod := filepath.Join(moduleDir, "go.mod")

	for _, spec := range []string{"example.com/lib@v0.3.0", "example.com/lib@latest", "example.com/lib"} {
		reset(importsLib)
		output, err := captureStdout(t, func() error {
			return DepsAdd(context.Background(), root, []string{spec}, "", LifecycleEnv{})
		})
		if err != nil {
			t.Fatalf("deps add %s: %v\n%s", spec, err, output)
		}
		for _, require := range readModFile(t, goMod).Require {
			if require.Mod.Path == "example.com/lib" && require.Mod.Version != "v0.3.0" {
				t.Errorf("deps add %s: go.mod requires example.com/lib %s, want v0.3.0", spec, require.Mod.Version)
			}
		}
		if !requires(readModFile(t, goMod), "example.com/lib") {
			t.Errorf("deps add %s: go.mod lost the requirement:\n%s", spec, readFile(t, goMod))
		}
		if sum := readFile(t, filepath.Join(moduleDir, "go.sum")); !strings.Contains(sum, "example.com/lib v0.3.0") {
			t.Errorf("deps add %s: go.sum = %q, want the checksum of v0.3.0", spec, sum)
		}
		if _, err := os.Stat(filepath.Join(root, "go.work.sum")); !os.IsNotExist(err) {
			t.Errorf("deps add %s wrote go.work.sum: %v", spec, err)
		}
	}

	reset("package main\n\nfunc main() {}\n")
	output, err := captureStdout(t, func() error {
		return DepsRemove(context.Background(), root, []string{"example.com/lib"}, "", LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("deps remove: %v\n%s", err, output)
	}
	if requires(readModFile(t, goMod), "example.com/lib") {
		t.Errorf("deps remove: go.mod still requires example.com/lib:\n%s", readFile(t, goMod))
	}
	if _, err := os.Stat(filepath.Join(root, "go.work.sum")); !os.IsNotExist(err) {
		t.Errorf("deps remove wrote go.work.sum: %v", err)
	}

	// A bare spec queries @latest, which loads no module graph. An @upgrade
	// query would load it for a module go.mod does not require, where
	// example.com/lib v0.0.0 is unserved. go.work replaces lib with a directory
	// so the tidy resolves it.
	writeRawFile(t, filepath.Join(root, "lib", "go.mod"), "module example.com/lib\n\ngo 1.22\n")
	writeRawFile(t, filepath.Join(root, "lib", "lib.go"), "package lib\n\nconst Name = \"lib\"\n")
	writeRawFile(t, filepath.Join(root, "go.work"), "go 1.22\n\nuse ./api\n\nreplace example.com/lib => ./lib\n")
	reset("package main\n\nimport (\n\t\"example.com/dep\"\n\t\"example.com/lib\"\n)\n\nfunc main() { _, _ = lib.Name, dep.Name }\n")
	output, err = captureStdout(t, func() error {
		return DepsAdd(context.Background(), root, []string{"example.com/dep"}, "", LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("deps add example.com/dep: %v\n%s", err, output)
	}
	for _, require := range readModFile(t, goMod).Require {
		if require.Mod.Path == "example.com/dep" && require.Mod.Version != "v1.0.0" {
			t.Errorf("deps add example.com/dep: go.mod requires %s, want v1.0.0", require.Mod.Version)
		}
	}
	if !requires(readModFile(t, goMod), "example.com/dep") {
		t.Errorf("deps add example.com/dep: go.mod lacks the requirement:\n%s", readFile(t, goMod))
	}
}

// deps prune names a Go requirement by the module path the target's go.mod
// declares. The provider view replaces that path with the putnami.json name,
// and `go mod edit -droprequire=<name>` drops nothing.
func TestDepsPruneDropsTheModulePathTheTargetGoModDeclares(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "declared-edge-check",
		"prune-drops-the-module-path-the-target-go-mod-declares")
	f := newGoCreateFixture(t)
	f.withGoProject(t)
	writeRawFile(t, filepath.Join(f.root, "lib", "putnami.json"), `{"name":"libs/lib"}`)
	writeRawFile(t, filepath.Join(f.root, "lib", "go.mod"), "module example.com/lib\n\ngo "+goFixtureVersion+"\n")
	writeRawFile(t, filepath.Join(f.root, "app", "go.mod"),
		"module example.com/app\n\ngo "+goFixtureVersion+"\n\nrequire example.com/lib v0.0.0\n")
	f.pin(t)
	f.writeGo(t, f.pinned, goFixtureVersion)
	ws := workspace.NewWorkspace(f.root, nil, []*workspace.Project{
		{ID: "/lib", Name: "libs/lib", SourceName: "libs/lib", Path: "lib"},
		{
			ID: "/app", Name: "app", SourceName: "app", Path: "app",
			Dependencies:      []string{"libs/lib"},
			DependencySources: map[string]wsproto.DependencySource{"libs/lib": wsproto.DependencySourceDeclared},
		},
	})

	var out bytes.Buffer
	output, err := captureStdout(t, func() error {
		return prunePlan(context.Background(), f.root, ws, "", false, &out, f.refusingGoInstall(t))
	})
	if err != nil {
		t.Fatalf("deps prune: %v\n%s%s", err, out.String(), output)
	}
	want := []string{"mod edit -droprequire=example.com/lib|" + f.pinned, "mod tidy -modfile=<copy>|" + f.pinned}
	if got := f.depsCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("go calls =\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(output, "Removing example.com/lib in app") {
		t.Errorf("prune output = %q, want it to name the module path", output)
	}
}

// The tidy copy states a go.work replace directive unless a directive of the
// module covers one of the versions it covers, and dropping what it stated
// gives back the module's go.mod. A go.work this parser does not read adds
// nothing.
func TestWithGoWorkReplacesStatesOnlyWhatTheModuleDoesNot(t *testing.T) {
	root := t.TempDir()
	goWork := filepath.Join(root, "go.work")
	writeRawFile(t, goWork, `go 1.22

use ./app

replace (
	example.com/forked => example.com/forked v1.2.0
	example.com/pinned v0.0.1 => example.com/pinned v0.9.3
	example.com/pinned v0.0.2 => example.com/pinned v0.9.3
	go.putnami.dev/client => go.putnami.dev/client v0.9.3
	go.putnami.dev/local => ./third_party/local
)
`)
	goModPath := filepath.Join(root, "app", "go.mod")
	goMod := []byte(`module example.com/app

go 1.22

replace example.com/forked => ../forked

replace example.com/pinned v0.0.1 => ../pinned

replace go.putnami.dev/client v0.0.0 => ../old-client
`)
	copied, added, err := withGoWorkReplaces(goModPath, goMod, goWork)
	if err != nil {
		t.Fatal(err)
	}
	var stated []string
	for _, old := range added {
		stated = append(stated, old.String())
	}
	if got, want := strings.Join(stated, ","), "example.com/pinned@v0.0.2,go.putnami.dev/local"; got != want {
		t.Fatalf("stated replaces = %s, want %s", got, want)
	}
	file, err := modfile.Parse(goModPath, copied, nil)
	if err != nil {
		t.Fatal(err)
	}
	targets := make(map[string]string)
	for _, replace := range file.Replace {
		targets[replace.Old.String()] = replace.New.String()
	}
	if got, want := targets["go.putnami.dev/local"], filepath.Join(root, "third_party", "local"); got != want {
		t.Errorf("go.putnami.dev/local => %s, want the directory from go.work's directory, %s", got, want)
	}
	if got := targets["example.com/forked"]; got != "../forked" {
		t.Errorf("example.com/forked => %s, want the module's own directive", got)
	}
	restored, err := withoutReplaces(goModPath, copied, added)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(goMod) {
		t.Errorf("dropping the stated directives left:\n%s\nwant:\n%s", restored, goMod)
	}

	writeRawFile(t, goWork, "go 1.22\n\nnot-a-directive\n")
	if copied, added, err := withGoWorkReplaces(goModPath, goMod, goWork); err != nil || len(added) != 0 || string(copied) != string(goMod) {
		t.Errorf("an unreadable go.work = %q, %v, %v; want go.mod as it is", copied, added, err)
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
