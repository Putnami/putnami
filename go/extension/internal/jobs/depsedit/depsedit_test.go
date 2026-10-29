package depsedit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

func TestMain(m *testing.M) {
	jobtest.ServeFake()
	os.Exit(m.Run())
}

// editRun is one deps-edit run under test.
type editRun struct {
	rec    *jobtest.Recorder
	status string
	// goCalls are the go commands the job ran on a module, each as its
	// arguments after -C <dir>, then "|" and the module directory relative to
	// the workspace. Only a run with a recording go has them.
	goCalls []string
}

// runEdit runs the job in-process on ws with the job parameters params.
// proxy is the GOPROXY the go command reads. PATH holds the shell traps and a
// go command that records every call and runs the real one, unless extra sets
// PATH; extra environment entries win.
func runEdit(t *testing.T, ws, params, proxy string, extra ...string) *editRun {
	t.Helper()
	fakes := jobtest.NewFakes(t)
	recording := false
	if goBinary, err := exec.LookPath("go"); err == nil {
		fakes.RecordingGo(t, goBinary)
		recording = true
	}
	overrides := append(append(fakes.Environ(), "PATH="+fakes.Dir), extra...)
	env := jobtest.Env(t, proxy, jobtest.CacheRoot(t), overrides...)
	rec := &jobtest.Recorder{}
	j, stdout, _ := jobtest.NewJob(t, rec, env, ws)

	result := &editRun{rec: rec}
	result.status = run(j, jobtest.Params(t, params), filepath.Join(ws, "context.json"))
	if stdout.Len() > 0 {
		t.Errorf("a command wrote to the job event stream:\n%s", stdout)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("deps-edit started shell programs: %v", invocations)
	}
	if recording {
		for _, record := range fakes.GoRecords(t) {
			if len(record.Args) < 3 || record.Args[0] != "-C" {
				continue
			}
			args := strings.Join(record.Args[2:], " ")
			if i := strings.Index(args, "-modfile="); i >= 0 {
				args = args[:i] + "-modfile=<copy>"
			}
			rel, err := filepath.Rel(ws, record.Args[1])
			if err != nil {
				t.Fatal(err)
			}
			result.goCalls = append(result.goCalls, args+"|"+filepath.ToSlash(rel))
		}
	}
	return result
}

func (r *editRun) requireStatus(t *testing.T, want string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("deps-edit = %s, want %s:\n%s", r.status, want, r.rec.Transcript())
	}
}

func (r *editRun) requireTranscript(t *testing.T, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !r.rec.Contains(want) {
			t.Errorf("the transcript does not say %q:\n%s", want, r.rec.Transcript())
		}
	}
}

func (r *editRun) requireGoCalls(t *testing.T, want ...string) {
	t.Helper()
	if strings.Join(r.goCalls, "\n") != strings.Join(want, "\n") {
		t.Errorf("go calls =\n%s\nwant:\n%s", strings.Join(r.goCalls, "\n"), strings.Join(want, "\n"))
	}
}

// editParams is the params document of an action on edits.
func editParams(t *testing.T, action string, edits ...edit) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"action": action, "edits": edits})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// libProxy is a file:// module proxy that lists each module at the versions
// given, and serves each version a package that declares Name.
func libProxy(t *testing.T, versions map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	for module, list := range versions {
		name := filepath.Base(module)
		for _, version := range list {
			jobtest.WriteModuleProxyAt(t, dir, jobtest.ProxyModule{
				Path:    module,
				Version: version,
				GoMod:   "module " + module + "\n\ngo 1.22\n",
				Source:  "package " + name + "\n\nconst Name = \"" + name + "\"\n",
			})
		}
		jobtest.WriteVersionList(t, dir, module, list...)
	}
	return jobtest.FileURL(dir)
}

