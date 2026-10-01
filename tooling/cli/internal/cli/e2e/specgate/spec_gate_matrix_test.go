package specgate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The midpoint matrix, end to end: a synthetic workspace whose fixture
// extension emits the criteria projection and observation report artifacts
// from plain shell tasks — no language runner anywhere — driven through the
// real CLI (App.Run → Engine.Run → scheduler → finalizer → reducer), so what
// is proven is the exit code a user sees and the record a session keeps.
//
// Watch iterations and MCP run_jobs replan through the same Engine.Run these
// runs take (engine/watch.go binds watch.RunIteration; internal/mcp's
// run_jobs is an adapter over Engine.Run), so the tri-state
// proven here is theirs by construction; the finalizer's own attachment
// conditions are pinned unit-side in engine/spec_gate_test.go.

// gateMatrixFixture writes the synthetic workspace: two spec-owning projects
// (alpha, beta) whose validate command emits a criteria projection with one
// executable requirement each, and whose test command emits the observation
// report stated in the project's report.src.json.
func gateMatrixFixture(t *testing.T, wsRoot string) {
	t.Helper()
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "gate-fixture-ws",
  "includes": ["gate-extension", "alpha", "beta"]
}`)

	extDir := filepath.Join(wsRoot, "gate-extension")
	clitest.WriteFile(t, filepath.Join(extDir, "putnami.json"), `{"name":"@putnami/gate-fixture"}`)
	clitest.WriteFile(t, filepath.Join(extDir, "emit.sh"), `#!/bin/sh
# $1 names the fixture source and the artifact to publish. The artifact is
# emitted only when the project states one, mirroring optionalEmpty; otherwise
# a previous run's copy is removed, because the task owns that path and the
# capture records whatever sits there.
source="$PUTNAMI_PROJECT_ROOT/$1.src.json"
if [ -f "$source" ]; then
  mkdir -p "$PUTNAMI_OUTPUT_PATH"
  cp "$source" "$PUTNAMI_OUTPUT_PATH/$1"
  printf '%s\n' "{\"v\":2,\"type\":\"artifact\",\"id\":\"$2\",\"name\":\"$2\",\"kind\":\"report\",\"path\":\"$PUTNAMI_OUTPUT_PATH/$1\"}"
else
  rm -f "$PUTNAMI_OUTPUT_PATH/$1"
fi
if [ -f "$PUTNAMI_PROJECT_ROOT/fail.marker" ] && [ "$2" = "putnami-feature-verification" ]; then
  exit 1
