package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

const depsEditFeature = "typescript/typescript-project-toolchain"
const depsEditRequirement = "deps-edit-keeps-the-catalog"

// depsEditWorkspace writes the given files under a fresh workspace root and
// returns a context whose params ask for action on the edits.
func depsEditWorkspace(t *testing.T, files map[string]string, action string, edits []depsEditEntry, dryRun bool) (*pctx.Context, string) {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	for name, content := range files {
		writeProbeFile(t, filepath.Join(dir, name), content)
	}
	ctx.Params["action"] = mustJSON(t, action)
	ctx.Params["edits"] = mustJSON(t, edits)
	if dryRun {
		ctx.Params["dry-run"] = []byte("true")
	}
	return ctx, dir
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// stubRegistry answers every dist-tag lookup from versions, keyed by
// name@tag, and records the lookups.
func stubRegistry(t *testing.T, versions map[string]string) *[]string {
	t.Helper()
	orig := lookupDepsEditVersion
	t.Cleanup(func() { lookupDepsEditVersion = orig })
	var asked []string
	lookupDepsEditVersion = func(_ *pctx.Context, name, tag string) (string, error) {
		asked = append(asked, name+"@"+tag)
		if v, ok := versions[name+"@"+tag]; ok {
			return v, nil
		}
		return "", errors.New("no dist-tag with that name")
	}
	return &asked
}

func stubBun(t *testing.T) {
	t.Helper()
	orig := resolveBunBin
	t.Cleanup(func() { resolveBunBin = orig })
	resolveBunBin = func() (string, error) { return "/usr/bin/bun", nil }
}

func runDepsEditOK(t *testing.T, ctx *pctx.Context) {
	t.Helper()
	if status, _, err := runDepsEdit(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runDepsEdit = %q, %v; want OK", status, err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Fatalf("%s =\n%s\nwant\n%s", filepath.Base(path), got, want)
	}
}

const bunRoot = "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"react\": \"^19.0.0\"\n  }\n}\n"

// A Bun add puts the package in the root catalog with ^ the version the
// latest dist-tag names, references it with catalog: from the project, and
// installs with bun.
func TestABunAddWritesTheCatalogAndReferencesIt(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-bun-add-writes-the-catalog-and-references-it")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          bunRoot,
		"bun.lock":              "{}",
		"apps/api/package.json": "{\n  \"name\": \"api\",\n  \"version\": \"0.0.0\",\n  \"dependencies\": {\n    \"react\": \"catalog:\"\n  }\n}\n",
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"@putnami/database", "react"}}}, false)
	asked := stubRegistry(t, map[string]string{"@putnami/database@latest": "1.4.2"})
	stubBun(t)
	calls := recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "package.json"), "{\n  \"name\": \"shop\",\n  \"private\": true,\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"@putnami/database\": \"^1.4.2\",\n    \"react\": \"^19.0.0\"\n  }\n}\n")
	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"version\": \"0.0.0\",\n  \"dependencies\": {\n    \"@putnami/database\": \"catalog:\",\n    \"react\": \"catalog:\"\n  }\n}\n")
	if !slices.Equal(*asked, []string{"@putnami/database@latest"}) {
		t.Fatalf("registry lookups = %v; react is in the catalog and needs none", *asked)
	}
	if len(*calls) != 1 || (*calls)[0].bin != "/usr/bin/bun" || !slices.Equal((*calls)[0].args, workspaceInstallArgs(false, false)) {
		t.Fatalf("the add ran %v, want one bun install", *calls)
	}

	// A spec that names a version changes the entry the catalog holds.
	ctx.Params["edits"] = mustJSON(t, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"@putnami/database@1.5.0"}}})
	runDepsEditOK(t, ctx)
	if got := readFile(t, filepath.Join(dir, "package.json")); !strings.Contains(got, `"@putnami/database": "1.5.0"`) {
		t.Fatalf("the catalog kept the old version:\n%s", got)
	}
}