// requires reports the version the go.mod at path requires module at, or "".
func requires(t *testing.T, path, module string) string {
	t.Helper()
	file, err := modfile.Parse(path, []byte(jobtest.ReadFile(t, path)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, require := range file.Require {
		if require.Mod.Path == module {
			return require.Mod.Version
		}
	}
	return ""
}

// replaces reports whether the go.mod at path replaces module.
func replaces(t *testing.T, path, module string) bool {
	t.Helper()
	file, err := modfile.Parse(path, []byte(jobtest.ReadFile(t, path)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, replace := range file.Replace {
		if replace.Old.Path == module {
			return true
		}
	}
	return false
}

// `go get` loads the module graph, so it resolves a requirement of
// example.com/lib v0.0.0 through the module proxy and fails when none serves
// it: the placeholder a workspace module keeps after its local replace is gone.
// deps add resolves the module with `go list -m`, which loads no graph, and
// deps remove drops the requirement without resolving it. The proxy serves
// example.com/lib v0.3.0 only, and the workspace has a go.work, where an
// @upgrade query would read the build list.
func TestDepsEditAddAndRemoveEditAModuleThatRequiresAnUnservedVersion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-edit-reads-no-replaced-version",
		"add-and-remove-edit-a-module-that-requires-an-unserved-version")
	spectest.Proves(t, "go/deps-edit", "deps-run-the-pinned-go",
		"edit-runs-every-go-command-with-the-go-the-extension-resolves")
	jobtest.RequireGo(t)
	proxy := libProxy(t, map[string][]string{"example.com/lib": {"v0.3.0"}, "example.com/dep": {"v1.0.0"}})
	unserved := "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n"
	importsLib := "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Name }\n"
	newWorkspace := func(t *testing.T, source string) (string, string) {
		ws := jobtest.RealTempDir(t)
		jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n")
		jobtest.WriteFile(t, ws, "api/main.go", source)
		return ws, jobtest.WriteFile(t, ws, "api/go.mod", unserved)
	}

	for _, spec := range []string{"example.com/lib@v0.3.0", "example.com/lib@latest", "example.com/lib"} {
		t.Run("add "+spec, func(t *testing.T) {
			t.Parallel()
			ws, goMod := newWorkspace(t, importsLib)
			run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{spec}}), proxy)
			run.requireStatus(t, statusOK)
			run.requireTranscript(t, "Adding "+spec+" in api...", "✓ updated api/go.mod")
			query := spec
			if !strings.Contains(spec, "@") {
				query += "@latest"
			}
			run.requireGoCalls(t,
				"list -m -json -mod=readonly "+query+"|api",
				"mod edit -require=example.com/lib@v0.3.0|api",
				"mod tidy -modfile=<copy>|api")
			if got := requires(t, goMod, "example.com/lib"); got != "v0.3.0" {
				t.Errorf("go.mod requires example.com/lib %q, want v0.3.0", got)
			}
			if sum := jobtest.ReadFile(t, filepath.Join(ws, "api", "go.sum")); !strings.Contains(sum, "example.com/lib v0.3.0") {
				t.Errorf("go.sum = %q, want the checksum of v0.3.0", sum)
			}
			if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, "go.work.sum")); ok {
				t.Error("deps add wrote go.work.sum")
			}
		})
	}

	t.Run("remove", func(t *testing.T) {
		t.Parallel()
		ws, goMod := newWorkspace(t, "package main\n\nfunc main() {}\n")
		run := runEdit(t, ws, editParams(t, actionRemove, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib@v0.3.0"}}), proxy)
		run.requireStatus(t, statusOK)
		run.requireTranscript(t, "Removing example.com/lib@v0.3.0 in api...")
		run.requireGoCalls(t, "mod edit -droprequire=example.com/lib|api", "mod tidy -modfile=<copy>|api")
		if got := requires(t, goMod, "example.com/lib"); got != "" {
			t.Errorf("go.mod still requires example.com/lib %s", got)
		}
	})

	// A bare spec queries @latest, which loads no module graph; an @upgrade
	// query would load it, where example.com/lib v0.0.0 is unserved. go.work
	// replaces lib with a directory, so the tidy resolves it.
	t.Run("add beside a go.work replace", func(t *testing.T) {
		t.Parallel()
		ws, goMod := newWorkspace(t, "package main\n\nimport (\n\t\"example.com/dep\"\n\t\"example.com/lib\"\n)\n\nfunc main() { _, _ = lib.Name, dep.Name }\n")
		jobtest.WriteFile(t, ws, "lib/go.mod", "module example.com/lib\n\ngo 1.22\n")
		jobtest.WriteFile(t, ws, "lib/lib.go", "package lib\n\nconst Name = \"lib\"\n")
		jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n\nreplace example.com/lib => ./lib\n")
		run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/dep"}}), proxy)
		run.requireStatus(t, statusOK)
		if got := requires(t, goMod, "example.com/dep"); got != "v1.0.0" {
			t.Errorf("go.mod requires example.com/dep %q, want v1.0.0", got)
		}
		if replaces(t, goMod, "example.com/lib") {
			t.Error("deps add wrote go.work's replace directive into go.mod")
		}
	})
}

// Under the param dry-run, deps add and deps remove make the edit and the
// tidy on a copy of go.mod and go.sum, report each requirement the edit
// changes and the go.sum lines it changes, and leave every file of the
// workspace as it was.
func TestDepsEditDryRunReportsTheEditAndWritesNoFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-dry-run-writes-no-file",
		"add-and-remove-under-dry-run-report-the-edit-and-write-no-file")
	jobtest.RequireGo(t)
	proxy := libProxy(t, map[string][]string{"example.com/lib": {"v0.3.0"}})
	dryRunParams := func(t *testing.T, action string, e edit) string {
		t.Helper()
		data, err := json.Marshal(map[string]any{"action": action, "dry-run": true, "edits": []edit{e}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// newWorkspace writes a go.work with the module api, which requires
	// example.com/lib v0.0.0, and the go.sum line of an earlier dependency.
	newWorkspace := func(t *testing.T, source string) string {
		ws := jobtest.RealTempDir(t)
		jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n")
		jobtest.WriteFile(t, ws, "api/main.go", source)
		jobtest.WriteFile(t, ws, "api/go.mod", "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n")
		jobtest.WriteFile(t, ws, "api/go.sum", "example.com/old v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
		return ws
	}
	// visiblePlan requires one summary the CLI always prints, holding wants.
	visiblePlan := func(t *testing.T, run *editRun, wants ...string) {
		t.Helper()
		var plans []string
		for _, event := range run.rec.Events() {
			if data, _ := event.Value.(map[string]any); event.Kind == "summary" && data["visibility"] == "always" {
				plans = append(plans, event.Message)
			}
		}
		if len(plans) != 1 {
			t.Fatalf("the dry run emitted %d always-visible summaries, want 1:\n%s", len(plans), run.rec.Transcript())
		}
		for _, want := range wants {
			if !strings.Contains(plans[0], want) {
				t.Errorf("the plan does not say %q:\n%s", want, plans[0])
			}
		}
	}
	unchanged := func(t *testing.T, ws string) func() {
		t.Helper()
		files := []string{"go.work", "api/go.mod", "api/go.sum", "api/main.go"}
		before := map[string]string{}
		for _, file := range files {
			before[file] = jobtest.ReadFile(t, filepath.Join(ws, file))
		}
		return func() {
			t.Helper()
			for _, file := range files {
				if after := jobtest.ReadFile(t, filepath.Join(ws, file)); after != before[file] {
					t.Errorf("the dry run changed %s:\nbefore:\n%s\nafter:\n%s", file, before[file], after)
				}
			}
			if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, "go.work.sum")); ok {
				t.Error("the dry run wrote go.work.sum")
			}
		}
	}

	t.Run("add", func(t *testing.T) {
		t.Parallel()
		ws := newWorkspace(t, "package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Name }\n")
		check := unchanged(t, ws)
		run := runEdit(t, ws, dryRunParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib@v0.3.0"}}), proxy)
		run.requireStatus(t, statusOK)
		run.requireTranscript(t,
			"Planning to add example.com/lib@v0.3.0 in api...",
			"Planned edit of api/go.mod:",
			"~ require example.com/lib v0.0.0 -> v0.3.0",
			"go.sum: 2 line(s) added, 1 removed",
			"Dry run complete; no Go files were changed")
		visiblePlan(t, run, "Planned edit of api/go.mod:", "~ require example.com/lib v0.0.0 -> v0.3.0", "no Go files were changed")
		if run.rec.Contains("✓ updated") {
			t.Errorf("the dry run says it updated a file:\n%s", run.rec.Transcript())
		}
		check()
	})

	t.Run("remove", func(t *testing.T) {
		t.Parallel()
		ws := newWorkspace(t, "package main\n\nfunc main() {}\n")
		check := unchanged(t, ws)
		run := runEdit(t, ws, dryRunParams(t, actionRemove, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib"}}), proxy)
		run.requireStatus(t, statusOK)
		run.requireTranscript(t,
			"Planning to remove example.com/lib in api...",
			"Planned edit of api/go.mod:",
			"- require example.com/lib v0.0.0",
			"go.sum: 0 line(s) added, 1 removed",
			"Dry run complete; no Go files were changed")
		visiblePlan(t, run, "Planned edit of api/go.mod:", "- require example.com/lib v0.0.0")
		if run.rec.Contains("✓ updated") {
			t.Errorf("the dry run says it updated a file:\n%s", run.rec.Transcript())
		}
		check()
	})
}

// A bare spec takes the latest version the proxy answers, unless go.mod
// requires a newer one: `go get` never downgrades a module the way a plain
// @latest would.
func TestDepsEditAddKeepsARequiredVersionNewerThanTheLatest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-edit-reads-no-replaced-version",
		"add-keeps-a-required-version-newer-than-the-latest")
	jobtest.RequireGo(t)
	// The proxy lists v0.3.0 as the latest and serves v0.4.0 too, the version
	// go.mod requires.
	proxyDir := t.TempDir()
	for _, version := range []string{"v0.3.0", "v0.4.0"} {
		jobtest.WriteModuleProxyAt(t, proxyDir, jobtest.SimpleProxyModules(version, "example.com/lib")...)
	}
	jobtest.WriteVersionList(t, proxyDir, "example.com/lib", "v0.3.0")
	ws := jobtest.RealTempDir(t)
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.4.0\n")
	jobtest.WriteFile(t, ws, "api/main.go", "package main\n\nimport _ \"example.com/lib\"\n\nfunc main() {}\n")

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib"}}), jobtest.FileURL(proxyDir))
	run.requireStatus(t, statusOK)
	run.requireGoCalls(t,
		"list -m -json -mod=readonly example.com/lib@latest|api",
		"mod edit -require=example.com/lib@v0.4.0|api",
		"mod tidy -modfile=<copy>|api")
	if got := requires(t, goMod, "example.com/lib"); got != "v0.4.0" {
		t.Errorf("go.mod requires example.com/lib %q, want v0.4.0 kept", got)
	}
}

// A bare spec means @upgrade: the latest version, unless go.mod requires a
// newer one, which stays. An exact version or a module go.mod does not require
// is taken as it was resolved.
func TestKeepNewerRequiredNeverDowngradesABareSpec(t *testing.T) {
	t.Parallel()
	resolved := []workspacejob.Module{
		{Path: "example.com/newer", Version: "v0.3.0"},
		{Path: "example.com/older", Version: "v0.3.0"},
		{Path: "example.com/same", Version: "v0.3.0"},
		{Path: "example.com/placeholder", Version: "v0.3.0"},
		{Path: "example.com/pinned", Version: "v0.3.0"},
		{Path: "example.com/new", Version: "v0.3.0"},
		{Path: "example.com/pseudo", Version: "v0.3.0"},
	}
	bare := []bool{true, true, true, true, false, true, true}
	required := map[string]string{
		"example.com/newer":       "v0.4.0",
		"example.com/older":       "v0.2.0",
		"example.com/same":        "v0.3.0",
		"example.com/placeholder": "v0.0.0",
		"example.com/pinned":      "v0.4.0",
		"example.com/pseudo":      "v0.3.1-0.20260101000000-abcdefabcdef",
	}
	want := []string{"v0.4.0", "v0.3.0", "v0.3.0", "v0.3.0", "v0.3.0", "v0.3.0", "v0.3.1-0.20260101000000-abcdefabcdef"}
	for i, m := range keepNewerRequired(resolved, bare, required) {
		if m.Version != want[i] {
			t.Errorf("%s = %s, want %s", m.Path, m.Version, want[i])
		}
	}
}

// A go list -m that fails stops deps add before any edit and leaves go.mod as
// it was; the error quotes go and names the rule a spec follows.
func TestDepsEditAddReportsAFailedGoList(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-edit-reads-no-replaced-version",
		"add-reports-a-failed-go-list")
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	unchanged := "module example.com/api\n\ngo 1.22\n"
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", unchanged)

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib@v1.0.0"}}),
		libProxy(t, map[string][]string{"example.com/dep": {"v1.0.0"}}))
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "go list -m", "a spec names a module path, not a package path", "example.com/lib@v1.0.0", "back as it was")
	run.requireGoCalls(t, "list -m -json -mod=readonly example.com/lib@v1.0.0|api")
	if got := jobtest.ReadFile(t, goMod); got != unchanged {
		t.Errorf("a failed deps add changed go.mod:\n%s", got)
	}
}

