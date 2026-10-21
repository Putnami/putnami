package depsedit

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// memberWorkspace writes a workspace whose go.work uses api and libs/proto,
// the module example.com/proto, which no registry serves. api/main.go holds
// source and api/go.mod holds goMod, whose path it returns.
func memberWorkspace(t *testing.T, goMod, source string) (string, string) {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./libs/proto\n)\n")
	jobtest.WriteFile(t, ws, "libs/proto/go.mod", "module example.com/proto\n\ngo 1.22\n")
	jobtest.WriteFile(t, ws, "libs/proto/proto.go", "package proto\n\nconst Name = \"proto\"\n")
	jobtest.WriteFile(t, ws, "api/main.go", source)
	return ws, jobtest.WriteFile(t, ws, "api/go.mod", goMod)
}

const (
	bareAPIGoMod  = "module example.com/api\n\ngo 1.22\n"
	importsProto  = "package main\n\nimport \"example.com/proto\"\n\nfunc main() { _ = proto.Name }\n"
	importsBoth   = "package main\n\nimport (\n\t\"example.com/lib\"\n\t\"example.com/proto\"\n)\n\nfunc main() { _, _ = lib.Name, proto.Name }\n"
	importsNoneGo = "package main\n\nfunc main() {}\n"
)

// replaceTarget returns the target of the directive in the go.mod at path
// that replaces module, and the version it replaces, or "" for none.
func replaceTarget(t *testing.T, path, module string) (string, string) {
	t.Helper()
	file, err := modfile.Parse(path, []byte(jobtest.ReadFile(t, path)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, replace := range file.Replace {
		if replace.Old.Path == module {
			return replace.New.Path, replace.Old.Version
		}
	}
	return "", ""
}

// A bare spec that names the module of a go.work member requires the member
// from the tree: at the placeholder of its major version, replaced by its
// directory relative to the edited module. No registry is asked: the module
// proxy is off, so any query for example.com/proto would fail the add.
func TestDepsEditAddRequiresAnUnpublishedWorkspaceMemberFromTheTree(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"add-requires-an-unpublished-workspace-member-with-no-registry-query")
	jobtest.RequireGo(t)
	ws, goMod := memberWorkspace(t, bareAPIGoMod, importsProto)

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/proto"}}), "off")
	run.requireStatus(t, statusOK)
	run.requireTranscript(t, "Adding example.com/proto in api...",
		"example.com/proto is the go.work member libs/proto: requiring v0.0.0, replaced by ../libs/proto",
		"✓ updated api/go.mod")
	run.requireGoCalls(t,
		"mod edit -require=example.com/proto@v0.0.0 -replace=example.com/proto=../libs/proto|api",
		"mod tidy -modfile=<copy>|api")
	if got := requires(t, goMod, "example.com/proto"); got != "v0.0.0" {
		t.Errorf("go.mod requires example.com/proto %q, want v0.0.0", got)
	}
	if target, version := replaceTarget(t, goMod, "example.com/proto"); target != "../libs/proto" || version != "" {
		t.Errorf("go.mod replaces example.com/proto %q with %q, want every version with ../libs/proto", version, target)
	}
	if strings.Contains(jobtest.ReadFile(t, goMod), ws) {
		t.Errorf("go.mod names an absolute path:\n%s", jobtest.ReadFile(t, goMod))
	}
}

// One add can name a go.work member and a published module: the member is
// required from the tree, and only the published module reaches `go list -m`.
// The requirements keep the order of the specs.
func TestDepsEditAddResolvesMembersFromTheTreeAndTheRestFromTheRegistry(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"add-resolves-members-from-the-tree-and-other-specs-from-the-registry")
	jobtest.RequireGo(t)
	ws, goMod := memberWorkspace(t, bareAPIGoMod, importsBoth)
	// The proxy serves example.com/lib only: a query for example.com/proto fails.
	proxy := libProxy(t, map[string][]string{"example.com/lib": {"v0.3.0"}})

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/proto", "example.com/lib"}}), proxy)
	run.requireStatus(t, statusOK)
	run.requireGoCalls(t,
		"list -m -json -mod=readonly example.com/lib@latest|api",
		"mod edit -require=example.com/proto@v0.0.0 -require=example.com/lib@v0.3.0 -replace=example.com/proto=../libs/proto|api",
		"mod tidy -modfile=<copy>|api")
	if got := requires(t, goMod, "example.com/lib"); got != "v0.3.0" {
		t.Errorf("go.mod requires example.com/lib %q, want v0.3.0", got)
	}
	if got := requires(t, goMod, "example.com/proto"); got != "v0.0.0" {
		t.Errorf("go.mod requires example.com/proto %q, want v0.0.0", got)
	}
}

