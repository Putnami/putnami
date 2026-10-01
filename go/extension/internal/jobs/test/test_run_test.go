package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// --- Run: skips when no go.mod ---

func TestRun_SkipsWhenNoGoMod(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project: pctx.Project{
			Name:     "test-project",
			FullPath: dir,
		},
		Params: pctx.Params{},
	}
	emit := jsonl.New()

	status, _, err := Run(ctx, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("status = %q, want %q", status, "SKIP")
	}
}

// --- Run: coverage threshold gate (end-to-end) ---

// writeCoverageModule writes a tiny module whose test covers one of two
// functions, yielding ~50% statement coverage.
func writeCoverageModule(t *testing.T, dir string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module covmod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "covmod.go"),
		"package covmod\n\nfunc Covered() int { return 1 }\n\nfunc Uncovered() int { return 2 }\n")
	mustWrite(t, filepath.Join(dir, "covmod_test.go"),
		"package covmod\n\nimport \"testing\"\n\nfunc TestCovered(t *testing.T) {\n\tif Covered() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRun_CoverageThresholdNotMet(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covmod", FullPath: dir},
		// Validation cadence: --enforce-coverage collects the profile and the gate
		// fires. ~50% coverage cannot meet a 99% threshold → the job must fail even
		// though the test itself passes.
		Params: pctx.Params{
			"enforce-coverage":   json.RawMessage(`true`),
			"coverage-threshold": json.RawMessage(`99`),
		},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("status = %q, want FAILED (coverage below threshold)", status)
	}
}

// TestRun_DefaultEnforcesCoverage pins the new default: a plain `putnami test`
// on a ~50%-covered module with a 99% threshold FAILS. Under the old opt-in
// cadence this passed, which is how coverage drops reached CI unnoticed.
func TestRun_DefaultEnforcesCoverage(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covmod", FullPath: dir},
		// No --enforce-coverage anywhere: the gate is on because it defaults on.
		Params: pctx.Params{"coverage-threshold": json.RawMessage(`99`)},
	}

	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("status = %q, want FAILED (the gate is on by default)", status)
	}
	if _, ok := data["coverageSummary"]; !ok {
		t.Error("data has no coverageSummary; the default run must measure")
	}
}

// TestRun_NoEnforceStillMeasuresButNeverFails pins the escape hatch:
// --no-enforce-coverage zeroes the threshold so the same ~50%-covered module
// passes a 99% gate — but it is still instrumented and still reports its
// coverageSummary. You buy out of failing, not out of seeing.
func TestRun_NoEnforceStillMeasuresButNeverFails(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covmod", FullPath: dir},
		Params: pctx.Params{
			"enforce-coverage":   json.RawMessage(`false`),
			"coverage-threshold": json.RawMessage(`99`),
		},
	}

	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK (--no-enforce-coverage cannot fail)", status)
	}
	summary, ok := data["coverageSummary"].(map[string]any)
	if !ok {
		t.Fatalf("data has no coverageSummary %v; the escape hatch must still measure", data["coverageSummary"])
	}
	if pct, _ := summary["percentage"].(float64); pct <= 0 {
		t.Errorf("coverage percentage = %v, want the real measurement", summary["percentage"])
	}
}

// TestRun_CoverageOptOutSkipsInstrumentation pins the only remaining way to skip
// the CPU cost: `coverage: false`, which the default-on gate never overrides.
func TestRun_CoverageOptOutSkipsInstrumentation(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covmod", FullPath: dir},
		Params: pctx.Params{
			"coverage":           json.RawMessage(`false`),
			"coverage-threshold": json.RawMessage(`99`),
		},
	}

	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK (opt-out zeroes the threshold)", status)
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("data has coverageSummary %v, want none (coverage not collected)", data["coverageSummary"])
	}
}

