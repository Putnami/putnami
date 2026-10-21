package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// ---- expandTemplates ----

func TestExpandTemplates_AllPlaceholders(t *testing.T) {
	s := "{workspaceRoot}/node_modules/{extensionRoot}/bin/{projectRoot}/out"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	want := "/ws/node_modules//ext/bin//proj/out"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_NoPlaceholders(t *testing.T) {
	s := "some/plain/path"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	if got != s {
		t.Errorf("expandTemplates() = %q, want %q", got, s)
	}
}

func TestExpandTemplates_MultipleSamePlaceholder(t *testing.T) {
	s := "{extensionRoot}/a/{extensionRoot}/b"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	want := "/ext/a//ext/b"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_EmptyString(t *testing.T) {
	got := expandTemplates("", "/ws", "/proj", "/ext")
	if got != "" {
		t.Errorf("expandTemplates() = %q, want empty", got)
	}
}

func TestExpandTemplates_OnlyExtensionRoot(t *testing.T) {
	got := expandTemplates("{extensionRoot}/bin/hook", "/ws", "/proj", "/my/ext")
	want := "/my/ext/bin/hook"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_OnlyProjectRoot(t *testing.T) {
	got := expandTemplates("{projectRoot}/src", "/ws", "/my/proj", "/ext")
	want := "/my/proj/src"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_OnlyWorkspaceRoot(t *testing.T) {
	got := expandTemplates("{workspaceRoot}/.putnami", "/my/ws", "/proj", "/ext")
	want := "/my/ws/.putnami"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

// ---- parseSummary ----

func TestParseSummary_ValidSummary(t *testing.T) {
	output := `{"type":"log","data":{"message":"starting"}}
{"type":"summary","data":{"exports":{"./index":"./dist/index.js"},"assets":{"main.css":"./dist/main.css"}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}
	if exports["./index"] != "./dist/index.js" {
		t.Errorf("exports[./index] = %q, want %q", exports["./index"], "./dist/index.js")
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if assets["main.css"] != "./dist/main.css" {
		t.Errorf("assets[main.css] = %q, want %q", assets["main.css"], "./dist/main.css")
	}
}

func TestParseSummary_NoSummaryEvent(t *testing.T) {
	output := `{"type":"log","data":{"message":"hello"}}
{"type":"progress","data":{"current":1,"total":3}}
`
	_, _, _, err := parseSummary(output)
	if err == nil {
		t.Fatal("expected error when no summary event is present")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestParseSummary_EmptyOutput(t *testing.T) {
	_, _, _, err := parseSummary("")
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestParseSummary_TruncatedSummaryLine(t *testing.T) {
	// A summary cut off mid-line (e.g. the emitting process exited before its
	// stdout pipe drained) must fail loudly, not silently drop the exports.
	full := `{"type":"summary","data":{"exports":{"static-loader":"/p/.gen/src/static/.static.gen.ts"},"assets":{"public/install.sh":"/p/.gen/public/install.sh"}}}`
	truncated := full[:len(full)-40]
	_, _, _, err := parseSummary(truncated + "\n")
	if err == nil {
		t.Fatal("expected error for truncated summary line")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got %q", err.Error())
	}
}

func TestParseSummary_SummaryWithEmptyMaps(t *testing.T) {
	output := `{"type":"summary","data":{"exports":{},"assets":{}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exports) != 0 {
		t.Errorf("expected empty exports, got %d entries", len(exports))
	}
	if len(assets) != 0 {
		t.Errorf("expected empty assets, got %d entries", len(assets))
	}
}

func TestParseSummary_SummaryWithNullFields(t *testing.T) {
	output := `{"type":"summary","data":{"exports":null,"assets":null}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// parseSummary replaces nil maps with empty maps
	if exports == nil {
		t.Error("expected non-nil exports map")
	}
	if assets == nil {
		t.Error("expected non-nil assets map")
	}
}

func TestParseSummary_MultipleEventsFirstSummaryWins(t *testing.T) {
	output := `{"type":"log","data":{}}
{"type":"summary","data":{"exports":{"a":"1"},"assets":{"b":"2"}}}
{"type":"summary","data":{"exports":{"x":"9"},"assets":{"y":"8"}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["a"] != "1" {
		t.Errorf("expected first summary exports, got %v", exports)
	}
	if assets["b"] != "2" {
		t.Errorf("expected first summary assets, got %v", assets)
	}
}

func TestParseSummary_InvalidJSONLines(t *testing.T) {
	output := `not json at all
{"type":"summary","data":{"exports":{"k":"v"},"assets":{}}}
also not json
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["k"] != "v" {
		t.Errorf("expected exports[k]=v, got %v", exports)
	}
	if len(assets) != 0 {
		t.Errorf("expected empty assets, got %d entries", len(assets))
	}
}

func TestParseSummary_BlankLines(t *testing.T) {
	output := "\n\n{\"type\":\"summary\",\"data\":{\"exports\":{\"a\":\"b\"},\"assets\":{}}}\n\n"
	exports, _, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["a"] != "b" {
		t.Errorf("expected exports[a]=b, got %v", exports)
	}
}

func TestParseSummary_MultipleExportsAndAssets(t *testing.T) {
	output := `{"type":"summary","data":{"exports":{"./a":"./d/a.js","./b":"./d/b.js"},"assets":{"x.css":"./x.css","y.js":"./y.js"}}}`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exports) != 2 {
		t.Fatalf("expected 2 exports, got %d", len(exports))
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
}

func TestParseSummary_LargeStaticAssetSummary(t *testing.T) {
	data := hookSummaryData{
		Exports: map[string]string{
			"api-loader":    "/project/.gen/src/api/.api-application.gen.ts",
			"static-loader": "/project/.gen/src/static/.static.gen.ts",
		},
		Assets: map[string]string{
			"public/install.sh": "/project/.gen/public/install.sh",
		},
	}
	for i := range 3000 {
		key := fmt.Sprintf("public/static/docs/page-%04d.html.gz", i)
		data.Assets[key] = "/project/.gen/" + key
	}
	event := struct {
		Type string          `json:"type"`
		Data hookSummaryData `json:"data"`
	}{
		Type: "summary",
		Data: data,
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if len(line) <= 64*1024 {
		t.Fatalf("test summary should exceed bufio.Scanner default token size, got %d bytes", len(line))
	}

	exports, assets, _, err := parseSummary(string(line) + "\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["api-loader"] == "" {
		t.Fatalf("expected api-loader export, got %v", exports)
	}
	if exports["static-loader"] == "" {
		t.Fatalf("expected static-loader export, got %v", exports)
	}
	if assets["public/install.sh"] == "" {
		t.Fatalf("expected install script asset, got %v", assets)
	}
}

// ---- discoverHooks ----

// mustDiscoverHooks is discoverHooks for a fixture whose dependencies are all
// installed under dir, outside any workspace root, and whose manifests are
// all readable: discovery must report no gap.
func mustDiscoverHooks(t *testing.T, dir, kind string) []discoveredHook {
	t.Helper()
	hooks, gaps := discoverHooksWithGaps(t, dir, kind)
	if len(gaps) != 0 {
		t.Fatalf("discoverHooks reported gaps for a readable fixture: %q", gaps)
	}
	return hooks
}

// discoverHooksWithGaps is discoverHooks for a fixture installed under dir
// that may hold a manifest discovery cannot use.
func discoverHooksWithGaps(t *testing.T, dir, kind string) ([]discoveredHook, []string) {
	t.Helper()
	hooks, gaps, err := discoverHooks("", dir, kind)
	if err != nil {
		t.Fatalf("discoverHooks: %v", err)
	}
	return hooks, gaps
}

func TestDiscoverHooks_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	hooks, gaps := discoverHooksWithGaps(t, dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for missing package.json, got %d", len(hooks))
	}
	if want := []string{"package.json is missing"}; !slices.Equal(gaps, want) {
		t.Errorf("gaps = %q, want %q", gaps, want)
	}
}

// A package.json that exists but cannot be read or parsed is a gap, not a
// project without dependencies: discovery cannot tell which hooks it misses.
func TestDiscoverHooks_UnusablePackageJSONIsAGap(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, path string)
		want  string
	}{
		"malformed": {
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"name":`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "package.json is malformed: ",
		},
		"wrong field type": {
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"name":"test","dependencies":["@putnami/application"]}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "package.json is malformed: ",
		},
		"unreadable": {
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "package.json cannot be read: ",
		},
		// encoding/json decodes null into an empty value without an error, so
		// each of these would otherwise read as a project without dependencies.
		"JSON null": {
			setup: writeFixture(`null`),
			want:  "package.json is malformed: the document is null, not an object",
		},
		"null dependencies": {
			setup: writeFixture(`{"name":"test","dependencies":null}`),
			want:  "package.json is malformed: dependencies is null, not an object",
		},
		"null devDependencies": {
			setup: writeFixture(`{"name":"test","devDependencies":null}`),
			want:  "package.json is malformed: devDependencies is null, not an object",
		},
		"not an object": {
			setup: writeFixture(`[]`),
			want:  "package.json is malformed: ",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, filepath.Join(dir, "package.json"))

			hooks, gaps := discoverHooksWithGaps(t, dir, HookPreBuild)
			if len(hooks) != 0 {
				t.Errorf("expected 0 hooks, got %d", len(hooks))
			}
			if len(gaps) != 1 || !strings.HasPrefix(gaps[0], tc.want) {
				t.Fatalf("gaps = %q, want one entry starting %q", gaps, tc.want)
			}
			if strings.Contains(gaps[0], dir) {
				t.Errorf("gap %q names the absolute fixture path %q", gaps[0], dir)
			}
		})
	}
}

// writeFixture is a setup that writes body to the fixture path.
func writeFixture(body string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A dependency manifest that is JSON null, or that sets hooks or the
// requested hook to null, is malformed: each would otherwise read as a
// dependency without that hook. A manifest without a hooks member, and a
// dependency without a manifest, declare no hook and are no gap.
func TestDiscoverHooks_NullManifestMembersAreGaps(t *testing.T) {
	cases := map[string]struct {
		manifest string
		want     string
	}{
		"JSON null":              {`null`, "ext: putnami.extension.json is malformed: the document is null, not an object"},
		"null hooks":             {`{"hooks":null}`, "ext: putnami.extension.json is malformed: hooks is null, not an object"},
		"null preBuild hook":     {`{"hooks":{"preBuild":null}}`, "ext: putnami.extension.json is malformed: hooks.preBuild is null, not an object"},
		"no hooks member":        {`{"name":"ext"}`, ""},
		"hooks without preBuild": {`{"hooks":{}}`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			depDir := filepath.Join(dir, "node_modules", "ext")
			if err := os.MkdirAll(depDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFixture(tc.manifest)(t, filepath.Join(depDir, "putnami.extension.json"))
			if err := os.MkdirAll(filepath.Join(dir, "node_modules", "plain-lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFixture(`{"name":"test","dependencies":{"ext":"^1.0.0","plain-lib":"^1.0.0"}}`)(t, filepath.Join(dir, "package.json"))

			hooks, gaps := discoverHooksWithGaps(t, dir, HookPreBuild)
			if len(hooks) != 0 {
				t.Errorf("hooks = %v, want none", hookNames(hooks))
			}
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if !slices.Equal(gaps, want) {
				t.Errorf("gaps = %q, want %q", gaps, want)
			}
		})
	}
}

func TestDiscoverHooks_NoDependencies(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks with no deps, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithHook(t *testing.T) {
	dir := t.TempDir()

	// Create a dependency with a preBuild hook
	depDir := filepath.Join(dir, "node_modules", "@putnami", "web")
	os.MkdirAll(depDir, 0755)

	manifest := map[string]any{
		"hooks": map[string]any{
			"preBuild": map[string]any{
				"kind":    "command",
				"command": "bun",
				"args":    []string{"{extensionRoot}/bin/generate.ts"},
			},
		},
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), manifestData, 0644)

	pkgJSON := `{"name":"test","dependencies":{"@putnami/web":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	if hooks[0].name != "@putnami/web" {
		t.Errorf("expected hook from '@putnami/web', got %q", hooks[0].name)
	}
	if hooks[0].hook.Command != "bun" {
		t.Errorf("expected command 'bun', got %q", hooks[0].hook.Command)
	}
}

func TestDiscoverHooks_DevDependencyWithHook(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "my-dev-ext")
	os.MkdirAll(depDir, 0755)

	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"node","args":["generate.js"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkgJSON := `{"name":"test","devDependencies":{"my-dev-ext":"^2.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook from devDependencies, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithoutHook(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "some-lib")
	os.MkdirAll(depDir, 0755)

	// Manifest without preBuild hook
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(`{"hooks":{}}`), 0644)

	pkgJSON := `{"name":"test","dependencies":{"some-lib":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks when no preBuild hook, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithoutManifest(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "plain-lib")
	os.MkdirAll(depDir, 0755)
	// No putnami.extension.json

	pkgJSON := `{"name":"test","dependencies":{"plain-lib":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks when no manifest, got %d", len(hooks))
	}
}

func TestDiscoverHooks_InvalidManifestJSON(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "bad-manifest")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte("not json"), 0644)

	pkgJSON := `{"name":"test","dependencies":{"bad-manifest":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks, gaps := discoverHooksWithGaps(t, dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for invalid JSON manifest, got %d", len(hooks))
	}
	if len(gaps) != 1 || !strings.HasPrefix(gaps[0], "bad-manifest: putnami.extension.json is malformed: ") {
		t.Errorf("gaps = %q, want one malformed-manifest entry for bad-manifest", gaps)
	}
}

// A dependency manifest that cannot be used makes discovery incomplete, and
// the hooks of the other dependencies are still discovered.
func TestDiscoverHooks_UnreadableManifestIsAGapOthersStillDiscovered(t *testing.T) {
	dir := t.TempDir()

	goodDir := filepath.Join(dir, "node_modules", "good-ext")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
	if err := os.WriteFile(filepath.Join(goodDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where the manifest file belongs: it exists and cannot be read.
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "dir-manifest", "putnami.extension.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := `{"name":"test","dependencies":{"dir-manifest":"^1.0.0","good-ext":"^1.0.0"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks, gaps := discoverHooksWithGaps(t, dir, HookPreBuild)
	if got := hookNames(hooks); !slices.Equal(got, []string{"good-ext"}) {
		t.Errorf("hooks = %v, want [good-ext]", got)
	}
	if len(gaps) != 1 || !strings.HasPrefix(gaps[0], "dir-manifest: putnami.extension.json cannot be read: ") {
		t.Errorf("gaps = %q, want one unreadable-manifest entry for dir-manifest", gaps)
	}
	if len(gaps) == 1 && strings.Contains(gaps[0], dir) {
		t.Errorf("gap %q names the absolute fixture path %q", gaps[0], dir)
	}
}

func TestDiscoverHooks_MultipleDepsWithHooks(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		os.MkdirAll(depDir, 0755)
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["generate.ts"]}}}`
		os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	}

	pkgJSON := `{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(hooks) != 2 {
		t.Errorf("expected 2 hooks, got %d", len(hooks))
	}
}

// ---- discoverHooks: ordering contract ----

// writeHookDependency registers dep as a project dependency whose extension
// manifest declares a preBuild hook at the given rank (order 0 is written as an
// absent key, which is what a manifest without the key looks like).
func writeHookDependency(t *testing.T, projectDir, dep string, order int) {
	t.Helper()
	depDir := filepath.Join(projectDir, "node_modules", filepath.FromSlash(dep))
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	hook := map[string]any{"kind": "command", "command": "bun", "args": []string{"generate.ts"}}
	if order != 0 {
		hook["order"] = order
	}
	manifest, err := json.Marshal(map[string]any{"hooks": map[string]any{"preBuild": hook}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
}

func hookNames(hooks []discoveredHook) []string {
	names := make([]string, 0, len(hooks))
	for _, h := range hooks {
		names = append(names, h.name)
	}
	return names
}

// TestDiscoverHooks_OrderIsDeterministicAndRankedFirst pins the exact sequence
// RunHooks will invoke, not merely "it is sorted".
//
// Hooks share one .gen tree, so the sequence IS the build contract: iterating
// the dependency map made it a per-process coin flip and half the fresh-tree
// builds died in capabilityManifestForBundledServe. Go randomizes map
// order per range, so one sample can pass by luck — with 8 ranked dependencies
// the odds that a single random permutation matches are 1/8!, and the loop below
// takes enough samples that a map-order implementation cannot survive it.
//
// The fixture is also chosen so a plain name sort fails: the two extensions that
// must run LAST ("@acme/aardvark", "@putnami/application") are the two whose
// names sort FIRST.
func TestDiscoverHooks_OrderIsDeterministicAndRankedFirst(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "ranked-manifests-are-discovered-first")
	dir := t.TempDir()

	ranks := map[string]int{
		"@acme/aardvark":         100,
		"@putnami/application":   100,
		"@putnami/web":           0,
		"@putnami/ui":            0,
		"zzz-late-producer":      0,
		"aaa-early-producer":     -50,
		"@acme/very-early":       -100,
		"middle-of-the-alphabet": 0,
	}
	deps := make(map[string]string, len(ranks))
	for name, order := range ranks {
		writeHookDependency(t, dir, name, order)
		deps[name] = "^1.0.0"
	}
	pkgJSON, err := json.Marshal(map[string]any{"name": "test", "dependencies": deps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkgJSON, 0644); err != nil {
		t.Fatal(err)
	}

	want := []string{
		// order -100
		"@acme/very-early",
		// order -50
		"aaa-early-producer",
		// order 0, byte order
		"@putnami/ui",
		"@putnami/web",
		"middle-of-the-alphabet",
		"zzz-late-producer",
		// order 100, byte order
		"@acme/aardvark",
		"@putnami/application",
	}

	for attempt := range 64 {
		got := hookNames(mustDiscoverHooks(t, dir, HookPreBuild))
		if len(got) != len(want) {
			t.Fatalf("attempt %d: discovered %d hooks, want %d (%v)", attempt, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: hook order = %v, want %v", attempt, got, want)
			}
		}
	}
}

// TestDiscoverHooks_UnrankedManifestsKeepByteOrder covers every manifest written
// before the order key existed: they all rank 0, so the sequence is byte order
// and, most importantly, is the same on every run and every platform.
func TestDiscoverHooks_UnrankedManifestsKeepByteOrder(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "unranked-manifests-keep-a-stable-order")
	dir := t.TempDir()

	for _, name := range []string{"ext-c", "ext-a", "ext-b"} {
		writeHookDependency(t, dir, name, 0)
	}
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"test","dependencies":{"ext-c":"^1.0.0","ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644)

	want := []string{"ext-a", "ext-b", "ext-c"}
	for attempt := range 32 {
		got := hookNames(mustDiscoverHooks(t, dir, HookPreBuild))
		if len(got) != len(want) {
			t.Fatalf("attempt %d: discovered %v, want %v", attempt, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: hook order = %v, want %v", attempt, got, want)
			}
		}
	}
}

// TestRunHooks_InvokesInDiscoveredOrder proves the rank reaches invocation, not
// just discovery: RunHooks walks the slice, so a producer/finalizer inversion
// here is exactly the missing-capability-manifest failure.
func TestRunHooks_InvokesInDiscoveredOrder(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "invocation-follows-discovery-order")
	dir := t.TempDir()
	writeHookDependency(t, dir, "@putnami/application", 100)
	writeHookDependency(t, dir, "@putnami/web", 0)
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"test","dependencies":{"@putnami/application":"^1.0.0","@putnami/web":"^1.0.0"}}`), 0644)

	var invoked []string
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		contextPath := args[len(args)-1]
		data, err := os.ReadFile(contextPath) //nolint:gosec // test fixture path
		if err != nil {
			return nil, err
		}
		var ctx hookContext
		if err := json.Unmarshal(data, &ctx); err != nil {
			return nil, err
		}
		invoked = append(invoked, ctx.Extension)
		return &exec.Result{
			Success: true,
			Stdout:  `{"type":"summary","data":{"exports":{},"assets":{}}}`,
		}, nil
	})

	if _, err := RunHooks("", dir, "test", HookPreBuild, "build", "bun", false, nil); err != nil {
		t.Fatalf("RunHooks: %v", err)
	}
	want := []string{"@putnami/web", "@putnami/application"}
	if len(invoked) != len(want) || invoked[0] != want[0] || invoked[1] != want[1] {
		t.Fatalf("invocation order = %v, want %v (the finalizer must observe the producer's output)", invoked, want)
	}
}

// ---- discoverHooks: an uninstalled dependency ----

// failOnHookExec fails the test if any hook process starts.
func failOnHookExec(t *testing.T) {
	t.Helper()
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("a hook ran (%v) although a dependency of the project is not installed", args)
		return &exec.Result{Success: true, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}`}, nil
	})
}

// TestRunHooks_UninstalledDependencyFails pins that a project whose declared
// dependency is not installed fails hook discovery instead of running with
// fewer hooks. The hook set is not part of any cache key: a generate that ran
// without the @putnami/application hook succeeded in milliseconds with a .gen
// lacking clientgen/config.json and schema/, under the same key as a complete
// run, and the shared cache then served that tree to every checkout.
func TestRunHooks_UninstalledDependencyFails(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-discovery-fails-closed", "an-uninstalled-dependency-fails-discovery")
	root := t.TempDir()
	projectDir := filepath.Join(root, "apps", "api")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"),
		[]byte(`{"name":"api","dependencies":{"@putnami/application":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	failOnHookExec(t)

	result, err := RunHooks(root, projectDir, "api", HookPreBuild, "build", "bun", false, nil)
	if err == nil {
		t.Fatalf("RunHooks = %+v, nil; want an error naming the uninstalled dependency", result)
	}
	for _, want := range []string{"@putnami/application", "not installed", "putnami install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRunHooks_DanglingWorkspaceLinkFails covers a workspace: dependency whose
// node_modules link points nowhere: it is not installed either.
func TestRunHooks_DanglingWorkspaceLinkFails(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-discovery-fails-closed", "an-uninstalled-dependency-fails-discovery")
	root := t.TempDir()
	projectDir := filepath.Join(root, "apps", "api")
	linkDir := filepath.Join(projectDir, "node_modules", "@acme")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "libs", "missing"), filepath.Join(linkDir, "gen")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"),
		[]byte(`{"name":"api","dependencies":{"@acme/gen":"workspace:*"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	failOnHookExec(t)

	if _, err := RunHooks(root, projectDir, "api", HookPreBuild, "build", "bun", false, nil); err == nil ||
		!strings.Contains(err.Error(), "@acme/gen") {
		t.Fatalf("RunHooks error = %v, want one naming @acme/gen", err)
	}
}

// TestRunHooks_DependencyInstalledAtWorkspaceRootRuns pins the other half of
// the rule: a dependency counts as installed where Node resolution finds it,
// from the project's node_modules up to the workspace root's, so a hoisted
// install still runs its hooks and is not reported as missing.
func TestRunHooks_DependencyInstalledAtWorkspaceRootRuns(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-discovery-fails-closed", "a-hoisted-dependency-is-installed")
	root := t.TempDir()
	projectDir := filepath.Join(root, "apps", "api")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHookDependency(t, root, "@putnami/application", 100)
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"),
		[]byte(`{"name":"api","dependencies":{"@putnami/application":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var invoked []string
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		data, err := os.ReadFile(args[len(args)-1]) //nolint:gosec // test fixture path
		if err != nil {
			return nil, err
		}
		var ctx hookContext
		if err := json.Unmarshal(data, &ctx); err != nil {
			return nil, err
		}
		invoked = append(invoked, ctx.Extension)
		return &exec.Result{Success: true, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}`}, nil
	})

	if _, err := RunHooks(root, projectDir, "api", HookPreBuild, "build", "bun", false, nil); err != nil {
		t.Fatalf("RunHooks: %v", err)
	}
	if len(invoked) != 1 || invoked[0] != "@putnami/application" {
		t.Fatalf("invoked = %v, want the hoisted @putnami/application hook", invoked)
	}
}

// ---- RunHooksPreBuild ----

// ---- mockExecRun helper ----

func withMockExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = fn
}

// ---- invokeHook ----

func TestInvokeHook_Success(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	summaryJSON := `{"type":"summary","data":{"exports":{"./index":"./dist/index.js"},"assets":{"style.css":"./dist/style.css"}}}`

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summaryJSON + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"{extensionRoot}/bin/generate.ts"},
		},
	}

	exports, assets, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["./index"] != "./dist/index.js" {
		t.Errorf("expected export ./index=./dist/index.js, got %v", exports)
	}
	if assets["style.css"] != "./dist/style.css" {
		t.Errorf("expected asset style.css=./dist/style.css, got %v", assets)
	}
}