// A pnpm add writes the entry to the pnpm-workspace.yaml catalog, keeping its
// other lines, and installs with pnpm.
func TestAPNPMAddWritesThePNPMWorkspaceCatalog(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-pnpm-add-writes-the-pnpm-workspace-catalog")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`,
		"pnpm-workspace.yaml":   "packages:\n  - apps/*\n\n# shared versions\ncatalog:\n  react: ^19.0.0\n",
		"pnpm-lock.yaml":        "lockfileVersion: '9.0'\n",
		"apps/api/package.json": `{"name":"api"}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod@^3.23.0"}}}, false)
	asked := stubRegistry(t, nil)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	calls := recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - apps/*\n\n# shared versions\ncatalog:\n  react: ^19.0.0\n  zod: ^3.23.0\n")
	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"zod\": \"catalog:\"\n  }\n}\n")
	if len(*asked) != 0 {
		t.Fatalf("a range needs no registry lookup, got %v", *asked)
	}
	if len(*calls) != 1 || (*calls)[0].bin != "/usr/bin/pnpm" || !slices.Equal((*calls)[0].args, []string{"install"}) {
		t.Fatalf("the add ran %v, want one pnpm install", *calls)
	}
}

// npm reads no catalog: the version goes in the project's own manifest.
func TestAnNPMAddWritesTheVersionInTheProject(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "an-npm-add-writes-the-version-in-the-project")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          `{"name":"shop","private":true,"workspaces":["apps/*"]}`,
		"package-lock.json":     "{}",
		"apps/api/package.json": `{"name":"api","devDependencies":{"typescript":"^5.9.0"}}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod", "typescript@next"}}}, false)
	stubRegistry(t, map[string]string{"zod@latest": "3.24.1", "typescript@next": "6.0.0-dev.1"})
	mockLookPath(t, map[string]string{"npm": "/usr/bin/npm"})
	calls := recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"devDependencies\": {\n    \"typescript\": \"^6.0.0-dev.1\"\n  },\n  \"dependencies\": {\n    \"zod\": \"^3.24.1\"\n  }\n}\n")
	assertFile(t, filepath.Join(dir, "package.json"), `{"name":"shop","private":true,"workspaces":["apps/*"]}`)
	if len(*calls) != 1 || (*calls)[0].bin != "/usr/bin/npm" {
		t.Fatalf("the add ran %v, want one npm install", *calls)
	}
}

