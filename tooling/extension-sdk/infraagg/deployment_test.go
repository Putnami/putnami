package infraagg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/jsonl"
)

// pinnedAggregate is what build~infra writes for closureContext(root, "app",
// "app", "application", "lib") with dbManifest in lib, no runtime file and the
// noHTTP2 hook. pinnedDefaultsSidecar is the defaults sidecar it writes beside
// it. Both are pinned so a change to how the aggregate is computed cannot move
// the build artifact.
const pinnedAggregate = `{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "workload": "app",
  "databases": [
    {
      "name": "primary",
      "engine": "postgres",
      "schemas": [
        "iam"
      ],
      "sources": [
        {
          "project": "lib",
          "contributor": "framework:requirements"
        }
      ]
    }
  ],
  "runtime": {
    "ingress": {
      "public": false
    },
    "scaling": {
      "max": 1,
      "concurrency": 500
    },
    "protocols": {
      "http2": false
    }
  }
}
`

const pinnedDefaultsSidecar = `{
  "ingress": {
    "public": false
  },
  "scaling": {
    "max": 1,
    "concurrency": 500
  },
  "protocols": {
    "http2": false
  }
}
`

// pinnedDeployment is the deployment declaration of the same inputs.
const pinnedDeployment = `{"protocolVersion":2,"workload":"app",` +
	`"databases":[{"name":"primary","engine":"postgres","schemas":["iam"],"sources":[{"project":"lib","contributor":"framework:requirements"}]}],` +
	`"runtime":{"ingress":{"public":false},"scaling":{"max":1,"concurrency":500},"protocols":{"http2":false}}}`

// defaultsSidecarPath is the runtime defaults sidecar of the fixture's
// workload, the file only build-infra writes.
func defaultsSidecarPath(root string) string {
	return filepath.Join(root, "app", ".gen", "infra", "runtime.json")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertAbsent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: %s exists (stat err = %v)", why, path, err)
	}
}

// TestAggregate_OutputIsPinnedByteForByte pins the build artifact and the
// defaults sidecar: computing the aggregate once for both tasks must not move
// a byte of what build~infra writes.
func TestAggregate_OutputIsPinnedByteForByte(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)

	result := Aggregate(ctx, Options{RuntimeCompatibility: noHTTP2})
	if result.Outcome != OutcomeEmitted || len(result.Diagnostics) != 0 {
		t.Fatalf("Aggregate = %q %v", result.Outcome, result.Diagnostics)
	}
	if got := readFile(t, aggregatedPath(root, "app")); got != pinnedAggregate {
		t.Errorf("build aggregate drifted:\n%s\nwant:\n%s", got, pinnedAggregate)
	}
	if got := readFile(t, defaultsSidecarPath(root)); got != pinnedDefaultsSidecar {
		t.Errorf("defaults sidecar drifted:\n%s\nwant:\n%s", got, pinnedDefaultsSidecar)
	}
	assertAbsent(t, DeploymentPath(filepath.Join(root, "app")), "build~infra writes no deployment declaration")
}

