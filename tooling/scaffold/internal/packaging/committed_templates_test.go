package packaging

import (
	"go.putnami.dev/protocol/features/spectest"

	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	templateproto "go.putnami.dev/protocol/template"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/jsonl"
)

// A template is a product surface, not a fixture: `putnami projects create`
// turns it into somebody's first Putnami project, and the first thing that
// project does is build, test and lint. Nothing else in the workspace compiles,
// type-checks or renders these directories — a template carries no go.mod and no
// package.json of its own — so the checks here are the only guard between a
// broken committed template and a user's empty workspace.
//
// They stay deliberately vocabulary-free about rendering: the render engine and
// its variable list belong to @putnami/cli (protocols/template deliberately
// declares neither), so restating that list here would be a second inventory
// that silently goes stale. What these tests assert instead is structural and
// owner-side: where a placeholder may appear, that a rendered manifest still
// parses, and that the manifest pair agrees with itself.

// placeholderPattern matches the render engine's substitution syntax. Only the
// SHAPE is asserted, never the set of legal variable names.
var placeholderPattern = regexp.MustCompile(`<%=\s*[A-Za-z0-9_]+\s*%>`)

// neutralValue stands in for whatever the render engine would substitute. It is
// a syntactically boring string so a rendered JSON document's validity depends
// on the template's punctuation, not on the value.
const neutralValue = "x"

type committedTemplate struct {
	// dir is the absolute template directory.
	dir string
	// rel is the workspace-relative path, used in failure messages.
	rel string
	// manifest is the parsed putnami.template.json.
	manifest *templateproto.Manifest
}

// workspaceRoot walks up from the test's working directory to the directory
// holding putnami.workspace.json.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no putnami.workspace.json found above the test working directory")
		}
		dir = parent
	}
}

// discoverCommittedTemplates walks the tracked tree for putnami.template.json.
// It walks rather than globbing a <lang>/templates/<name> shape so a template
// added somewhere else is still covered instead of silently unchecked. Fixture
// and testdata trees are skipped: those manifests exist to be invalid. Every
// hidden directory below the root is skipped too — that covers .git, .putnami,
// .gen, .context, and any .<name>.tmp-materialize-* staging directory another
// task may create and rename away mid-walk.
func discoverCommittedTemplates(t *testing.T) []committedTemplate {
	t.Helper()
	root := workspaceRoot(t)
	skip := map[string]bool{
		"node_modules": true, "testdata": true, "fixtures": true, "vendor": true,
	}

	var found []committedTemplate
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || skip[entry.Name()]) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != templateManifestFilename {
			return nil
		}
		dir := filepath.Dir(path)
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path this walk produced inside the workspace
		if err != nil {
			return err
		}
		manifest, diagnostics := templateproto.ParseAndValidateManifest(data)
		if manifest == nil || diag.HasErrors(diagnostics) {
			t.Errorf("%s: %s is not a valid template manifest: %s", rel, templateManifestFilename,
				formatTemplateDiagnostics(diagnostics))
			return nil
		}
		found = append(found, committedTemplate{dir: dir, rel: filepath.ToSlash(rel), manifest: manifest})
		return nil
	})
	if err != nil {
		t.Fatalf("walk workspace: %v", err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].rel < found[j].rel })
	if len(found) == 0 {
		t.Fatal("discovered no committed templates — the walk itself is broken")
	}
	return found
}

func formatTemplateDiagnostics(diagnostics []diag.Diagnostic) string {
	if len(diagnostics) == 0 {
		return "<none>"
	}
	messages := make([]string, 0, len(diagnostics))
	for _, d := range diagnostics {
		messages = append(messages, d.String())
	}
	return strings.Join(messages, "; ")
}

// walkTemplateFiles visits every file a template ships, relative to its root.
func walkTemplateFiles(t *testing.T, template committedTemplate, visit func(rel string, data []byte)) {
	t.Helper()
	err := filepath.WalkDir(template.dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(template.dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path this walk produced inside the workspace
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), data)
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk template: %v", template.rel, err)
	}
}