// A package another workspace member publishes is written as workspace:*,
// with no registry lookup and no catalog entry.
func TestAnAddOfAMemberPackageWritesWorkspace(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "an-add-of-a-member-package-writes-workspace")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":           bunRoot,
		"bun.lock":               "{}",
		"apps/api/package.json":  `{"name":"api"}`,
		"apps/core/package.json": `{"name":"@shop/core"}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"@shop/core"}}}, false)
	asked := stubRegistry(t, nil)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"@shop/core\": \"workspace:*\"\n  }\n}\n")
	assertFile(t, filepath.Join(dir, "package.json"), bunRoot)
	if len(*asked) != 0 {
		t.Fatalf("a workspace package needs no registry lookup, got %v", *asked)
	}

	// A project does not depend on its own package.
	ctx.Params["edits"] = mustJSON(t, []depsEditEntry{{Project: "core", Path: "apps/core", Modules: []string{"@shop/core"}}})
	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("an add of the project's own package = %q, want FAILED", status)
	}
}

// A remove drops the package from the project and the catalog entry no other
// member references; an entry another member still references stays. A
// package the project does not list fails the remove.
func TestARemoveDropsAnEntryNoMemberReferences(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-remove-drops-an-entry-no-member-references")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          "{\n  \"name\": \"shop\",\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"react\": \"^19.0.0\",\n    \"zod\": \"^3.0.0\"\n  }\n}\n",
		"bun.lock":              "{}",
		"apps/api/package.json": "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"react\": \"catalog:\",\n    \"zod\": \"catalog:\"\n  }\n}\n",
		"apps/web/package.json": "{\n  \"name\": \"web\",\n  \"dependencies\": {\n    \"react\": \"catalog:\"\n  }\n}\n",
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod", "react"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "package.json"), "{\n  \"name\": \"shop\",\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"react\": \"^19.0.0\"\n  }\n}\n")
	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\"\n}\n")

	before := readFile(t, filepath.Join(dir, "apps/web/package.json"))
	ctx.Params["edits"] = mustJSON(t, []depsEditEntry{{Project: "web", Path: "apps/web", Modules: []string{"zod"}}})
	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("a remove of an absent package = %q, want FAILED", status)
	}
	assertFile(t, filepath.Join(dir, "apps/web/package.json"), before)
}

// A prune names the target project by its directory; the job drops the
// package that project publishes.
func TestAPruneDropsTheTargetPackage(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-prune-drops-the-target-package")
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":           bunRoot,
		"bun.lock":               "{}",
		"apps/api/package.json":  `{"name":"api","dependencies":{"@shop/core":"workspace:*","react":"catalog:"}}`,
		"apps/core/package.json": `{"name":"@shop/core"}`,
	}, depsEditPrune, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"apps/core"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"react\": \"catalog:\"\n  }\n}\n")
}

// A failed install restores the project manifest, the root manifest and the
// lockfile it changed.
func TestAFailedInstallRestoresTheEditedFiles(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-failed-install-restores-the-edited-files")
	files := map[string]string{
		"package.json":          bunRoot,
		"bun.lock":              "{\"lockfileVersion\": 1}",
		"apps/api/package.json": `{"name":"api"}`,
	}
	ctx, dir := depsEditWorkspace(t, files, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod@^3.0.0"}}}, false)
	stubBun(t)
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		_ = os.WriteFile(filepath.Join(dir, "bun.lock"), []byte("{\"broken\": true}"), 0o644)
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "error: zod@^3.0.0 failed to resolve"}, nil
	})

	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("runDepsEdit = %q, want FAILED", status)
	}
	for name, content := range files {
		assertFile(t, filepath.Join(dir, name), content)
	}
}

// A dry run reports the edit and writes no file, runs no install.
func TestADryRunWritesNoFile(t *testing.T) {
	spectest.Proves(t, depsEditFeature, depsEditRequirement, "a-dry-run-writes-no-file")
	files := map[string]string{
		"package.json":          bunRoot,
		"bun.lock":              "{}",
		"apps/api/package.json": `{"name":"api","dependencies":{"react":"catalog:"}}`,
	}
	ctx, dir := depsEditWorkspace(t, files, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod@^3.0.0"}}}, true)
	stubBun(t)
	calls := recordCalls(t)
	var lines []string
	emit := jsonl.New()
	emit.Observe(func(message string) { lines = append(lines, message) })

	if status, _, err := runDepsEdit(ctx, emit, nil); err != nil || status != "OK" {
		t.Fatalf("runDepsEdit = %q, %v; want OK", status, err)
	}
	for name, content := range files {
		assertFile(t, filepath.Join(dir, name), content)
	}
	if len(*calls) != 0 {
		t.Fatalf("a dry run ran %v", *calls)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "zod") && strings.Contains(l, "^3.0.0") }) {
		t.Fatalf("the dry run did not report the planned entry: %v", lines)
	}
}

// The job refuses what it cannot do before it writes anything: a version
// none, a hosted run, a directory without package.json and a bad name.
func TestDepsEditRefusals(t *testing.T) {
	files := map[string]string{"package.json": bunRoot, "bun.lock": "{}", "apps/api/package.json": `{"name":"api"}`}
	cases := map[string]struct {
		action string
		edit   depsEditEntry
		env    string
	}{
		"none":         {depsEditAdd, depsEditEntry{Project: "api", Path: "apps/api", Modules: []string{"zod@none"}}, ""},
		"hosted":       {depsEditAdd, depsEditEntry{Project: "api", Path: "apps/api", Modules: []string{"zod"}}, "1"},
		"no manifest":  {depsEditAdd, depsEditEntry{Project: "go", Path: "apps/go", Modules: []string{"zod"}}, ""},
		"bad name":     {depsEditAdd, depsEditEntry{Project: "api", Path: "apps/api", Modules: []string{"Not A Name"}}, ""},
		"outside root": {depsEditRemove, depsEditEntry{Project: "api", Path: "../x", Modules: []string{"zod"}}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, dir := depsEditWorkspace(t, files, c.action, []depsEditEntry{c.edit}, false)
			t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", c.env)
			stubRegistry(t, map[string]string{"zod@latest": "3.0.0"})
			stubBun(t)
			calls := recordCalls(t)
			if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
				t.Fatalf("runDepsEdit = %q, want FAILED", status)
			}
			for name, content := range files {
				assertFile(t, filepath.Join(dir, name), content)
			}
			if len(*calls) != 0 {
				t.Fatalf("a refused edit ran %v", *calls)
			}
		})
	}
}

func TestSplitPackageSpec(t *testing.T) {
	for spec, want := range map[string][2]string{
		"zod":                 {"zod", ""},
		"zod@^3":              {"zod", "^3"},
		"@putnami/database":   {"@putnami/database", ""},
		"@putnami/database@1": {"@putnami/database", "1"},
		"@scope/pkg@next":     {"@scope/pkg", "next"},
	} {
		if name, version := splitPackageSpec(spec); name != want[0] || version != want[1] {
			t.Errorf("splitPackageSpec(%q) = %q, %q; want %q, %q", spec, name, version, want[0], want[1])
		}
	}
}

// A pnpm remove drops the catalog entry from pnpm-workspace.yaml, keeping its
// other lines.
func TestAPNPMRemoveDropsThePNPMWorkspaceEntry(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`,
		"pnpm-workspace.yaml":   "packages:\n  - apps/*\ncatalogs:\n  tools:\n    zod: ^3.0.0 # validation\n    react: ^19.0.0\n",
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"catalog:tools"},"peerDependencies":{"react":"catalog:tools"}}`,
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod@^3"}}}, false)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), "packages:\n  - apps/*\ncatalogs:\n  tools:\n    react: ^19.0.0\n")
	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"peerDependencies\": {\n    \"react\": \"catalog:tools\"\n  }\n}\n")
}

// npm reads no workspace: protocol: a member package gets its own version.
func TestAnNPMAddOfAMemberPackageWritesItsVersion(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":           `{"name":"shop","private":true,"workspaces":["apps/*"]}`,
		"package-lock.json":      "{}",
		"apps/api/package.json":  `{"name":"api"}`,
		"apps/core/package.json": `{"name":"@shop/core","version":"1.2.0"}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"@shop/core"}}}, false)
	mockLookPath(t, map[string]string{"npm": "/usr/bin/npm"})
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"@shop/core\": \"^1.2.0\"\n  }\n}\n")
}