func TestRun_CoverageThresholdNoInstrumentedStatements(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	// A package whose only non-test file declares a type carries zero
	// instrumented statements, so the coverage profile has no usable data. A
	// threshold must fail it rather than evaluate a meaningless 0%.
	mustWrite(t, filepath.Join(dir, "go.mod"), "module covempty\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "covempty.go"), "package covempty\n\ntype T struct{ X int }\n")
	mustWrite(t, filepath.Join(dir, "covempty_test.go"),
		"package covempty\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {\n\t_ = T{X: 1}\n}\n")

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covempty", FullPath: dir},
		// Validation cadence: --enforce-coverage on, threshold enforced. This module
		// did NOT opt out (no coverage:false), so an empty profile is a real failure.
		Params: pctx.Params{
			"enforce-coverage":   json.RawMessage(`true`),
			"coverage-threshold": json.RawMessage(`80`),
		},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("status = %q, want FAILED (no instrumented statements is not a pass)", status)
	}
}

func TestRun_CoverageThresholdSkippedWhenCoverageDisabled(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	// A pure-vocabulary / generated-only module carries zero instrumented
	// statements (mirrors go.putnami.dev/protocol/identity). It opts out with
	// `coverage: false`; that opt-out must be authoritative and suppress the
	// inherited workspace coverage-threshold gate rather than failing the job
	// with "no coverage data was produced" — EVEN under the validation cadence.
	// This is the review regression: --enforce-coverage is a global cadence
	// switch and must NOT override a project's coverage:false opt-out (it is a
	// distinct param key, so it never merges over the project's `coverage`).
	mustWrite(t, filepath.Join(dir, "go.mod"), "module covoptout\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "covoptout.go"), "package covoptout\n\ntype T struct{ X int }\n")
	mustWrite(t, filepath.Join(dir, "covoptout_test.go"),
		"package covoptout\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {\n\t_ = T{X: 1}\n}\n")

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covoptout", FullPath: dir},
		// Validation cadence (--enforce-coverage) + workspace-inherited threshold +
		// explicit project-level opt-out. The opt-out must win.
		Params: pctx.Params{
			"enforce-coverage":   json.RawMessage(`true`),
			"coverage-threshold": json.RawMessage(`80`),
			"coverage":           json.RawMessage(`false`),
		},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK (coverage:false opt-out survives --enforce-coverage)", status)
	}
}

func TestRun_CoverageThresholdMet(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "covmod", FullPath: dir},
		// Validation cadence: --enforce-coverage on. A low threshold the ~50%
		// coverage clears → the job passes.
		Params: pctx.Params{
			"enforce-coverage":   json.RawMessage(`true`),
			"coverage-threshold": json.RawMessage(`10`),
		},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK (coverage above threshold)", status)
	}
}

func TestRun_InjectsDatabaseTestBindingsFromEnvTestConfig(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("DATABASE_TEST_BINDINGS", "")

	dir := t.TempDir()
	outDir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module envtest\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "envtest.go"), "package envtest\n\nfunc OK() bool { return true }\n")
	mustWrite(t, filepath.Join(dir, "envtest_test.go"), `package envtest

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDatabaseTestBindingEnv(t *testing.T) {
	if !OK() {
		t.Fatal("unexpected")
	}
	if got := os.Getenv("APP_ENV"); got != "test" {
		t.Fatalf("APP_ENV = %q, want test", got)
	}
	raw := os.Getenv("DATABASE_TEST_BINDINGS")
	if raw == "" {
		t.Fatal("DATABASE_TEST_BINDINGS is empty")
	}
	var doc struct {
		ProtocolVersion int `+"`json:\"protocolVersion\"`"+`
		Mode string `+"`json:\"mode\"`"+`
		Isolation string `+"`json:\"isolation\"`"+`
		ApplyMigrations bool `+"`json:\"applyMigrations\"`"+`
		Databases map[string]struct {
			Engine string `+"`json:\"engine\"`"+`
			Schema string `+"`json:\"schema\"`"+`
			Connection struct {
				Host string `+"`json:\"host\"`"+`
				Port int `+"`json:\"port\"`"+`
				Database string `+"`json:\"database\"`"+`
				User string `+"`json:\"user\"`"+`
				Password string `+"`json:\"password\"`"+`
				SSL *bool `+"`json:\"ssl\"`"+`
			} `+"`json:\"connection\"`"+`
		} `+"`json:\"databases\"`"+`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse DATABASE_TEST_BINDINGS: %v", err)
	}
	if doc.ProtocolVersion != 1 || doc.Mode != "skip" || doc.Isolation != "schema" || !doc.ApplyMigrations {
		t.Fatalf("unexpected policy: %#v", doc)
	}
	db := doc.Databases["default"]
	if db.Engine != "postgres" || db.Schema != "iam" {
		t.Fatalf("unexpected default database metadata: %#v", db)
	}
	if db.Connection.Host != "localhost" || db.Connection.Port != 5432 || db.Connection.Database != "putnami_platform_test" || db.Connection.User != "putnami" || db.Connection.Password != "putnami" {
		t.Fatalf("unexpected default connection: %#v", db.Connection)
	}
	if db.Connection.SSL == nil || *db.Connection.SSL {
		t.Fatalf("ssl = %v, want false", db.Connection.SSL)
	}
}
`)
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "conf", ".env.test.yaml"), `databaseTest:
  mode: skip
  isolation: schema
  applyMigrations: true
database:
  default:
    schema: iam
    host: localhost
    port: 5432
    database: putnami_platform_test
    user: putnami
    password: putnami
    ssl: false
`)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "envtest", FullPath: dir},
		Params:        pctx.Params{},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK", status)
	}
}