func TestInvokeHook_Failure(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "hook error"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Error("expected error on hook failure")
	}
	if !strings.Contains(err.Error(), "hook exited") {
		t.Errorf("expected 'hook exited' in error, got %q", err.Error())
	}
}

func TestInvokeHook_ExecError(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Error("expected error on exec failure")
	}
}

func TestInvokeHook_BunCommand(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledCommand string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledCommand = name
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "/usr/bin/bun", h, false, nil)
	if calledCommand != "/usr/bin/bun" {
		t.Errorf("expected bun to be replaced with bunBin, got %q", calledCommand)
	}
}

func TestInvokeHook_CustomCommand(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledCommand string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledCommand = name
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "node",
			Args:    []string{"generate.js"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "/usr/bin/bun", h, false, nil)
	if calledCommand != "node" {
		t.Errorf("expected custom command 'node', got %q", calledCommand)
	}
}

func TestInvokeHook_CustomTimeout(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:      "command",
			Command:   "bun",
			Args:      []string{"gen.ts"},
			TimeoutMs: 30000,
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInvokeHook_SuccessWithoutSummaryFails(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"v":1,"type":"log","level":"info","message":"running"}` + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error when a successful hook emits no summary")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestInvokeHook_TruncatedSummaryFails(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	// Simulate a summary line cut off at a pipe-buffer boundary: the hook
	// process exited before stdout drained, so the line is not valid JSON.
	full := `{"v":1,"type":"summary","level":"info","message":"preBuild completed","data":{"exports":{"static-loader":"/p/.gen/src/static/.static.gen.ts"},"assets":{"public/install.sh":"/p/.gen/public/install.sh"}}}`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: full[:128]}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error for truncated summary output")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got %q", err.Error())
	}
}

func TestInvokeHook_TemplateExpansionInArgs(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledArgs []string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledArgs = args
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"{extensionRoot}/bin/gen.ts", "--project={projectRoot}"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	// Check that templates were expanded
	foundExpandedExt := false
	for _, a := range calledArgs {
		if strings.Contains(a, extRoot) {
			foundExpandedExt = true
		}
		if strings.Contains(a, "{extensionRoot}") {
			t.Errorf("template not expanded in arg: %q", a)
		}
	}
	if !foundExpandedExt {
		t.Error("expected extensionRoot template to be expanded in args")
	}
}

func TestInvokeHook_LongStderrTruncated(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	longStderr := strings.Repeat("x", 5000)
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: longStderr}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"gen.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error on failure")
	}
	// Error message should contain truncated stderr (last 2000 chars) plus prefix
	errMsg := err.Error()
	if len(errMsg) > 2100 {
		t.Errorf("expected truncated error message, got length %d", len(errMsg))
	}
	if len(errMsg) < 2000 {
		t.Errorf("expected at least 2000 chars from truncated stderr, got length %d", len(errMsg))
	}
}

func TestInvokeHook_FailureSurfacesStdoutErrorEvent(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"meta","level":"info","message":"Starting preBuild","data":{"extension":"@putnami/web","hook":"preBuild"}}
{"v":1,"type":"error","level":"error","message":"react-client.gen.tsx: Could not resolve './missing'","data":{"code":"BuildError","stack":"Error: Could not resolve './missing'\n    at Bundler.build (/path/to/bundler.ts:42:11)"}}
`

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout, Stderr: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@putnami/web",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error on hook failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Could not resolve './missing'") {
		t.Errorf("expected stdout error message to be surfaced, got %q", msg)
	}
	if !strings.Contains(msg, "Bundler.build") {
		t.Errorf("expected first stack frame to be surfaced, got %q", msg)
	}
}

