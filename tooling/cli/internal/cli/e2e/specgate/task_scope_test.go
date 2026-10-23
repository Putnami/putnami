package specgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// taskScopeEmitScript copies the project's fixture for the artifact $1 to
// command output and announces it as the artifact $2, or removes a previous
// copy when the project states none.
const taskScopeEmitScript = `#!/bin/sh
source="$PUTNAMI_PROJECT_ROOT/$1.src.json"
if [ -f "$source" ]; then
  mkdir -p "$PUTNAMI_OUTPUT_PATH"
  cp "$source" "$PUTNAMI_OUTPUT_PATH/$1"
  printf '%s\n' "{\"v\":2,\"type\":\"artifact\",\"id\":\"$2\",\"name\":\"$2\",\"kind\":\"report\",\"path\":\"$PUTNAMI_OUTPUT_PATH/$1\"}"
else
  rm -f "$PUTNAMI_OUTPUT_PATH/$1"
fi
exit 0
`

// taskScopeFixture writes a workspace whose spec gate and tests come from two
// in-workspace extensions: gate-criteria serves `validate`, the criteria
// projection producer, and gate-tester serves `test`, the report producer.
// alpha consumes both under an enforced spec gate, and its own test attests
// its one requirement. No task reads gate-criteria/implementation.txt, so a
// change to it rebuilds gate-criteria and reaches alpha for gate-criteria's
// tasks only.
func taskScopeFixture(t *testing.T, wsRoot string) {
	t.Helper()
	clitest.WriteFile(t, filepath.Join(wsRoot, ".gitignore"), ".putnami/\n")
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"),
		`{"name":"task-scope-gate","includes":["gate-criteria","gate-tester","alpha"]}`)
	extension := func(dir, name, command, kind, activation, artifact, artifactID string) {
		clitest.WriteFile(t, filepath.Join(wsRoot, dir, "putnami.json"), `{"name":"`+name+`"}`)
		clitest.WriteFile(t, filepath.Join(wsRoot, dir, "emit.sh"), taskScopeEmitScript)
		if err := os.Chmod(filepath.Join(wsRoot, dir, "emit.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		clitest.WriteFile(t, filepath.Join(wsRoot, dir, "putnami.extension.json"), `{
  "name": "`+name+`",
  "version": "0.1.0",
  "cliContract": 11,
  "commands": {
    "`+command+`": {
      "activationFiles": ["`+activation+`"],
      "traits": {"kind": "`+kind+`"},
      "run": [{"id": "`+command+`", "task": "emit"}]
    }
  },
  "tasks": {
    "emit": {
      "kind": "command",
      "command": "{extensionRoot}/emit.sh",
      "args": ["`+artifact+`", "`+artifactID+`"],
      "cwd": "{projectRoot}",
      "timeoutMs": -1,
      "inputs": {"sources": {"from": "project", "files": ["*.src.json", "*_check.txt", "`+activation+`"]}},
      "cache": {"enabled": true},
      "declares": {"outputs": {"file": {"kind": "file", "root": "command-output", "path": "`+artifact+`", "optionalEmpty": true}}}
    }
  }
}`)
	}
	extension("gate-criteria", "@putnami/gate-criteria", "validate", "criteria", "gate.specs",
		features.SpecCriteriaProjectionFilename, "putnami-spec-criteria")
	clitest.WriteFile(t, filepath.Join(wsRoot, "gate-criteria", "implementation.txt"), "v1\n")
	extension("gate-tester", "@putnami/gate-tester", "test", "verification", "gate.project",
		features.VerificationReportFilename, "putnami-feature-verification")

	alpha := filepath.Join(wsRoot, "alpha")
	clitest.WriteFile(t, filepath.Join(alpha, "putnami.json"),
		`{"name":"alpha","extensions":["/gate-criteria","/gate-tester"],"options":{"sdd":{"verification":{"specs":"enforce"}}}}`)
	clitest.WriteFile(t, filepath.Join(alpha, "gate.project"), "")
	clitest.WriteFile(t, filepath.Join(alpha, "gate.specs"), "")
	clitest.WriteFile(t, filepath.Join(alpha, "alpha_check.txt"), "fixture declaration\n")
	clitest.WriteFile(t, filepath.Join(alpha, features.SpecCriteriaProjectionFilename+".src.json"), `{
  "protocolVersion": 1,
  "groups": [{
    "feature": "gate/alpha",
    "spec": "alpha/specs/alpha.json",
    "specRequirements": ["holds"],
    "requirements": [{
      "id": "holds", "stage": "coded", "evidenceKinds": ["attestation"],
      "verification": {"kind": "acceptance", "checks": ["alpha-check"]}
    }]
  }]
}`)
	setGateReport(t, wsRoot, "alpha", "passed")
	workspace.InvalidateLoadCache(wsRoot)
}

// A change to the extension that provides the spec gate reaches its consumer
// for that extension's tasks only, so the plan keeps the consumer's spec gate
// and drops its test. The gate then reads the test's report from the task's
// cache entry. Where no entry answers, it warns, names the consumer, and the
// run passes.
func TestSpecGateEvidenceScope_ATaskScopeKeepsTheSpecGateAndNotTheTest(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-kept-spec-gate-keeps-no-unscoped-test")
	wsRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	taskScopeFixture(t, wsRoot)
	clitest.InitGitRepo(t, wsRoot)
	clitest.RunGit(t, wsRoot, "checkout", "-b", "feature")

	// The trunk's run: alpha's test runs and publishes its entry.
	if code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--all"); code != cli.ExitSuccess {
		t.Fatalf("the warming session exited %d:\n%s", code, output)
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, "gate-criteria", "implementation.txt"), "v2\n")
	workspace.InvalidateLoadCache(wsRoot)

	t.Run("the owner's report resolves from its cache entry", func(t *testing.T) {
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--impacted", "--baseline", "main")
		if code != cli.ExitSuccess {
			t.Fatalf("the impacted run exited %d:\n%s", code, output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementVerified || group.Blocked {
			t.Fatalf("group = %+v, want holds verified from alpha's cached report", group)
		}
		// A restored reference is the record's statement that the test did not
		// run in this session.
		if len(group.Reports) != 1 || !group.Reports[0].Restored || group.Reports[0].Project != "/alpha" {
			t.Fatalf("report refs = %+v, want one restored reference to /alpha: the plan ran its test", group.Reports)
		}
	})

	t.Run("a run that serves no cached report warns and does not block", func(t *testing.T) {
		code, output := clitest.RunGateArgs(t, wsRoot, "validate,test", "--impacted", "--baseline", "main", "--no-cache")
		if code != cli.ExitSuccess {
			t.Fatalf("an unresolvable report sanctioned the run (exit %d):\n%s", code, output)
		}
		if !strings.Contains(output, "/alpha") || !strings.Contains(output, "--no-cache") {
			t.Errorf("the warning does not name the project and the reason:\n%s", output)
		}
		record, _ := latestGateRecord(t, wsRoot)
		group := gateState(t, record, "gate/alpha")
		if group.Requirements[0].State != features.RequirementUnobserved || group.Blocked || len(group.Reports) != 0 {
			t.Fatalf("group = %+v, want holds unobserved, unblocked, with no report: the plan ran no test", group)
		}
	})
}
