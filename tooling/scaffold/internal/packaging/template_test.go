package packaging

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	templateproto "go.putnami.dev/protocol/template"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// writeTemplateProject creates a project directory under workspaceRoot containing
// a valid putnami.template.json plus a few template files exercising the file,
// directory, exclude, and dotfile branches of Template's staging loop.
func writeTemplateProject(t *testing.T, workspaceRoot, manifest string) string {
	t.Helper()
	projectDir := filepath.Join(workspaceRoot, "mytemplate")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	writeFile := func(rel, content string) {
		full := filepath.Join(projectDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	writeFile("putnami.template.json", manifest)
	// Regular files.
	writeFile("README.md", "# hello\n")
	writeFile("foo.template", "name: {{name}}\n")
	// Subdirectory to exercise the nested copy.
	writeFile("src/index.ts", "export const x = 1;\n")
	// Excluded project config + dotfile to exercise skip branches.
	writeFile("putnami.json", `{"name":"tpl"}`)
	// Declarations about the template as a project HERE, which a scaffolded
	// project must not inherit.
	writeFile("putnami.features.json", `{"protocolVersion":1,"namespace":"tooling","features":[]}`)
	writeFile("specs/tpl.json", `{"protocolVersion":1,"feature":"tooling/tpl"}`)
	writeFile(".gitignore", "node_modules\n")

	return projectDir
}

func newCtx(workspaceRoot string) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		Project:       pctx.Project{Name: "tpl", Path: "mytemplate"},
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Params:        pctx.Params{},
	}
}

const validManifest = `{"name":"my-template","version":"0.0.0"}`

func TestTemplate_HappyPath(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "manifest-selects-the-template", "a-project-with-a-template-manifest-is-packaged")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	ctx := newCtx(tmp)
	emit := jsonl.New()

	status, result, err := Template(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}

	// Archive named from manifest.name + resolved version (Workspace.Version).
	outputDir := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	wantArchive := filepath.Join(outputDir, "my-template-1.2.3.tar.gz")
	if _, err := os.Stat(wantArchive); err != nil {
		t.Fatalf("archive not found at %s: %v", wantArchive, err)
	}

	// Returned map points at the same archive + a written metadata.json.
	if got := result["archive"]; got != wantArchive {
		t.Fatalf("result archive = %v, want %v", got, wantArchive)
	}
	metaPath, ok := result["metadata"].(string)
	if !ok {
		t.Fatalf("result metadata missing/not a string: %v", result["metadata"])
	}
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if meta["version"] != "1.2.3" {
		t.Fatalf("metadata version = %v, want 1.2.3", meta["version"])
	}
	if meta["artifact"] != "my-template" {
		t.Fatalf("metadata artifact = %v, want my-template", meta["artifact"])
	}
	if meta["template"] != true {
		t.Fatalf("metadata template = %v, want true", meta["template"])
	}
	if meta["stable"] != false {
		t.Fatalf("metadata stable = %v, want false", meta["stable"])
	}

	if result["version"] != "1.2.3" {
		t.Fatalf("result version = %v, want 1.2.3", result["version"])
	}
}

func TestTemplate_VersionFromContextWins(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "resolved-version", "the-invocations-resolved-version-wins")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	ctx := newCtx(tmp)
	ctx.Version = &pctx.Version{Full: "2.0.0"} // takes precedence over Workspace.Version

	emit := jsonl.New()
	status, result, err := Template(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if result["version"] != "2.0.0" {
		t.Fatalf("result version = %v, want 2.0.0", result["version"])
	}

	outputDir := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	wantArchive := filepath.Join(outputDir, "my-template-2.0.0.tar.gz")
	if _, err := os.Stat(wantArchive); err != nil {
		t.Fatalf("archive not found at %s: %v", wantArchive, err)
	}
}