func TestRun_PreservesExplicitDatabaseTestBindings(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("DATABASE_TEST_BINDINGS", "manual-binding")

	dir := t.TempDir()
	outDir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module envmanual\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "envmanual.go"), "package envmanual\n\nfunc OK() bool { return true }\n")
	mustWrite(t, filepath.Join(dir, "envmanual_test.go"), `package envmanual

import (
	"os"
	"testing"
)

func TestManualBindingWins(t *testing.T) {
	if !OK() {
		t.Fatal("unexpected")
	}
	if got := os.Getenv("DATABASE_TEST_BINDINGS"); got != "manual-binding" {
		t.Fatalf("DATABASE_TEST_BINDINGS = %q, want manual-binding", got)
	}
}
`)
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "conf", ".env.test.yaml"), `database:
  default:
    schema: iam
    host: localhost
    database: ignored
`)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "envmanual", FullPath: dir},
		Params:        pctx.Params{},
	}

	status, _, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK", status)
	}
}

func TestLoadDatabaseTestBindingSupportsCanonicalSection(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "conf", ".env.test.yaml"), `databaseTest:
  protocolVersion: 1
  mode: require
  databases:
    auth:
      engine: postgres
      schema: iam
      connection:
        host: localhost
        database: auth_test
`)

	raw, ok, err := loadDatabaseTestBinding(dir, "test")
	if err != nil {
		t.Fatalf("loadDatabaseTestBinding: %v", err)
	}
	if !ok {
		t.Fatal("expected binding")
	}
	if !strings.Contains(raw, `"mode":"require"`) || !strings.Contains(raw, `"auth"`) {
		t.Fatalf("unexpected binding: %s", raw)
	}
}

// --- Run: output directory resolution ---

func TestRun_OutputDirectoryResolution(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "output")
	os.MkdirAll(outDir, 0o755)

	// Create go.mod so tests don't skip
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644)

	// Test relative outputdir
	customDir := "custom-output"
	expected := filepath.Join(outDir, customDir)

	// Verify resolution logic
	outputdir := customDir
	resolved := outDir
	if outputdir != "" {
		if filepath.IsAbs(outputdir) {
			resolved = outputdir
		} else {
			resolved = filepath.Join(outDir, outputdir)
		}
	}

	if resolved != expected {
		t.Errorf("resolved = %q, want %q", resolved, expected)
	}
}

func TestRun_OutputDirectoryAbsolute(t *testing.T) {
	dir := t.TempDir()
	absOutDir := filepath.Join(dir, "abs-output")

	outputdir := absOutDir
	resolved := dir
	if outputdir != "" {
		if filepath.IsAbs(outputdir) {
			resolved = outputdir
		} else {
			resolved = filepath.Join(dir, outputdir)
		}
	}

	if resolved != absOutDir {
		t.Errorf("resolved = %q, want %q", resolved, absOutDir)
	}
}