// A spec whose version is none asks `go get` to drop a module: deps add
// refuses it before any go command runs and names the deps remove command.
func TestDepsEditAddRefusesTheVersionNone(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-edit-reads-no-replaced-version",
		"add-refuses-the-version-none-naming-deps-remove")
	ws := jobtest.RealTempDir(t)
	unchanged := "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n"
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", unchanged)

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib@none", "example.com/other@v1.0.0"}}), "off")
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "deps add does not drop a module", "putnami deps remove example.com/lib")
	run.requireGoCalls(t)
	if got := jobtest.ReadFile(t, goMod); got != unchanged {
		t.Errorf("deps add edited go.mod:\n%s", got)
	}
}

// placeholderClientWorkspace writes a workspace where apps/c imports the
// generated client of apps/b, whose go.mod requires go.putnami.dev/client at
// the v0.0.1 placeholder the client generator writes, and requires libs/a,
// which it does not import. clientReplace is the go.work replace directive
// that supplies go.putnami.dev/client, or "" for none.
func placeholderClientWorkspace(t *testing.T, clientReplace string) string {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "libs/a/go.mod", "module example.com/a\n\ngo 1.22\n")
	jobtest.WriteFile(t, ws, "libs/a/a.go", "package a\n")
	jobtest.WriteFile(t, ws, "apps/b/clients/go/go.mod", "module example.com/b/clients/go\n\ngo 1.22\n\nrequire go.putnami.dev/client v0.0.1\n")
	jobtest.WriteFile(t, ws, "apps/b/clients/go/client.gen.go", "package client\n\nimport _ \"go.putnami.dev/client\"\n")
	jobtest.WriteFile(t, ws, "apps/c/go.mod", `module example.com/c

go 1.22

require (
	example.com/a v0.0.0
	example.com/b/clients/go v0.0.0
)

replace example.com/a => ../../libs/a

replace example.com/b/clients/go => ../b/clients/go
`)
	jobtest.WriteFile(t, ws, "apps/c/main.go", "package main\n\nimport _ \"example.com/b/clients/go\"\n\nfunc main() {}\n")
	goWork := "go 1.22\n\nuse (\n\t./apps/b/clients/go\n\t./apps/c\n\t./libs/a\n)\n"
	if clientReplace != "" {
		goWork += "\nreplace " + clientReplace + "\n"
	}
	jobtest.WriteFile(t, ws, "go.work", goWork)
	return ws
}