fi
exit 0
`)
	if err := os.Chmod(filepath.Join(extDir, "emit.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(extDir, "putnami.extension.json"), `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/gate-fixture",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "validate": {
      "description": "Emit the criteria projection",
      "activationFiles": ["gate.project"],
      "run": [{"id": "specs", "task": "criteria-exec"}]
    },
    "test": {
      "description": "Emit the observation report",
      "activationFiles": ["gate.project"],
      "run": [{"id": "test", "task": "test-exec"}]
    }
  },
  "tasks": {
    "criteria-exec": {
      "description": "Copy the project's projection fixture to command output.",
      "kind": "command",
      "command": "{extensionRoot}/emit.sh",
      "args": ["spec-criteria.json", "putnami-spec-criteria"],
      "cwd": "{projectRoot}",
      "timeoutMs": 60000,
      "inputs": {"sources": {"from": "project", "files": ["*.json", "*.txt", "*.marker", "gate.project"]}},
      "cache": {"enabled": true},
      "declares": {"outputs": {"criteria": {"kind": "file", "root": "command-output", "path": "spec-criteria.json", "optionalEmpty": true}}}
    },
    "test-exec": {
      "description": "Copy the project's report fixture to command output.",
      "kind": "command",
      "command": "{extensionRoot}/emit.sh",
      "args": ["putnami-feature-verification.json", "putnami-feature-verification"],
      "cwd": "{projectRoot}",
      "timeoutMs": 60000,
      "inputs": {"sources": {"from": "project", "files": ["*.json", "*.txt", "*.marker", "gate.project"]}},
      "cache": {"enabled": true},
      "declares": {"outputs": {"report": {"kind": "file", "root": "command-output", "path": "putnami-feature-verification.json", "optionalEmpty": true}}}
    }
  }
}`)

	for _, project := range []string{"alpha", "beta"} {
		dir := filepath.Join(wsRoot, project)
		clitest.WriteFile(t, filepath.Join(dir, "putnami.json"),
			`{"name":"`+project+`","extensions":["@putnami/gate-fixture"]}`)
		clitest.WriteFile(t, filepath.Join(dir, "gate.project"), "")
		clitest.WriteFile(t, filepath.Join(dir, project+"_check.txt"), "fixture declaration\n")
		clitest.WriteFile(t, filepath.Join(dir, "spec-criteria.json.src.json"), `{
  "protocolVersion": 1,
  "groups": [{
    "feature": "gate/`+project+`",
    "spec": "`+project+`/specs/`+project+`.json",
    "specRequirements": ["holds"],
    "requirements": [{
      "id": "holds", "stage": "coded", "evidenceKinds": ["attestation"],
      "verification": {"kind": "acceptance", "checks": ["`+project+`-check"]}
    }]
  }]
}`)
	}
}

// setGateReport states what one project's test run observes; empty status
// removes the report entirely (the check goes missing).
func setGateReport(t *testing.T, wsRoot, project, status string) {
	t.Helper()
	path := filepath.Join(wsRoot, project, "putnami-feature-verification.json.src.json")
	if status == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return
	}
	clitest.WriteFile(t, path, `{
  "protocolVersion": 1,
  "observations": [{
    "feature": "gate/`+project+`",
    "requirement": "holds",
    "check": "`+project+`-check",
    "status": "`+status+`",
    "provenance": {"path": "`+project+`_check.txt", "symbol": "FixtureCheck"}
  }]
}`)
}

func setGatePolicy(t *testing.T, wsRoot, project, mode string) {
	t.Helper()
	options := ""
	if mode != "" {
		options = `,"options":{"sdd":{"verification":{"specs":"` + mode + `"}}}`
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, project, "putnami.json"),
		`{"name":"`+project+`","extensions":["@putnami/gate-fixture"]`+options+`}`)
	// Every real CLI invocation is a fresh process; these scenarios run
	// several invocations in one test process, so the load memoization must
	// be dropped for a config rewrite to be seen (workspace.Load's documented
	// test contract).
	workspace.InvalidateLoadCache(wsRoot)
}

// runGateSession drives `putnami validate,test --all` through the real CLI.
func runGateSession(t *testing.T, wsRoot string) (int, string) {
	t.Helper()
	return clitest.RunGateArgs(t, wsRoot, "validate,test", "--all")
}

func latestGateRecord(t *testing.T, wsRoot string) (*features.SpecVerificationRecord, string) {
	t.Helper()
	store := workspace_state.NewSessionStore(wsRoot)
	target, err := os.Readlink(filepath.Join(wsRoot, ".putnami", "sessions", "latest"))
	if err != nil {
		t.Fatalf("no latest session: %v", err)
	}
	sessionID := filepath.Base(target)
	record, found, err := store.ReadSpecVerification(sessionID)
	if err != nil || !found {
		t.Fatalf("latest session %s carries no record (found %v, err %v)", sessionID, found, err)
	}
	return record, sessionID
}

// optionalLatestGateRecord is latestGateRecord for the one case where absence
// is a legal answer: a session that projected no criteria never ran the gate,
// and the persistence seam contracts a missing record as exactly that.
func optionalLatestGateRecord(t *testing.T, wsRoot string) (*features.SpecVerificationRecord, bool) {
	t.Helper()
	store := workspace_state.NewSessionStore(wsRoot)
	target, err := os.Readlink(filepath.Join(wsRoot, ".putnami", "sessions", "latest"))
	if err != nil {
		t.Fatalf("no latest session: %v", err)
	}
	record, found, err := store.ReadSpecVerification(filepath.Base(target))
	if err != nil {
		t.Fatalf("reading the latest session's record: %v", err)
	}
	return record, found
}

func gateState(t *testing.T, record *features.SpecVerificationRecord, feature string) features.SpecVerificationGroup {
	t.Helper()
	for _, group := range record.Groups {
		if group.Feature == feature {
			return group
		}
	}
	t.Fatalf("feature %s missing from record: %+v", feature, record.Groups)
	return features.SpecVerificationGroup{}
}

func TestSpecGateMatrix_EndToEnd(t *testing.T) {
	clitest.RequireShell(t)

	newWorkspace := func(t *testing.T) string {
		wsRoot := t.TempDir()
		gateMatrixFixture(t, wsRoot)
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
		return wsRoot
	}

	t.Run("default report never changes a clean exit", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		// No reports at all: both requirements resolve missing under the
		// built-in report default, and the run stays green — the midpoint's
		// "default report introduces no new failing exit anywhere".
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d, want success under the default report mode\n%s", code, output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		alpha := gateState(t, record, "gate/alpha")
		if alpha.Mode != features.VerificationModeReport || alpha.Blocked ||
			alpha.Requirements[0].State != features.RequirementMissing {
			t.Fatalf("alpha group = %+v", alpha)
		}
	})

	t.Run("enforce blocks exactly the failing project", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		setGateReport(t, wsRoot, "alpha", "passed")
		setGateReport(t, wsRoot, "beta", "failed")

		// Mixed policy, everything green for the enforce project: success.
		code, _ := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; a verified enforce project plus a failing report project must pass", code)
		}
		record, _ := latestGateRecord(t, wsRoot)
		if gateState(t, record, "gate/alpha").Requirements[0].State != features.RequirementVerified {
			t.Fatal("alpha did not verify")
		}
		beta := gateState(t, record, "gate/beta")
		if beta.Requirements[0].State != features.RequirementContradicted || beta.Blocked {
			t.Fatalf("beta group = %+v; report mode must record the contradiction without blocking", beta)
		}

		// Delete alpha's protecting observation: the enforce project now blocks
		// the run through the canonical reducer.
		setGateReport(t, wsRoot, "alpha", "")
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want %d once the enforce project's check goes missing", code, cli.ExitError)
		}
		if !strings.Contains(output, "specs~verify") || !strings.Contains(output, "spec verification") {
			t.Fatalf("output does not surface the synthetic sanction:\n%s", output)
		}
		record, _ = latestGateRecord(t, wsRoot)
		if !gateState(t, record, "gate/alpha").Blocked {
			t.Fatal("the record does not carry alpha's blocked decision")
		}
		if gateState(t, record, "gate/beta").Blocked {
			t.Fatal("report-mode beta blocked")
		}
	})

	t.Run("off attaches nothing", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "gate-fixture-ws",
  "includes": ["gate-extension", "alpha", "beta"],
  "options": {"sdd": {"verification": {"specs": "off"}}}
}`)
		code, _ := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d under off", code)
		}
		sessionsDir := filepath.Join(wsRoot, ".putnami", "sessions")
		target, err := os.Readlink(filepath.Join(sessionsDir, "latest"))
		if err != nil {
			t.Fatalf("no session recorded: %v", err)
		}
		recordPath := filepath.Join(sessionsDir, filepath.Base(target), features.SpecVerificationRecordFilename)
		if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
			t.Fatalf("off still persisted a verification record (stat err = %v)", err)
		}
	})

	t.Run("invalid policy fails before jobs execute", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "on")
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitUsage {
			t.Fatalf("exit = %d, want %d for invalid committed policy", code, cli.ExitUsage)
		}
		if !strings.Contains(output, "options.sdd.verification") {
			t.Fatalf("refusal does not name the policy key:\n%s", output)
		}
	})

	t.Run("failed tests gain no secondary sanction", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		// alpha's test task fails outright AND its check never reports: the
		// original failure is authoritative, and no missing-report sanction
		// stacks on top.
		clitest.WriteFile(t, filepath.Join(wsRoot, "alpha", "fail.marker"), "")
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want the test failure's own %d", code, cli.ExitError)
		}
		if strings.Contains(output, "specs~verify") {
			t.Fatalf("an incomplete session acquired a secondary sanction:\n%s", output)
		}
		// Whatever the audit surface records, it may not claim an evaluation
		// that did not happen. Fail-fast may or may not have canceled the
		// sibling validate tasks, so this session either projected criteria —
		// in which case the record persists and carries their groups — or
		// projected none, in which case there is no record at all. The one
		// state ruled out is the third: a persisted record with zero groups,
		// which `specs verify` replays as "every enforced feature was
		// evaluated and observed nothing" and blocks the whole workspace on.
		record, found := optionalLatestGateRecord(t, wsRoot)
		if found && len(record.Groups) == 0 {
			t.Fatal("a session that evaluated nothing persisted a record claiming an empty evaluation")
		}
	})

	t.Run("cache hits restore the gate byte for byte", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		setGateReport(t, wsRoot, "alpha", "passed")
		setGateReport(t, wsRoot, "beta", "passed")
		code, _ := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatal("cold run failed")
		}
		cold, _ := latestGateRecord(t, wsRoot)

		// The warm run executes nothing: the projections and reports are
		// restored declared outputs and replayed artifact events, and the gate
		// must decide identically — a hit may not silently lose verification.
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("warm run failed:\n%s", output)
		}
		if !strings.Contains(output, "cached") {
			t.Fatalf("warm run reports no cache reuse:\n%s", output)
		}
		warm, _ := latestGateRecord(t, wsRoot)
		coldGroups, err := json.Marshal(cold.Groups)
		if err != nil {
			t.Fatal(err)
		}
		warmGroups, err := json.Marshal(warm.Groups)
		if err != nil {
			t.Fatal(err)
		}
		if string(coldGroups) != string(warmGroups) {
			t.Fatalf("warm verdict differs from cold:\n%s\n---\n%s", coldGroups, warmGroups)
		}
		if gateState(t, warm, "gate/alpha").Requirements[0].State != features.RequirementVerified {
			t.Fatal("the warm session lost the verification")
		}
	})
}