// TestCommittedTemplateManifestsAreValid is the discovery half of the suite:
// every committed putnami.template.json parses and validates under the protocol
// its consumers read it with. Discovery itself reports the failures, so this
// test also proves the walk found something to check.
func TestCommittedTemplateManifestsAreValid(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "committed-template-validity", "every-committed-template-manifest-parses-and-validates")
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Name == "" || template.manifest.Description == "" {
			t.Errorf("%s: manifest name/description = %q/%q, want both set",
				template.rel, template.manifest.Name, template.manifest.Description)
		}
	}
}

// TestCommittedTemplatesDeclareTheScaffoldProject pins the project side of the
// pair. A template directory is only reachable by `putnami package` — and only
// publishable as a template archive — when its putnami.json says so, and the
// packaging task in this package is what that declaration selects.
func TestCommittedTemplatesDeclareTheScaffoldProject(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "committed-template-validity", "each-sibling-putnami-json-declares-the-template-contract")
	for _, template := range discoverCommittedTemplates(t) {
		path := filepath.Join(template.dir, "putnami.json")
		data, err := os.ReadFile(path) //nolint:gosec // a workspace path from discovery
		if err != nil {
			t.Errorf("%s: read putnami.json: %v", template.rel, err)
			continue
		}
		var project struct {
			Name       string   `json:"name"`
			Type       string   `json:"type"`
			Extensions []string `json:"extensions"`
			Publish    []string `json:"publish"`
		}
		if err := json.Unmarshal(data, &project); err != nil {
			t.Errorf("%s: parse putnami.json: %v", template.rel, err)
			continue
		}
		if project.Type != "template" {
			t.Errorf("%s: putnami.json type = %q, want \"template\"", template.rel, project.Type)
		}
		if project.Name != template.manifest.Name {
			t.Errorf("%s: putnami.json name = %q, want the template name %q",
				template.rel, project.Name, template.manifest.Name)
		}
		if !containsString(project.Extensions, "@putnami/scaffold") {
			t.Errorf("%s: putnami.json extensions = %v, want @putnami/scaffold (the packager)",
				template.rel, project.Extensions)
		}
		if !containsString(project.Publish, "template-archives") {
			t.Errorf("%s: putnami.json publish = %v, want template-archives",
				template.rel, project.Publish)
		}
	}
}

// TestCommittedTemplatePlaceholdersOnlyAppearInTemplateFiles is the render-side
// invariant that does not need the variable vocabulary: substitution happens
// only for files whose name ends in .template. A placeholder anywhere else is
// copied through verbatim and ships to the user as literal "<%= … %>" text.
func TestCommittedTemplatePlaceholdersOnlyAppearInTemplateFiles(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "placeholders-are-substituted", "a-placeholder-appears-only-in-a-file-the-engine-substitutes-into")
	for _, template := range discoverCommittedTemplates(t) {
		walkTemplateFiles(t, template, func(rel string, data []byte) {
			if strings.HasSuffix(rel, templateFileSuffix) {
				return
			}
			if match := placeholderPattern.Find(data); match != nil {
				t.Errorf("%s/%s: contains the placeholder %q but is not a %s file, so it is copied verbatim",
					template.rel, rel, string(match), templateFileSuffix)
			}
		})
	}
}