// clientProxy serves go.putnami.dev/client at v0.9.3 only, never the v0.0.1
// placeholder.
func clientProxy(t *testing.T) string {
	t.Helper()
	return jobtest.FileURL(jobtest.WriteModuleProxy(t, jobtest.SimpleProxyModules("v0.9.3", "go.putnami.dev/client")...))
}

// `go mod tidy` reads no go.work, so it asks the module proxy for the v0.0.1
// placeholder of a generated client, which no proxy serves. deps prune tidies
// apps/c as go.work resolves it, with the release go.work pins, and writes none
// of go.work's directives into apps/c's go.mod. The requirement it drops is
// named by the module path libs/a's go.mod declares.
func TestDepsEditPruneTidiesAsTheWorkspaceResolves(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"tidy-reads-the-go-work-replace-directives")
	spectest.Proves(t, "go/deps-edit", "prune-names-the-module-the-target-declares",
		"prune-drops-the-module-path-the-target-go-mod-declares")
	jobtest.RequireGo(t)
	ws := placeholderClientWorkspace(t, "go.putnami.dev/client => go.putnami.dev/client v0.9.3")

	run := runEdit(t, ws, editParams(t, actionPrune, edit{Project: "apps/c", Path: "apps/c", Modules: []string{"libs/a"}}), clientProxy(t))
	run.requireStatus(t, statusOK)
	run.requireTranscript(t, "Removing example.com/a in apps/c...", "✓ updated apps/c/go.mod")
	goMod := filepath.Join(ws, "apps", "c", "go.mod")
	if got := requires(t, goMod, "example.com/a"); got != "" {
		t.Errorf("apps/c/go.mod still requires example.com/a %s", got)
	}
	if got := requires(t, goMod, "example.com/b/clients/go"); got == "" {
		t.Error("apps/c/go.mod lost the requirement its import backs")
	}
	if replaces(t, goMod, "go.putnami.dev/client") {
		t.Error("deps prune wrote go.work's replace directive into apps/c/go.mod")
	}
	if sum := jobtest.ReadFile(t, filepath.Join(ws, "apps", "c", "go.sum")); !strings.Contains(sum, "go.putnami.dev/client v0.9.3") {
		t.Errorf("apps/c/go.sum = %q, want the checksum of the release go.work pins", sum)
	}
}