// An add with no version keeps the version the project already pins, with
// no registry lookup and no install.
func TestAnAddKeepsAVersionTheProjectPins(t *testing.T) {
	files := map[string]string{
		"package.json":          bunRoot,
		"bun.lock":              "{}",
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"^3.22.0"}}`,
	}
	ctx, dir := depsEditWorkspace(t, files, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	asked := stubRegistry(t, map[string]string{"zod@latest": "4.0.0"})
	stubBun(t)
	calls := recordCalls(t)

	runDepsEditOK(t, ctx)

	for name, content := range files {
		assertFile(t, filepath.Join(dir, name), content)
	}
	if len(*asked) != 0 || len(*calls) != 0 {
		t.Fatalf("a kept version looked up %v and ran %v", *asked, *calls)
	}
}

// A prune drops the target from the putnami section's dependencies too, and
// fails when the project lists it nowhere.
func TestAPruneDropsThePutnamiSectionEntry(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":           bunRoot,
		"bun.lock":               "{}",
		"apps/api/package.json":  `{"name":"api","putnami":{"type":"service","dependencies":["@shop/core","other"]}}`,
		"apps/core/package.json": `{"name":"@shop/core"}`,
	}, depsEditPrune, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"apps/core"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	var got struct {
		Putnami struct {
			Type         string   `json:"type"`
			Dependencies []string `json:"dependencies"`
		} `json:"putnami"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "apps/api/package.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got.Putnami.Type != "service" || !slices.Equal(got.Putnami.Dependencies, []string{"other"}) {
		t.Fatalf("putnami section = %+v, want type service and dependencies [other]", got.Putnami)
	}
	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("a prune of a target the project no longer lists = %q, want FAILED", status)
	}
	ctx.Params["edits"] = mustJSON(t, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"../outside"}}})
	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Fatalf("a prune of a target outside the workspace = %q, want FAILED", status)
	}
}