// TestCommittedTemplatesShipAReadmeWhoseLinksResolve is the template half of
// the README requirement: a scaffolded project starts with a README that states its purpose, its
// commands and where its docs live, and whose links pass the lint check the
// project's first `putnami lint` runs. The template is rendered into a
// scratch workspace the way the engine renders it — the .template suffix
// dropped and every placeholder substituted — and checked with the same SDK
// rule the language extensions' lint-docs task applies.
func TestCommittedTemplatesShipAReadmeWhoseLinksResolve(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "templates-ship-a-readme", "every-template-renders-a-readme-whose-links-resolve")
	for _, template := range discoverCommittedTemplates(t) {
		readme := filepath.Join(template.dir, "README.md"+templateFileSuffix)
		if _, err := os.Stat(readme); err != nil {
			t.Errorf("%s: ships no README.md%s: %v", template.rel, templateFileSuffix, err)
			continue
		}
		workspace := t.TempDir()
		project := filepath.Join(workspace, "project")
		renderTemplate(t, template.dir, project)
		data, err := os.ReadFile(filepath.Join(project, "README.md"))
		if err != nil {
			t.Errorf("%s: the rendered project has no README.md: %v", template.rel, err)
			continue
		}
		text := string(data)
		for _, want := range []string{"# ", "putnami lint", "doc/"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the rendered README lacks %q; it states the purpose, the commands and where docs live", template.rel, want)
			}
		}
		findings, err := docslinks.CheckProject(workspace, project)
		if err != nil {
			t.Errorf("%s: check the rendered README: %v", template.rel, err)
			continue
		}
		for _, finding := range findings {
			t.Errorf("%s: rendered %s:%d: %s", template.rel, filepath.Base(finding.File), finding.Line, finding.Message)
		}
	}
}

// renderTemplate copies a template directory to dst the way the render engine
// writes it: a .template file loses its suffix and every placeholder in it is
// substituted with neutralValue.
func renderTemplate(t *testing.T, src, dst string) {
	t.Helper()
	copyTree(t, src, dst)
	err := filepath.WalkDir(dst, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, templateFileSuffix) {
			return walkErr
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path this walk produced in a scratch directory
		if err != nil {
			return err
		}
		rendered := placeholderPattern.ReplaceAll(data, []byte(neutralValue))
		if err := os.WriteFile(strings.TrimSuffix(path, templateFileSuffix), rendered, 0o644); err != nil {
			return err
		}
		return os.Remove(path)
	})
	if err != nil {
		t.Fatalf("render %s: %v", src, err)
	}
}

// TestCommittedTemplateJSONRendersToValidJSON is the regression for a scaffolded
// project whose manifests do not parse. Substitution is textual, so a stray
// comma or quote in a *.json.template is invisible until `projects create` has
// already written the broken file into somebody's workspace.
func TestCommittedTemplateJSONRendersToValidJSON(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "rendered-manifests-parse", "every-json-template-renders-to-valid-json")
	for _, template := range discoverCommittedTemplates(t) {
		walkTemplateFiles(t, template, func(rel string, data []byte) {
			if !strings.HasSuffix(rel, ".json"+templateFileSuffix) {
				return
			}
			rendered := placeholderPattern.ReplaceAll(data, []byte(neutralValue))
			var document any
			if err := json.Unmarshal(rendered, &document); err != nil {
				t.Errorf("%s/%s: renders to invalid JSON: %v", template.rel, rel, err)
			}
		})
	}
}

// TestCommittedTemplateProjectSelectsTheDeclaredExtension keeps the two halves of
// a template's identity from drifting: putnami.template.json tells the CLI which
// extension the resulting project uses, and putnami.json.template is what the
// project actually gets. A mismatch scaffolds a project no extension activates
// for, which fails as "nothing to build" rather than as a template defect.
func TestCommittedTemplateProjectSelectsTheDeclaredExtension(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "declared-extension-activates", "the-rendered-putnami-json-declares-the-manifests-extension")
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension == "" {
			continue
		}
		path := filepath.Join(template.dir, "putnami.json"+templateFileSuffix)
		data, err := os.ReadFile(path) //nolint:gosec // a workspace path from discovery
		if err != nil {
			t.Errorf("%s: template declares extension %q but ships no putnami.json%s: %v",
				template.rel, template.manifest.Extension, templateFileSuffix, err)
			continue
		}
		var project struct {
			Extensions []string `json:"extensions"`
		}
		rendered := placeholderPattern.ReplaceAll(data, []byte(neutralValue))
		if err := json.Unmarshal(rendered, &project); err != nil {
			t.Errorf("%s: parse rendered putnami.json%s: %v", template.rel, templateFileSuffix, err)
			continue
		}
		if !containsString(project.Extensions, template.manifest.Extension) {
			t.Errorf("%s: rendered project extensions = %v, want the declared %q",
				template.rel, project.Extensions, template.manifest.Extension)
		}
	}
}