// A go.work replace directive that names a directory is relative to go.work,
// so the tidy states it from the module's own directory and resolves with no
// module proxy at all.
func TestDepsEditRemoveTidiesThroughAGoWorkDirectoryReplace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"tidy-states-a-go-work-directory-replace-from-the-module")
	jobtest.RequireGo(t)
	ws := placeholderClientWorkspace(t, "go.putnami.dev/client => ./third_party/client")
	jobtest.WriteFile(t, ws, "third_party/client/go.mod", "module go.putnami.dev/client\n\ngo 1.22\n")
	jobtest.WriteFile(t, ws, "third_party/client/client.go", "package client\n")

	run := runEdit(t, ws, editParams(t, actionRemove, edit{Project: "apps/c", Path: "apps/c", Modules: []string{"example.com/a"}}), "off")
	run.requireStatus(t, statusOK)
	goMod := filepath.Join(ws, "apps", "c", "go.mod")
	if got := requires(t, goMod, "example.com/a"); got != "" {
		t.Errorf("apps/c/go.mod still requires example.com/a %s", got)
	}
	if replaces(t, goMod, "go.putnami.dev/client") {
		t.Error("deps remove wrote go.work's replace directive into apps/c/go.mod")
	}
}

// A go.work replace directive that pins every version of a module to a
// release reaches the tidy only for the versions no proxy serves. go.work
// pins example.com/dep and example.com/feat to v1.1.0, whose go.mod requires
// example.com/feat and example.com/extra at v1.1.0. deps add of
// example.com/feat@v1.0.0 requires that version, and the tidy, which reads the
// go.mod of example.com/dep v1.0.0, raises no requirement and adds none.
func TestDepsEditAddKeepsEachServedRequirementAGoWorkPinWouldRaise(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"tidy-keeps-a-served-requirement-a-go-work-pin-would-raise")
	jobtest.RequireGo(t)
	module := func(path, version, requires string) jobtest.ProxyModule {
		name := filepath.Base(path)
		return jobtest.ProxyModule{
			Path:    path,
			Version: version,
			GoMod:   "module " + path + "\n\ngo 1.22\n" + requires,
			Source:  "package " + name + "\n\nconst Name = \"" + name + "\"\n",
		}
	}
	proxyDir := jobtest.WriteModuleProxy(t,
		module("example.com/dep", "v1.0.0", ""),
		module("example.com/dep", "v1.1.0", "\nrequire (\n\texample.com/extra v1.1.0\n\texample.com/feat v1.1.0\n)\n"),
		module("example.com/feat", "v1.0.0", ""),
		module("example.com/feat", "v1.1.0", ""),
		module("example.com/extra", "v1.1.0", ""),
	)
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n\nreplace (\n"+
		"\texample.com/dep => example.com/dep v1.1.0\n"+
		"\texample.com/feat => example.com/feat v1.1.0\n)\n")
	jobtest.WriteFile(t, ws, "api/main.go", "package main\n\nimport (\n\t\"example.com/dep\"\n\t\"example.com/feat\"\n)\n\n"+
		"func main() { _, _ = dep.Name, feat.Name }\n")
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", "module example.com/api\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n")
	add := edit{Project: "api", Path: "api", Modules: []string{"example.com/feat@v1.0.0"}}

	dryRun, err := json.Marshal(map[string]any{"action": actionAdd, "dry-run": true, "edits": []edit{add}})
	if err != nil {
		t.Fatal(err)
	}
	plan := runEdit(t, ws, string(dryRun), jobtest.FileURL(proxyDir))
	plan.requireStatus(t, statusOK)
	var summary string
	for _, event := range plan.rec.Events() {
		if event.Kind == "summary" {
			summary += event.Message
		}
	}
	if !strings.Contains(summary, "+ require example.com/feat v1.0.0") {
		t.Errorf("the dry run does not plan the requested version:\n%s", summary)
	}
	for _, unrelated := range []string{"example.com/dep", "example.com/extra", "v1.1.0"} {
		if strings.Contains(summary, unrelated) {
			t.Errorf("the dry run plans a change of %s:\n%s", unrelated, summary)
		}
	}

	run := runEdit(t, ws, editParams(t, actionAdd, add), jobtest.FileURL(proxyDir))
	run.requireStatus(t, statusOK)
	for module, want := range map[string]string{"example.com/dep": "v1.0.0", "example.com/feat": "v1.0.0", "example.com/extra": ""} {
		if got := requires(t, goMod, module); got != want {
			t.Errorf("go.mod requires %s %q, want %q:\n%s", module, got, want, jobtest.ReadFile(t, goMod))
		}
	}
	if replaces(t, goMod, "example.com/dep") || replaces(t, goMod, "example.com/feat") {
		t.Error("deps add wrote go.work's replace directive into go.mod")
	}
}

