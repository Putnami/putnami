package sdd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	contracts "go.putnami.dev/protocol/contracts"
)

// baseManifest is a valid contract manifest exercising every vocabulary the
// semantic-compatibility pass classifies: an enum, scopes, a capability drawing
// on a scope, a grant conferring the capability, and claims.
const baseManifest = `{
  "protocolVersion": 1,
  "name": "go.putnami.dev/example/payments",
  "enums": [
    {
      "name": "Currency",
      "values": [
        { "name": "USD", "value": "usd" },
        { "name": "EUR", "value": "eur" }
      ]
    }
  ],
  "scopes": [
    { "name": "payments:read" },
    { "name": "payments:write" }
  ],
  "capabilities": [
    { "name": "processPayments", "scopes": ["payments:write"] }
  ],
  "grants": [
    { "name": "paymentsAdmin", "capability": "processPayments" }
  ],
  "claims": [
    { "name": "sub", "type": "string", "required": true },
    { "name": "tenantId", "type": "string" }
  ]
}`

// priorManifest is baseManifest with an extra enum value, scope, claim, and
// grant. Comparing it (as prior) against baseManifest (as current) yields one
// breaking removal in each of the four classified categories.
const priorManifest = `{
  "protocolVersion": 1,
  "name": "go.putnami.dev/example/payments",
  "enums": [
    {
      "name": "Currency",
      "values": [
        { "name": "USD", "value": "usd" },
        { "name": "EUR", "value": "eur" },
        { "name": "GBP", "value": "gbp" }
      ]
    }
  ],
  "scopes": [
    { "name": "payments:read" },
    { "name": "payments:write" },
    { "name": "payments:refund" }
  ],
  "capabilities": [
    { "name": "processPayments", "scopes": ["payments:write"] }
  ],
  "grants": [
    { "name": "paymentsAdmin", "capability": "processPayments" },
    { "name": "refunder", "capability": "processPayments" }
  ],
  "claims": [
    { "name": "sub", "type": "string", "required": true },
    { "name": "tenantId", "type": "string" },
    { "name": "role", "type": "string" }
  ]
}`

const testLabel = "/example/payments"

// writeProject creates a temp project directory whose schema/contracts.json is
// baseManifest, and returns the project dir. Core took the source as a
// parameter; every caller passed baseManifest, and the tests that need other
// bytes (a mutated manifest, a prior IR) write them over the top themselves.
func writeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatalf("mkdir schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ContractsSourcePath), []byte(baseManifest), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return dir
}

func parseManifest(t *testing.T, src string) *contracts.Manifest {
	t.Helper()
	m, diags := contracts.ParseAndValidateManifest([]byte(src))
	if m == nil {
		t.Fatalf("manifest did not validate: %v", diags)
	}
	return m
}

// staticPrior returns a PriorLoader that yields src, so a check compares against
// a fixed baseline instead of the working tree's git history.
func staticPrior(src string) PriorLoader {
	return func() ([]byte, error) { return []byte(src), nil }
}

// TestContractsGenerate_Deterministic asserts the emitted artifacts are
// byte-stable across repeated generations: no map-iteration order or other
// nondeterminism leaks into a committed artifact.
func TestContractsGenerate_Deterministic(t *testing.T) {
	m := parseManifest(t, baseManifest)
	first, err := generateArtifacts(m)
	if err != nil {
		t.Fatalf("generateArtifacts: %v", err)
	}
	for i := range 20 {
		got, err := generateArtifacts(m)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if len(got) != len(first) {
			t.Fatalf("iteration %d: artifact count changed", i)
		}
		for j := range got {
			if got[j].path != first[j].path {
				t.Fatalf("iteration %d: artifact[%d] path changed: %s != %s", i, j, got[j].path, first[j].path)
			}
			if string(got[j].bytes) != string(first[j].bytes) {
				t.Fatalf("iteration %d: artifact %s not byte-stable", i, got[j].path)
			}
		}
	}
}