// TestCommittedApplicationTemplatesAreServable is the regression for a scaffolded
// application that cannot be started. `putnami serve` resolves its entrypoint
// from the "./serve" export and fails with "no ./serve export found" otherwise,
// so an application template that omits it produces a project whose primary
// development command does not work.
func TestCommittedApplicationTemplatesAreServable(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "servable-applications", "an-application-template-declares-and-ships-its-serve-export")
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/typescript" {
			continue
		}
		projectData, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "putnami.json"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: read rendered project manifest: %v", template.rel, err)
			continue
		}
		var project struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(placeholderPattern.ReplaceAll(projectData, []byte(neutralValue)), &project); err != nil {
			t.Errorf("%s: parse rendered project manifest: %v", template.rel, err)
			continue
		}
		if project.Type != "application" {
			continue
		}

		packageData, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "package.json"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: application template ships no package.json%s: %v",
				template.rel, templateFileSuffix, err)
			continue
		}
		var manifest struct {
			Exports map[string]json.RawMessage `json:"exports"`
		}
		if err := json.Unmarshal(placeholderPattern.ReplaceAll(packageData, []byte(neutralValue)), &manifest); err != nil {
			t.Errorf("%s: parse rendered package.json: %v", template.rel, err)
			continue
		}
		entry, declared := manifest.Exports["./serve"]
		if !declared {
			t.Errorf("%s: package.json declares no \"./serve\" export, so `putnami serve` cannot resolve an entrypoint",
				template.rel)
			continue
		}
		var target string
		if err := json.Unmarshal(entry, &target); err != nil {
			// The condition-object form is equally valid; only the flat form can be
			// resolved to a file here, so stop at "it is declared".
			continue
		}
		resolved := filepath.Join(template.dir, filepath.FromSlash(strings.TrimPrefix(target, "./")))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("%s: \"./serve\" points at %q, which the template does not ship: %v",
				template.rel, target, err)
		}
	}
}

// renderedProjectManifest returns a template's rendered putnami.json fields the
// language checks below select on.
func renderedProjectManifest(t *testing.T, template committedTemplate) (kind, main string, ok bool) {
	t.Helper()
	data, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
		filepath.Join(template.dir, "putnami.json"+templateFileSuffix))
	if err != nil {
		t.Errorf("%s: read rendered project manifest: %v", template.rel, err)
		return "", "", false
	}
	var project struct {
		Type string `json:"type"`
		Main string `json:"main"`
	}
	if err := json.Unmarshal(placeholderPattern.ReplaceAll(data, []byte(neutralValue)), &project); err != nil {
		t.Errorf("%s: parse rendered project manifest: %v", template.rel, err)
		return "", "", false
	}
	return project.Type, project.Main, true
}

// bareImportPattern matches the package specifier of an import or require. Only
// the specifier's shape matters here, not the binding it introduces.
var bareImportPattern = regexp.MustCompile(`(?:from|require\(|import\()\s*['"]([^'"]+)['"]`)

