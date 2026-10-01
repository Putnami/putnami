package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// The v3 task contract makes this extension's manifest
// load-bearing: a declared output is what the cache captures and restores, and
// two tasks claiming one path is a correctness bug that surfaces in a
// consumer's workspace at plan time, far from here. These tests are the
// extension's own conformance harness — they run the SAME predicates the CLI
// and the extension SDK run (proto.FullValidateManifest, which calls
// ValidateTaskContracts and ValidateOutputOwnership), against the REAL
// manifest, so a declaration that would be rejected downstream is rejected in
// this project's own test run first.

// loadExtensionManifest parses the shipped putnami.extension.json exactly as
// `putnami dev extension validate` and the package-time gate do.
func loadExtensionManifest(t *testing.T) *proto.Manifest {
	t.Helper()
	path := filepath.Join("..", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	m, diags := proto.ParseManifest(data)
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("parse extension manifest: %s", formatDiagnostics(diags))
	}
	return m
}

func formatDiagnostics(diags []diag.Diagnostic) string {
	if len(diags) == 0 {
		return "<none>"
	}
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.String())
	}
	return strings.Join(msgs, "\n  ")
}

// TestExtensionManifestPassesFullValidation is the `putnami dev extension
// validate` gate as a unit test: the real manifest must produce ZERO
// diagnostics under the comprehensive protocol entry point, which includes the
// v3 task-contract harness (exactness, one owner per output, honest effects).
func TestExtensionManifestPassesFullValidation(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "declared-output-ownership", "the-manifest-passes-the-protocol-validation-the-cli-applies")
	m := loadExtensionManifest(t)
	if diags := proto.FullValidateManifest(m); len(diags) > 0 {
		t.Fatalf("manifest is not conformant:\n  %s", formatDiagnostics(diags))
	}
}

// TestManifestExercisesTheV3TaskContract keeps the migration from silently
// regressing to v2: a manifest whose tasks lose their `declares` blocks would
// still validate (v3 is additive), so the version itself is asserted.
func TestManifestExercisesTheV3TaskContract(t *testing.T) {
	m := loadExtensionManifest(t)
	if got := proto.ManifestProtocolVersion(m); got != proto.ProtocolVersionV3 {
		t.Fatalf("manifest protocol version = %d, want %d (v3 task declarations)", got, proto.ProtocolVersionV3)
	}
}

// TestPackageStepsSelectContentForms pins the template step used by the Cloud
// release-set probe and the activation of each content form. An extension that
// provides Go binaries may have a putnami.extension.json of its own; only an
// agentContent declaration activates this packager for that project.
func TestPackageStepsSelectContentForms(t *testing.T) {
	m := loadExtensionManifest(t)
	run := m.Commands["package"].Run
	if len(run) != 2 {
		t.Fatalf("package run = %+v, want template and agent-content steps", run)
	}
	seen := map[string]bool{}
	for _, step := range run {
		if step.Task != "package-content" || step.Activation == nil {
			t.Errorf("package step %+v has no content packager or activation", step)
			continue
		}
		seen[step.ID] = true
		switch step.ID {
		case "template":
			if len(step.Activation.Files) != 1 || step.Activation.Files[0] != "putnami.template.json" || len(step.Activation.Contains) != 0 {
				t.Errorf("template activation = %+v, want only putnami.template.json", step.Activation)
			}
		case "agent-content":
			if got := step.Activation.Contains["putnami.extension.json"]; got != `"agentContent"` || len(step.Activation.Files) != 0 {
				t.Errorf("agent-content activation = %+v, want agentContent declaration", step.Activation)
			}
		default:
			t.Errorf("unexpected package step %q", step.ID)
		}
	}
	if !seen["template"] || !seen["agent-content"] {
		t.Errorf("package steps = %v, want template and agent-content", seen)
	}
}

// TestEveryTaskCarriesAV3Declaration pins that the migration is COMPLETE. v3 is
// additive per task, so a task that quietly loses (or never gains) its
// `declares` block keeps v2 inferred capture while the rest of the manifest is
// declared — the mixed state this contract exists to end.
func TestEveryTaskCarriesAV3Declaration(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "declared-output-ownership", "every-task-carries-a-v3-declaration")
	m := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(m) {
		if m.Tasks[name].Declares == nil {
			t.Errorf("task %q has no v3 declaration; every task in this manifest must state its outputs, effects and source mutation (an empty object states \"none of the three\")", name)
		}
	}
}

// ownerRow is one declared output and the task that owns it, in the form a
// reviewer reads the ownership decision in.
type ownerRow struct {
	task          string
	id            string
	kind          string
	root          string
	path          string
	optionalEmpty bool
}

func (r ownerRow) String() string {
	return fmt.Sprintf("%s.%s = %s %s:%s optionalEmpty=%t", r.task, r.id, r.kind, r.root, r.path, r.optionalEmpty)
}

