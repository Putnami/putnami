package proof

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
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
	goFrameworkVersion    string
}

// golangciConfig is the golangci-lint configuration the Go extension ships,
// relative to the workspace root. A rendered workspace holds no
// .golangci.yml, so `putnami lint` runs with this one
// (toolchain.ResolveGolangciConfig in go/extension/internal/toolchain/resolve.go).
const golangciConfig = "go/extension/config/.golangci.yml"

// toolVersionsFile pins the linters `putnami lint` runs, relative to the
// workspace root. The go.putnami.dev/go/extension/tools package embeds it.
const toolVersionsFile = "go/extension/tools/versions.json"

// TestGoTemplatesBuildTestAndLint renders each Go template beside this
// project into a throwaway workspace whose go.work is this repository's, and
// runs `go vet`, `go build`, `go test` and the linters `putnami lint` runs
// there with the network off. A template that no longer compiles against the
// framework, whose own tests fail, or that the linters reject, fails here. The
// third-party modules the rendered project compiles come from the module
// cache, which a fresh machine does not have: a first step downloads exactly
// those, checked against go.sum, before the network goes.
func TestGoTemplatesBuildTestAndLint(t *testing.T) {
	proofDir := workingDirectory(t)
	root := workspaceRoot(t, proofDir)
	templates := discoverTemplates(t, filepath.Dir(proofDir))
	patterns := declaredPatterns(t, proofDir)
	fetchEnv := fetchGoEnv(t)
	env := offlineGoEnv(t)
	config := filepath.Join(root, filepath.FromSlash(golangciConfig))
	// A missing linter fails each template's lint step, after its vet, build
	// and test results are known.
	linters, lintErr := resolveLinters(t, root, env)
	if lintErr == nil {
		checkLintControl(t, env, linters.golangci, config)
	}

	for _, tpl := range templates {
		t.Run(tpl.name, func(t *testing.T) {
			// The real path: go matches the working directory it reads against
			// the go.work entries, and a temporary directory can sit behind a
			// symbolic link (/var on macOS).
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			vars := testVars(tpl)
			project := filepath.Join(workspace, vars.projectPath)
			renderTemplate(t, tpl.dir, project, vars)
			workFile := writeWorkFile(t, root, workspace, project)
			projectEnv := append(slices.Clone(env), "GOWORK="+workFile)

			runGo(t, project, append(slices.Clone(fetchEnv), "GOWORK="+workFile), "list", "-deps", "-test", "./...")
			runGo(t, project, projectEnv, "vet", "./...")
			runGo(t, project, projectEnv, "build", "./...")
			runGo(t, project, projectEnv, "test", "-count=1", "./...")

			// Every workspace module the rendered project compiles is a declared
			// input of this project, whole, so a change to it reruns this test.
			for _, dir := range strings.Fields(runGo(t, project, projectEnv,
				"list", "-deps", "-test", "-f", "{{with .Module}}{{.Dir}}{{end}}", "./...")) {
				rel, err := filepath.Rel(root, dir)
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

			// The read-only pass of the lint job
			// (go/extension/internal/jobs/lint/lint.go): golangci-lint with the
			// extension's configuration, then staticcheck.
			if lintErr != nil {
				t.Fatal(lintErr)
			}
			runCommand(t, project, projectEnv, linters.golangci,
				"run", "--allow-parallel-runners", "--config", config, "./...")
			runCommand(t, project, projectEnv, linters.staticcheck, "./...")
		})
	}
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

// testVars returns the variables `putnami dev template test` renders with:
// DefaultTestVars, with the manifest's testVariables overriding projectName and
// projectModule only (tooling/cli/internal/commands/extensions/templates.go).
func testVars(tpl committedTemplate) renderVars {
	vars := renderVars{
		projectName:           "test-project",
		projectPath:           "test-project",
		projectModule:         "test_project",
		putnamiVersion:        "latest",
		workspaceRelativePath: "..",
		goFrameworkVersion:    "v0.0.0",
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
// byte order, the root manifest is skipped, `__module__` in a path becomes
// projectModule, and a `.template` file gets `<%= name %>` substitution and
// loses its suffix. Unlike the CLI, a placeholder left after rendering fails
// here: it would reach the user as literal text.
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
		if rel == "putnami.template.json" {
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
		"goFrameworkVersion":    vars.goFrameworkVersion,
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
// paths below the working directory only. The proof fails unless each run
// reports every finding.
func checkLintControl(t *testing.T, env []string, golangci, config string) {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{project}
	link := filepath.Join(workspace, "vendor", "linked-project")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	switch err := os.Symlink(project, link); {
	case err == nil:
		dirs = append(dirs, link)
	case runtime.GOOS == "windows":
		// Creating a symlink needs a privilege Windows does not grant by default.
		t.Logf("lint control skips the symlinked run: %v", err)
	default:
		t.Fatal(err)
	}
	for _, dir := range dirs {
		cmd := exec.Command(golangci, "run", "--allow-parallel-runners", "--config", config, "./...")
		cmd.Dir = dir
		cmd.Env = append(slices.Clone(env), "GOWORK=off", "PWD="+dir)
		out, err := cmd.CombinedOutput()
		for _, name := range planted {
			if err == nil || !strings.Contains(string(out), filepath.FromSlash(name)) || !strings.Contains(string(out), "errcheck") {
				t.Fatalf("golangci-lint run from %s did not report the unchecked error planted in %s (%v), so it would not report a template's findings either. Either a directory of that path matches an exclusions.paths entry of %s, errcheck is no longer enabled there, or the control module did not load:\n%s",
					dir, name, err, config, out)
			}
		}
	}
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