// A member go.mod already requires keeps that version, and a replace
// directive go.mod states for it stays as written, even when it names another
// directory than the member.
func TestDepsEditAddKeepsTheVersionAndTheReplaceGoModStatesForAMember(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"add-keeps-the-required-version-and-the-replace-go-mod-states-for-a-member")
	jobtest.RequireGo(t)

	t.Run("a required version", func(t *testing.T) {
		t.Parallel()
		ws, goMod := memberWorkspace(t, bareAPIGoMod+"\nrequire example.com/proto v0.2.0\n", importsProto)
		run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/proto"}}), "off")
		run.requireStatus(t, statusOK)
		run.requireGoCalls(t,
			"mod edit -require=example.com/proto@v0.2.0 -replace=example.com/proto=../libs/proto|api",
			"mod tidy -modfile=<copy>|api")
		if got := requires(t, goMod, "example.com/proto"); got != "v0.2.0" {
			t.Errorf("go.mod requires example.com/proto %q, want v0.2.0 kept", got)
		}
	})

	t.Run("a replace go.mod states", func(t *testing.T) {
		t.Parallel()
		ws, goMod := memberWorkspace(t, bareAPIGoMod+"\nrequire example.com/proto v0.0.1\n\nreplace example.com/proto => ./fork\n", importsProto)
		jobtest.WriteFile(t, ws, "api/fork/go.mod", "module example.com/proto\n\ngo 1.22\n")
		jobtest.WriteFile(t, ws, "api/fork/proto.go", "package proto\n\nconst Name = \"fork\"\n")
		run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/proto"}}), "off")
		run.requireStatus(t, statusOK)
		run.requireTranscript(t, "example.com/proto is the go.work member libs/proto: requiring v0.0.1, keeping the replace go.mod states")
		run.requireGoCalls(t,
			"mod edit -require=example.com/proto@v0.0.1|api",
			"mod tidy -modfile=<copy>|api")
		if target, _ := replaceTarget(t, goMod, "example.com/proto"); target != "./fork" {
			t.Errorf("go.mod replaces example.com/proto with %q, want ./fork kept", target)
		}
	})
}

// A spec that states a version asks the registry, even for a module a go.work
// member declares, and adds no replace directive: the caller asked for a
// published version.
func TestDepsEditAddAsksTheRegistryForAMemberSpecWithAVersion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"add-asks-the-registry-for-a-member-spec-that-states-a-version")
	jobtest.RequireGo(t)
	ws, goMod := memberWorkspace(t, bareAPIGoMod, importsProto)
	proxy := libProxy(t, map[string][]string{"example.com/proto": {"v0.3.0"}})

	run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{"example.com/proto@v0.3.0"}}), proxy)
	run.requireStatus(t, statusOK)
	run.requireGoCalls(t,
		"list -m -json -mod=readonly example.com/proto@v0.3.0|api",
		"mod edit -require=example.com/proto@v0.3.0|api",
		"mod tidy -modfile=<copy>|api")
	if got := requires(t, goMod, "example.com/proto"); got != "v0.3.0" {
		t.Errorf("go.mod requires example.com/proto %q, want v0.3.0", got)
	}
	if target, _ := replaceTarget(t, goMod, "example.com/proto"); target != "" {
		t.Errorf("go.mod replaces example.com/proto with %q, want no replace", target)
	}
}