// TestContractsGenerate_WritesArtifacts asserts generate writes the canonical
// IR, Go twin, JSON Schema, and Markdown reference into both the staged
// .gen/schema/ tree and the committed schema/ tree, byte-identically.
func TestContractsGenerate_WritesArtifacts(t *testing.T) {
	dir := writeProject(t)
	report, err := ContractsGenerate(dir, testLabel)
	if err != nil {
		t.Fatalf("ContractsGenerate: %v", err)
	}
	want := []string{ContractsSourcePath, contractsGoPath, contractsSchemaPath, contractsMarkdownPath}
	if len(report.Artifacts) != len(want) {
		t.Fatalf("artifacts = %v, want %v", report.Artifacts, want)
	}
	for _, rel := range want {
		committed, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read committed %s: %v", rel, err)
		}
		staged, err := os.ReadFile(filepath.Join(dir, ".gen", rel))
		if err != nil {
			t.Fatalf("read staged %s: %v", rel, err)
		}
		if string(committed) != string(staged) {
			t.Fatalf("staged and committed bytes differ for %s", rel)
		}
	}
	// A second generate over the now-canonical source is byte-stable.
	before, _ := os.ReadFile(filepath.Join(dir, contractsGoPath))
	if _, err := ContractsGenerate(dir, testLabel); err != nil {
		t.Fatalf("second ContractsGenerate: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, contractsGoPath))
	if string(before) != string(after) {
		t.Fatal("regenerating a canonical project changed the Go twin")
	}
}

// TestContractsGenerate_FailureLeavesArtifactsUntouched asserts that a failed
// generation (an invalid manifest) never overwrites a previously good committed
// artifact — the atomicity invariant.
func TestContractsGenerate_FailureLeavesArtifactsUntouched(t *testing.T) {
	dir := writeProject(t)
	if _, err := ContractsGenerate(dir, testLabel); err != nil {
		t.Fatalf("initial ContractsGenerate: %v", err)
	}
	goldGo, err := os.ReadFile(filepath.Join(dir, contractsGoPath))
	if err != nil {
		t.Fatalf("read good Go twin: %v", err)
	}
	// Corrupt the source with an unsupported protocol version, then regenerate.
	bad := `{"protocolVersion": 99, "name": "go.putnami.dev/example/payments"}`
	if err := os.WriteFile(filepath.Join(dir, ContractsSourcePath), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad source: %v", err)
	}
	if _, err := ContractsGenerate(dir, testLabel); err == nil {
		t.Fatal("ContractsGenerate on an invalid manifest should fail")
	} else if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Fatalf("invalid-manifest exit code = %d, want %d", protocolcli.ExitCodeForError(err), protocolcli.ExitUsage)
	}
	stillGo, err := os.ReadFile(filepath.Join(dir, contractsGoPath))
	if err != nil {
		t.Fatalf("read Go twin after failed generate: %v", err)
	}
	if string(stillGo) != string(goldGo) {
		t.Fatal("failed generation overwrote the committed Go twin")
	}
}

// TestContractsCheck_Clean asserts a freshly generated project with an
// unchanged prior reports clean and exits 0.
func TestContractsCheck_Clean(t *testing.T) {
	dir := writeProject(t)
	if _, err := ContractsGenerate(dir, testLabel); err != nil {
		t.Fatalf("ContractsGenerate: %v", err)
	}
	report, err := ContractsCheck(dir, testLabel, staticPrior(baseManifest))
	if err != nil {
		t.Fatalf("ContractsCheck clean returned error: %v", err)
	}
	if report.Outcome != OutcomeClean {
		t.Fatalf("outcome = %q, want %q", report.Outcome, OutcomeClean)
	}
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitSuccess {
		t.Fatalf("clean exit code = %d, want 0", protocolcli.ExitCodeForError(err))
	}
	assertGoldenReport(t, "check_clean.json", report)
}

// TestContractsCheck_Drift asserts hand-edited generated documentation reports
// drift and exits 2, with the report carried on the error for the failure envelope.
func TestContractsCheck_Drift(t *testing.T) {
	dir := writeProject(t)
	if _, err := ContractsGenerate(dir, testLabel); err != nil {
		t.Fatalf("ContractsGenerate: %v", err)
	}
	// Hand-edit the reference documentation so it no longer matches a fresh
	// projection of the canonical contract.
	if err := os.WriteFile(filepath.Join(dir, contractsMarkdownPath), []byte("# tampered\n"), 0o644); err != nil {
		t.Fatalf("tamper Markdown reference: %v", err)
	}
	report, err := ContractsCheck(dir, testLabel, staticPrior(baseManifest))
	if err == nil {
		t.Fatal("ContractsCheck should fail on drift")
	}
	if report.Outcome != OutcomeDrift {
		t.Fatalf("outcome = %q, want %q", report.Outcome, OutcomeDrift)
	}
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Fatalf("drift exit code = %d, want %d", protocolcli.ExitCodeForError(err), protocolcli.ExitUsage)
	}
	if data, ok := ResultData(err).(CheckReport); !ok || data.Outcome != OutcomeDrift {
		t.Fatalf("drift error does not carry the report as ResultData: %#v", ResultData(err))
	}
	assertGoldenReport(t, "check_drift.json", report)
}