func TestTemplate_StableStripsPreRelease(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "resolved-version", "stable-strips-the-pre-release-suffix")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	ctx := newCtx(tmp)
	ctx.Workspace.Version = "1.2.3-rc.1"

	emit := jsonl.New()
	status, result, err := Template(ctx, emit, []string{"--stable"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if result["version"] != "1.2.3" {
		t.Fatalf("result version = %v, want stripped 1.2.3", result["version"])
	}

	outputDir := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	wantArchive := filepath.Join(outputDir, "my-template-1.2.3.tar.gz")
	if _, err := os.Stat(wantArchive); err != nil {
		t.Fatalf("archive not found at %s: %v", wantArchive, err)
	}

	// metadata.json should record stable=true.
	metaBytes, err := os.ReadFile(result["metadata"].(string))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if meta["stable"] != true {
		t.Fatalf("metadata stable = %v, want true", meta["stable"])
	}
}

func TestTemplate_DryRun(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "dry-run-produces-nothing", "a-dry-run-creates-no-archive-and-no-metadata")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	ctx := newCtx(tmp)
	emit := jsonl.New()

	status, result, err := Template(ctx, emit, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if result["dryRun"] != true {
		t.Fatalf("result dryRun = %v, want true", result["dryRun"])
	}
	if result["name"] != "my-template" {
		t.Fatalf("result name = %v, want my-template", result["name"])
	}

	// No archive should have been created.
	outputDir := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "archives")
	if _, err := os.Stat(filepath.Join(outputDir, "my-template-1.2.3.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("archive should not exist on dry-run, stat err = %v", err)
	}
}

func TestTemplate_NoProjectContext(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "resolved-version", "the-workspace-version-is-used-when-there-is-no-resolved-one")
	tmp := t.TempDir()
	ctx := newCtx(tmp)
	ctx.Project.Name = "" // no project context

	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", status)
	}
}

func TestTemplate_MissingManifest(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "manifest-selects-the-template", "a-missing-manifest-fails-the-job-with-a-diagnostic")
	tmp := t.TempDir()
	// Create the project dir but no putnami.template.json.
	if err := os.MkdirAll(filepath.Join(tmp, "mytemplate"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx := newCtx(tmp)
	status, result, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if result != nil {
		t.Fatalf("result = %v, want nil", result)
	}
}

func TestTemplate_InvalidManifestJSON(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "manifest-selects-the-template", "an-unparseable-manifest-fails-the-job-with-a-diagnostic")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, "{ this is not json")

	ctx := newCtx(tmp)
	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
}