// A go.work pin is stated for a placeholder no proxy serves, and the run names
// it. gen, a member apps/api replaces by its directory, requires
// example.com/client at the v0.0.1 placeholder, which go.work pins to v0.9.3,
// whose go.mod requires example.com/feat v1.1.0. The tidy reads that release
// for the placeholder and raises feat, and both the plan and the run name the
// pin. The served example.com/dep v1.0.0 stays: its pin is not stated.
func TestDepsEditNamesAGoWorkPinStatedForAnUnservedPlaceholder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"tidy-keeps-a-served-requirement-a-go-work-pin-would-raise")
	jobtest.RequireGo(t)
	module := func(path, version, requires string) jobtest.ProxyModule {
		name := filepath.Base(path)
		return jobtest.ProxyModule{
			Path:    path,
			Version: version,
			GoMod:   "module " + path + "\n\ngo 1.22\n" + requires,
			Source:  "package " + name + "\n\nconst Name = \"" + name + "\"\n",
		}
	}
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t,
		module("example.com/dep", "v1.0.0", ""),
		module("example.com/dep", "v1.1.0", "\nrequire example.com/extra v1.1.0\n"),
		module("example.com/extra", "v1.1.0", ""),
		module("example.com/client", "v0.9.3", "\nrequire example.com/feat v1.1.0\n"),
		module("example.com/feat", "v1.0.0", ""),
		module("example.com/feat", "v1.1.0", ""),
	))
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./gen\n)\n\nreplace (\n"+
		"\texample.com/client => example.com/client v0.9.3\n"+
		"\texample.com/dep => example.com/dep v1.1.0\n)\n")
	jobtest.WriteFile(t, ws, "gen/go.mod", "module example.com/gen\n\ngo 1.22\n\nrequire example.com/client v0.0.1\n")
	jobtest.WriteFile(t, ws, "gen/gen.go", "package gen\n\nimport \"example.com/client\"\n\nconst Name = client.Name\n")
	jobtest.WriteFile(t, ws, "api/main.go", "package main\n\nimport (\n\t\"example.com/dep\"\n\t\"example.com/feat\"\n\t\"example.com/gen\"\n)\n\n"+
		"func main() { _, _, _ = dep.Name, feat.Name, gen.Name }\n")
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", "module example.com/api\n\ngo 1.22\n\nrequire (\n"+
		"\texample.com/dep v1.0.0\n\texample.com/gen v0.0.0\n)\n\nreplace example.com/gen => ../gen\n")
	add := edit{Project: "api", Path: "api", Modules: []string{"example.com/feat@v1.0.0"}}
	pin := "go.work pins example.com/client to example.com/client v0.9.3: the tidy reads that release for example.com/client v0.0.1"

	dryRun, err := json.Marshal(map[string]any{"action": actionAdd, "dry-run": true, "edits": []edit{add}})
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []string{string(dryRun), editParams(t, actionAdd, add)} {
		run := runEdit(t, ws, params, proxy)
		run.requireStatus(t, statusOK)
		var summary string
		for _, event := range run.rec.Events() {
			if data, _ := event.Value.(map[string]any); event.Kind == "summary" && data["visibility"] == "always" {
				summary += event.Message
			}
		}
		if !strings.Contains(summary, pin) {
			t.Errorf("the always-visible summary does not name the pin %q:\n%s", pin, summary)
		}
		if strings.Contains(summary, "pins example.com/dep") {
			t.Errorf("the run states the pin of the served example.com/dep v1.0.0:\n%s", summary)
		}
	}
	for module, want := range map[string]string{"example.com/dep": "v1.0.0", "example.com/feat": "v1.1.0", "example.com/extra": ""} {
		if got := requires(t, goMod, module); got != want {
			t.Errorf("go.mod requires %s %q, want %q:\n%s", module, got, want, jobtest.ReadFile(t, goMod))
		}
	}
}