// TestDeployment_WritesTheCanonicalAggregate pins the declaration: the
// manifest build~infra aggregates from the same files, without $schema, in the
// canonical bytes the infra protocol and the Put registry both accept, the
// same bytes on every run.
func TestDeployment_WritesTheCanonicalAggregate(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-deployment-declaration-is-the-canonical-aggregate")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)

	result, err := Deployment(ctx, Options{RuntimeCompatibility: noHTTP2})
	if err != nil || result.Outcome != OutcomeEmitted || len(result.Diagnostics) != 0 {
		t.Fatalf("Deployment = %q %v %v", result.Outcome, result.Diagnostics, err)
	}
	if result.ManifestPath != DeploymentPath(filepath.Join(root, "app")) || result.Contributions != 1 {
		t.Fatalf("result = %+v", result)
	}
	first := readFile(t, result.ManifestPath)
	if first != pinnedDeployment {
		t.Fatalf("deployment declaration = %s\nwant %s", first, pinnedDeployment)
	}
	if _, err := os.Stat(result.ManifestPath + ".tmp"); err == nil {
		t.Error("temp file left behind after the atomic write")
	}

	if _, err := Deployment(ctx, Options{RuntimeCompatibility: noHTTP2}); err != nil {
		t.Fatal(err)
	}
	if second := readFile(t, result.ManifestPath); second != first {
		t.Fatalf("two runs over the same files wrote different bytes:\n%s\n%s", first, second)
	}

	if m, diags := infra.ParseDeployment([]byte(first)); m == nil || diag.HasErrors(diags) {
		t.Fatalf("the declaration is not a canonical deployment declaration: %v", diags)
	}
	if diags := put.ValidatePayload([]byte(first)); len(diags) != 0 {
		t.Fatalf("the declaration is not a canonical Put payload: %v", diags)
	}

	// The same manifest build~infra writes, minus its $schema.
	Aggregate(ctx, Options{RuntimeCompatibility: noHTTP2})
	aggregate := readAggregated(t, root, "app")
	fromAggregate, diags := infra.MarshalDeployment(aggregate)
	if diag.HasErrors(diags) || !bytes.Equal(fromAggregate, []byte(first)) {
		t.Fatalf("the declaration and the build aggregate differ:\n%s\n%s %v", first, fromAggregate, diags)
	}
}

// TestDeployment_NamesTheWorkloadByProjectID pins the declaration's workload to
// the identity a release-set member records for the same project, so a deployer
// that matches the declaration to its member accepts a project whose name is
// not its path.
func TestDeployment_NamesTheWorkloadByProjectID(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "sites/example.dev", "example.dev", "application")

	result, err := Deployment(ctx, Options{})
	if err != nil || result.Outcome != OutcomeEmitted {
		t.Fatalf("Deployment = %q %v %v", result.Outcome, result.Diagnostics, err)
	}
	m, diags := infra.ParseDeployment([]byte(readFile(t, result.ManifestPath)))
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("parse declaration: %v", diags)
	}
	if m.Workload != "sites/example.dev" {
		t.Errorf("workload = %q, want the project id sites/example.dev, not the name example.dev", m.Workload)
	}
}

// TestDeployment_CarriesOverridesAndTheAuthoredRuntime pins the inputs beyond
// the closure: the workload's overrides and its authored runtime reach the
// declaration, with the language's compatibility hook applied.
func TestDeployment_CarriesOverridesAndTheAuthoredRuntime(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-deployment-declaration-is-the-canonical-aggregate")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application", "lib")
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres"}],"secrets":["jwks"]}`)
	writeProjectFile(t, root, "app", "infra/overrides.json",
		`{"ignore":{"databases":[{"name":"primary","engine":"postgres"}]}}`)
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":10},"protocols":{"http2":true}}`)

	result, err := Deployment(ctx, Options{RuntimeCompatibility: noHTTP2})
	if err != nil || result.Outcome != OutcomeEmitted {
		t.Fatalf("Deployment = %q %v %v", result.Outcome, result.Diagnostics, err)
	}
	const want = `{"protocolVersion":2,"workload":"app",` +
		`"secrets":[{"name":"jwks","sources":[{"project":"lib","contributor":"framework:requirements"}]}],` +
		`"runtime":{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":10},"protocols":{"http2":false}}}`
	if got := readFile(t, result.ManifestPath); got != want {
		t.Fatalf("deployment declaration = %s\nwant %s", got, want)
	}
}