func TestStripPreRelease(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "resolved-version", "pre-release-stripping-is-exact")
	tests := []struct {
		in   string
		want string
	}{
		{"1.2.3-rc1", "1.2.3"},
		{"1.2.3-rc.1", "1.2.3"},
		{"1.2.3", "1.2.3"},
		{"", ""},
		{"2.0.0-alpha-beta", "2.0.0"}, // cut at first dash only
	}
	for _, tt := range tests {
		if got := stripPreRelease(tt.in); got != tt.want {
			t.Errorf("stripPreRelease(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStampManifestVersion(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "stamped-manifest", "the-packaged-manifest-carries-the-archives-version")
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.template.json")
	if err := os.WriteFile(path, []byte(`{"name":"x","version":"0.0.0"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := stampManifestVersion(path, "9.9.9"); err != nil {
		t.Fatalf("stampManifestVersion: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m["version"] != "9.9.9" {
		t.Fatalf("version = %v, want 9.9.9", m["version"])
	}
	if m["name"] != "x" {
		t.Fatalf("name = %v, want x (preserved)", m["name"])
	}
}

// The archive carries the only copy of a template's version that a consumer
// ever sees, so a stamp that cannot land must be reported rather than skipped:
// an unstamped manifest inside an archive whose FILENAME states a version is
// the silent failure this pair of tests exists to prevent.

func TestStampManifestVersion_MissingFileIsAnError(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "stamped-manifest", "an-unwritable-stamp-fails-the-job")
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	if err := stampManifestVersion(path, "1.0.0"); err == nil {
		t.Fatal("stampManifestVersion on a missing staged manifest = nil, want error")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist, stat err = %v", err)
	}
}

func TestStampManifestVersion_InvalidJSONIsAnError(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "stamped-manifest", "an-unparseable-manifest-is-never-stamped")
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	original := "not json at all"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := stampManifestVersion(path, "1.0.0"); err == nil {
		t.Fatal("stampManifestVersion on an unparseable staged manifest = nil, want error")
	}

	// File left untouched because JSON parse failed.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != original {
		t.Fatalf("file content = %q, want unchanged %q", string(data), original)
	}
}

// TestTemplate_ExcludesWorkspaceSideDeclarations pins what a scaffolded project
// must NOT inherit. putnami.json, putnami.features.json and specs/ describe the
// template as a project in THIS workspace — a project typed "template", the
// intent behind it, and specs linking decision records that exist only here.
// Staging any of them hands the user declarations about somebody else's repo.
func TestTemplate_ExcludesWorkspaceSideDeclarations(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "archive-contents", "workspace-side-declarations-are-excluded-from-the-archive")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)
	ctx := newCtx(tmp)

	status, result, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}

	archive, _ := result["archive"].(string)
	names := archiveEntries(t, archive)
	for name := range names {
		head, _, _ := strings.Cut(name, "/")
		if templateproto.IsTemplateRootPackagingExclusion(head) {
			t.Errorf("archive ships %q, a workspace-side declaration", name)
		}
	}
	// The template's own content is still there.
	for _, want := range []string{"putnami.template.json", "foo.template", "src/index.ts", "README.md"} {
		if !names[want] {
			t.Errorf("archive omits template content %q; entries: %v", want, sortedKeys(names))
		}
	}
}

// TestTemplate_RecordsItsChannelInsideItsOwnedOutput is the regression for the
// merge point this task no longer has: the channel record lives inside
// the archives directory the task owns, so a sibling packager's record in ITS
// own directory is untouched — and the derived index reports both without
// either writer having cooperated. The task also states the whole archive
// publication manifest, which it alone owns.
func TestTemplate_RecordsItsChannelInsideItsOwnedOutput(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "shared-metadata-index", "a-sibling-packagers-record-in-its-own-directory-survives")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)
	ctx := newCtx(tmp)

	// A sibling packager recorded its own channel in the directory IT owns.
	siblingDir := pkgmeta.PackageOutputDir(tmp, ctx.Project.Path, "docker")
	if err := pkgmeta.WriteChannelRecord(siblingDir, pkgmeta.ChannelRecord{
		Version: "9.9.9", Channels: []string{"docker"}, Extra: map[string]any{"digest": "sha256:abc"},
	}); err != nil {
		t.Fatalf("seed sibling record: %v", err)
	}

	status, _, err := Template(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("Template status = %q, want OK", status)
	}

	index, err := pkgmeta.ReadChannelIndex(tmp, ctx.Project.Path)
	if err != nil {
		t.Fatalf("read channel index: %v", err)
	}
	if !index.HasChannel("template-archives") {
		t.Errorf("channels = %v, want template-archives recorded", index.Channels)
	}
	if !index.HasChannel("docker") {
		t.Errorf("channels = %v, want the sibling packager's docker channel still reported", index.Channels)
	}
	sibling := index.Records["docker"]
	if sibling.Version != "9.9.9" || sibling.Extra["digest"] != "sha256:abc" {
		t.Errorf("sibling record = %+v, want it untouched", sibling)
	}
	own := index.Records["archives"]
	if own.Extra["template"] != true {
		t.Errorf("own record = %+v, want the template marker", own)
	}

	// The archive publication manifest is this task's own declared output and
	// carries what the archive uploader reads.
	meta, err := pkgmeta.ReadPackageMetadata(tmp, ctx.Project.Path)
	if err != nil {
		t.Fatalf("read archive publication manifest: %v", err)
	}
	if !meta.HasChannel("template-archives") || !meta.Template || meta.Version != "1.2.3" {
		t.Errorf("archive publication manifest = %+v", meta)
	}
	if meta.HasChannel("docker") {
		t.Error("the archive manifest absorbed a sibling's channel; it describes the archives this task built")
	}
}

// Staging is an implementation detail, but the spec makes its cleanup a
// promise: the task stages into a temporary directory and removes it before
// returning. A leak would accumulate a full copy of every template across a
// long-lived agent or CI runner.
func TestTemplate_RemovesItsStagingDirectory(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "no-source-mutation", "the-staging-directory-is-removed-before-the-task-returns")
	tmp := t.TempDir()
	writeTemplateProject(t, tmp, validManifest)

	before := stagingDirs(t)
	if _, _, err := Template(newCtx(tmp), jsonl.New(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := stagingDirs(t)

	for dir := range after {
		if !before[dir] {
			t.Errorf("staging directory %s survived the task", dir)
		}
	}
}

// stagingDirs lists the staging directories Template creates, by the prefix
// it passes to os.MkdirTemp.
func stagingDirs(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "putnami-tpl-pkg-*"))
	if err != nil {
		t.Fatalf("glob staging dirs: %v", err)
	}
	found := make(map[string]bool, len(matches))
	for _, m := range matches {
		found[m] = true
	}
	return found
}