// --- thresholds --------------------------------------------------------------

// gateRollingWindowSeconds is the ONE rolling span of the threshold fixture:
// the criterion declares it and every measurement's observed span covers
// exactly it, so the freshness sub-tests fail for age or environment reasons
// — never because the two numbers drifted apart.
const gateRollingWindowSeconds = 2592000

// setGateThresholdCriteria states one project's projection as a single
// threshold requirement. An invocation KPI stays on stage coded; a rolling
// SLO must sit on live-verified with an environment and a freshness bound,
// exactly the closed criterion shape an earlier change landed.
func setGateThresholdCriteria(t *testing.T, wsRoot, project, kind string) {
	t.Helper()
	criterion := `{
      "kind": "threshold", "checks": ["` + project + `-benchmark"],
      "metric": "` + project + `.flush.duration", "aggregation": "p95",
      "operator": "lte", "target": 25, "unit": "ms",
      "window": {"kind": "invocation"}
    }`
	stage := "coded"
	if kind == "rolling" {
		stage = "live-verified"
		criterion = `{
      "kind": "threshold", "checks": ["` + project + `-benchmark"],
      "metric": "` + project + `.delivery.success", "aggregation": "ratio",
      "operator": "gte", "target": 0.999, "unit": "ratio",
      "window": {"kind": "rolling", "seconds": ` + strconv.Itoa(gateRollingWindowSeconds) + `},
      "environment": "production", "maxAgeSeconds": 3600
    }`
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, project, "spec-criteria.json.src.json"), `{
  "protocolVersion": 1,
  "groups": [{
    "feature": "gate/`+project+`",
    "spec": "`+project+`/specs/`+project+`.json",
    "specRequirements": ["holds"],
    "requirements": [{
      "id": "holds", "stage": "`+stage+`", "evidenceKinds": ["attestation"],
      "verification": `+criterion+`
    }]
  }]
}`)
}