// TestContractsCheck_Breaking asserts a prior with removed enum values, scopes,
// claims, and grants is classified as a breaking change and exits 2.
func TestContractsCheck_Breaking(t *testing.T) {
	dir := writeProject(t)
	if _, err := ContractsGenerate(dir, testLabel); err != nil {
		t.Fatalf("ContractsGenerate: %v", err)
	}
	report, err := ContractsCheck(dir, testLabel, staticPrior(priorManifest))
	if err == nil {
		t.Fatal("ContractsCheck should fail on a breaking change")
	}
	if report.Outcome != OutcomeBreaking {
		t.Fatalf("outcome = %q, want %q", report.Outcome, OutcomeBreaking)
	}
	if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
		t.Fatalf("breaking exit code = %d, want %d", protocolcli.ExitCodeForError(err), protocolcli.ExitUsage)
	}
	if len(report.BreakingChanges) != 4 {
		t.Fatalf("breaking changes = %d, want 4: %#v", len(report.BreakingChanges), report.BreakingChanges)
	}
	assertGoldenReport(t, "check_breaking.json", report)
}

// TestCompareManifests_RenamedEnumWire asserts an enum value whose wire value
// changes under the same constant name is classified as a rename (breaking).
func TestCompareManifests_RenamedEnumWire(t *testing.T) {
	prior := parseManifest(t, baseManifest)
	renamed := `{
  "protocolVersion": 1,
  "name": "go.putnami.dev/example/payments",
  "enums": [
    {
      "name": "Currency",
      "values": [
        { "name": "USD", "value": "usdollar" },
        { "name": "EUR", "value": "eur" }
      ]
    }
  ],
  "scopes": [
    { "name": "payments:read" },
    { "name": "payments:write" }
  ],
  "capabilities": [
    { "name": "processPayments", "scopes": ["payments:write"] }
  ],
  "grants": [
    { "name": "paymentsAdmin", "capability": "processPayments" }
  ],
  "claims": [
    { "name": "sub", "type": "string", "required": true },
    { "name": "tenantId", "type": "string" }
  ]
}`
	current := parseManifest(t, renamed)
	changes := CompareManifests(prior, current)
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1: %#v", len(changes), changes)
	}
	c := changes[0]
	if c.Category != compatEnumValue || c.Kind != compatRenamed || c.Name != "Currency.USD" {
		t.Fatalf("change = %#v, want renamed enumValue Currency.USD", c)
	}
}

// TestCompareManifests_AdditionsAreCompatible asserts that adding an enum value,
// scope, claim, or grant is not a breaking change.
func TestCompareManifests_AdditionsAreCompatible(t *testing.T) {
	prior := parseManifest(t, baseManifest)
	current := parseManifest(t, priorManifest) // priorManifest is a superset of base
	if changes := CompareManifests(prior, current); len(changes) != 0 {
		t.Fatalf("additions should be compatible, got %#v", changes)
	}
}

// TestGitPriorManifest reads the committed HEAD manifest, not the working tree,
// so `check` compares against the last committed contract. It also returns
// (nil, nil) when there is no prior at HEAD.
func TestGitPriorManifest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")

	projRel := "svc"
	if err := os.MkdirAll(filepath.Join(repo, projRel, "schema"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := filepath.Join(repo, projRel, ContractsSourcePath)
	if err := os.WriteFile(src, []byte(priorManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "commit prior")

	// Mutate the working tree AFTER committing; the loader must yield the
	// committed (prior) bytes, not the working-tree edit.
	if err := os.WriteFile(src, []byte(baseManifest), 0o644); err != nil {
		t.Fatalf("mutate manifest: %v", err)
	}

	got, err := GitPriorManifest(repo, projRel)()
	if err != nil {
		t.Fatalf("GitPriorManifest: %v", err)
	}
	if string(got) != priorManifest {
		t.Fatalf("loader returned working-tree bytes, want committed prior:\n%s", got)
	}

	// A project with no committed manifest yields no prior (not an error).
	none, err := GitPriorManifest(repo, "missing")()
	if err != nil {
		t.Fatalf("GitPriorManifest(missing): %v", err)
	}
	if none != nil {
		t.Fatalf("expected no prior for an uncommitted path, got %q", none)
	}
}

// TestGitPriorManifest_GitUnrunnable asserts that when git cannot be executed at
// all (here: an empty PATH so the binary is not found), the loader surfaces an
// error rather than silently reporting "no prior". Otherwise a broken CI
// toolchain would pass the compatibility gate while hiding breaking changes.
func TestGitPriorManifest_GitUnrunnable(t *testing.T) {
	t.Setenv("PATH", "")
	got, err := GitPriorManifest(t.TempDir(), "svc")()
	if err == nil {
		t.Fatalf("expected an error when git cannot be run, got nil prior=%q (fail-open)", got)
	}
}

// assertGoldenReport compares the canonical JSON of report against the committed
// golden, regenerating with PUTNAMI_UPDATE_GOLDEN=1.
func assertGoldenReport(t *testing.T, name string, report CheckReport) {
	t.Helper()
	path := filepath.Join("testdata", "contracts", name)
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	got = append(got, '\n')
	if os.Getenv("PUTNAMI_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with PUTNAMI_UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch for %s\n--- got:\n%s\n--- want:\n%s", path, got, want)
	}
}