// When the precedence of a member over a published module cannot be decided,
// a bare add fails before any go command runs and leaves go.mod as it was:
// two members declare the module, a member has no go.mod, go.work does not
// parse, or the spec names the edited module itself.
func TestDepsEditAddRefusesAMemberItCannotIdentify(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"add-refuses-a-member-it-cannot-identify-before-any-go-command")
	jobtest.RequireGo(t)
	tests := []struct {
		name  string
		spec  string
		setup func(t *testing.T, ws string)
		want  string
	}{
		{
			name: "two members declare the module",
			spec: "example.com/proto",
			setup: func(t *testing.T, ws string) {
				jobtest.WriteFile(t, ws, "libs/copy/go.mod", "module example.com/proto\n\ngo 1.22\n")
				jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./libs/proto\n\t./libs/copy\n)\n")
			},
			want: "go.work uses 2 directories that declare example.com/proto (libs/proto, libs/copy); deps add does not choose one",
		},
		{
			name: "a member has no go.mod",
			spec: "example.com/lib",
			setup: func(t *testing.T, ws string) {
				jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./libs/proto\n\t./libs/missing\n)\n")
			},
			want: "go.work: the member ./libs/missing has no go.mod that declares a module path; a spec without a version may name a go.work member",
		},
		{
			name: "go.work does not parse",
			spec: "example.com/proto",
			setup: func(t *testing.T, ws string) {
				jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./api\n\nnot-a-directive ./libs/proto\n")
			},
			want: "a spec without a version may name a go.work member, so deps add resolves none",
		},
		{
			name: "the spec names the edited module",
			spec: "example.com/api",
			want: "example.com/api is the module deps add edits; a module does not require itself",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws, goMod := memberWorkspace(t, bareAPIGoMod, importsNoneGo)
			if tt.setup != nil {
				tt.setup(t, ws)
			}
			run := runEdit(t, ws, editParams(t, actionAdd, edit{Project: "api", Path: "api", Modules: []string{tt.spec}}), "off")
			run.requireStatus(t, statusFailed)
			run.requireTranscript(t, tt.want, "back as it was")
			run.requireGoCalls(t)
			if got := jobtest.ReadFile(t, goMod); got != bareAPIGoMod {
				t.Errorf("a refused deps add changed go.mod:\n%s", got)
			}
		})
	}
}

// A member add is part of the run's transaction: when a later module's tidy
// fails, the go.mod the member add already wrote is put back with the rest.
func TestDepsEditPutsAMemberAddBackWhenALaterStepFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"a-failed-later-step-puts-a-member-add-back")
	jobtest.RequireGo(t)
	ws, apiGoMod := memberWorkspace(t, bareAPIGoMod, importsProto)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./libs/proto\n\t./svc\n)\n")
	const svcModule = "module example.com/svc\n\ngo 1.22\n"
	svcGoMod := jobtest.WriteFile(t, ws, "svc/go.mod", svcModule)
	// No registry serves example.com/missing, so the tidy of svc fails.
	jobtest.WriteFile(t, ws, "svc/main.go", "package main\n\nimport (\n\t_ \"example.com/missing\"\n\t_ \"example.com/proto\"\n)\n\nfunc main() {}\n")

	run := runEdit(t, ws, editParams(t, actionAdd,
		edit{Project: "api", Path: "api", Modules: []string{"example.com/proto"}},
		edit{Project: "svc", Path: "svc", Modules: []string{"example.com/proto"}},
	), "off")
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "✓ updated api/go.mod", "go mod tidy", "example.com/missing", "back as it was")
	for path, want := range map[string]string{apiGoMod: bareAPIGoMod, svcGoMod: svcModule} {
		if got := jobtest.ReadFile(t, path); got != want {
			t.Errorf("a failed deps add changed %s:\n%s", path, got)
		}
	}
	for _, sum := range []string{"api/go.sum", "svc/go.sum"} {
		if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, sum)); ok {
			t.Errorf("a failed deps add left %s behind", sum)
		}
	}
}

// A member is required at the version go.mod requires it at, else at the
// placeholder of its path's major version; its replace target is relative to
// the edited module and starts with ./ or ../.
func TestMemberVersionAndReplaceTarget(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/deps-edit", "deps-add-resolves-a-workspace-member-from-the-tree",
		"a-member-is-required-at-its-version-or-its-major-placeholder")
	goMod := "module example.com/api\n\ngo 1.22\n\nrequire (\n\texample.com/kept v0.4.1\n\texample.com/kept/v3 v3.2.0\n)\n"
	file, err := modfile.Parse("go.mod", []byte(goMod), nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"example.com/proto":    "v0.0.0",
		"example.com/proto/v3": "v3.0.0",
		"gopkg.in/yaml.v2":     "v2.0.0",
		"example.com/kept":     "v0.4.1",
		"example.com/kept/v3":  "v3.2.0",
	} {
		if got, err := memberVersion(file, path); err != nil || got != want {
			t.Errorf("memberVersion(%s) = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := memberVersion(file, "example.com/bad/v1"); err == nil {
		t.Error("memberVersion accepted a module path with a /v1 suffix")
	}

	root := t.TempDir()
	for _, tt := range []struct{ dir, member, want string }{
		{"apps/api", "libs/proto", "../../libs/proto"},
		{"apps/api", "apps/api/internal/proto", "./internal/proto"},
	} {
		got, err := relativeReplace(filepath.Join(root, tt.dir), filepath.Join(root, tt.member))
		if err != nil || got != tt.want {
			t.Errorf("relativeReplace(%s, %s) = %q, %v; want %q", tt.dir, tt.member, got, err, tt.want)
		}
	}
}
