package configmerge

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"gopkg.in/yaml.v3"
)

// runResult captures everything an orchestrator observes from a config-merge
// run: the legacy result tuple plus the JSONL phase envelopes emitted on stdout.
type runResult struct {
	status string           // legacy result status: OK / SKIP / FAILED
	data   map[string]any   // legacy result data payload
	err    error            // error returned by Run
	phases []map[string]any // phase events parsed off stdout, in emit order
}

// canonicalPhaseStatuses is the runtime protocol's PhaseStatus vocabulary.
// The non-canonical legacy strings (OK/SKIP/FAILED) must never appear as a
// phase-end status on the wire.
var canonicalPhaseStatuses = map[string]bool{"success": true, "failed": true, "skipped": true}

// runConfigMerge executes Run with os.Stdout redirected to a pipe so the
// emitted JSONL contract can be asserted line-by-line.
func runConfigMerge(t *testing.T, ctx *pctx.Context) runResult {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	status, data, runErr := Run(ctx, jsonl.New(), nil)

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()

	res := runResult{status: status, data: data, err: runErr}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			t.Fatalf("non-JSON line emitted on stdout: %q", line)
		}
		if event["type"] == "phase" {
			res.phases = append(res.phases, event)
		}
	}
	return res
}

// phaseEndStatus returns the status carried by the config-merge phase-end event.
func (r runResult) phaseEndStatus(t *testing.T) string {
	t.Helper()
	for _, p := range r.phases {
		if p["action"] == "end" {
			s, _ := p["status"].(string)
			return s
		}
	}
	t.Fatal("no phase-end event was emitted")
	return ""
}

// isolateEnv neutralizes the ambient process env so the skip branches and the
// environment-suffixed output path are deterministic regardless of the host.
func isolateEnv(t *testing.T, appEnv string) {
	t.Helper()
	t.Setenv("CONFIG_DATA", "")
	t.Setenv("K_SERVICE", "")
	t.Setenv("APP_ENV", appEnv)
}

// writeFile writes content to dir/rel, creating parent directories.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readMergedYAML reads and parses the merged output file for a project/env.
func readMergedYAML(t *testing.T, projectPath, env string) map[string]any {
	t.Helper()
	out := filepath.Join(projectPath, ".gen", "conf", ".env."+env+".yaml")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read merged output %s: %v", out, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse merged output: %v", err)
	}
	return m
}

// readManifestSources reads the source paths the merge manifest of a project
// records. The paths are compared as parsed keys, not as bytes of the JSON
// file, where a Windows path's backslashes are escaped.
func readManifestSources(t *testing.T, projectPath string) map[string]int64 {
	t.Helper()
	manifestPath := filepath.Join(projectPath, ".gen", "conf", ".manifest.json")
	mdata, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Sources map[string]int64 `json:"sources"`
	}
	if err := json.Unmarshal(mdata, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return manifest.Sources
}

// --- skip logic ---

func TestRun_SkipsWhenConfigDataInjected(t *testing.T) {
	isolateEnv(t, "local")
	t.Setenv("CONFIG_DATA", `{"server":{"port":3000}}`)

	// Even with a real config file present, the injection branch must win and
	// short-circuit before any file is read or written.
	dir := t.TempDir()
	writeFile(t, dir, "conf/.env.yaml", "server:\n  port: 8080\n")
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", res.status)
	}
	if got := res.data["reason"]; got != "runtime config injection active" {
		t.Fatalf("skip reason = %v, want runtime config injection active", got)
	}
	if s := res.phaseEndStatus(t); s != "skipped" {
		t.Fatalf("phase-end status = %q, want skipped", s)
	}
	// The skip must not have written an output file.
	if _, err := os.Stat(filepath.Join(dir, ".gen", "conf", ".env.local.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected no output file on injection skip, stat err = %v", err)
	}
}

func TestRun_SkipsWhenKServiceSet(t *testing.T) {
	isolateEnv(t, "local")
	t.Setenv("K_SERVICE", "my-cloud-run-service")

	dir := t.TempDir()
	writeFile(t, dir, "conf/.env.yaml", "server:\n  port: 8080\n")
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", res.status)
	}
	if s := res.phaseEndStatus(t); s != "skipped" {
		t.Fatalf("phase-end status = %q, want skipped", s)
	}
}

func TestRun_SkipsWhenNoDepsAndNoConfig(t *testing.T) {
	isolateEnv(t, "local")

	dir := t.TempDir() // no putnami.json, no conf/ files
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", res.status)
	}
	if got := res.data["reason"]; got != "no dependencies and no config files" {
		t.Fatalf("skip reason = %v, want no dependencies and no config files", got)
	}
	if s := res.phaseEndStatus(t); s != "skipped" {
		t.Fatalf("phase-end status = %q, want skipped", s)
	}
}

