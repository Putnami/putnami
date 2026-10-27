package proof

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	templateproto "go.putnami.dev/protocol/template"
)

// goExtension is the extension every Go template names.
const goExtension = "@putnami/go"

// committedTemplate is a template directory beside this project, as its
// putnami.template.json declares it.
type committedTemplate struct {
	name     string
	dir      string
	manifest struct {
		Extension     string            `json:"extension"`
		TestVariables map[string]string `json:"testVariables"`
	}
}

// renderVars are the render variables of the CLI
// (tooling/cli/internal/template/render.go).
type renderVars struct {
	projectName           string
	projectPath           string
	projectModule         string
	putnamiVersion        string
	workspaceRelativePath string
}

// golangciConfig is the golangci-lint configuration the Go extension ships,
// relative to the workspace root. A rendered workspace holds no
// .golangci.yml, so `putnami lint` runs with this one
// (toolchain.ResolveGolangciConfig in go/extension/internal/toolchain/resolve.go).
const golangciConfig = "go/extension/config/.golangci.yml"

// toolVersionsFile pins the linters `putnami lint` runs, relative to the
// workspace root. The go.putnami.dev/go/extension/tools package embeds it.
const toolVersionsFile = "go/extension/tools/versions.json"

// provenTemplates maps each Go template beside this project to the feature it
// declares. Each bound test below renders one template into a throwaway
// workspace whose go.work is this repository's, and runs one step there
// without downloading a module: go vet and go build, go test, or the linters
// `putnami lint` runs. A template that no longer compiles against the
// framework, whose own tests fail, or that the linters reject, fails here. A
// binding names its feature, requirement and check literally, as the first
// statement of a top-level test, so a template added beside this project fails
// TestEveryGoTemplateDeclaresTheChecksThisProofObserves until provenTemplates
// lists it and it declares the three checks.
var provenTemplates = map[string]string{
	"go-library": "go/library-template",
	"go-server":  "go/server-template",
}

// provenSteps are the requirement and check of each step a bound test runs,
// the same for every template.
var provenSteps = []struct{ requirement, check string }{
	{"a-rendered-project-builds", "go-vet-and-go-build-pass"},
	{"a-rendered-project-passes-its-tests", "go-test-passes"},
	{"a-rendered-project-passes-lint", "the-linters-putnami-lint-runs-report-nothing"},
}

func TestGoLibraryTemplateBuilds(t *testing.T) {
	spectest.Proves(t, "go/library-template", "a-rendered-project-builds", "go-vet-and-go-build-pass")
	vetAndBuild(t, renderProject(t, "go-library"))
}

func TestGoLibraryTemplatePassesItsTests(t *testing.T) {
	spectest.Proves(t, "go/library-template", "a-rendered-project-passes-its-tests", "go-test-passes")
	goTest(t, renderProject(t, "go-library"), "TestHello")
}

func TestGoLibraryTemplatePassesLint(t *testing.T) {
	spectest.Proves(t, "go/library-template", "a-rendered-project-passes-lint", "the-linters-putnami-lint-runs-report-nothing")
	lint(t, renderProject(t, "go-library"))
}

func TestGoServerTemplateBuilds(t *testing.T) {
	spectest.Proves(t, "go/server-template", "a-rendered-project-builds", "go-vet-and-go-build-pass")
	vetAndBuild(t, renderProject(t, "go-server"))
}

func TestGoServerTemplatePassesItsTests(t *testing.T) {
	spectest.Proves(t, "go/server-template", "a-rendered-project-passes-its-tests", "go-test-passes")
	goTest(t, renderProject(t, "go-server"), "TestHealthEndpoint")
}

func TestGoServerTemplatePassesLint(t *testing.T) {
	spectest.Proves(t, "go/server-template", "a-rendered-project-passes-lint", "the-linters-putnami-lint-runs-report-nothing")
	lint(t, renderProject(t, "go-server"))
}