func TestInvokeHook_FailureFallsBackToStderrWhenNoErrorEvent(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stdout:   `{"v":1,"type":"log","level":"info","message":"running"}` + "\n",
			Stderr:   "bun: command failed",
		}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bun: command failed") {
		t.Errorf("expected stderr fallback, got %q", err.Error())
	}
}

func TestInvokeHook_FailureSurfacesBothStdoutErrorAndStderr(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"error","level":"error","message":"build failed"}` + "\n"
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout, Stderr: "warning: deprecated API"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "build failed") {
		t.Errorf("expected stdout error message, got %q", msg)
	}
	if !strings.Contains(msg, "warning: deprecated API") {
		t.Errorf("expected stderr to also be included, got %q", msg)
	}
}

func TestInvokeHook_FailureNoOutputAtAll(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: "", Stderr: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no diagnostic output") {
		t.Errorf("expected diagnostic hint when no output, got %q", err.Error())
	}
}

func TestInvokeHook_FailureMultipleErrorEvents(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"error","level":"error","message":"first failure"}
{"v":1,"type":"error","level":"error","message":"second failure"}
`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first failure") || !strings.Contains(msg, "second failure") {
		t.Errorf("expected both error messages, got %q", msg)
	}
}

// ---- RunHooksPreBuild with mocked exec ----