// packageNameOf reduces an import specifier to the package a manifest declares:
// "@putnami/web/thing" is the package "@putnami/web", "react-dom/server" is
// "react-dom". A relative, absolute, builtin or test-runner specifier is not a
// declared dependency and returns "".
func packageNameOf(specifier string) string {
	if specifier == "" || strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
		return ""
	}
	if strings.HasPrefix(specifier, "node:") || strings.HasPrefix(specifier, "bun:") {
		return ""
	}
	parts := strings.Split(specifier, "/")
	if strings.HasPrefix(specifier, "@") {
		if len(parts) < 2 {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// TestCommittedTypeScriptTemplatesDeclareEveryPackageTheyImport is the TypeScript
// analog of the Go go.mod check. A scaffolded project whose package manifest
// omits a package its own sources import fails its first install or its first
// run, and reads as a broken framework rather than a broken template.
func TestCommittedTypeScriptTemplatesDeclareEveryPackageTheyImport(t *testing.T) {
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/typescript" {
			continue
		}
		packageData, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "package.json"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: TypeScript template ships no package.json%s: %v",
				template.rel, templateFileSuffix, err)
			continue
		}
		var manifest struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if err := json.Unmarshal(placeholderPattern.ReplaceAll(packageData, []byte(neutralValue)), &manifest); err != nil {
			t.Errorf("%s: parse rendered package.json: %v", template.rel, err)
			continue
		}
		declared := make(map[string]bool, len(manifest.Dependencies)+len(manifest.DevDependencies))
		for name := range manifest.Dependencies {
			declared[name] = true
		}
		for name := range manifest.DevDependencies {
			declared[name] = true
		}

		imported := make(map[string]string)
		walkTemplateFiles(t, template, func(rel string, data []byte) {
			base := strings.TrimSuffix(rel, templateFileSuffix)
			if !strings.HasSuffix(base, ".ts") && !strings.HasSuffix(base, ".tsx") {
				return
			}
			for _, match := range bareImportPattern.FindAllStringSubmatch(string(data), -1) {
				if name := packageNameOf(match[1]); name != "" {
					imported[name] = rel
				}
			}
		})

		for name, where := range imported {
			if !declared[name] {
				t.Errorf("%s/%s: imports %q, which the template's package.json does not declare",
					template.rel, where, name)
			}
		}
	}
}

// bundledServeImports are the packages the TypeScript extension's build writes
// into every application's generated serve entry (.gen/src/serve.bundled.ts,
// GenerateBundledServe in typescript/extension/internal/build/build.go). The
// application imports them, not the template's sources, so the check above
// cannot see them.
var bundledServeImports = []string{"@putnami/application", "@putnami/runtime"}

// TestCommittedApplicationTemplatesDeclareTheBundledServeImports: bun's isolated
// linker links into a project only the packages its package.json declares, so
// a scaffolded application that omits one its generated serve entry imports
// fails its first build with "Cannot find module".
func TestCommittedApplicationTemplatesDeclareTheBundledServeImports(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "generated-imports-are-declared", "an-application-template-declares-the-bundled-serve-imports")
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/typescript" {
			continue
		}
		if kind, _, ok := renderedProjectManifest(t, template); !ok || kind != "application" {
			continue
		}
		packageData, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "package.json"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: application template ships no package.json%s: %v",
				template.rel, templateFileSuffix, err)
			continue
		}
		var manifest struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if err := json.Unmarshal(placeholderPattern.ReplaceAll(packageData, []byte(neutralValue)), &manifest); err != nil {
			t.Errorf("%s: parse rendered package.json: %v", template.rel, err)
			continue
		}
		for _, name := range bundledServeImports {
			if _, declared := manifest.Dependencies[name]; !declared {
				t.Errorf("%s: the generated serve entry imports %q, which the template's package.json dependencies do not declare",
					template.rel, name)
			}
		}
	}
}

