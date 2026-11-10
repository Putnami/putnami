package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const initLanguageFeature = "cli/init-a-language"

// initLanguagesIndexComment is the $comment of the generated index.
const initLanguagesIndexComment = "Generated from the workspace adapter of every extension the root putnami.workspace.json declares by path whose manifest declares workspace.language. Do not edit: PUTNAMI_UPDATE_INIT_LANGUAGES=1 ./putnamiw test --projects @putnami/cli --run InitLanguages --no-cache --no-enforce-coverage rewrites it."

// initRepositoryRoot is the root of this repository: the directory above
// this file that holds putnami.workspace.json.
func initRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("putnami.workspace.json not found above " + file)
		}
		dir = parent
	}
}

// repositoryExtensionDirs maps the name of every extension the root declares
// by path to its directory.
func repositoryExtensionDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	cfg := wsproto.Load(root)
	dirs := map[string]string{}
	for _, entry := range cfg.Extensions.Names() {
		if !strings.HasPrefix(entry, "/") {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(entry, "/")))
		data, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		dirs[manifest.Name] = dir
	}
	return dirs
}

// TestInitLanguagesFollowTheExtensionManifests is the drift gate of the
// embedded language index (ADR 0086): it rebuilds the index from the manifest
// of every extension the root declares by path and fails when the committed
// file differs. PUTNAMI_UPDATE_INIT_LANGUAGES=1 rewrites the file.
func TestInitLanguagesFollowTheExtensionManifests(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "languages-come-from-the-extensions", "the-language-index-follows-the-extension-manifests")
	root := initRepositoryRoot(t)
	index := initLanguageIndex{Comment: initLanguagesIndexComment}
	for name, dir := range repositoryExtensionDirs(t, root) {
		data, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Workspace *struct {
				Language string   `json:"language"`
				Markers  []string `json:"markers"`
				Excludes []string `json:"excludes"`
			} `json:"workspace"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if manifest.Workspace == nil || manifest.Workspace.Language == "" {
			continue
		}
		index.Languages = append(index.Languages, initLanguage{
			Language:  manifest.Workspace.Language,
			Extension: name,
			Markers:   manifest.Workspace.Markers,
			Excludes:  manifest.Workspace.Excludes,
		})
	}
	sort.Slice(index.Languages, func(i, j int) bool { return index.Languages[i].Language < index.Languages[j].Language })
	if len(index.Languages) < 10 {
		t.Fatalf("the root declares %d language extensions, want at least the ten Putnami runs", len(index.Languages))
	}
	want, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("PUTNAMI_UPDATE_INIT_LANGUAGES") == "1" {
		if err := os.WriteFile(filepath.Join(filepath.Dir(initThisFile(t)), "init_languages.json"), want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(initLanguagesJSON, want) {
		t.Errorf("init_languages.json drifted from the extension manifests. Regenerate it with:\n"+
			"  PUTNAMI_UPDATE_INIT_LANGUAGES=1 ./putnamiw test --projects @putnami/cli --run InitLanguages --no-cache --no-enforce-coverage\nwant:\n%s", want)
	}
}

func initThisFile(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return file
}

// Each starter names a language of the index, the language's extension, and
// a template that sits next to that extension and names it.
func TestInitStartersNameTheirExtension(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "languages-come-from-the-extensions", "each-starter-template-names-its-extension")
	root := initRepositoryRoot(t)
	dirs := repositoryExtensionDirs(t, root)
	if len(initStarters) == 0 {
		t.Fatal("no starter, so this check proves nothing")
	}
	for name, starter := range initStarters {
		language, ok := findInitLanguage(name)
		if !ok || language.Language != name {
			t.Errorf("starter %q names no language of the index", name)
			continue
		}
		if starter.packageName != language.Extension || starter.template == "" {
			t.Errorf("starter %q = %+v, want the extension %s and a template", name, starter, language.Extension)
			continue
		}
		dir, ok := dirs[starter.packageName]
		if !ok {
			t.Errorf("starter %q: the root declares no %s by path", name, starter.packageName)
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "templates", starter.template, "putnami.template.json"))
		if err != nil {
			t.Errorf("starter %q: %v", name, err)
			continue
		}
		var template struct {
			Name      string `json:"name"`
			Extension string `json:"extension"`
		}
		if err := json.Unmarshal(data, &template); err != nil {
			t.Fatal(err)
		}
		if template.Name != starter.template || template.Extension != starter.packageName {
			t.Errorf("starter %q: template manifest names %q for %q, want %q for %q", name, template.Name, template.Extension, starter.template, starter.packageName)
		}
	}
}

// writeMarkers writes an empty file at each workspace-relative path under dir.
func writeMarkers(t *testing.T, dir string, paths ...string) {
	t.Helper()
	for _, rel := range paths {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// assertNothingWritten fails when init wrote any file of a new workspace.
func assertNothingWritten(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{wsproto.WorkspaceConfigFilename, "README.md", ".gitignore", ".gitattributes", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("init wrote %s (stat error: %v), want nothing written", name, err)
		}
	}
}

// sameNames reports whether two name lists hold the same names, in any order.
func sameNames(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", append([]string{"--workspace", "shop"}, args...), LifecycleEnv{})
	})
}

func TestInitExtensionTakesEveryLanguageAndAList(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "every-language", "extension-takes-every-language-and-a-list")
	for _, language := range initLanguages {
		t.Run(language.Language, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := stubInitFrom(t, false, "")
			out, err := runInit(t, "--extension", language.Language)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			cfg := readInitConfig(t, dir)
			if !slices.Equal(cfg.Extensions, []string{language.Extension}) || !slices.Equal(calls.installed, []string{language.Extension}) {
				t.Errorf("declared %v, installed %v, want %s", cfg.Extensions, calls.installed, language.Extension)
			}
		})
	}
	for value, want := range map[string][]string{
		"rust,java,rust":   {"@putnami/rust", "@putnami/java"},
		"ts, py":           {"@putnami/typescript", "@putnami/python"},
		"typescript,ts,go": {"@putnami/typescript", "@putnami/go"},
		"swift,dart,dotnet,ruby,php": {
			"@putnami/swift", "@putnami/dart", "@putnami/dotnet", "@putnami/ruby", "@putnami/php",
		},
	} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := stubInitFrom(t, false, "")
			out, err := runInit(t, "--extension", value)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if cfg := readInitConfig(t, dir); !slices.Equal(cfg.Extensions, want) || !slices.Equal(calls.installed, want) {
				t.Errorf("declared %v, installed %v, want %v", cfg.Extensions, calls.installed, want)
			}
			if !strings.Contains(out, "Extension: "+strings.Join(want, ", ")) {
				t.Errorf("the summary names the extensions:\n%s", out)
			}
		})
	}
}

func TestInitRefusesAnUnknownLanguageBeforeWritingAnything(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "every-language", "an-unknown-language-is-refused-before-anything-is-written")
	for value, want := range map[string]string{
		"kotlin":  `unknown language "kotlin" for --extension (choose: dart, dotnet, go, java, php, python, ruby, rust, swift, typescript; ts and py name typescript and python)`,
		"rust,":   `--extension "rust," names an empty language`,
		",go":     `--extension ",go" names an empty language`,
		"":        "--extension needs a language (choose: dart, dotnet",
		"Rust":    `unknown language "Rust"`,
		"go,java": "",
	} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			stubInitFrom(t, false, "")
			_, err := runInit(t, "--extension", value)
			if want == "" {
				if err != nil {
					t.Fatalf("WorkspaceInit: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want a usage error with %q", err, want)
			}
			assertNothingWritten(t, dir)
		})
	}
}

func TestInitAdoptsTheLanguagesTheRepositoryHolds(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-adopts-what-it-finds", "a-language-whose-markers-the-directory-holds-is-synchronized")
	dir := t.TempDir()
	t.Chdir(dir)
	writeMarkers(t, dir, "Cargo.toml", "crates/api/Cargo.toml", "services/billing/pom.xml")
	calls := stubInitFrom(t, false, "")
	out, err := runInit(t, "--extension", "rust,java,go")
	if err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, out)
	}
	want := []string{"@putnami/rust", "@putnami/java", "@putnami/go"}
	if cfg := readInitConfig(t, dir); !slices.Equal(cfg.Extensions, want) || !slices.Equal(calls.installed, want) {
		t.Errorf("declared %v, installed %v, want %v", cfg.Extensions, calls.installed, want)
	}
	if calls.synced == nil || !sameNames(calls.synced.Extensions.Names(), want) {
		t.Fatalf("projects sync ran on %+v, want the workspace that declares %v", calls.synced, want)
	}
	for _, line := range []string{"Synchronizing the rust and java projects", "putnami projects list", "putnami lint,test,build --impacted"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
}

func TestInitInstallsALanguageWithoutMarkersAndSynchronizesNothing(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-adopts-what-it-finds", "a-language-without-markers-is-installed-and-not-synchronized")
	for value, next := range map[string]string{
		"rust":  "putnami projects sync    # once the repository holds a Cargo.toml",
		"go":    "putnami projects create my-app --template go-server",
		"swift": "putnami projects sync    # once the repository holds a Package.swift",
	} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeMarkers(t, dir, "docs/index.md")
			calls := stubInitFrom(t, false, "")
			out, err := runInit(t, "--extension", value)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if calls.synced != nil || len(calls.installed) != 1 {
				t.Errorf("installed %v, synced %v: want one install and no sync", calls.installed, calls.synced != nil)
			}
			if !strings.Contains(out, next) {
				t.Errorf("the summary lacks %q:\n%s", next, out)
			}
		})
	}
}

// A sync failure stops init before its ready summary and names the command
// that retries it.
func TestInitAdoptionSyncFailureNamesProjectsSync(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-adopts-what-it-finds", "a-language-whose-markers-the-directory-holds-is-synchronized")
	dir := t.TempDir()
	t.Chdir(dir)
	writeMarkers(t, dir, "composer.json")
	failure := stubWorkspaceInitStages(t, "sync", 0)
	out, err := runInit(t, "--extension", "php")
	if !errors.Is(err, failure) || protocolcli.SuggestedNext(err) != "putnami projects sync" {
		t.Fatalf("WorkspaceInit = %v (next %q), want the sync failure and putnami projects sync", err, protocolcli.SuggestedNext(err))
	}
	if strings.Contains(out, "Workspace ready") {
		t.Errorf("a failed sync printed the ready summary:\n%s", out)
	}
}

func TestInitProjectTakesTheStarterOfTheFirstLanguage(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "project-needs-a-starter", "a-project-takes-the-starter-of-the-first-language")
	dir := t.TempDir()
	t.Chdir(dir)
	calls := stubInitFrom(t, false, "")
	var templates, created []string
	installInitTemplate = func(_ context.Context, _ string, _ *wsproto.Config, args []string, _ string) error {
		templates = append(templates, args...)
		return nil
	}
	createInitProject = func(_ context.Context, _ string, _ *wsproto.Config, args []string, _ LifecycleEnv) error {
		created = args
		return nil
	}
	out, err := runInit(t, "--extension", "go,rust,ts", "--project", "api")
	if err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, out)
	}
	cfg := readInitConfig(t, dir)
	if !slices.Equal(cfg.Templates, []string{"go-server"}) || !slices.Equal(templates, []string{"go-server"}) || !slices.Equal(created, []string{"api", "--template", "go-server"}) {
		t.Errorf("templates %v, installed %v, created %v: want the go-server starter", cfg.Templates, templates, created)
	}
	if want := []string{"@putnami/go", "@putnami/rust", "@putnami/typescript"}; !slices.Equal(calls.installed, want) {
		t.Errorf("installed %v, want %v", calls.installed, want)
	}
	if got := registriesOf(t, dir)["npm"]; got != `{"scopes":{"@putnami":"https://npm.putnami.dev"}}` {
		t.Errorf("registries.npm = %s: a declared language that has a starter declares its registries", got)
	}
	if calls.synced != nil {
		t.Errorf("the starter's own files made init synchronize; it adopts only what it found")
	}
}

func TestInitRefusesAProjectForALanguageWithoutAStarter(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "project-needs-a-starter", "a-language-without-a-starter-refuses-a-project")
	for _, value := range []string{"rust", "java,go", "dotnet"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			stubInitFrom(t, false, "")
			_, err := runInit(t, "--extension", value, "--project", "app")
			first := strings.Split(value, ",")[0]
			want := "and " + first + " ships none (languages with a starter: go, python, typescript)"
			if err == nil || !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want a usage error with %q", err, want)
			}
			assertNothingWritten(t, dir)
		})
	}
}

func TestInitAsksWhichDetectedLanguagesToDeclare(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-proposes-what-it-detects", "a-terminal-asks-with-every-detected-language-preselected")
	for answer, want := range map[string][]string{
		"\n":                {"@putnami/rust", "@putnami/typescript"},
		"":                  {"@putnami/rust", "@putnami/typescript"},
		"2\n":               {"@putnami/typescript"},
		"rust\n":            {"@putnami/rust"},
		"9\n1, 2\n":         {"@putnami/rust", "@putnami/typescript"},
		"1 go\n":            {"@putnami/rust", "@putnami/go"},
		"kotlin\nts,rust\n": {"@putnami/typescript", "@putnami/rust"},
	} {
		t.Run(strings.ReplaceAll(answer, "\n", "⏎"), func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeMarkers(t, dir, "web/package.json", "Cargo.toml")
			calls := stubInitFrom(t, true, answer)
			out, err := runInit(t)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if !strings.Contains(out, "1) rust") || !strings.Contains(out, "at the root") ||
				!strings.Contains(out, "2) typescript") || !strings.Contains(out, "in web") || !strings.Contains(out, "Languages [1,2]: ") {
				t.Errorf("the question lists each detected language, all preselected:\n%s", out)
			}
			if cfg := readInitConfig(t, dir); !slices.Equal(cfg.Extensions, want) {
				t.Errorf("declared %v, want %v", cfg.Extensions, want)
			}
			if calls.synced == nil {
				t.Errorf("init did not synchronize the languages it found")
			}
		})
	}
}

func TestInitAsksAtMostThreeTimes(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-proposes-what-it-detects", "a-terminal-asks-with-every-detected-language-preselected")
	detected := []initDetection{{language: mustInitLanguage(t, "rust"), dir: "."}}
	var out bytes.Buffer
	_, err := askInitLanguages(detected, strings.NewReader("x\ny\nz\n"), &out)
	if err == nil || !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), "rerun with --extension rust") {
		t.Fatalf("err = %v, want a usage error naming --extension rust", err)
	}
	if _, err := askInitLanguages(detected, strings.NewReader("x"), &out); err == nil {
		t.Fatal("an unreadable last answer chose a language")
	}
}

func mustInitLanguage(t *testing.T, name string) initLanguage {
	t.Helper()
	language, ok := findInitLanguage(name)
	if !ok {
		t.Fatalf("no language %q in the index", name)
	}
	return language
}

func TestInitWithoutATerminalDeclaresNothingAndNamesTheCommand(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-proposes-what-it-detects", "without-a-terminal-init-writes-nothing-and-names-the-command")
	dir := t.TempDir()
	t.Chdir(dir)
	writeMarkers(t, dir, "src/App/App.csproj", "pyproject.toml")
	calls := stubInitFrom(t, false, "")
	_, err := runInit(t)
	if err == nil || !errors.Is(err, protocolcli.ErrUsage) ||
		!strings.Contains(err.Error(), "holds the markers of dotnet in src/App, python at the root: without a terminal init cannot ask") {
		t.Fatalf("err = %v, want a usage error that names the detected languages", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami init --extension dotnet,python" {
		t.Errorf("next = %q, want putnami init --extension dotnet,python", next)
	}
	if len(calls.installed) != 0 {
		t.Errorf("installed %v, want nothing", calls.installed)
	}
	assertNothingWritten(t, dir)
}

func TestInitWithoutAMarkerDeclaresTypeScript(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "init-proposes-what-it-detects", "a-directory-without-a-marker-declares-typescript")
	for name, c := range map[string]struct {
		markers []string
		args    []string
	}{
		"no marker":                            {markers: []string{"README.md", "docs/guide.md"}},
		"a project without --extension":        {markers: []string{"Cargo.toml"}, args: []string{"--project", "web"}},
		"a marker under an excluded directory": {markers: []string{"node_modules/dep/Cargo.toml", ".putnami/cache/go.mod"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			writeMarkers(t, dir, c.markers...)
			calls := stubInitFrom(t, true, "")
			out, err := runInit(t, c.args...)
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if strings.Contains(out, "Languages [") {
				t.Errorf("init asked without a reason:\n%s", out)
			}
			if cfg := readInitConfig(t, dir); !slices.Equal(cfg.Extensions, []string{"@putnami/typescript"}) || calls.synced != nil {
				t.Errorf("declared %v, synced %v, want typescript alone and no sync", cfg.Extensions, calls.synced != nil)
			}
		})
	}
}

func TestInitPointsAMonorepoToolAtFrom(t *testing.T) {
	spectest.Proves(t, initLanguageFeature, "monorepo-tool-hint", "a-monorepo-tool-marker-names-init-from-and-writes-nothing")
	for marker, tool := range map[string]string{
		"nx.json":             "nx",
		"turbo.json":          "turbo",
		"rush.json":           "rush",
		".moon/workspace.yml": "moon",
		"pants.toml":          "pants",
		"MODULE.bazel":        "bazel",
	} {
		t.Run(tool, func(t *testing.T) {
			for _, terminal := range []bool{true, false} {
				dir := t.TempDir()
				t.Chdir(dir)
				writeMarkers(t, dir, marker, "package.json", "go.mod")
				calls := stubInitFrom(t, terminal, "\n")
				_, err := runInit(t)
				if err == nil || !errors.Is(err, protocolcli.ErrUsage) || !strings.Contains(err.Error(), marker+" ("+tool+")") {
					t.Fatalf("terminal %v: err = %v, want a usage error naming %s", terminal, err, marker)
				}
				if next := protocolcli.SuggestedNext(err); next != "putnami init --from "+tool {
					t.Errorf("next = %q, want putnami init --from %s", next, tool)
				}
				if len(calls.installed) != 0 {
					t.Errorf("installed %v, want nothing", calls.installed)
				}
				assertNothingWritten(t, dir)
			}
		})
	}

	// An explicit --extension is the user's choice: the marker does not stop it.
	dir := t.TempDir()
	t.Chdir(dir)
	writeMarkers(t, dir, "nx.json", "package.json")
	calls := stubInitFrom(t, false, "")
	if out, err := runInit(t, "--extension", "ts"); err != nil || !slices.Equal(calls.installed, []string{"@putnami/typescript"}) {
		t.Fatalf("WorkspaceInit = %v, installed %v\n%s", err, calls.installed, out)
	}
}