// A lookup that fails for another reason than a missing version cannot tell
// whether a proxy serves it, so the edit fails and puts go.mod back, rather
// than stating the pin and raising the requirement. GOPROXY=off answers every
// lookup with "module lookup disabled".
func TestDepsEditFailsWhenAGoWorkPinLookupCannotTell(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"tidy-keeps-a-served-requirement-a-go-work-pin-would-raise")
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n\nreplace example.com/dep => example.com/dep v1.1.0\n")
	jobtest.WriteFile(t, ws, "api/main.go", "package main\n\nfunc main() {}\n")
	unchanged := "module example.com/api\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n"
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", unchanged)

	run := runEdit(t, ws, editParams(t, actionRemove, edit{Project: "api", Path: "api", Modules: []string{"example.com/other"}}), "off")
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "cannot tell whether a proxy serves example.com/dep@v1.0.0", "every file the command edited is back as it was")
	if got := jobtest.ReadFile(t, goMod); got != unchanged {
		t.Errorf("the failed edit left go.mod changed:\n%s", got)
	}
}

// The edits of one run are one transaction: a go step that fails puts every
// go.mod and go.sum the run edited back as it was, those of the modules it
// already tidied included. Here no go.work directive supplies the generated
// client's placeholder, so the second module's tidy fails.
func TestDepsEditPutsEveryFileBackWhenAGoStepFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-tidy-resolves-as-the-workspace-does",
		"a-failed-go-step-leaves-every-file-as-it-was")
	jobtest.RequireGo(t)
	ws := placeholderClientWorkspace(t, "")
	jobtest.WriteFile(t, ws, "libs/a/go.mod", "module example.com/a\n\ngo 1.22\n\nrequire example.com/unused v0.0.0\n")
	files := []string{"libs/a/go.mod", "apps/c/go.mod"}
	before := map[string]string{}
	for _, file := range files {
		before[file] = jobtest.ReadFile(t, filepath.Join(ws, file))
	}

	run := runEdit(t, ws, editParams(t, actionRemove,
		edit{Project: "libs/a", Path: "libs/a", Modules: []string{"example.com/unused"}},
		edit{Project: "apps/c", Path: "apps/c", Modules: []string{"example.com/a"}},
	), clientProxy(t))
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "✓ updated libs/a/go.mod", "go mod tidy", "go.putnami.dev/client", "back as it was")
	for _, file := range files {
		if after := jobtest.ReadFile(t, filepath.Join(ws, file)); after != before[file] {
			t.Errorf("a failed deps remove changed %s:\n%s", file, after)
		}
	}
	for _, sum := range []string{"libs/a/go.sum", "apps/c/go.sum"} {
		if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, sum)); ok {
			t.Errorf("a failed deps remove left %s behind", sum)
		}
	}
}