func TestRunHooksPreBuild_WithHookSuccess(t *testing.T) {
	dir := t.TempDir()

	// Set up a dependency with a preBuild hook
	depDir := filepath.Join(dir, "node_modules", "my-ext")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"my-ext":"^1.0.0"}}`), 0644)

	summaryJSON := `{"type":"summary","data":{"exports":{"loader":"./loader.js"},"assets":{"css":"./style.css"}}}`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summaryJSON + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Exports["loader"] != "./loader.js" {
		t.Errorf("expected export loader=./loader.js, got %v", result.Exports)
	}
	if result.Assets["css"] != "./style.css" {
		t.Errorf("expected asset css=./style.css, got %v", result.Assets)
	}
}

func TestRunHooksPreBuild_HookError(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "bad-ext")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"bad-ext":"^1.0.0"}}`), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrPermission
	})

	_, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err == nil {
		t.Error("expected error when hook fails")
	}
	if !strings.Contains(err.Error(), "bad-ext") {
		t.Errorf("expected hook name in error, got %q", err.Error())
	}
}

func TestRunHooksPreBuild_MergesMultipleHooks(t *testing.T) {
	dir := t.TempDir()

	// Two hooks
	for i, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		os.MkdirAll(depDir, 0755)
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
		os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
		_ = i
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644)

	callCount := 0
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		callCount++
		key := "a"
		if callCount == 2 {
			key = "b"
		}
		summary := `{"type":"summary","data":{"exports":{"` + key + `":"` + key + `.js"},"assets":{}}}`
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Exports) != 2 {
		t.Errorf("expected 2 merged exports, got %d: %v", len(result.Exports), result.Exports)
	}
	if result.HookCount != 2 {
		t.Errorf("expected HookCount=2, got %d", result.HookCount)
	}
}