// TestDeployment_LeavesTheDefaultsSidecarAlone pins that the declaration task
// can run beside build~infra: it declares the defaults without writing the
// sidecar, and it neither reads nor removes one that exists.
func TestDeployment_LeavesTheDefaultsSidecarAlone(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-deployment-declaration-leaves-the-defaults-sidecar-alone")
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")

	if _, err := Deployment(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, defaultsSidecarPath(root), "the declaration task wrote the defaults sidecar")
	const defaults = `{"protocolVersion":2,"workload":"app","runtime":{"ingress":{"public":false},"scaling":{"max":1,"concurrency":500}}}`
	if got := readFile(t, DeploymentPath(filepath.Join(root, "app"))); got != defaults {
		t.Fatalf("declaration without a runtime file = %s\nwant %s", got, defaults)
	}

	const foreign = `{"scaling":{"max":99}}` + "\n"
	writeProjectFile(t, root, "app", ".gen/infra/runtime.json", foreign)
	if _, err := Deployment(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, defaultsSidecarPath(root)); got != foreign {
		t.Fatalf("the declaration task rewrote the defaults sidecar: %s", got)
	}
	if got := readFile(t, DeploymentPath(filepath.Join(root, "app"))); got != defaults {
		t.Fatalf("the declaration read the sidecar instead of the defaults: %s", got)
	}

	// An authored runtime does not make the declaration task remove the sidecar
	// either: that is build~infra's statement.
	writeProjectFile(t, root, "app", "infra/runtime.json", `{"scaling":{"max":3}}`)
	if _, err := Deployment(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, defaultsSidecarPath(root)); got != foreign {
		t.Fatalf("the declaration task removed or rewrote the defaults sidecar: %s", got)
	}
}