func TestRun_SkipsWhenConfigFilesAreEmpty(t *testing.T) {
	isolateEnv(t, "local")

	// A dependency declared but every resolvable config source parses to an
	// empty map: the merged result has no keys, so the final no-values branch
	// fires (distinct reason from the no-deps branch).
	ws := t.TempDir()
	depDir := filepath.Join(ws, "dep")
	appDir := filepath.Join(ws, "app")
	writeFile(t, appDir, "putnami.json", `{"dependencies":["dep"]}`)
	// Dependency config file exists but is empty (parses to nil map).
	writeFile(t, depDir, "conf/.env.yaml", "")

	ctx := &pctx.Context{
		WorkspaceRoot: ws,
		Project:       pctx.Project{Name: "app", FullPath: appDir},
	}

	res := runConfigMerge(t, ctx)
	if res.status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", res.status)
	}
	if got := res.data["reason"]; got != "no config values to merge" {
		t.Fatalf("skip reason = %v, want no config values to merge", got)
	}
	if s := res.phaseEndStatus(t); s != "skipped" {
		t.Fatalf("phase-end status = %q, want skipped", s)
	}
}

// --- JSONL output contract + manifest ---

func TestRun_EmitsCanonicalSuccessEnvelopeAndWritesOutput(t *testing.T) {
	isolateEnv(t, "local")

	dir := t.TempDir()
	writeFile(t, dir, "conf/.env.yaml", "server:\n  host: localhost\n  port: 8080\n")
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}

	// Phase envelope contract: a start then a canonical-success end, both named
	// "config-merge".
	if len(res.phases) < 2 {
		t.Fatalf("expected start+end phase events, got %d: %v", len(res.phases), res.phases)
	}
	start := res.phases[0]
	if start["action"] != "start" || start["name"] != "config-merge" {
		t.Fatalf("first phase = %v, want {action:start, name:config-merge}", start)
	}
	end := res.phases[len(res.phases)-1]
	if end["action"] != "end" || end["name"] != "config-merge" {
		t.Fatalf("last phase = %v, want {action:end, name:config-merge}", end)
	}
	status := res.phaseEndStatus(t)
	if status != "success" {
		t.Fatalf("phase-end status = %q, want success", status)
	}
	if !canonicalPhaseStatuses[status] {
		t.Fatalf("phase-end status %q is not canonical", status)
	}

	// The output path reported in the result payload must exist and match the
	// env-suffixed convention.
	wantOut := filepath.Join(dir, ".gen", "conf", ".env.local.yaml")
	if got, _ := res.data["output"].(string); got != wantOut {
		t.Fatalf("result output = %q, want %q", got, wantOut)
	}
	merged := readMergedYAML(t, dir, "local")
	server, ok := merged["server"].(map[string]any)
	if !ok {
		t.Fatalf("merged output missing server map: %v", merged)
	}
	if server["host"] != "localhost" || server["port"] != 8080 {
		t.Fatalf("merged server = %v, want host=localhost port=8080", server)
	}

	// A staleness manifest is written alongside the output and records every
	// source that contributed to the merge.
	manifestPath := filepath.Join(dir, ".gen", "conf", ".manifest.json")
	mdata, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		GeneratedAt int64            `json:"generatedAt"`
		Sources     map[string]int64 `json:"sources"`
	}
	if err := json.Unmarshal(mdata, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.GeneratedAt == 0 {
		t.Fatal("manifest generatedAt was not stamped")
	}
	srcKey := filepath.Join(dir, "conf", ".env.yaml")
	if _, ok := manifest.Sources[srcKey]; !ok {
		t.Fatalf("manifest sources missing %q: %v", srcKey, manifest.Sources)
	}
	if n, _ := res.data["sources"].(int); n != len(manifest.Sources) {
		t.Fatalf("result sources count = %v, manifest has %d", res.data["sources"], len(manifest.Sources))
	}
}

// --- declared output port ---