func TestRunHooksPreBuild_RejectsConflictingExportKeys(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		if err := os.MkdirAll(depDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
		if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	callCount := 0
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		callCount++
		path := fmt.Sprintf(".gen/src/loader-%d.ts", callCount)
		summary := `{"type":"summary","data":{"exports":{"api-loader":"` + path + `"},"assets":{}}}`
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
	})

	_, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err == nil || !strings.Contains(err.Error(), `export "api-loader" conflicts`) {
		t.Fatalf("expected conflicting export error, got %v", err)
	}
}

func TestRunHooksPreBuild_NoHooks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	result, err := RunHooks("", dir, "", HookPreBuild, "", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Exports) != 0 {
		t.Errorf("expected empty exports, got %d", len(result.Exports))
	}
	if len(result.Assets) != 0 {
		t.Errorf("expected empty assets, got %d", len(result.Assets))
	}
	if result.HookCount != 0 {
		t.Errorf("expected HookCount=0, got %d", result.HookCount)
	}
	// A readable package.json that declares no hook is a known absence.
	if len(result.Unavailable) != 0 {
		t.Errorf("expected no unavailable cause, got %q", result.Unavailable)
	}
}

// ---- discoverHooks: configExtract kind ----

func TestDiscoverHooks_ConfigExtractKind(t *testing.T) {
	// A dependency that registers BOTH preBuild and configExtract hooks
	// should be discoverable under either kind, independently.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["pre.ts"]},"configExtract":{"kind":"command","command":"bun","args":["extract.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkg := `{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0644)

	pre := mustDiscoverHooks(t, dir, HookPreBuild)
	if len(pre) != 1 || pre[0].kind != HookPreBuild {
		t.Errorf("expected 1 preBuild hook, got %d", len(pre))
	}

	ext := mustDiscoverHooks(t, dir, HookConfigExtract)
	if len(ext) != 1 || ext[0].kind != HookConfigExtract {
		t.Errorf("expected 1 configExtract hook with kind=%q, got %d (kind=%q)", HookConfigExtract, len(ext), kindOrEmpty(ext))
	}
	if ext[0].hook.Args[0] != "extract.ts" {
		t.Errorf("expected configExtract args[0]='extract.ts', got %q", ext[0].hook.Args[0])
	}
}