// TestCommittedLibraryTemplatesAreConsumedThroughTheirEntryPoint keeps a library
// template's shipped test honest about the surface it is testing. A test that
// reaches past the declared entry point demonstrates a consumption path a real
// consumer does not have, and lets an entry point that exports nothing look
// tested.
func TestCommittedLibraryTemplatesAreConsumedThroughTheirEntryPoint(t *testing.T) {
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/typescript" {
			continue
		}
		kind, main, ok := renderedProjectManifest(t, template)
		if !ok || kind != "library" || main == "" {
			continue
		}
		entry := filepath.ToSlash(filepath.Clean(main))

		walkTemplateFiles(t, template, func(rel string, data []byte) {
			base := strings.TrimSuffix(rel, templateFileSuffix)
			if !strings.HasPrefix(base, "test/") ||
				(!strings.HasSuffix(base, ".ts") && !strings.HasSuffix(base, ".tsx")) {
				return
			}
			for _, match := range bareImportPattern.FindAllStringSubmatch(string(data), -1) {
				specifier := match[1]
				if !strings.HasPrefix(specifier, ".") {
					continue
				}
				resolved := filepath.ToSlash(filepath.Clean(
					filepath.Join(filepath.Dir(base), filepath.FromSlash(specifier))))
				// A directory specifier resolves through its index file, which is
				// exactly what a package entry point is.
				if resolved == entry || resolved+"/index.ts" == entry || resolved+".ts" == entry {
					continue
				}
				t.Errorf("%s/%s: imports %q, which resolves to %q rather than the declared entry point %q",
					template.rel, rel, specifier, resolved, entry)
			}
		})
	}
}

// TestCommittedGoTemplatesRequireEveryFrameworkModuleTheyImport keeps a Go
// template's go.mod and its sources in agreement. A missing require makes the
// scaffolded project fail its very first build with an unresolved import, which
// reads as a broken framework rather than a broken template.
func TestCommittedGoTemplatesRequireEveryFrameworkModuleTheyImport(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "go-imports-are-required", "a-go-template-requires-every-putnami-module-it-imports")
	importPattern := regexp.MustCompile(`"(go\.putnami\.dev/[A-Za-z0-9._/-]+)"`)

	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/go" {
			continue
		}
		goMod, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "go.mod"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: Go template ships no go.mod%s: %v", template.rel, templateFileSuffix, err)
			continue
		}
		required := string(goMod)

		walkTemplateFiles(t, template, func(rel string, data []byte) {
			if !strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, ".go"+templateFileSuffix) {
				return
			}
			for _, match := range importPattern.FindAllStringSubmatch(string(data), -1) {
				modulePath := match[1]
				if !strings.Contains(required, modulePath+" ") {
					t.Errorf("%s/%s: imports %q, which go.mod%s does not require",
						template.rel, rel, modulePath, templateFileSuffix)
				}
			}
		})
	}
}

// TestCommittedGoTemplatesDeclareNothingTidyRemoves keeps a Go template's
// go.mod as `go mod tidy` leaves it. Tidy drops a toolchain line that names
// the go line's version, so a template that writes one gets its go.mod
// rewritten by the first install, and the build after it misses the cache on
// a file the user never edited.
func TestCommittedGoTemplatesDeclareNothingTidyRemoves(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "go-mod-survives-tidy", "a-go-template-writes-no-toolchain-line-tidy-drops")
	goLine := regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	toolchainLine := regexp.MustCompile(`(?m)^toolchain\s+(\S+)\s*$`)

	checked := 0
	for _, template := range discoverCommittedTemplates(t) {
		if template.manifest.Extension != "@putnami/go" {
			continue
		}
		goMod, err := os.ReadFile( //nolint:gosec // a workspace path from discovery
			filepath.Join(template.dir, "go.mod"+templateFileSuffix))
		if err != nil {
			t.Errorf("%s: Go template ships no go.mod%s: %v", template.rel, templateFileSuffix, err)
			continue
		}
		checked++
		version := goLine.FindSubmatch(goMod)
		if version == nil {
			t.Errorf("%s: go.mod%s declares no go line", template.rel, templateFileSuffix)
			continue
		}
		if toolchain := toolchainLine.FindSubmatch(goMod); toolchain != nil && string(toolchain[1]) == "go"+string(version[1]) {
			t.Errorf("%s: go.mod%s declares %q, which go mod tidy removes because the go line already says %s",
				template.rel, templateFileSuffix, "toolchain "+string(toolchain[1]), string(version[1]))
		}
	}
	if checked == 0 {
		t.Fatal("found no Go template to check")
	}
}