// TestDeclaredOutputOwnerTable pins the declared output ownership for the
// scaffold extension as a table.
//
// The Go extension's package-archives task also declares
// `<command-output>/archives`. The content steps activate only for a template
// manifest or an agentContent declaration, so the scaffold's own Go extension
// project has one archive owner. The manifest-local half of the invariant is
// checked here; the planner checks cross-extension ownership for resolved refs.
//
// The row count is also the reason the two content packagers share one task:
// they write the same archive directory and the same archive publication
// manifest, and a second task declaring either would be a second owner of one
// path. The cross-extension note above applies to `metadata.json` too: the Go
// extension's package-archives declares it in ITS manifest.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "declared-output-ownership", "the-task-is-the-sole-owner-of-the-archives-directory")
	m := loadExtensionManifest(t)

	want := []ownerRow{
		{task: "package-content", id: "archives", kind: "directory", root: "command-output", path: "archives", optionalEmpty: true},
		// The archive publication manifest, whose only writer for a
		// content project is this task.
		{task: "package-content", id: "publication-manifest", kind: "file", root: "command-output", path: "metadata.json", optionalEmpty: true},
	}

	got := declaredOwnerRows(m)
	if len(got) != len(want) {
		t.Fatalf("declared output count = %d, want %d\n got: %s\nwant: %s",
			len(got), len(want), rowsString(got), rowsString(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("owner row %d = %s, want %s", i, got[i], want[i])
		}
	}

	// The manifest-local half of ONE OWNER PER OUTPUT, run exactly as the CLI
	// and the SDK run it.
	if diags := proto.ValidateOutputOwnership(m); len(diags) > 0 {
		t.Fatalf("declared outputs are not exclusively owned:\n  %s", formatDiagnostics(diags))
	}
}

func declaredOwnerRows(m *proto.Manifest) []ownerRow {
	var rows []ownerRow
	for name, task := range m.Tasks {
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			rows = append(rows, ownerRow{
				task:          name,
				id:            id,
				kind:          output.Kind,
				root:          output.EffectiveRoot(),
				path:          output.Path + output.PathFrom,
				optionalEmpty: output.OptionalEmpty,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].task != rows[j].task {
			return rows[i].task < rows[j].task
		}
		return rows[i].id < rows[j].id
	})
	return rows
}

func rowsString(rows []ownerRow) string {
	if len(rows) == 0 {
		return "\n  <none>"
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.String())
	}
	return "\n  " + strings.Join(out, "\n  ")
}

// TestPackageContentOwnsTheArchivePublicationManifest pins the file this
// extension writes at the root of the package output directory.
//
// It used to be a channel INDEX whose path the cross-extension package→publish
// contract fixed and which several packagers read-merge-wrote, so this task
// deliberately did not claim it — and the `package` command lost cache restore
// for everyone as a result. Splitting the two jobs it was doing fixed that: the channel
// index is DERIVED from a record each packager writes inside the directory it
// owns, and what remains at this path is the archive publication manifest the
// archive uploader reads. For a content project this task is its only writer, so
// it declares it and a cache restore of this task reproduces it.
func TestPackageContentOwnsTheArchivePublicationManifest(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "shared-metadata-index", "the-packager-owns-the-archive-publication-manifest")
	m := loadExtensionManifest(t)
	task, ok := m.Tasks["package-content"]
	if !ok {
		t.Fatal("manifest task \"package-content\" is missing")
	}
	if task.Declares == nil {
		t.Fatal("package-content lost its v3 declaration")
	}

	owned := map[string]string{}
	for id, output := range task.Declares.Outputs {
		if output.Root == proto.OutputRootCommandOutput {
			owned[output.Path] = id
		}
	}
	if _, claimed := owned["metadata.json"]; !claimed {
		t.Errorf("package-content does not declare <command-output>/metadata.json; it is the only writer of the "+
			"archive publication manifest for a content project, and an undeclared write is one a cache restore "+
			"cannot reproduce. Declared command-output paths: %v", owned)
	}
	if _, claimed := owned["archives"]; !claimed {
		t.Errorf("package-content does not declare <command-output>/archives: %v", owned)
	}
	// The channel record needs no declaration: it lives inside archives/, which
	// this task already owns whole.
	for path, id := range owned {
		if path != "archives" && path != "metadata.json" {
			t.Errorf("package-content declares an unexpected command-output path %q as %q", path, id)
		}
	}
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs. package-content declares none — it copies files
// with cp, compresses them with tar, and touches no registry, network, or
// machine-global cache — so the table is empty and any effect appearing here is
// a reviewed change.
func TestTaskEffectTable(t *testing.T) {
	m := loadExtensionManifest(t)

	want := map[string][]string{}

	for _, name := range sortedTaskNames(m) {
		task := m.Tasks[name]
		got := []string(nil)
		if task.Declares != nil {
			got = append(got, task.Declares.Effects...)
		}
		sort.Strings(got)
		expected := want[name]
		if strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Errorf("task %q effects = %v, want %v", name, got, expected)
		}
		for _, effect := range got {
			if proto.IsExternalTaskEffect(effect) && task.Cache.IsEnabled() {
				t.Errorf("task %q declares the external effect %q while caching is enabled", name, effect)
			}
		}
	}
}