func TestDiscoverHooks_OnlyOneKindRegistered(t *testing.T) {
	// A dependency that registers configExtract only should not surface
	// under preBuild — and vice versa.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "extract-only")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkg := `{"name":"test","dependencies":{"extract-only":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0644)

	if got := mustDiscoverHooks(t, dir, HookPreBuild); len(got) != 0 {
		t.Errorf("expected 0 preBuild hooks, got %d", len(got))
	}
	if got := mustDiscoverHooks(t, dir, HookConfigExtract); len(got) != 1 {
		t.Errorf("expected 1 configExtract hook, got %d", len(got))
	}
}

func kindOrEmpty(hooks []discoveredHook) string {
	if len(hooks) == 0 {
		return ""
	}
	return hooks[0].kind
}

// ---- RunHooks: configExtract dispatch ----

func TestRunHooks_ConfigExtractForwardsKind(t *testing.T) {
	// Verify the hook kind makes it into the JSON context the subprocess
	// reads — that's what lets a multi-hook extension script dispatch
	// to the right handler.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var capturedContext hookContext
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		// The runner appends --putnami-context <path> after the script args.
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				data, err := os.ReadFile(args[i+1])
				if err == nil {
					_ = json.Unmarshal(data, &capturedContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HookCount != 1 {
		t.Errorf("expected HookCount=1, got %d", result.HookCount)
	}
	if capturedContext.Hook != HookConfigExtract {
		t.Errorf("expected context.hook=%q, got %q", HookConfigExtract, capturedContext.Hook)
	}
	// A summary without a status yields "" — legacy hooks stay trusted.
	if len(result.Statuses) != 1 || result.Statuses[0] != "" {
		t.Errorf("expected Statuses=[\"\"], got %v", result.Statuses)
	}
}

func TestRunHooks_ForwardsProjectName(t *testing.T) {
	// The putnami project identity (ctx.Project.Name) must reach the bun hook
	// through the context file so the config-extract hook can write it as the
	// manifest appName instead of the npm package.json name.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"auth.example.com","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var rawContext map[string]any
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				if data, err := os.ReadFile(args[i+1]); err == nil {
					_ = json.Unmarshal(data, &rawContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	const projectName = "identity/workloads/auth-server"
	if _, err := RunHooks(dir, dir, projectName, HookConfigExtract, "config-extract", "bun", false, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := rawContext["projectName"].(string); got != projectName {
		t.Errorf("expected context.projectName=%q (the putnami identity, not the package.json name), got %q", projectName, got)
	}
}

func TestRunHooks_OmitsEmptyProjectName(t *testing.T) {
	// Backward compatibility: when no project identity is supplied the field is
	// omitempty on the wire, so a context file from an older runner is
	// byte-identical to the pre-fix one and the TS side falls back to
	// getCurrentProject().name.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var rawContext map[string]any
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				if data, err := os.ReadFile(args[i+1]); err == nil {
					_ = json.Unmarshal(data, &rawContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	if _, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := rawContext["projectName"]; present {
		t.Errorf("expected projectName absent from context when empty, got %v", rawContext["projectName"])
	}
}

func TestRunHooks_CollectsSummaryStatus(t *testing.T) {
	// The config-extract runner distinguishes "hook regenerated its output"
	// from "hook ran but produced nothing" via the summary status — a stale
	// file on disk cannot make that distinction.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0,
			Stdout: `{"type":"summary","data":{"exports":{},"assets":{},"status":"empty"}}` + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Statuses) != 1 || result.Statuses[0] != "empty" {
		t.Errorf("expected Statuses=[\"empty\"], got %v", result.Statuses)
	}
}

// writePreBuildDeps installs one preBuild hook per name under dir and declares
// them all in dir/package.json.
func writePreBuildDeps(t *testing.T, dir string, names ...string) {
	t.Helper()
	deps := map[string]string{}
	for _, name := range names {
		depDir := filepath.Join(dir, "node_modules", name)
		if err := os.MkdirAll(depDir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
		if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		deps[name] = "^1.0.0"
	}
	pkg, err := json.Marshal(map[string]any{"name": "test", "dependencies": deps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkg, 0o644); err != nil {
		t.Fatal(err)
	}
}

// completeRouteEvaluation and unavailableRouteEvaluation are the exact
// version 1 literals completeHttpRoutesEvaluation() and
// unavailableHttpRoutesEvaluation('the application factory failed') from
// @putnami/application/http-routes serialize to; the helper's own test pins
// the same bytes.
const (
	completeRouteEvaluation    = `{"v":1,"state":"complete"}`
	unavailableRouteEvaluation = `{"v":1,"state":"unavailable","reason":"the application factory failed"}`
)

// preBuildSummary is the summary line of a hook with the given exports object,
// generic status and data.httpRoutes member. An empty status or httpRoutes
// leaves that member out, as a hook that does not report it does.
func preBuildSummary(exports, status, httpRoutes string) string {
	data := `"exports":` + exports + `,"assets":{}`
	if status != "" {
		data += `,"status":"` + status + `"`
	}
	if httpRoutes != "" {
		data += `,"httpRoutes":` + httpRoutes
	}
	return `{"type":"summary","data":{` + data + `}}` + "\n"
}

// routeEvaluationGuidanceFor is the remedy a route evaluation cause names for
// the hook of dependency name.
func routeEvaluationGuidanceFor(name string) string {
	return "upgrade " + name + " together with @putnami/typescript to releases that report data.httpRoutes, " +
		`or adapt a custom hook to return {"v":1,"state":"complete"} as data.httpRoutes once it has evaluated its routes`
}

// The helper's exact literals are what the reader accepts.
func TestParseRouteEvaluation_AcceptsTheHelperLiterals(t *testing.T) {
	state, reason, err := parseRouteEvaluation([]byte(completeRouteEvaluation))
	if err != nil || state != "complete" || reason != "" {
		t.Errorf("complete literal = (%q, %q, %v), want (complete, \"\", nil)", state, reason, err)
	}
	state, reason, err = parseRouteEvaluation([]byte(unavailableRouteEvaluation))
	if err != nil || state != "unavailable" || reason != "the application factory failed" {
		t.Errorf("unavailable literal = (%q, %q, %v), want its reason", state, reason, err)
	}
}

// Every preBuild hook returns a route evaluation, and RunHooks records each
// one that is not complete, in invocation order, after it has run every hook:
// a hook built before the member existed, beside one that reports its routes
// unavailable and one that reports them complete. The outputs of every hook
// still merge; the caller fails before it publishes them.
func TestRunHooks_CollectsEveryRouteEvaluationCause(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "unavailable-route-producers-fail-generate")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-missing-or-malformed-route-evaluation-fails-naming-the-hook")
	dir := t.TempDir()
	writePreBuildDeps(t, dir, "ext-a", "ext-b", "ext-c")

	summaries := []string{
		preBuildSummary(`{"a":"a.js"}`, "ok", ""),
		preBuildSummary(`{}`, "", unavailableRouteEvaluation),
		preBuildSummary(`{"c":"c.js"}`, "", completeRouteEvaluation),
	}
	calls := 0
	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summaries[calls-1]}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("hooks run = %d, want every one of the 3", calls)
	}
	want := []string{
		"hook ext-a reported no route evaluation: its summary has no data.httpRoutes; " + routeEvaluationGuidanceFor("ext-a"),
		"hook ext-b reported its routes unavailable: the application factory failed",
	}
	if !slices.Equal(result.Unavailable, want) {
		t.Errorf("Unavailable = %q\nwant %q", result.Unavailable, want)
	}
	if !slices.Equal(result.Statuses, []string{"ok", "", ""}) {
		t.Errorf("Statuses = %q, want [ok \"\" \"\"]", result.Statuses)
	}
	if result.Exports["a"] != "a.js" || result.Exports["c"] != "c.js" {
		t.Errorf("every hook's exports must still merge, got %v", result.Exports)
	}
}

// A data.httpRoutes member that is not exactly a version 1 evaluation is a
// cause that names the hook, the defect and the remedy. Neither a generic
// status nor a JSON null certifies a legacy hook.
func TestRunHooks_MalformedRouteEvaluationNamesTheHook(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-missing-or-malformed-route-evaluation-fails-naming-the-hook")
	missing := "hook ext-a reported no route evaluation: its summary has no data.httpRoutes; " + routeEvaluationGuidanceFor("ext-a")
	malformed := func(detail string) string {
		return "hook ext-a reported a malformed route evaluation: data.httpRoutes " + detail + "; " + routeEvaluationGuidanceFor("ext-a")
	}
	cases := []struct {
		name       string
		status     string
		httpRoutes string
		want       string
	}{
		{"a legacy summary", "", "", missing},
		{"a legacy summary with status ok", "ok", "", missing},
		{"null", "", `null`, malformed("is not an object")},
		{"a string", "", `"complete"`, malformed("is not an object")},
		{"an array", "", `[]`, malformed("is not an object")},
		{"an unknown member", "", `{"v":1,"state":"complete","routes":[]}`, malformed(`has the member "routes", which version 1 does not define`)},
		{"no version", "", `{"state":"complete"}`, malformed("has no v")},
		{"version 2", "", `{"v":2,"state":"complete"}`, malformed("has v 2, and only version 1 is supported")},
		{"a string version", "", `{"v":"1","state":"complete"}`, malformed(`has v "1", and only version 1 is supported`)},
		{"no state", "", `{"v":1}`, malformed("has no state")},
		{"an unknown state", "", `{"v":1,"state":"partial"}`, malformed(`has state "partial", not "complete" or "unavailable"`)},
		{"a null state", "", `{"v":1,"state":null}`, malformed(`has state null, not "complete" or "unavailable"`)},
		{"a reason on complete", "", `{"v":1,"state":"complete","reason":"x"}`, malformed("is complete and has a reason, which only an unavailable evaluation carries")},
		{"unavailable without a reason", "", `{"v":1,"state":"unavailable"}`, malformed("is unavailable without a reason string that is not blank")},
		{"an empty reason", "", `{"v":1,"state":"unavailable","reason":""}`, malformed("is unavailable without a reason string that is not blank")},
		{"a blank reason", "", `{"v":1,"state":"unavailable","reason":" \n\t"}`, malformed("is unavailable without a reason string that is not blank")},
		{"a reason that is not a string", "", `{"v":1,"state":"unavailable","reason":{"code":42}}`, malformed("is unavailable without a reason string that is not blank")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePreBuildDeps(t, dir, "ext-a")
			summary := preBuildSummary(`{}`, tc.status, tc.httpRoutes)
			withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
				return &exec.Result{Success: true, ExitCode: 0, Stdout: summary}, nil
			})

			result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := []string{tc.want}; !slices.Equal(result.Unavailable, want) {
				t.Errorf("Unavailable = %q\nwant %q", result.Unavailable, want)
			}
		})
	}
}

// The route evaluation is independent of the generic status: a complete one
// is no cause whatever status the hook reports, and the status is still
// recorded for its own consumers.
func TestRunHooks_ACompleteRouteEvaluationIsNoCauseWhateverTheStatus(t *testing.T) {
	for _, status := range []string{"", "ok", "empty", "skipped", "unavailable"} {
		t.Run("status="+status, func(t *testing.T) {
			dir := t.TempDir()
			writePreBuildDeps(t, dir, "ext-a")
			summary := preBuildSummary(`{}`, status, completeRouteEvaluation)
			withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
				return &exec.Result{Success: true, ExitCode: 0, Stdout: summary}, nil
			})

			result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result.Unavailable) != 0 {
				t.Errorf("Unavailable = %q, want none", result.Unavailable)
			}
			if !slices.Equal(result.Statuses, []string{status}) {
				t.Errorf("Statuses = %q, want [%q]", result.Statuses, status)
			}
		})
	}
}