// TestCommittedTemplatesPackageIntoAConsumableArchive runs the REAL packaging job
// over each committed template in a throwaway workspace. It is the end-to-end
// check that what `putnami package` publishes still matches what the template
// directory holds: the manifest is stamped with a version, the project's own
// putnami.json is left behind (it configures the template, it is not part of the
// scaffolded project), and every .template file survives the trip.
func TestCommittedTemplatesPackageIntoAConsumableArchive(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "archive-contents", "the-archive-carries-the-manifest-and-every-template-file")
	for _, template := range discoverCommittedTemplates(t) {
		t.Run(template.rel, func(t *testing.T) {
			workspace := t.TempDir()
			projectPath := "template-under-test"
			copyTree(t, template.dir, filepath.Join(workspace, projectPath))

			ctx := &pctx.Context{
				WorkspaceRoot: workspace,
				Project:       pctx.Project{Name: template.manifest.Name, Path: projectPath},
				Workspace:     pctx.Workspace{Version: "9.9.9"},
			}
			status, result, err := Template(ctx, jsonl.New(), nil)
			if err != nil {
				t.Fatalf("Template: %v", err)
			}
			if status != "OK" {
				t.Fatalf("status = %q, want OK", status)
			}

			archive, _ := result["archive"].(string)
			wantName := template.manifest.Name + "-9.9.9.tar.gz"
			if filepath.Base(archive) != wantName {
				t.Errorf("archive = %q, want a file named %q", archive, wantName)
			}
			if _, err := os.Stat(archive); err != nil {
				t.Fatalf("archive not written: %v", err)
			}

			names := archiveEntries(t, archive)
			if !names[templateManifestFilename] {
				t.Errorf("archive omits %s; entries: %v", templateManifestFilename, sortedKeys(names))
			}
			// A scaffolded project must not inherit this workspace's declarations
			// about the template: a project manifest saying it is a template, a
			// feature manifest stating the template's intent, or specs linking to
			// decision records that do not exist in the user's tree.
			for _, name := range sortedKeys(names) {
				head, _, _ := strings.Cut(name, "/")
				if templateproto.IsTemplateRootPackagingExclusion(head) {
					t.Errorf("archive ships %q, which declares the template as a project in THIS workspace rather than content for the scaffolded one", name)
				}
			}
			walkTemplateFiles(t, template, func(rel string, _ []byte) {
				if !strings.HasSuffix(rel, templateFileSuffix) {
					return
				}
				if !names[rel] {
					t.Errorf("archive omits %q; entries: %v", rel, sortedKeys(names))
				}
			})

			stamped := stampedManifestVersion(t, archive)
			if stamped != "9.9.9" {
				t.Errorf("packaged manifest version = %q, want the version the archive name claims", stamped)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// archiveEntries returns the slash-separated member names of a packaged
// template archive, with the leading "./" the staging tar emits removed.
func archiveEntries(t *testing.T, archivePath string) map[string]bool {
	t.Helper()
	file, err := os.Open(archivePath) //nolint:gosec // an archive this test just produced
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only handle in a test

	decompressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gunzip archive: %v", err)
	}
	defer decompressed.Close() //nolint:errcheck // read-only handle in a test

	names := make(map[string]bool)
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names[strings.TrimPrefix(filepath.ToSlash(header.Name), "./")] = true
	}
	return names
}

// stampedManifestVersion reads the packaged template manifest's version back out
// of the archive, which is the only copy a consumer of the archive ever sees.
func stampedManifestVersion(t *testing.T, archivePath string) string {
	t.Helper()
	file, err := os.Open(archivePath) //nolint:gosec // an archive this test just produced
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only handle in a test

	decompressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gunzip archive: %v", err)
	}
	defer decompressed.Close() //nolint:errcheck // read-only handle in a test

	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if strings.TrimPrefix(filepath.ToSlash(header.Name), "./") != templateManifestFilename {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read packaged manifest: %v", err)
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("parse packaged manifest: %v", err)
		}
		return manifest.Version
	}
	t.Fatalf("archive contains no %s", templateManifestFilename)
	return ""
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path this walk produced inside the workspace
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("copy %s: %v", src, err)
	}
}