// setGateMeasurement states what one project's test run measured. The
// window is stated relative to now so rolling freshness is a property of
// the scenario, not of when the suite happens to run; its span covers the
// longest declared rolling duration, because an observation that covers
// less than the criterion's window is stale by rule, not fresh.
func setGateMeasurement(t *testing.T, wsRoot, project string, metric, aggregation string, value float64, unit string, windowEnd time.Time, environment, extra string) {
	t.Helper()
	start := windowEnd.Add(-gateRollingWindowSeconds * time.Second)
	envField := ""
	if environment != "" {
		envField = `,
    "environment": "` + environment + `"`
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, project, "putnami-feature-verification.json.src.json"), `{
  "protocolVersion": 1,
  "observations": [{
    "feature": "gate/`+project+`",
    "requirement": "holds",
    "check": "`+project+`-benchmark",`+extra+`
    "measurement": {"name": "`+metric+`", "aggregation": "`+aggregation+`", "value": `+strconv.FormatFloat(value, 'f', -1, 64)+`, "unit": "`+unit+`"},
    "window": {"start": "`+start.UTC().Format(time.RFC3339)+`", "end": "`+windowEnd.UTC().Format(time.RFC3339)+`"}`+envField+`,
    "provenance": {"path": "`+project+`_check.txt", "symbol": "FixtureBenchmark"}
  }]
}`)
}