// TestEveryGoTemplateIsADeclaredInput pins the other half of the cache key:
// every template directory beside this project is read whole, a template
// added beside it changes the key through the manifest glob, and so do the
// lint configuration and the linter pins the proof reads.
func TestEveryGoTemplateIsADeclaredInput(t *testing.T) {
	proofDir := workingDirectory(t)
	root := workspaceRoot(t, proofDir)
	patterns := declaredPatterns(t, proofDir)
	if !patterns["../*/putnami.template.json"] {
		t.Error(`options.test.filePatterns lacks "../*/putnami.template.json", so a new template beside this project would not change its key`)
	}
	for _, file := range []string{golangciConfig, toolVersionsFile} {
		fromProof, err := filepath.Rel(proofDir, filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		if pattern := filepath.ToSlash(fromProof); !patterns[pattern] {
			t.Errorf("options.test.filePatterns lacks %q, so a change to %s would not rerun this proof", pattern, file)
		}
	}
	for _, tpl := range discoverTemplates(t, filepath.Dir(proofDir)) {
		if pattern := "../" + tpl.name + "/**"; !patterns[pattern] {
			t.Errorf("options.test.filePatterns lacks %q, so a change to the %s template would not rerun this proof", pattern, tpl.name)
		}
	}
}

// TestEveryModuleARenderedProjectCompilesIsADeclaredInput pins the cache key
// from the rendered side: every workspace module a rendered template compiles
// is a declared input of this project, whole, so a change to it reruns the
// proof.
func TestEveryModuleARenderedProjectCompilesIsADeclaredInput(t *testing.T) {
	proofDir := workingDirectory(t)
	patterns := declaredPatterns(t, proofDir)
	for _, tpl := range discoverTemplates(t, filepath.Dir(proofDir)) {
		p := renderProject(t, tpl.name)
		for _, dir := range strings.Fields(runGo(t, p.dir, p.env,
			"list", "-deps", "-test", "-f", "{{with .Module}}{{.Dir}}{{end}}", "./...")) {
			rel, err := filepath.Rel(p.root, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			fromProof, err := filepath.Rel(proofDir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if pattern := filepath.ToSlash(fromProof) + "/**"; !patterns[pattern] {
				t.Errorf("the rendered %s compiles module %s, which is not a declared input: add %q to options.test.filePatterns in putnami.json",
					tpl.name, filepath.ToSlash(rel), pattern)
			}
		}
	}
}

// TestEveryGoTemplateDeclaresOneFeatureUnderProjectScaffolding pins ADR 0003 of
// tooling/scaffold: each template declares one feature of its own, whose
// parent is tooling/project-scaffolding.
func TestEveryGoTemplateDeclaresOneFeatureUnderProjectScaffolding(t *testing.T) {
	for _, tpl := range discoverTemplates(t, filepath.Dir(workingDirectory(t))) {
		features := templateFeatures(t, tpl)
		if len(features) != 1 {
			t.Errorf("%s declares %d features, want 1", tpl.name, len(features))
			continue
		}
		feature := features[0]
		if want := provenTemplates[tpl.name]; feature.ID != want {
			t.Errorf("%s declares the feature %q, want %q", tpl.name, feature.ID, want)
		}
		relations := feature.Relations
		if len(relations) != 1 || relations[0].Kind != "parent" || relations[0].Target != "tooling/project-scaffolding" {
			t.Errorf("%s: the feature %s has the relations %+v, want one parent relation to tooling/project-scaffolding",
				tpl.name, feature.ID, relations)
		}
	}
}

// TestEveryGoTemplateEnforcesItsSpec pins the setting that makes a check no
// test observes fail the template's spec gate: in report mode, the default,
// the gate reports check-not-observed without failing.
func TestEveryGoTemplateEnforcesItsSpec(t *testing.T) {
	for _, tpl := range discoverTemplates(t, filepath.Dir(workingDirectory(t))) {
		data, err := os.ReadFile(filepath.Join(tpl.dir, "putnami.json"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Extensions []string `json:"extensions"`
			Options    struct {
				SDD struct {
					Verification struct {
						Specs string `json:"specs"`
					} `json:"verification"`
				} `json:"sdd"`
			} `json:"options"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatalf("%s/putnami.json: %v", tpl.name, err)
		}
		if !slices.Contains(config.Extensions, "/tooling/sdd-extension") {
			t.Errorf("%s/putnami.json does not list /tooling/sdd-extension in extensions, so no spec gate runs on it", tpl.name)
		}
		if got := config.Options.SDD.Verification.Specs; got != "enforce" {
			t.Errorf("%s/putnami.json sets options.sdd.verification.specs to %q, want \"enforce\"", tpl.name, got)
		}
	}
}

// TestEveryGoTemplateIsADependencyOfThisProof pins the edge the spec gate
// follows: a session that validates a template without running this proof
// reads the template's checks from this proof's last test result only when
// the proof depends on the template.
func TestEveryGoTemplateIsADependencyOfThisProof(t *testing.T) {
	proofDir := workingDirectory(t)
	root := workspaceRoot(t, proofDir)
	data, err := os.ReadFile(filepath.Join(proofDir, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Dependencies []string `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range discoverTemplates(t, filepath.Dir(proofDir)) {
		rel, err := filepath.Rel(root, tpl.dir)
		if err != nil {
			t.Fatal(err)
		}
		if project := "/" + filepath.ToSlash(rel); !slices.Contains(config.Dependencies, project) {
			t.Errorf("putnami.json does not list %s in dependencies, so the spec gate of %s cannot read this proof's result", project, tpl.name)
		}
	}
}

// TestEveryGoTemplateDeclaresTheChecksThisProofObserves keeps each template's
// declared checks equal to provenSteps: each template beside this project is
// listed in provenTemplates and declares the check of each step, and only
// those.
func TestEveryGoTemplateDeclaresTheChecksThisProofObserves(t *testing.T) {
	want := make([]string, 0, len(provenSteps))
	for _, step := range provenSteps {
		want = append(want, step.requirement+"#"+step.check)
	}
	slices.Sort(want)
	discovered := map[string]bool{}
	for _, tpl := range discoverTemplates(t, filepath.Dir(workingDirectory(t))) {
		discovered[tpl.name] = true
		feature, bound := provenTemplates[tpl.name]
		if !bound {
			t.Errorf("no test of this proof observes the template %s: give it a feature and a spec, add it to provenTemplates, and add one bound test per step, as the go-library tests do", tpl.name)
			continue
		}
		var declared []string
		for _, f := range templateFeatures(t, tpl) {
			for _, requirement := range f.Requirements {
				for _, check := range requirement.Verification.Checks {
					declared = append(declared, requirement.ID+"#"+check)
				}
			}
		}
		slices.Sort(declared)
		if !slices.Equal(declared, want) {
			t.Errorf("%s declares the checks %v under %s, want %v: the bound tests of this proof observe exactly those", tpl.name, declared, feature, want)
		}
	}
	for name := range provenTemplates {
		if !discovered[name] {
			t.Errorf("provenTemplates names %s, which is no Go template beside this project", name)
		}
	}
}

// TestARenderLeavesOutWhatTheArchiveLeavesOut pins the render rule that keeps a
// template's own declarations out of the project created from it: the rendered
// project holds no putnami.features.json, specs/ or specs.baseline.json, and
// its putnami.json is the one putnami.json.template renders.
func TestARenderLeavesOutWhatTheArchiveLeavesOut(t *testing.T) {
	for _, tpl := range discoverTemplates(t, filepath.Dir(workingDirectory(t))) {
		vars := testVars(tpl)
		project := filepath.Join(t.TempDir(), vars.projectPath)
		renderTemplate(t, tpl.dir, project, vars)
		for _, name := range []string{"putnami.features.json", "specs", "specs.baseline.json"} {
			if _, err := os.Stat(filepath.Join(project, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the rendered %s holds %s, which the archive leaves out (%v)", tpl.name, name, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(project, "putnami.json"))
		if err != nil {
			t.Fatalf("the rendered %s has no putnami.json: %v", tpl.name, err)
		}
		var config struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatalf("the rendered %s/putnami.json: %v", tpl.name, err)
		}
		if config.Name != vars.projectName {
			t.Errorf("the rendered %s/putnami.json names %q, want %q from putnami.json.template", tpl.name, config.Name, vars.projectName)
		}
	}
}

// renderedProject is a template rendered into a throwaway workspace whose
// go.work is this repository's, with the third-party modules it compiles in
// the module cache.
type renderedProject struct {
	root    string   // this repository
	dir     string   // the rendered project
	offline []string // the environment of the offline steps
	env     []string // offline, with GOWORK naming the rendered workspace's go.work
}

// renderProject renders the Go template called name, beside this project, into
// a throwaway workspace. The third-party modules the rendered project compiles
// come from the module cache, which a fresh machine does not have: this step
// downloads exactly those, checked against go.sum, before the network goes.
func renderProject(t *testing.T, name string) renderedProject {
	t.Helper()
	proofDir := workingDirectory(t)
	root := workspaceRoot(t, proofDir)
	tpl := templateNamed(t, filepath.Dir(proofDir), name)
	// The real path: go matches the working directory it reads against the
	// go.work entries, and a temporary directory can sit behind a symbolic link
	// (/var on macOS).
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vars := testVars(tpl)
	project := filepath.Join(workspace, vars.projectPath)
	renderTemplate(t, tpl.dir, project, vars)
	workFile := writeWorkFile(t, root, workspace, project)
	runGo(t, project, append(fetchGoEnv(t), "GOWORK="+workFile), "list", "-deps", "-test", "./...")
	offline := offlineGoEnv(t)
	return renderedProject{
		root:    root,
		dir:     project,
		offline: offline,
		env:     append(slices.Clone(offline), "GOWORK="+workFile),
	}
}

// templateNamed returns the Go template called name in dir.
func templateNamed(t *testing.T, dir, name string) committedTemplate {
	t.Helper()
	for _, tpl := range discoverTemplates(t, dir) {
		if tpl.name == name {
			return tpl
		}
	}
	t.Fatalf("no Go template named %s beside this project", name)
	return committedTemplate{}
}

// templateFeature is a feature a template declares, with what this proof
// reads of it.
type templateFeature struct {
	ID        string `json:"id"`
	Relations []struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	} `json:"relations"`
	Requirements []struct {
		ID           string `json:"id"`
		Verification struct {
			Checks []string `json:"checks"`
		} `json:"verification"`
	} `json:"requirements"`
}

// templateFeatures reads the features tpl declares in its putnami.features.json.
func templateFeatures(t *testing.T, tpl committedTemplate) []templateFeature {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tpl.dir, "putnami.features.json"))
	if err != nil {
		t.Fatalf("%s declares no feature: %v", tpl.name, err)
	}
	var manifest struct {
		Features []templateFeature `json:"features"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("%s/putnami.features.json: %v", tpl.name, err)
	}
	return manifest.Features
}

// vetAndBuild runs go vet, then go build, in p.
func vetAndBuild(t *testing.T, p renderedProject) {
	t.Helper()
	runGo(t, p.dir, p.env, "vet", "./...")
	runGo(t, p.dir, p.env, "build", "./...")
}

// goTest runs the tests p ships and fails unless the test named want reports a
// pass: go test also exits 0 on a package without test files.
func goTest(t *testing.T, p renderedProject, want string) {
	t.Helper()
	passed := map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(runGo(t, p.dir, p.env, "test", "-count=1", "-json", "./...")))
	for {
		var event struct{ Action, Test string }
		if err := dec.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go test -json: %v", err)
		}
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	if !passed[want] {
		t.Errorf("go test reported no pass for %s", want)
	}
}

// lintControl holds the verdict of checkLintControl. It runs once per test
// process: every lint step depends on it, and it lints a planted module.
var lintControl struct {
	once sync.Once
	err  error
}

// lint runs the read-only pass of the lint job
// (go/extension/internal/jobs/lint/lint.go) in p: golangci-lint with the
// extension's configuration, then staticcheck. A linter the proof cannot run,
// or a control showing that the configuration hides a finding, fails the step.
func lint(t *testing.T, p renderedProject) {
	t.Helper()
	config := filepath.Join(p.root, filepath.FromSlash(golangciConfig))
	tools, err := resolveLinters(t, p.root, p.offline)
	if err != nil {
		t.Fatal(err)
	}
	lintControl.once.Do(func() { lintControl.err = checkLintControl(t, p.offline, tools.golangci, config) })
	if lintControl.err != nil {
		t.Fatal(lintControl.err)
	}
	runCommand(t, p.dir, p.env, tools.golangci, "run", "--allow-parallel-runners", "--config", config, "./...")
	runCommand(t, p.dir, p.env, tools.staticcheck, "./...")
}

func workingDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// workspaceRoot returns the nearest directory at or above start that holds
// putnami.workspace.json.
func workspaceRoot(t *testing.T, start string) string {
	t.Helper()
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no putnami.workspace.json above %s", start)
		}
		dir = parent
	}
}

// discoverTemplates lists the Go templates directly under dir, in byte order.
func discoverTemplates(t *testing.T, dir string) []committedTemplate {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var templates []committedTemplate
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name(), "putnami.template.json"))
		if err != nil {
			continue
		}
		tpl := committedTemplate{name: entry.Name(), dir: filepath.Join(dir, entry.Name())}
		if err := json.Unmarshal(data, &tpl.manifest); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if tpl.manifest.Extension == goExtension {
			templates = append(templates, tpl)
		}
	}
	if len(templates) == 0 {
		t.Fatalf("no template beside this project names %s, so the proof would check nothing", goExtension)
	}
	return templates
}

// declaredPatterns returns the set of options.test.filePatterns this project
// declares in its putnami.json.
func declaredPatterns(t *testing.T, proofDir string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(proofDir, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	patterns := map[string]bool{}
	for _, pattern := range config.Options.Test.FilePatterns {
		patterns[pattern] = true
	}
	return patterns
}

// testVars returns the variables `putnami templates test` renders with:
// DefaultTestVars, with the manifest's testVariables overriding projectName and
// projectModule only (tooling/cli/internal/commands/extensions/templates.go).
func testVars(tpl committedTemplate) renderVars {
	vars := renderVars{
		projectName:           "test-project",
		projectPath:           "test-project",
		projectModule:         "test_project",
		putnamiVersion:        "latest",
		workspaceRelativePath: "..",
	}
	if v, ok := tpl.manifest.TestVariables["projectName"]; ok {
		vars.projectName = v
	}
	if v, ok := tpl.manifest.TestVariables["projectModule"]; ok {
		vars.projectModule = v
	}
	return vars
}

// leftPlaceholder finds a render placeholder that survived rendering.
var leftPlaceholder = regexp.MustCompile(`<%=[^%]*%>`)

// renderTemplate replays template.RenderDir
// (tooling/cli/internal/template/render.go), whose rules
// protocols/template/doc/02-variables.md documents: the walk is depth-first in
// byte order, the root manifest and every root entry the archive leaves out
// (template.IsTemplateRootPackagingExclusion) are skipped, `__module__` in a
// path becomes projectModule, and a `.template` file gets `<%= name %>`
// substitution and loses its suffix. Unlike the CLI, a placeholder left after
// rendering fails here: it would reach the user as literal text.
func renderTemplate(t *testing.T, src, dst string, vars renderVars) {
	t.Helper()
	replacer := placeholderReplacer(vars)
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == templateproto.ManifestFilename {
			return nil
		}
		if rel != "." && filepath.Dir(rel) == "." && templateproto.IsTemplateRootPackagingExclusion(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, strings.ReplaceAll(rel, "__module__", vars.projectModule))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if trimmed, ok := strings.CutSuffix(target, ".template"); ok {
			target = trimmed
			data = []byte(replacer.Replace(string(data)))
			if left := leftPlaceholder.Find(data); left != nil {
				t.Fatalf("%s still carries the placeholder %s after rendering", filepath.ToSlash(rel), left)
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("render %s: %v", src, err)
	}
}

// placeholderReplacer replays template.EvalTemplate: an empty value leaves its
// placeholder in place.
func placeholderReplacer(vars renderVars) *strings.Replacer {
	var pairs []string
	for key, value := range map[string]string{
		"projectName":           vars.projectName,
		"projectPath":           vars.projectPath,
		"projectModule":         vars.projectModule,
		"putnamiVersion":        vars.putnamiVersion,
		"workspaceRelativePath": vars.workspaceRelativePath,
	} {
		if value != "" {
			pairs = append(pairs, "<%= "+key+" %>", value)
		}
	}
	return strings.NewReplacer(pairs...)
}

// writeWorkFile writes workspace/go.work: this repository's go.work with every
// path made absolute, plus the rendered project. A go.putnami.dev module the
// template requires therefore resolves to this repository's source, as the
// module proxy resolves it for a consumer. go.work.sum is copied beside it.
func writeWorkFile(t *testing.T, root, workspace, project string) string {
	t.Helper()
	cmd := exec.Command("go", "work", "edit", "-json", filepath.Join(root, "go.work"))
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go work edit -json: %v", err)
	}
	var work struct {
		Go        string
		Toolchain string
		Use       []struct{ DiskPath string }
		Replace   []struct {
			Old struct{ Path, Version string }
			New struct{ Path, Version string }
		}
	}
	if err := json.Unmarshal(out, &work); err != nil {
		t.Fatal(err)
	}
	absolute := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(root, filepath.FromSlash(path))
	}
	var b bytes.Buffer
	b.WriteString("go " + work.Go + "\n")
	if work.Toolchain != "" {
		b.WriteString("toolchain " + work.Toolchain + "\n")
	}
	b.WriteString("\nuse (\n")
	for _, use := range work.Use {
		b.WriteString("\t" + strconv.Quote(absolute(use.DiskPath)) + "\n")
	}
	b.WriteString("\t" + strconv.Quote(project) + "\n)\n")
	for _, r := range work.Replace {
		old := strconv.Quote(r.Old.Path)
		if r.Old.Version != "" {
			old += " " + r.Old.Version
		}
		target := r.New.Path
		if r.New.Version != "" {
			target = strconv.Quote(target) + " " + r.New.Version
		} else {
			target = strconv.Quote(absolute(target))
		}
		b.WriteString("replace " + old + " => " + target + "\n")
	}
	file := filepath.Join(workspace, "go.work")
	if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.work.sum")); err == nil {
		if err := os.WriteFile(file+".sum", sum, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return file
}

// goEnvSettings reads the go command's settings the proof passes on
// explicitly: the cache locations, which keep the build and module caches the
// shared ones, and the settings that decide where a download goes.
func goEnvSettings(t *testing.T) map[string]string {
	t.Helper()
	out, err := exec.Command("go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH",
		"GOPROXY", "GONOPROXY", "GOPRIVATE", "GONOSUMDB", "GOSUMDB", "GOINSECURE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	var settings map[string]string
	if err := json.Unmarshal(out, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

// baseGoEnv is the caller's environment without the user's go env file.
// GOENV=off drops that file, which can name another toolchain's GOROOT or send
// a go.putnami.dev lookup to the network; the cache locations it may set are
// passed explicitly. GOFLAGS pins -mod=readonly, so no go.mod or go.sum
// changes and an ambient flag cannot change the check, and the CLI's own
// PUTNAMI_* variables, which describe this project, are dropped. GOCACHEPROG
// goes with them: the Go extension's cache helper finds its cache directory
// through those variables, and a helper without one refuses every write, which
// the go command reports as a standard package that is "not in std". The
// steps use GOCACHE, the directory that helper stores its objects in. os/exec
// keeps the last value of a repeated variable.
func baseGoEnv(settings map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PUTNAMI_") && !strings.HasPrefix(kv, "GOCACHEPROG=") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GOTOOLCHAIN=local",
		"GOCACHE="+settings["GOCACHE"],
		"GOMODCACHE="+settings["GOMODCACHE"],
		"GOPATH="+settings["GOPATH"],
	)
}

// fetchGoEnv is the environment of the step that fills the module cache: the
// go command's own download settings reach the network, and every download is
// checked against go.sum. The framework modules resolve through go.work and are
// never fetched.
func fetchGoEnv(t *testing.T) []string {
	t.Helper()
	settings := goEnvSettings(t)
	env := baseGoEnv(settings)
	for _, name := range []string{"GOPROXY", "GONOPROXY", "GOPRIVATE", "GONOSUMDB", "GOSUMDB", "GOINSECURE"} {
		env = append(env, name+"="+settings[name])
	}
	return env
}

// offlineGoEnv is the environment of the vet, build and test steps: the
// network is taken away from the go command, so a module that is neither in
// go.work nor in the module cache fails loudly instead of being fetched.
func offlineGoEnv(t *testing.T) []string {
	t.Helper()
	return append(baseGoEnv(goEnvSettings(t)), "GOPROXY=off")
}

// runGo runs the go command in dir and returns its standard output; a failure
// fails the test with the command's combined output.
func runGo(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	return runCommand(t, dir, env, "go", args...)
}

// runCommand runs name in dir and returns its standard output; a failure fails
// the test with the command's combined output. PWD names dir, as a shell would
// set it: an explicit environment otherwise carries this process's own.
func runCommand(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), "PWD="+dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s%s", filepath.Base(name), strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// linters are the binaries of the linters `putnami lint` runs on a Go
// project.
type linters struct {
	golangci    string
	staticcheck string
}

// resolveLinters finds the pinned golangci-lint and staticcheck. The error
// names the first one that is missing.
func resolveLinters(t *testing.T, root string, env []string) (linters, error) {
	t.Helper()
	// The go on PATH is the Go the extension resolves (toolchain.ResolveGo),
	// the one `putnami install` keys the tool home by: the extension runs
	// `go test` with it, and `go test` puts its GOROOT/bin first on the
	// test's PATH. The first field: a toolchain can append a suffix such as
	// " X:boringcrypto".
	fields := strings.Fields(runCommand(t, root, env, "go", "env", "GOVERSION"))
	if len(fields) == 0 {
		t.Fatal("go env GOVERSION printed nothing")
	}
	local := localGo{lang: version.Lang(fields[0]), key: "go" + goMajorMinor(fields[0])}
	if local.lang == "" {
		t.Fatalf("go env GOVERSION printed %q, which names no Go language version", fields[0])
	}
	golangci, err := pinnedTool(t, root, env, "golangci-lint", local)
	if err != nil {
		return linters{}, err
	}
	staticcheck, err := pinnedTool(t, root, env, "staticcheck", local)
	if err != nil {
		return linters{}, err
	}
	return linters{golangci: golangci, staticcheck: staticcheck}, nil
}

// localGo is the local Go toolchain as the tool resolver sees it: lang is its
// language version ("go1.26"), which a tool's build must reach, and key the
// tool-home directory it installs into ("go1.26", or "go1.26rc1" for a
// release candidate).
type localGo struct {
	lang string
	key  string
}

// goMajorMinor replays goMajorMinor (go/extension/internal/toolchain/resolve.go),
// which names the tool-home directory: the first two dot-separated fields of
// the version, without its "go" prefix.
func goMajorMinor(goVersion string) string {
	parts := strings.Split(strings.TrimPrefix(goVersion, "go"), ".")
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "." + parts[1]
}

// pinnedTool returns the binary of tool that `putnami lint` runs: the version
// toolVersionsFile pins, built with the local Go minor or a newer one. It
// replays toolchain.ResolvePinnedToolBinary
// (go/extension/internal/toolchain/resolve.go), which looks on PATH, in the
// machine tool home, then in the workspace's legacy tool directories.
// `putnami install` puts the pinned tools in the tool home, and CI runs it
// before its gate. When none of them holds the tool, the error names it: a
// lint the proof cannot run is not a lint that passed.
func pinnedTool(t *testing.T, root string, env []string, tool string, local localGo) (string, error) {
	t.Helper()
	pinned := pinnedVersion(t, root, tool)
	var rejected []string
	for _, candidate := range toolCandidates(root, tool, pinned, local) {
		if reason := toolMismatch(candidate, tool, pinned, local, env); reason != "" {
			rejected = append(rejected, reason)
			continue
		}
		return candidate, nil
	}
	detail := "no candidate exists"
	if len(rejected) > 0 {
		detail = strings.Join(rejected, "; ")
	}
	return "", fmt.Errorf("%s %s, built with %s or newer, is not installed (%s): run `putnami install`, which installs the linters `putnami lint` runs",
		tool, pinned, local.lang, detail)
}

// checkLintControl runs golangci-lint on planted findings in packages named
// build and bin, in a module under directories named like each exclusions.paths
// entry. It lints from the module directory, then through a symlink under a
// vendor directory: golangci-lint resolves the symlink in its working directory
// but not in the file paths, so each reported path climbs out of the module and
// crosses that vendor directory. The configuration matches its patterns against
// paths below the working directory only. It returns an error unless each run
// reports every finding. It runs once for every lint step, so it reports its
// verdict instead of failing the test that runs it first.
func checkLintControl(t *testing.T, env []string, golangci, config string) error {
	t.Helper()
	tmp, err := os.MkdirTemp("", "lint-control-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	workspace, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return err
	}
	project := filepath.Join(workspace, ".putnami", "vendor", "node_modules", "dist", "build", "bin", "test-project")
	planted := []string{"internal/build/control.go", "bin/control.go"}
	files := map[string]string{"go.mod": "module control\n\ngo 1.25\n"}
	for _, name := range planted {
		pkg := filepath.Base(filepath.Dir(name))
		files[name] = "// Package " + pkg + " leaves an error unchecked.\npackage " + pkg + "\n\nimport \"os\"\n\n// Control ignores the error of os.Chdir.\nfunc Control() {\n\tos.Chdir(\"/\")\n}\n"
	}
	for name, content := range files {
		path := filepath.Join(project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	dirs := []string{project}
	link := filepath.Join(workspace, "vendor", "linked-project")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	switch err := os.Symlink(project, link); {
	case err == nil:
		dirs = append(dirs, link)
	case runtime.GOOS == "windows":
		// Creating a symlink needs a privilege Windows does not grant by default.
		t.Logf("lint control skips the symlinked run: %v", err)
	default:
		return err
	}
	for _, dir := range dirs {
		cmd := exec.Command(golangci, "run", "--allow-parallel-runners", "--config", config, "./...")
		cmd.Dir = dir
		cmd.Env = append(slices.Clone(env), "GOWORK=off", "PWD="+dir)
		out, err := cmd.CombinedOutput()
		for _, name := range planted {
			if err == nil || !strings.Contains(string(out), filepath.FromSlash(name)) || !strings.Contains(string(out), "errcheck") {
				return fmt.Errorf("golangci-lint run from %s did not report the unchecked error planted in %s (%w), so it would not report a template's findings either. Either a directory of that path matches an exclusions.paths entry of %s, errcheck is no longer enabled there, or the control module did not load:\n%s",
					dir, name, err, config, out)
			}
		}
	}
	return nil
}

// pinnedVersion reads the version toolVersionsFile pins for tool.
func pinnedVersion(t *testing.T, root, tool string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(toolVersionsFile)))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tools map[string]struct {
			Version string `json:"version"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("%s: %v", toolVersionsFile, err)
	}
	pinned := manifest.Tools[tool].Version
	if pinned == "" {
		t.Fatalf("%s pins no version of %s", toolVersionsFile, tool)
	}
	return pinned
}

// toolCandidates lists, in the resolver's order, the executable files that
// could be tool: PATH, the machine tool home
// (<PUTNAMI_HOME>/tools/go/<tool>/<version>/go<major.minor>/<os>-<arch>/<tool>,
// toolchain.ToolHome), then the workspace's legacy tool directories
// (toolchain.LegacyManagedToolPaths).
func toolCandidates(root, tool, pinned string, local localGo) []string {
	name := tool
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var paths []string
	if path, err := exec.LookPath(tool); err == nil {
		paths = append(paths, path)
	}
	if home := toolHomeRoot(); home != "" {
		paths = append(paths, filepath.Join(home, tool, pinned, local.key, runtime.GOOS+"-"+runtime.GOARCH, name))
	}
	paths = append(paths,
		filepath.Join(root, ".putnami", "bin", "extensions", "putnami-go", "bin", "tools", name),
		filepath.Join(root, ".putnami", "extensions", "@putnami-go", "bin", "tools", name),
	)
	var candidates []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || slices.Contains(candidates, path) {
			continue
		}
		if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			continue
		}
		candidates = append(candidates, path)
	}
	return candidates
}

// toolHomeRoot replays toolchain.ResolveGoToolHomeRoot
// (go/extension/internal/toolchain/toolhome.go): the relocated Putnami home,
// the ~/.putnami default, then the extension cache root, or "" when none
// resolves.
func toolHomeRoot() string {
	if home := strings.TrimSpace(os.Getenv("PUTNAMI_HOME")); home != "" {
		return filepath.Join(home, "tools", "go")
	}
	if user, err := os.UserHomeDir(); err == nil && user != "" {
		return filepath.Join(user, ".putnami", "tools", "go")
	}
	if cache := strings.TrimSpace(os.Getenv("PUTNAMI_EXTENSION_CACHE_ROOT")); cache != "" {
		return filepath.Join(cache, "go", "tools")
	}
	return ""
}

// toolMismatch returns why binary cannot serve as tool, or "" when it can: it
// must report the pinned version, and the Go that built it must be the local
// minor or a newer one, since an older one cannot type-check the workspace
// (toolchain.ToolServesLocalGo).
func toolMismatch(binary, tool, pinned string, local localGo, env []string) string {
	args := []string{"-version"}
	if tool == "golangci-lint" {
		args = []string{"version", "--short"}
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	reported := strings.TrimSpace(string(out))
	if err != nil {
		return binary + " could not report its version (" + err.Error() + ")"
	}
	pattern := regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(strings.TrimPrefix(pinned, "v")) + `([^0-9]|$)`)
	if !pattern.MatchString(reported) {
		return binary + " reports " + strconv.Quote(reported)
	}
	if info, err := buildinfo.ReadFile(binary); err == nil && info.GoVersion != "" {
		built := version.Lang(strings.Fields(info.GoVersion)[0])
		if built == "" || version.Compare(built, local.lang) < 0 {
			return binary + " was built with " + info.GoVersion
		}
	}
	return ""
}