// TestNoTaskMutatesSources pins that packaging never rewrites the template it
// reads: it stages a copy in a temp directory and archives that, so neither
// mutatesSources nor the project-scoped "sources" write resource belongs here.
func TestNoTaskMutatesSources(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "no-source-mutation", "no-task-declares-or-performs-a-source-mutation")
	m := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(m) {
		task := m.Tasks[name]
		mutates := task.Declares != nil && task.Declares.MutatesSources
		if mutates {
			t.Errorf("task %q declares mutatesSources; packaging stages into a temp directory and must not rewrite the template", name)
		}
		if mutates != declaresSourceWrite(task) {
			t.Errorf("task %q mutatesSources = %t but sources write resource = %t; the two must agree",
				name, mutates, declaresSourceWrite(task))
		}
	}
}

// TestTemplateFixturesShipNoV2ExtensionManifest is the scaffold half of "new
// projects are born v3".
//
// The templates this extension packages (`<language>/templates/<name>`) are
// project fixtures: they ship putnami.json, sources and a putnami.template.json,
// and NONE of them ships a putnami.extension.json — a scaffolded project consumes
// the language extensions, it does not carry one. That is why this changed
// no template fixture. This test keeps that true going forward: the day a
// template does ship an extension manifest, it must be born v3 and conformant,
// and the assertion fires here rather than in a user's fresh workspace.
func TestTemplateFixturesShipNoV2ExtensionManifest(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "*", "templates", "*", "putnami.template.json"))
	if err != nil {
		t.Fatalf("glob template fixtures: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("found no template fixtures at <language>/templates/<name>/putnami.template.json; " +
			"this guard must be repointed if the templates moved, not left silently passing")
	}

	for _, manifestPath := range matches {
		dir := filepath.Dir(manifestPath)
		extPath := filepath.Join(dir, "putnami.extension.json")
		data, err := os.ReadFile(extPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Errorf("read %s: %v", extPath, err)
			continue
		}
		m, diags := proto.ParseManifest(data)
		if m == nil || diag.HasErrors(diags) {
			t.Errorf("%s does not parse: %s", extPath, formatDiagnostics(diags))
			continue
		}
		if got := proto.ManifestProtocolVersion(m); got != proto.ProtocolVersionV3 {
			t.Errorf("%s is a v%d manifest; a template ships the shape new projects are born with, so it must declare "+
				"the v3 task contract", extPath, got)
		}
		if vDiags := proto.FullValidateManifest(m); len(vDiags) > 0 {
			t.Errorf("%s is not conformant:\n  %s", extPath, formatDiagnostics(vDiags))
		}
	}
}

// TestHarnessRejectsADishonestDeclaration is the mutation check that keeps the
// conformance tests above from being vacuous: flip one row of the real manifest
// and the SAME predicates that pass on it must fail. Without this, a harness
// that silently validated nothing would look identical to a passing one.
func TestHarnessRejectsADishonestDeclaration(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m *proto.Manifest)
		wantMsg string
	}{
		{
			// Two ids of one task claiming the same subtree: the archive
			// directory cannot have two owners even inside one declaration.
			name: "one path claimed twice",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["package-content"]
				task.Declares.Outputs["archivesAgain"] = proto.DeclaredOutput{
					Kind: proto.OutputKindFile,
					Root: proto.OutputRootCommandOutput,
					Path: "archives/template.tar.gz",
				}
				m.Tasks["package-content"] = task
			},
			wantMsg: "exactly one owner",
		},
		{
			// A glob is not a path anything can own.
			name: "glob path",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["package-content"]
				output := task.Declares.Outputs["archives"]
				output.Path = "archives/*.tar.gz"
				task.Declares.Outputs["archives"] = output
				m.Tasks["package-content"] = task
			},
			wantMsg: "glob",
		},
		{
			// An external effect on a cacheable task: a cache hit would replay
			// the stored result and never publish anything.
			name: "external effect while caching is enabled",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["package-content"]
				task.Declares.Effects = []string{proto.EffectRegistry}
				m.Tasks["package-content"] = task
			},
			wantMsg: "cache.enabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadExtensionManifest(t)
			if diags := proto.ValidateTaskContracts(m); len(diags) > 0 {
				t.Fatalf("the real manifest must be clean before it is mutated:\n  %s", formatDiagnostics(diags))
			}
			tc.mutate(m)
			diags := proto.ValidateTaskContracts(m)
			if len(diags) == 0 {
				t.Fatal("the mutated manifest produced no diagnostics; the conformance harness is not checking anything")
			}
			if !strings.Contains(formatDiagnostics(diags), tc.wantMsg) {
				t.Errorf("diagnostics do not mention %q:\n  %s", tc.wantMsg, formatDiagnostics(diags))
			}
		})
	}
}

func declaresSourceWrite(task proto.TaskDefinition) bool {
	for _, ref := range task.Writes {
		if ref.ID == proto.ResourceIDSources && ref.EffectiveScope() == proto.ResourceScopeProject {
			return true
		}
	}
	return false
}

func sortedTaskNames(m *proto.Manifest) []string {
	names := make([]string, 0, len(m.Tasks))
	for name := range m.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