// TestDeployment_NeverLeavesAStaleDeclaration pins the two ways a run writes
// no declaration: a library, and an aggregate with an error finding. Both
// remove what an earlier run left, so a publisher never reads a declaration of
// other inputs.
func TestDeployment_NeverLeavesAStaleDeclaration(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "a-deployment-declaration-is-never-stale")
	const stale = `{"protocolVersion":2,"workload":"old"}`

	t.Run("library", func(t *testing.T) {
		root := t.TempDir()
		ctx := closureContext(root, "lib", "lib", "library")
		writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
		writeProjectFile(t, root, "lib", DeploymentFile, stale)

		result, err := Deployment(ctx, Options{})
		if err != nil || result.Outcome != OutcomeSkipped {
			t.Fatalf("Deployment = %q %v", result.Outcome, err)
		}
		assertAbsent(t, DeploymentPath(filepath.Join(root, "lib")), "a library kept a deployment declaration")
	})

	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		code  string
	}{
		{"unreadable contribution", func(t *testing.T, root string) {
			writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2,"databazes":[]}`)
		}, infra.ErrorCodeUnknownField},
		{"same-precedence conflict", func(t *testing.T, root string) {
			writeProjectFile(t, root, "lib", "infra/requirements.json",
				`{"protocolVersion":2,"storage":[{"name":"assets","retention":"30d"}]}`)
			writeProjectFile(t, root, "app", "infra/requirements.json",
				`{"protocolVersion":2,"storage":[{"name":"assets","retention":"90d"}]}`)
		}, infra.ErrorCodeConflictingValue},
		{"malformed runtime", func(t *testing.T, root string) {
			writeProjectFile(t, root, "app", "infra/runtime.json", `{"scaling":{"min":1}}`)
		}, infra.ErrorCodeUnknownField},
		{"invalid runtime", func(t *testing.T, root string) {
			writeProjectFile(t, root, "app", "infra/runtime.json", `{"scaling":{"max":-1}}`)
		}, infra.ErrorCodeInvalidScaling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctx := closureContext(root, "app", "app", "application", "lib")
			tc.setup(t, root)
			writeProjectFile(t, root, "app", DeploymentFile, stale)

			result, err := Deployment(ctx, Options{})
			if err != nil || result.Outcome != OutcomeWithheld {
				t.Fatalf("Deployment = %q %v %v", result.Outcome, result.Diagnostics, err)
			}
			if !diag.HasErrors(result.Diagnostics) || !hasDiagCode(result.Diagnostics, tc.code) {
				t.Fatalf("diagnostics = %v, want an error %s", result.Diagnostics, tc.code)
			}
			assertAbsent(t, DeploymentPath(filepath.Join(root, "app")), "a withheld aggregate kept a declaration")
		})
	}
}

// TestDeployment_AFailedWriteFailsTheTask pins the one fatal case: a write or
// removal that fails can leave an earlier declaration in place.
func TestDeployment_AFailedWriteFailsTheTask(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	// A non-empty directory where the declaration goes can be neither replaced
	// by a rename nor removed.
	writeProjectFile(t, root, "app", DeploymentFile+"/blocker", "x")

	if _, err := Deployment(ctx, Options{}); err == nil {
		t.Fatal("a failed write did not fail")
	}
	ctx.Project.Type = "library"
	if _, err := Deployment(ctx, Options{}); err == nil {
		t.Fatal("a failed removal did not fail")
	}
	if result, err := Deployment(nil, Options{}); err != nil || result.Outcome != OutcomeSkipped {
		t.Fatalf("Deployment(nil) = %q %v", result.Outcome, err)
	}
}

func TestDeploymentJob_ReportsEveryOutcome(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "infra-aggregation-neutrality", "findings-never-fail-the-task")

	t.Run("emitted", func(t *testing.T) {
		root := t.TempDir()
		ctx := closureContext(root, "app", "app", "application", "lib")
		writeProjectFile(t, root, "lib", "infra/requirements.json", dbManifest)
		var status string
		var data map[string]any
		var err error
		out := captureStdout(t, func() {
			status, data, err = DeploymentJob(Options{})(ctx, jsonl.NewForVersion(1), nil)
		})
		if err != nil || status != "OK" || data["outcome"] != string(OutcomeEmitted) {
			t.Fatalf("job = %q %v %v", status, data, err)
		}
		if !strings.Contains(out, DeploymentPhaseName) || !strings.Contains(out, infra.DeploymentFilename) {
			t.Errorf("event stream does not report the phase and the artifact:\n%s", out)
		}
	})

	t.Run("library", func(t *testing.T) {
		root := t.TempDir()
		ctx := closureContext(root, "lib", "lib", "library")
		var status string
		var err error
		captureStdout(t, func() {
			status, _, err = DeploymentJob(Options{})(ctx, jsonl.NewForVersion(1), nil)
		})
		if err != nil || status != "SKIP" {
			t.Fatalf("job = %q %v, want SKIP", status, err)
		}
	})

	t.Run("withheld", func(t *testing.T) {
		root := t.TempDir()
		ctx := closureContext(root, "app", "app", "application", "lib")
		writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2,"databazes":[]}`)
		var status string
		var data map[string]any
		var err error
		out := captureStdout(t, func() {
			status, data, err = DeploymentJob(Options{})(ctx, jsonl.NewForVersion(1), nil)
		})
		if err != nil || status != "SKIP" || data["outcome"] != string(OutcomeWithheld) {
			t.Fatalf("job = %q %v %v, want a SKIP that does not fail", status, data, err)
		}
		if !strings.Contains(out, `"warn"`) || !strings.Contains(out, "deployment declaration:") {
			t.Errorf("finding was not surfaced as a warning:\n%s", out)
		}
		if strings.Contains(out, `"level":"error"`) {
			t.Errorf("finding was escalated to an error level:\n%s", out)
		}
	})

	t.Run("failed write", func(t *testing.T) {
		root := t.TempDir()
		ctx := closureContext(root, "app", "app", "application")
		writeProjectFile(t, root, "app", DeploymentFile+"/blocker", "x")
		var status string
		var err error
		captureStdout(t, func() {
			status, _, err = DeploymentJob(Options{})(ctx, jsonl.NewForVersion(1), nil)
		})
		if err == nil || status != "FAILED" {
			t.Fatalf("job = %q %v, want FAILED with an error", status, err)
		}
	})
}