// --- Run: coverage flag logic ---
//
// coverageEnabled mirrors the real solo expression in test.go:
//
//	coverageEnabled := coverage || coverprofile != "" || covermode != "" || coverhtml
//
// It no longer includes `|| coverageThreshold > 0` — a configured threshold no
// longer turns coverage on by itself (two-cadence design). The batch path uses
// the byte-identical expression in coverageEnabled(options) (test_batch.go).
// These cases exercise the expression given an already-resolved `coverage` bool;
// how the cadence (--enforce-coverage) resolves that bool is pinned by
// TestGoCoverageCadenceSoloBatchParity.
func soloCoverageEnabled(coverage bool, coverprofile, covermode string, coverhtml bool) bool {
	return coverage || coverprofile != "" || covermode != "" || coverhtml
}

func TestCoverageEnabled_DefaultOff(t *testing.T) {
	// With coverage resolved to false (e.g. the inner loop forced it off, or an
	// explicit opt-out) and no other signal, the expression is off so no
	// instrumentation runs.
	if soloCoverageEnabled(false, "", "", false) {
		t.Error("coverage should be OFF when the resolved coverage bool is false")
	}
}

func TestCoverageEnabled_ViaCoverageFlag(t *testing.T) {
	if !soloCoverageEnabled(true, "", "", false) {
		t.Error("coverage should be enabled when the resolved coverage bool is true")
	}
}

func TestCoverageEnabled_ViaProfile(t *testing.T) {
	if !soloCoverageEnabled(false, "custom.out", "", false) {
		t.Error("coverage should be enabled when coverprofile is set")
	}
}

func TestCoverageEnabled_ViaMode(t *testing.T) {
	if !soloCoverageEnabled(false, "", "atomic", false) {
		t.Error("coverage should be enabled when covermode is set")
	}
}

func TestCoverageEnabled_ViaHTML(t *testing.T) {
	if !soloCoverageEnabled(false, "", "", true) {
		t.Error("coverage should be enabled when coverhtml is set")
	}
}

func TestCoverageEnabled_AllFalse(t *testing.T) {
	if soloCoverageEnabled(false, "", "", false) {
		t.Error("coverage should be disabled when all explicit signals are false/empty")
	}
}

// TestCoverageEnabled_ThresholdDoesNotAutoEnable pins the dropped auto-enable
// term: a configured coverage-threshold must NOT turn coverage on by itself.
// The `coverage` policy is the sole authority on whether a run instruments, so a
// project that opted out stays opted out no matter what threshold it inherits.
func TestCoverageEnabled_ThresholdDoesNotAutoEnable(t *testing.T) {
	// Explicit signals all off; a threshold of 80 is configured elsewhere but is
	// not an input to coverageEnabled.
	if soloCoverageEnabled(false, "", "", false) {
		t.Error("a coverage-threshold must not auto-enable coverage over a coverage:false opt-out")
	}
}

func TestParamsFloatThreshold(t *testing.T) {
	// Kebab-case primary key.
	if got := (pctx.Params{"coverage-threshold": json.RawMessage(`80`)}).Float("coverage-threshold", 0, "coverageThreshold"); got != 80 {
		t.Errorf("threshold = %v, want 80", got)
	}
	// camelCase fallback (as injected by the orchestrator).
	if got := (pctx.Params{"coverageThreshold": json.RawMessage(`75.5`)}).Float("coverage-threshold", 0, "coverageThreshold"); got != 75.5 {
		t.Errorf("threshold fallback = %v, want 75.5", got)
	}
	// Default when unset.
	if got := (pctx.Params{}).Float("coverage-threshold", 0, "coverageThreshold"); got != 0 {
		t.Errorf("threshold default = %v, want 0", got)
	}
}

func TestCapitalize(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"coverage 5% low": "Coverage 5% low",
		"Already":         "Already",
		"123":             "123",
	}
	for in, want := range cases {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Run: timeout conversion ---

func TestTimeoutConversion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"30000", "30000ms"},
		{"60", "60ms"},
		{"30s", "30s"},
		{"5m", "5m"},
		{"1h", "1h"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var result string
			if isNumeric(tt.input) {
				result = tt.input + "ms"
			} else {
				result = tt.input
			}
			if result != tt.want {
				t.Errorf("timeout(%q) = %q, want %q", tt.input, result, tt.want)
			}
		})
	}
}

// --- Run: Params integration ---