// On a host without a go command and with no Go pinned, deps-edit edits
// nothing and names putnami install, which pins and installs it.
func TestDepsEditWithoutGoNamesPutnamiInstall(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-run-the-pinned-go",
		"edit-names-putnami-install-without-go")
	ws := jobtest.RealTempDir(t)
	unchanged := "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n"
	goMod := jobtest.WriteFile(t, ws, "api/go.mod", unchanged)

	run := runEdit(t, ws, editParams(t, actionRemove, edit{Project: "api", Path: "api", Modules: []string{"example.com/lib"}}), "off",
		"PATH="+t.TempDir())
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "putnami install", "deps remove: no go command is available")
	var errors []string
	for _, event := range run.rec.Events() {
		if event.Kind == "diagnostic" && event.Level == "error" {
			errors = append(errors, event.Message)
		}
	}
	if len(errors) != 1 {
		t.Errorf("a missing Go emits %d error diagnostics, want the one that names putnami install: %q", len(errors), errors)
	}
	if got := jobtest.ReadFile(t, goMod); got != unchanged {
		t.Errorf("deps remove edited go.mod:\n%s", got)
	}
}

// The job refuses what it cannot edit, before any go command runs.
func TestDepsEditRefusesWhatItCannotEdit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-run-the-pinned-go",
		"edit-refuses-a-hosted-run-before-any-go-command")
	tests := []struct {
		name   string
		params string
		extra  []string
		want   string
	}{
		{"no action", `{"edits":[{"project":"api","path":"api","modules":["example.com/lib"]}]}`, nil,
			`params.action to be add, remove or prune, not ""`},
		{"an unknown action", `{"action":"upgrade","edits":[{"project":"api","path":"api","modules":["example.com/lib"]}]}`, nil,
			`not "upgrade"`},
		{"no edits", `{"action":"add"}`, nil, "a non-empty array of {project, path, modules}"},
		{"empty edits", `{"action":"add","edits":[]}`, nil, "a non-empty array of {project, path, modules}"},
		{"a path outside the workspace", `{"action":"remove","edits":[{"project":"api","path":"../api","modules":["example.com/lib"]}]}`, nil,
			`the directory of "api" relative to the workspace root, not "../api"`},
		{"no module", `{"action":"add","edits":[{"project":"api","path":"api","modules":[]}]}`, nil,
			"deps add requires at least one module (e.g. `putnami deps add golang.org/x/text@latest`)"},
		{"not a Go module", `{"action":"remove","edits":[{"project":"web","path":"web","modules":["example.com/lib"]}]}`, nil,
			"web is not a Go module: web has no go.mod"},
		{"offline dependencies", `{"action":"remove","edits":[{"project":"api","path":"api","modules":["example.com/lib"]}]}`,
			[]string{"PUTNAMI_OFFLINE_DEPENDENCIES=1"}, "deps remove reaches the network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := jobtest.RealTempDir(t)
			unchanged := "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n"
			goMod := jobtest.WriteFile(t, ws, "api/go.mod", unchanged)
			run := runEdit(t, ws, tt.params, "off", tt.extra...)
			run.requireStatus(t, statusFailed)
			run.requireTranscript(t, tt.want)
			run.requireGoCalls(t)
			if got := jobtest.ReadFile(t, goMod); got != unchanged {
				t.Errorf("go.mod changed:\n%s", got)
			}
		})
	}
}

// prune names a requirement by the module path the target's go.mod declares,
// and refuses a target whose module it cannot name.
func TestPrunedModulesReadTheTargetGoMod(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "prune-names-the-module-the-target-declares",
		"prune-reads-the-module-path-of-each-target-and-names-one-it-cannot-read")
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "libs/b/go.mod", "module example.com/b\n\ngo 1.22\n")
	jobtest.WriteFile(t, ws, "libs/a/go.mod", "module example.com/a\n\ngo 1.22\n")
	got, err := prunedModules(ws, edit{Project: "apps/c", Modules: []string{"libs/b", "libs/a"}})
	if err != nil || strings.Join(got, " ") != "example.com/a example.com/b" {
		t.Fatalf("prunedModules() = %v, %v; want the sorted module paths", got, err)
	}
	for _, target := range []string{"libs/none", "../outside"} {
		if _, err := prunedModules(ws, edit{Project: "apps/c", Modules: []string{target}}); err == nil ||
			!strings.Contains(err.Error(), "cannot name the module of "+target+" to remove it from apps/c") {
			t.Errorf("prunedModules(%s) error = %v, want it to name the target and the project", target, err)
		}
	}
}