// TestSpecGateMatrix_Thresholds is the synthetic KPI/SLO matrix: the
// verdict is always recomputed from the authored target, boundary equality
// included; rolling objectives demand a fresh window in the declared
// environment; and a producer stating both a measurement and its own verdict
// is refused by the strict wire, which fails closed under enforce. No product
// SLO exists anywhere in these fixtures.
func TestSpecGateMatrix_Thresholds(t *testing.T) {
	clitest.RequireShell(t)

	newWorkspace := func(t *testing.T) string {
		wsRoot := t.TempDir()
		gateMatrixFixture(t, wsRoot)
		// Beta plays no part in the threshold scenarios: no report, default
		// report mode, so it never affects the exit.
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
		return wsRoot
	}
	now := time.Now().UTC()

	t.Run("invocation KPI is recomputed from the authored target", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		setGateThresholdCriteria(t, wsRoot, "alpha", "invocation")

		// Met, strictly under target.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.flush.duration", "p95", 12.5, "ms", now, "", "")
		code, _ := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d, want success for a met KPI", code)
		}
		record, _ := latestGateRecord(t, wsRoot)
		alpha := gateState(t, record, "gate/alpha")
		if alpha.Requirements[0].State != features.RequirementVerified {
			t.Fatalf("alpha = %+v", alpha.Requirements[0])
		}
		if checks := alpha.Requirements[0].Checks; len(checks) != 1 || checks[0].Measurement == nil ||
			checks[0].Measurement.Value != 12.5 {
			t.Fatalf("measured aggregate lost from the record: %+v", alpha.Requirements[0].Checks)
		}

		// Boundary: lte is inclusive, so exactly the target still satisfies.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.flush.duration", "p95", 25, "ms", now, "", "")
		if code, _ := runGateSession(t, wsRoot); code != cli.ExitSuccess {
			t.Fatalf("exit = %d, want success at the inclusive boundary", code)
		}

		// Missed: the measured value exceeds the authored target, and only
		// enforce turns that into an exit.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.flush.duration", "p95", 30, "ms", now, "", "")
		code, output := runGateSession(t, wsRoot)
		if code != cli.ExitError || !strings.Contains(output, "specs~verify") {
			t.Fatalf("exit = %d, want the synthetic sanction for a missed KPI:\n%s", code, output)
		}
		setGatePolicy(t, wsRoot, "alpha", "report")
		if code, output := runGateSession(t, wsRoot); code != cli.ExitSuccess {
			record, _ := latestGateRecord(t, wsRoot)
			group := gateState(t, record, "gate/alpha")
			t.Fatalf("exit = %d; report must record the miss without blocking (mode %s from %s):\n%s",
				code, group.Mode, group.ModeSource, output)
		}
		record, _ = latestGateRecord(t, wsRoot)
		if gateState(t, record, "gate/alpha").Requirements[0].State != features.RequirementContradicted {
			t.Fatal("the missed KPI was not recorded as contradicted")
		}
	})

	t.Run("rolling SLO demands freshness and the declared environment", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		setGateThresholdCriteria(t, wsRoot, "alpha", "rolling")

		// Fresh, in production, above target: verified.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.delivery.success", "ratio", 0.9995, "ratio", now, "production", "")
		code, _ := runGateSession(t, wsRoot)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d, want success for a fresh in-environment SLO", code)
		}
		record, _ := latestGateRecord(t, wsRoot)
		if gateState(t, record, "gate/alpha").Requirements[0].State != features.RequirementVerified {
			t.Fatal("fresh SLO did not verify")
		}

		// Same value, but the window ended beyond maxAgeSeconds: stale blocks.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.delivery.success", "ratio", 0.9995, "ratio", now.Add(-2*time.Hour), "production", "")
		code, _ = runGateSession(t, wsRoot)
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want the stale observation to block enforce", code)
		}
		record, _ = latestGateRecord(t, wsRoot)
		if gateState(t, record, "gate/alpha").Requirements[0].State != features.RequirementStale {
			t.Fatalf("state = %+v, want stale", gateState(t, record, "gate/alpha").Requirements[0])
		}

		// Fresh but measured in the wrong environment: never counts as support.
		setGateMeasurement(t, wsRoot, "alpha", "alpha.delivery.success", "ratio", 0.9995, "ratio", now, "staging", "")
		code, _ = runGateSession(t, wsRoot)
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want the out-of-environment observation to block enforce", code)
		}
	})

	t.Run("a producer cannot state a measurement and its own verdict", func(t *testing.T) {
		wsRoot := newWorkspace(t)
		setGatePolicy(t, wsRoot, "alpha", "enforce")
		setGateThresholdCriteria(t, wsRoot, "alpha", "invocation")
		setGateMeasurement(t, wsRoot, "alpha", "alpha.flush.duration", "p95", 12.5, "ms", now, "", `
    "status": "passed",`)
		code, output := runGateSession(t, wsRoot)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("self-certifying measurement session output:\n%s", output)
			}
		})
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want the self-certifying report refused and enforce failing closed", code)
		}
		record, _ := latestGateRecord(t, wsRoot)
		if !gateState(t, record, "gate/alpha").Blocked {
			t.Fatal("the self-certifying report did not block the enforce group")
		}
	})
}