// Only preBuild hooks report a route evaluation: a configExtract hook keeps
// its own result semantics, with or without the member.
func TestRunHooks_RouteEvaluationIsAPreBuildContract(t *testing.T) {
	for name, httpRoutes := range map[string]string{"absent": "", "malformed": `{"v":2}`} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			depDir := filepath.Join(dir, "node_modules", "ext-a")
			if err := os.MkdirAll(depDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFixture(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`)(t, filepath.Join(depDir, "putnami.extension.json"))
			writeFixture(`{"name":"test","dependencies":{"ext-a":"^1.0.0"}}`)(t, filepath.Join(dir, "package.json"))
			summary := preBuildSummary(`{}`, "ok", httpRoutes)
			withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
				return &exec.Result{Success: true, ExitCode: 0, Stdout: summary}, nil
			})

			result, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result.Unavailable) != 0 || !slices.Equal(result.Statuses, []string{"ok"}) {
				t.Errorf("result = %+v, want status ok and no cause", result)
			}
		})
	}
}

// A discovery gap reaches the caller through RunHooks, and the hooks that
// were discovered still run.
func TestRunHooks_DiscoveryGapIsRecordedAndOtherHooksRun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "unavailable-route-producers-fail-generate")
	dir := t.TempDir()
	writePreBuildDeps(t, dir, "ext-a", "ext-b")
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "ext-b", "putnami.extension.json"), []byte(`{"hooks":`), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		return &exec.Result{Success: true, ExitCode: 0, Stdout: preBuildSummary(`{"a":"a.js"}`, "", completeRouteEvaluation)}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 || result.HookCount != 1 || result.Exports["a"] != "a.js" {
		t.Errorf("the readable hook must still run once: calls=%d HookCount=%d exports=%v", calls, result.HookCount, result.Exports)
	}
	if len(result.Unavailable) != 1 || !strings.HasPrefix(result.Unavailable[0], "ext-b: putnami.extension.json is malformed: ") {
		t.Errorf("Unavailable = %q, want one malformed-manifest entry for ext-b", result.Unavailable)
	}
}

// A missing package.json reaches the caller as a gap even though no hook runs.
func TestRunHooks_MissingPackageJSONIsAGap(t *testing.T) {
	dir := t.TempDir()
	result, err := RunHooks("", dir, "", HookPreBuild, "", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HookCount != 0 {
		t.Errorf("HookCount = %d, want 0", result.HookCount)
	}
	if want := []string{"package.json is missing"}; !slices.Equal(result.Unavailable, want) {
		t.Errorf("Unavailable = %q, want %q", result.Unavailable, want)
	}
}

// Discover reports what RunHooks would run, in its order, and the manifests it
// could not use, without running a process.
func TestDiscover_NamesTheHooksAndGapsWithoutRunningThem(t *testing.T) {
	dir := t.TempDir()
	writePreBuildDeps(t, dir, "ext-b", "ext-a", "ext-c")
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "ext-c", "putnami.extension.json"), []byte(`{"hooks":`), 0o644); err != nil {
		t.Fatal(err)
	}
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("a process ran (%v) during discovery", args)
		return &exec.Result{Success: true}, nil
	})

	discovery, err := Discover(dir, dir, HookPreBuild)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{"ext-a", "ext-b"}; !slices.Equal(discovery.Hooks, want) {
		t.Errorf("Hooks = %q, want %q", discovery.Hooks, want)
	}
	if len(discovery.Unavailable) != 1 || !strings.HasPrefix(discovery.Unavailable[0], "ext-c: putnami.extension.json is malformed: ") {
		t.Errorf("Unavailable = %q, want one malformed-manifest entry for ext-c", discovery.Unavailable)
	}
}

// Discover finds no hook and no gap for a readable package.json without
// dependencies: a known absence. A missing package.json is a gap.
func TestDiscover_KnownAbsenceAndMissingPackageJSON(t *testing.T) {
	dir := t.TempDir()
	discovery, err := Discover(dir, dir, HookPreBuild)
	if err != nil {
		t.Fatalf("Discover without package.json: %v", err)
	}
	if len(discovery.Hooks) != 0 || !slices.Equal(discovery.Unavailable, []string{"package.json is missing"}) {
		t.Errorf("discovery = %+v, want no hook and the missing package.json gap", discovery)
	}

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"lib"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	discovery, err = Discover(dir, dir, HookPreBuild)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovery.Hooks) != 0 || len(discovery.Unavailable) != 0 {
		t.Errorf("discovery = %+v, want neither a hook nor a gap", discovery)
	}
}

// Discover fails where RunHooks fails before it runs a hook: on a declared
// dependency that no node_modules holds.
func TestDiscover_UninstalledDependencyFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"app","dependencies":{"absent-ext":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(dir, dir, HookPreBuild); err == nil || !strings.Contains(err.Error(), "absent-ext") {
		t.Fatalf("Discover error = %v, want one naming absent-ext", err)
	}
}