func TestParamsBoolDefault(t *testing.T) {
	params := pctx.Params{}

	// Both coverage axes default to on: the run measures, and the threshold can
	// fail it. A project opts out of measuring with coverage:false; a run opts out
	// of failing with --no-enforce-coverage.
	if !params.Bool("coverage", true) {
		t.Error("expected coverage policy to default to true (opt out with coverage:false)")
	}
	if !params.Bool("enforce-coverage", true, "enforceCoverage") {
		t.Error("expected enforce-coverage to default to true (a drop must fail the run that caused it)")
	}

	// race defaults to false
	race := params.Bool("race", false)
	if race {
		t.Error("expected race to default to false")
	}
}

func TestParamsBoolExplicit(t *testing.T) {
	params := pctx.Params{
		"coverage": json.RawMessage(`false`),
		"race":     json.RawMessage(`true`),
	}

	coverage := params.Bool("coverage", false)
	if coverage {
		t.Error("expected coverage to be false when explicitly set")
	}

	race := params.Bool("race", false)
	if !race {
		t.Error("expected race to be true when explicitly set")
	}
}

func TestParamsString(t *testing.T) {
	params := pctx.Params{
		"timeout": json.RawMessage(`"30s"`),
	}

	timeout := params.String("timeout")
	if timeout != "30s" {
		t.Errorf("timeout = %q, want %q", timeout, "30s")
	}

	// Missing key returns empty
	missing := params.String("nonexistent")
	if missing != "" {
		t.Errorf("missing = %q, want empty", missing)
	}
}

// TestRun_UnreadableCoverageProfile pins the solo path: a profile that exists
// but cannot be read gives no percentage. With a threshold it fails the gate on
// COVERAGE_PROFILE_UNREADABLE, never on a number computed from the part read
// before the failure; without one it warns and the run passes.
func TestRun_UnreadableCoverageProfile(t *testing.T) {
	for _, test := range []struct {
		name       string
		params     pctx.Params
		wantStatus string
		wantLevel  string
	}{
		{name: "with threshold", params: pctx.Params{"coverage-threshold": json.RawMessage(`60`)}, wantStatus: "FAILED", wantLevel: "error"},
		{name: "without threshold", params: pctx.Params{}, wantStatus: "OK", wantLevel: "warning"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCoverageModule(t, dir)
			mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
				// A directory opens, then every read fails.
				if err := os.MkdirAll(argumentAfter(t, args, "-coverprofile"), 0o755); err != nil {
					t.Fatal(err)
				}
				return []byte(testEvent("pass", "covmod", "TestCovered", "") + "\n"), nil
			})

			ctx := &pctx.Context{
				WorkspaceRoot: dir,
				OutputPath:    t.TempDir(),
				Project:       pctx.Project{Name: "covmod", FullPath: dir},
				Params:        test.params,
			}
			var status string
			var data map[string]any
			var runErr error
			events := captureJobEvents(t, func(emit *jsonl.Emitter) {
				status, data, runErr = Run(ctx, emit, nil)
			})
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			if status != test.wantStatus {
				t.Errorf("status = %q, want %q", status, test.wantStatus)
			}
			if _, ok := data["coverageSummary"]; ok {
				t.Errorf("data has coverageSummary %v, want none from an unreadable profile", data["coverageSummary"])
			}
			var found bool
			for _, diagnostic := range eventsOfType(events, "diagnostic") {
				if diagnostic["code"] == "COVERAGE_THRESHOLD_NOT_MET" {
					t.Errorf("diagnostic = %v, want the unreadable profile instead of a threshold verdict", diagnostic)
				}
				if diagnostic["code"] == "COVERAGE_PROFILE_UNREADABLE" {
					found = true
					if diagnostic["severity"] != test.wantLevel {
						t.Errorf("diagnostic = %v, want severity %s", diagnostic, test.wantLevel)
					}
				}
			}
			if !found {
				t.Errorf("diagnostics = %v, want COVERAGE_PROFILE_UNREADABLE", eventsOfType(events, "diagnostic"))
			}
			raw, _ := json.Marshal(eventsOfType(events, "summary"))
			if !strings.Contains(string(raw), "coverage profile unreadable") {
				t.Errorf("summary = %s, want it to say the coverage profile is unreadable", raw)
			}
		})
	}
}