// A catalog entry the root's overrides reference stays when the last project
// stops referencing it.
func TestARemoveKeepsAnEntryAnOverrideReferences(t *testing.T) {
	root := "{\n  \"name\": \"shop\",\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"zod\": \"^3.0.0\"\n  },\n  \"overrides\": {\n    \"lib\": {\n      \"zod\": \"catalog:\"\n    }\n  }\n}\n"
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          root,
		"bun.lock":              "{}",
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"catalog:"}}`,
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "package.json"), root)
	for key, want := range map[string]string{"a>b@^2": "b", "**/zod": "zod", "lib/zod": "zod", "@s/lib/@t/zod": "@t/zod"} {
		if got := overrideTargets(key); !slices.Contains(got, want) {
			t.Fatalf("overrideTargets(%q) = %v, want %s among them", key, got, want)
		}
	}
}

// A Rush repository is refused before any file is read.
func TestDepsEditRefusesRush(t *testing.T) {
	ctx, _ := depsEditWorkspace(t, map[string]string{
		"rush.json":             "{}",
		"apps/api/package.json": `{"name":"api"}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	calls := recordCalls(t)
	if status, _, _ := runDepsEdit(ctx, jsonl.New(), nil); status != "FAILED" || len(*calls) != 0 {
		t.Fatalf("runDepsEdit = %q after %v, want FAILED and no process", status, *calls)
	}
}

// A remove that empties the Bun catalog drops its key.
func TestARemoveDropsAnEmptiedCatalog(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          "{\n  \"name\": \"shop\",\n  \"workspaces\": [\n    \"apps/*\"\n  ],\n  \"catalog\": {\n    \"zod\": \"^3.0.0\"\n  }\n}\n",
		"bun.lock":              "{}",
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"catalog:"}}`,
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "package.json"), "{\n  \"name\": \"shop\",\n  \"workspaces\": [\n    \"apps/*\"\n  ]\n}\n")
}

// Yarn 1 reads no workspace: protocol: a member without a version gets *.
func TestAYarnClassicAddOfAMemberWithoutVersionWritesStar(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":           `{"name":"shop","private":true,"workspaces":["apps/*"]}`,
		"yarn.lock":              "",
		"apps/api/package.json":  `{"name":"api"}`,
		"apps/core/package.json": `{"name":"@shop/core"}`,
	}, depsEditAdd, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"@shop/core"}}}, false)
	mockLookPath(t, map[string]string{"yarn": "/usr/bin/yarn"})
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "apps/api/package.json"), "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"@shop/core\": \"*\"\n  }\n}\n")
}

// A remove that empties a named Bun catalog under the workspaces object drops
// the catalogs key there, and drops the package from the putnami section.
func TestARemoveDropsAnEmptiedNamedCatalogUnderWorkspaces(t *testing.T) {
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          `{"name":"shop","workspaces":{"packages":["apps/*"],"catalogs":{"tools":{"zod":"^3.0.0"}}}}`,
		"bun.lock":              "{}",
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"catalog:tools"},"putnami":{"dependencies":["zod"]}}`,
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	stubBun(t)
	recordCalls(t)

	runDepsEditOK(t, ctx)

	var root struct {
		Workspaces map[string]json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "package.json"))), &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root.Workspaces["catalogs"]; ok || root.Workspaces["packages"] == nil {
		t.Fatalf("workspaces = %v, want packages and no catalogs", root.Workspaces)
	}
	if got := readFile(t, filepath.Join(dir, "apps/api/package.json")); strings.Contains(got, "zod") {
		t.Fatalf("api still names zod:\n%s", got)
	}
}

// Unreadable pnpm overrides keep every catalog entry.
func TestUnreadablePNPMOverridesKeepTheCatalog(t *testing.T) {
	yaml := "packages:\n  - apps/*\ncatalog:\n  zod: ^3.0.0\noverrides:\n  zod:\n    nested: catalog:\n"
	ctx, dir := depsEditWorkspace(t, map[string]string{
		"package.json":          `{"name":"shop","private":true,"packageManager":"pnpm@10.26.1"}`,
		"pnpm-workspace.yaml":   yaml,
		"apps/api/package.json": `{"name":"api","dependencies":{"zod":"catalog:"}}`,
	}, depsEditRemove, []depsEditEntry{{Project: "api", Path: "apps/api", Modules: []string{"zod"}}}, false)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	recordCalls(t)

	runDepsEditOK(t, ctx)

	assertFile(t, filepath.Join(dir, "pnpm-workspace.yaml"), yaml)
}