// --- evidence attested by a project the selection did not plan -------

// attesterFixture extends the matrix workspace with `library`: a project alpha
// depends on, which owns no spec but whose test run reports the observation
// that attests alpha's requirement.
//
// That is the shape the issue describes and the one a narrowed run breaks:
// `spectest.Proves` calls live where the protected code lives, so a workload's
// requirement is routinely attested by a library's suite, and a selection that
// takes the workload without the library plans no test for the library at all.
func attesterFixture(t *testing.T, wsRoot string) {
	t.Helper()
	gateMatrixFixture(t, wsRoot)
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{
  "name": "gate-fixture-ws",
  "includes": ["gate-extension", "alpha", "beta", "library"]
}`)
	// alpha declares the dependency; the gate's candidate set is exactly this
	// edge's transitive closure, so without it nothing may be consulted.
	clitest.WriteFile(t, filepath.Join(wsRoot, "alpha", "putnami.json"),
		`{"name":"alpha","extensions":["@putnami/gate-fixture"],"dependencies":["library"],`+
			`"options":{"sdd":{"verification":{"specs":"enforce"}}}}`)

	dir := filepath.Join(wsRoot, "library")
	clitest.WriteFile(t, filepath.Join(dir, "putnami.json"),
		`{"name":"library","extensions":["@putnami/gate-fixture"]}`)
	clitest.WriteFile(t, filepath.Join(dir, "gate.project"), "")
	// The declaration the observation names. Provenance containment resolves it
	// against the REPORTING project's root, so it has to be a real file here.
	clitest.WriteFile(t, filepath.Join(dir, "library_check.txt"), "fixture declaration\n")
	clitest.WriteFile(t, filepath.Join(dir, "putnami-feature-verification.json.src.json"), `{
  "protocolVersion": 1,
  "observations": [{
    "feature": "gate/alpha",
    "requirement": "holds",
    "check": "alpha-check",
    "status": "passed",
    "provenance": {"path": "library_check.txt", "symbol": "FixtureCheck"}
  }]
}`)
	// alpha itself proves nothing: its only attesting evidence is the library's.
	setGateReport(t, wsRoot, "alpha", "")
	withGeneratePrerequisite(t, wsRoot)
	workspace.InvalidateLoadCache(wsRoot)
}

// withGeneratePrerequisite gives the fixture's `test` command the shape every
// real language extension has: a `generate` step that waits on its
// dependencies' `generate` (`^generate`) before the step that writes the report.
// A narrowed run then plans `library:test~generate` as a prerequisite of alpha —
// a `test` job for the library that never runs its tests. The step is
// deliberately uncacheable, so a lookup that counted it as a report producer
// would read the library as unreachable; the silent-attester subtest pins that
// it does not.
func withGeneratePrerequisite(t *testing.T, wsRoot string) {
	t.Helper()
	path := filepath.Join(wsRoot, "gate-extension", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, edit := range [][2]string{
		{`"run": [{"id": "test", "task": "test-exec"}]`,
			`"run": [{"id": "generate", "task": "generate-exec", "dependsOn": ["^generate"]}, ` +
				`{"id": "test", "task": "test-exec", "dependsOn": ["generate"]}]`},
		{`"tasks": {`, `"tasks": {
    "generate-exec": {
      "description": "Stand in for a prerequisite generation step; writes nothing.",
      "kind": "command",
      "command": "{extensionRoot}/emit.sh",
      "args": ["generated", "generated"],
      "cwd": "{projectRoot}",
      "timeoutMs": 60000
    },`},
	} {
		if strings.Count(manifest, edit[0]) != 1 {
			t.Fatalf("the fixture manifest no longer contains %q exactly once", edit[0])
		}
		manifest = strings.Replace(manifest, edit[0], edit[1], 1)
	}
	clitest.WriteFile(t, path, manifest)
}

// TestSpecGateEvidenceScope_EndToEnd drives evidence scope through the real CLI
// (App.Run → Engine.Run → scheduler → finalizer → reducer) over the fixture
// extension above.
//
// It is a HARNESS, not the primary evidence. The workspace's shell tasks stand
// in for the language test runners, which is what lets it stage the states this
// repository cannot produce on demand: an attester that reports a check FAILED,
// and a `--no-cache` run with nothing in the store. The production-representative
// proof is the repository itself — `test,validate --projects
// go.putnami.dev/protocol/architecture`, whose requirement is attested by
// @putnami/sdd's real suite — and it is recorded in
// tooling/cli/doc/adr/0026-spec-gate-evidence-scope.md.
func TestSpecGateEvidenceScope_EndToEnd(t *testing.T) {
	clitest.RequireShell(t)
	wsRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	attesterFixture(t, wsRoot)

	// 1. One full session: the library's test runs and publishes its entry.
	if code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--all"); code != cli.ExitSuccess {
		t.Fatalf("the warming session exited %d:\n%s", code, output)
	}

	t.Run("an unplanned attester resolves from its cache entry", func(t *testing.T) {
		spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
			"an-unplanned-attester-resolves-from-its-cache-entry")
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--projects", "alpha")
		if code != cli.ExitSuccess {
			t.Fatalf("a narrowed run exited %d; the attesting library's cached observation did not resolve it:\n%s",
				code, output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementVerified {
			t.Fatalf("holds = %s, want verified from the library's cached report; group = %+v",
				group.Requirements[0].State, group)
		}
		if group.Blocked {
			t.Error("a requirement verified from cache still blocked the run")
		}
		// The reference stays honest: it names the library and says the bytes
		// were restored rather than produced by a task of this session.
		if len(group.Reports) != 1 || !group.Reports[0].Restored ||
			group.Reports[0].Project != "/library" {
			t.Fatalf("report refs = %+v, want one restored reference to /library", group.Reports)
		}
	})

	t.Run("unresolvable evidence warns and does not block", func(t *testing.T) {
		spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
			"unresolvable-evidence-warns-with-the-attesting-projects-named")
		// --no-cache serves no stored byte, so the library cannot be consulted
		// at all. That is not a regression and must not sanction the run.
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--projects", "alpha", "--no-cache")
		if code != cli.ExitSuccess {
			t.Fatalf("unresolvable evidence sanctioned the run (exit %d):\n%s", code, output)
		}
		if !strings.Contains(output, "/library") || !strings.Contains(output, "--no-cache") {
			t.Fatalf("the warning does not name the attesting project and the reason:\n%s", output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementUnobserved {
			t.Fatalf("holds = %s, want unobserved", group.Requirements[0].State)
		}
		if group.Blocked || group.Counts.Unobserved != 1 {
			t.Fatalf("group = %+v, want an unblocked group counting one unobserved requirement", group)
		}
	})

	t.Run("a contradicting attester still blocks the selection that excluded it", func(t *testing.T) {
		spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
			"a-consulted-source-that-did-not-support-the-check-still-blocks")
		// The library now reports the check FAILED. Its entry is a new key, so
		// the narrowed run has to re-run the library... it is not selected, so
		// it cannot: the previous entry is gone from this key and nothing
		// answers. Warm the new entry first, exactly as a real branch would.
		clitest.WriteFile(t, filepath.Join(wsRoot, "library", "putnami-feature-verification.json.src.json"), `{
  "protocolVersion": 1,
  "observations": [{
    "feature": "gate/alpha",
    "requirement": "holds",
    "check": "alpha-check",
    "status": "failed",
    "provenance": {"path": "library_check.txt", "symbol": "FixtureCheck"}
  }]
}`)
		workspace.InvalidateLoadCache(wsRoot)
		if code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--all"); code != cli.ExitError {
			t.Fatalf("the whole-workspace run exited %d, want the contradiction's %d:\n%s",
				code, cli.ExitError, output)
		}
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--projects", "alpha")
		if code != cli.ExitError {
			t.Fatalf("a restored contradiction did not block the narrowed run (exit %d):\n%s", code, output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementContradicted || !group.Blocked {
			t.Fatalf("group = %+v, want a blocked contradiction", group)
		}
	})

	t.Run("a silent attester behind an uncacheable prerequisite still blocks", func(t *testing.T) {
		spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
			"an-exhausted-evidence-scope-blocks-and-says-so")
		// The library's suite now reports nothing. Its test~test entry records the
		// report empty, which is an answer; its test~generate has no entry at all
		// because it is not cacheable. Only the report producer decides whether
		// the library was reached, so the prerequisite must not turn that
		// answer into "unconsulted" and excuse the gap.
		if err := os.Remove(filepath.Join(wsRoot, "library", "putnami-feature-verification.json.src.json")); err != nil {
			t.Fatal(err)
		}
		workspace.InvalidateLoadCache(wsRoot)
		if code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--all"); code != cli.ExitError {
			t.Fatalf("the whole-workspace run exited %d, want the missing check's %d:\n%s",
				code, cli.ExitError, output)
		}
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--projects", "alpha")
		if code != cli.ExitError {
			t.Fatalf("a consulted-but-silent attester was excused (exit %d):\n%s", code, output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementMissing || !group.Blocked {
			t.Fatalf("group = %+v, want a blocked missing requirement", group)
		}
		exhausted := false
		for _, finding := range group.Diagnostics {
			exhausted = exhausted || (finding.Code == features.WarningCodeUnobservedRequirement &&
				strings.Contains(finding.Message, "evidence scope is exhausted"))
		}
		if !exhausted {
			t.Errorf("the group does not say its evidence scope was exhausted: %+v", group.Diagnostics)
		}
	})
}