// TestRun_ReportsMergedConfigPortProjectRelativeOnSuccess pins the shape the
// mergedConfig port carries: the CLI resolves a `pathFrom` declared output from
// it and DROPS an absolute path (tooling/cli task_capture.go cleanProjectRel),
// which leaves the merged file uncaptured. The port
// must therefore be project-relative, in slash form, and name the file this
// run actually wrote.
func TestRun_ReportsMergedConfigPortProjectRelativeOnSuccess(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "merged-config-survives-a-cache-hit", "config-merge-reports-its-merged-file-project-relative-on-success")
	isolateEnv(t, "test")

	dir := t.TempDir()
	writeFile(t, dir, "conf/.env.yaml", "database:\n  default:\n    engine: postgres\n")
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}

	got, ok := res.data[MergedConfigPort].(string)
	if !ok {
		t.Fatalf("result data %q = %#v, want a string", MergedConfigPort, res.data[MergedConfigPort])
	}
	// The manifest's config-merge-test-exec declares this literal path (APP_ENV
	// is pinned to test there), so the port and the literal declaration must
	// agree byte for byte.
	if want := ".gen/conf/.env.test.yaml"; got != want {
		t.Fatalf("%s = %q, want %q", MergedConfigPort, got, want)
	}
	if filepath.IsAbs(got) || strings.Contains(got, `\`) || strings.HasPrefix(got, "./") {
		t.Fatalf("%s = %q must be a clean project-relative slash path", MergedConfigPort, got)
	}
	// The port names the file that exists on disk, and the legacy absolute
	// `output` field keeps pointing at the same file.
	onDisk := filepath.Join(dir, filepath.FromSlash(got))
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("reported merged file %s does not exist: %v", onDisk, err)
	}
	if legacy, _ := res.data["output"].(string); legacy != onDisk {
		t.Fatalf("result output = %q, want %q (the same file the port names)", legacy, onDisk)
	}
}

// TestRun_ReportsNoMergedConfigPortOnSkip pins the other half of the
// optionalEmpty declaration: every SKIP branch reports NOTHING under the port,
// so the CLI records the output as absent instead of failing to resolve it —
// or worse, capturing a file an earlier run left behind.
func TestRun_ReportsNoMergedConfigPortOnSkip(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "merged-config-survives-a-cache-hit", "config-merge-reports-no-merged-file-on-skip")

	cases := []struct {
		name  string
		setup func(t *testing.T, ws, appDir string)
	}{
		{
			name: "runtime config injection",
			setup: func(t *testing.T, _, appDir string) {
				t.Setenv("K_SERVICE", "svc")
				writeFile(t, appDir, "conf/.env.yaml", "server:\n  port: 8080\n")
			},
		},
		{
			name:  "no dependencies and no config files",
			setup: func(*testing.T, string, string) {},
		},
		{
			name: "no config values to merge",
			setup: func(t *testing.T, ws, appDir string) {
				writeFile(t, appDir, "putnami.json", `{"dependencies":["dep"]}`)
				writeFile(t, filepath.Join(ws, "dep"), "conf/.env.yaml", "")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t, "test")
			ws := t.TempDir()
			appDir := filepath.Join(ws, "app")
			tc.setup(t, ws, appDir)
			ctx := &pctx.Context{WorkspaceRoot: ws, Project: pctx.Project{Name: "app", FullPath: appDir}}

			res := runConfigMerge(t, ctx)
			if res.err != nil {
				t.Fatalf("unexpected error: %v", res.err)
			}
			if res.status != "SKIP" {
				t.Fatalf("status = %q, want SKIP", res.status)
			}
			if v, present := res.data[MergedConfigPort]; present {
				t.Fatalf("a SKIP reported %s = %#v; the optionalEmpty output must be absent", MergedConfigPort, v)
			}
			if _, err := os.Stat(filepath.Join(appDir, ".gen", "conf", ".env.test.yaml")); !os.IsNotExist(err) {
				t.Fatalf("a SKIP must write no merged file, stat err = %v", err)
			}
		})
	}
}

func TestRun_EnvSuffixSelectsOutputAndConfigFile(t *testing.T) {
	isolateEnv(t, "prod")

	dir := t.TempDir()
	// Only the prod-suffixed file exists; the base file is intentionally absent
	// so we prove the env suffix drives both input selection and output naming.
	writeFile(t, dir, "conf/.env.prod.yaml", "server:\n  port: 443\n")
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	res := runConfigMerge(t, ctx)
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}
	// The local file must NOT be produced; only the prod one.
	if _, err := os.Stat(filepath.Join(dir, ".gen", "conf", ".env.local.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .env.local.yaml written, stat err = %v", err)
	}
	merged := readMergedYAML(t, dir, "prod")
	server, _ := merged["server"].(map[string]any)
	if server == nil || server["port"] != 443 {
		t.Fatalf("merged prod server = %v, want port=443", server)
	}
}

// --- merge precedence (own overrides dependency; .gen preferred over source) ---

func TestRun_OwnConfigOverridesDependency(t *testing.T) {
	isolateEnv(t, "local")

	ws := t.TempDir()
	depDir := filepath.Join(ws, "dep")
	appDir := filepath.Join(ws, "app")

	// Dependency contributes a default; the workload overrides one leaf and adds
	// its own. A nested map present in both must deep-merge, not get clobbered.
	writeFile(t, depDir, "conf/.env.yaml", "server:\n  host: dep-host\n  port: 1111\nfeature:\n  fromDep: true\n")
	writeFile(t, appDir, "putnami.json", `{"dependencies":["dep"]}`)
	writeFile(t, appDir, "conf/.env.yaml", "server:\n  port: 2222\nfeature:\n  fromApp: true\n")

	ctx := &pctx.Context{
		WorkspaceRoot: ws,
		Project:       pctx.Project{Name: "app", FullPath: appDir},
	}

	res := runConfigMerge(t, ctx)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}

	merged := readMergedYAML(t, appDir, "local")
	server, _ := merged["server"].(map[string]any)
	if server == nil {
		t.Fatalf("merged missing server: %v", merged)
	}
	// Own value overrides the dependency's port...
	if server["port"] != 2222 {
		t.Fatalf("server.port = %v, want 2222 (own overrides dep)", server["port"])
	}
	// ...but the dependency-only key under the same nested map survives.
	if server["host"] != "dep-host" {
		t.Fatalf("server.host = %v, want dep-host (preserved from dep)", server["host"])
	}
	feature, _ := merged["feature"].(map[string]any)
	if feature == nil || feature["fromDep"] != true || feature["fromApp"] != true {
		t.Fatalf("feature = %v, want both fromDep and fromApp true (deep merge)", feature)
	}

	// Manifest records both contributing source files.
	sources := readManifestSources(t, appDir)
	if _, ok := sources[filepath.Join(depDir, "conf", ".env.yaml")]; !ok {
		t.Fatalf("manifest should record the dependency source: %v", sources)
	}
	if _, ok := sources[filepath.Join(appDir, "conf", ".env.yaml")]; !ok {
		t.Fatalf("manifest should record the workload source: %v", sources)
	}
}

func TestRun_PrefersDependencyGenOverSource(t *testing.T) {
	isolateEnv(t, "local")

	ws := t.TempDir()
	depDir := filepath.Join(ws, "dep")
	appDir := filepath.Join(ws, "app")

	// The dependency exposes BOTH an already-merged .gen file and raw source
	// files. The runner must consume the .gen file and ignore the source ones
	// (they would otherwise contribute a stale "port: 9999").
	writeFile(t, depDir, ".gen/conf/.env.local.yaml", "server:\n  port: 1234\n")
	writeFile(t, depDir, "conf/.env.yaml", "server:\n  port: 9999\n")
	writeFile(t, depDir, "conf/.env.local.yaml", "server:\n  port: 9999\n")
	writeFile(t, appDir, "putnami.json", `{"dependencies":["dep"]}`)

	ctx := &pctx.Context{
		WorkspaceRoot: ws,
		Project:       pctx.Project{Name: "app", FullPath: appDir},
	}

	res := runConfigMerge(t, ctx)
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}

	merged := readMergedYAML(t, appDir, "local")
	server, _ := merged["server"].(map[string]any)
	if server == nil || server["port"] != 1234 {
		t.Fatalf("server.port = %v, want 1234 (dep .gen preferred over source)", server["port"])
	}

	// Only the .gen source should be in the manifest, not the raw conf files.
	sources := readManifestSources(t, appDir)
	if _, ok := sources[filepath.Join(depDir, ".gen", "conf", ".env.local.yaml")]; !ok {
		t.Fatalf("manifest should record the dep .gen source: %v", sources)
	}
	for _, superseded := range []string{
		filepath.Join(depDir, "conf", ".env.yaml"),
		filepath.Join(depDir, "conf", ".env.local.yaml"),
	} {
		if _, ok := sources[superseded]; ok {
			t.Fatalf("manifest should NOT record the superseded dep source %s: %v", superseded, sources)
		}
	}
}

func TestRun_DependencyOrderLaterWins(t *testing.T) {
	isolateEnv(t, "local")

	ws := t.TempDir()
	depA := filepath.Join(ws, "depA")
	depB := filepath.Join(ws, "depB")
	appDir := filepath.Join(ws, "app")

	// Two dependencies set the same leaf to different values. Deps are merged in
	// listed order, so the later dependency (depB) must win.
	writeFile(t, depA, "conf/.env.yaml", "shared:\n  value: from-a\n")
	writeFile(t, depB, "conf/.env.yaml", "shared:\n  value: from-b\n")
	writeFile(t, appDir, "putnami.json", `{"dependencies":["depA","depB"]}`)

	ctx := &pctx.Context{
		WorkspaceRoot: ws,
		Project:       pctx.Project{Name: "app", FullPath: appDir},
	}

	res := runConfigMerge(t, ctx)
	if res.status != "OK" {
		t.Fatalf("status = %q, want OK", res.status)
	}
	merged := readMergedYAML(t, appDir, "local")
	shared, _ := merged["shared"].(map[string]any)
	if shared == nil || shared["value"] != "from-b" {
		t.Fatalf("shared.value = %v, want from-b (later dependency wins)", shared)
	}
}

// --- DeepMerge unit semantics ---

func TestDeepMerge_NestedMapsMergeAndScalarsOverride(t *testing.T) {
	dst := map[string]any{
		"server": map[string]any{
			"host": "old-host",
			"port": 8080,
		},
		"keep": "untouched",
	}
	src := map[string]any{
		// port overrides the existing scalar; tls adds a new leaf to the
		// nested map; both must coexist with the preserved host.
		"server": map[string]any{
			"port": 9090,
			"tls":  true,
		},
		"added": "new",
	}

	DeepMerge(dst, src)

	server, ok := dst["server"].(map[string]any)
	if !ok {
		t.Fatalf("server is not a map after merge: %T", dst["server"])
	}
	if server["host"] != "old-host" {
		t.Errorf("server.host = %v, want old-host (preserved)", server["host"])
	}
	if server["port"] != 9090 {
		t.Errorf("server.port = %v, want 9090 (overridden)", server["port"])
	}
	if server["tls"] != true {
		t.Errorf("server.tls = %v, want true (added)", server["tls"])
	}
	if dst["keep"] != "untouched" {
		t.Errorf("keep = %v, want untouched", dst["keep"])
	}
	if dst["added"] != "new" {
		t.Errorf("added = %v, want new", dst["added"])
	}
}

func TestDeepMerge_MapReplacesScalarAndScalarReplacesMap(t *testing.T) {
	// When the destination holds a scalar but the source holds a map for the
	// same key, the source map replaces the scalar wholesale (the recursive
	// branch only fires when BOTH sides are maps).
	dst := map[string]any{"a": "scalar", "b": map[string]any{"x": 1}}
	src := map[string]any{"a": map[string]any{"deep": true}, "b": "now-scalar"}

	DeepMerge(dst, src)

	aMap, ok := dst["a"].(map[string]any)
	if !ok || aMap["deep"] != true {
		t.Errorf("a = %v, want map{deep:true} (map replaced scalar)", dst["a"])
	}
	if dst["b"] != "now-scalar" {
		t.Errorf("b = %v, want now-scalar (scalar replaced map)", dst["b"])
	}
}

func TestDeepMerge_SlicesOverwriteNotAppend(t *testing.T) {
	// Slices are not map[string]any, so the source slice overwrites the
	// destination slice rather than concatenating element-wise.
	dst := map[string]any{"hosts": []any{"a", "b", "c"}}
	src := map[string]any{"hosts": []any{"x"}}

	DeepMerge(dst, src)

	hosts, ok := dst["hosts"].([]any)
	if !ok {
		t.Fatalf("hosts is not a slice: %T", dst["hosts"])
	}
	if len(hosts) != 1 || hosts[0] != "x" {
		t.Errorf("hosts = %v, want [x] (overwrite, not append)", hosts)
	}
}

// --- dependency discovery ---

func TestReadDependencies(t *testing.T) {
	t.Run("missing putnami.json yields nil without error", func(t *testing.T) {
		deps, err := readDependencies(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deps != nil {
			t.Fatalf("deps = %v, want nil", deps)
		}
	})

	t.Run("reads dependencies array in order", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "putnami.json", `{"name":"app","dependencies":["a","b","c"]}`)
		deps, err := readDependencies(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Join(deps, ",") != "a,b,c" {
			t.Fatalf("deps = %v, want [a b c]", deps)
		}
	})

	t.Run("malformed putnami.json returns an error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "putnami.json", `{not json`)
		if _, err := readDependencies(dir); err == nil {
			t.Fatal("expected error for malformed putnami.json, got nil")
		}
	})
}

func TestEnvironment(t *testing.T) {
	t.Run("defaults to local", func(t *testing.T) {
		t.Setenv("APP_ENV", "")
		if got := environment(); got != "local" {
			t.Fatalf("environment() = %q, want local", got)
		}
	})
	t.Run("honors APP_ENV", func(t *testing.T) {
		t.Setenv("APP_ENV", "staging")
		if got := environment(); got != "staging" {
			t.Fatalf("environment() = %q, want staging", got)
		}
	})
}
