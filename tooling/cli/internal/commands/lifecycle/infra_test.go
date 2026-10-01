package lifecycle

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const aggregatedFixture = `{
  "$schema": "https://putnami.dev/schemas/putnami-infra.json",
  "protocolVersion": 2,
  "workload": "go.putnami.dev/example/api",
  "databases": [
    {
      "name": "primary",
      "engine": "postgres",
      "schemas": ["audit", "iam"],
      "sources": [
        { "project": "go.putnami.dev/example/audit", "contributor": "manual" },
        { "project": "go.putnami.dev/example/iam", "contributor": "framework:database" }
      ]
    }
  ],
  "events": {
    "publishes": [
      {
        "name": "workspace.deploy.completed",
        "sources": [{ "project": "go.putnami.dev/example/api", "contributor": "manual" }]
      }
    ],
    "subscribes": [
      {
        "name": "billing.invoice.issued",
        "sources": [{ "project": "go.putnami.dev/example/billing", "contributor": "manual" }]
      }
    ]
  },
  "storage": [
    {
      "name": "audit-logs",
      "retention": "90d",
      "sources": [{ "project": "go.putnami.dev/example/audit", "contributor": "manual" }]
    }
  ],
  "secrets": [
    {
      "name": "jwks_signing_key",
      "sources": [{ "project": "go.putnami.dev/example/iam", "contributor": "manual" }]
    }
  ],
  "scheduledJobs": [
    {
      "name": "nightly-rollup",
      "schedule": "0 2 * * *",
      "entrypoint": "jobs.NightlyRollup",
      "sources": [{ "project": "go.putnami.dev/example/audit", "contributor": "manual" }]
    }
  ],
  "runtime": {
    "ingress": { "domain": "api.example.com", "public": true },
    "scaling": { "max": 100, "concurrency": 80 }
  }
}
`

// captureInfraStdout swaps os.Stdout for a pipe while fn runs and returns
// everything fn wrote. Used to assert against the human-readable output
// of InfraPlan without refactoring it to accept a writer (the goal is
// to exercise the same path callers see).
func captureInfraStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	// Close the writer to signal EOF, wait for the copier to drain the
	// pipe, then restore stdout. Reading buf before the copier exits
	// would race and return a truncated string.
	_ = w.Close()
	<-done
	_ = r.Close()
	os.Stdout = old
	return buf.String()
}

// seedWorkload writes the minimum filesystem state a workspace.Load needs
// to discover one application workload, plus the aggregated manifest at
// <project>/.gen/requirements.json.
func seedWorkload(t *testing.T, root, projectPath, projectName string, manifest string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"includes":["`+projectPath+`"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(root, projectPath)
	if err := os.MkdirAll(filepath.Join(projDir, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "putnami.json"),
		[]byte(`{"name":"`+projectName+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(projDir, ".gen", "requirements.json"),
			[]byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInfraPlan_RendersEveryKind(t *testing.T) {
	root := t.TempDir()
	seedWorkload(t, root, "apps/api", "go.putnami.dev/example/api", aggregatedFixture)

	out := captureInfraStdout(t, func() {
		if err := InfraPlan(root, nil); err != nil {
			t.Fatalf("InfraPlan: %v", err)
		}
	})

	// Every kind from the fixture must appear in the plan — this is the
	// whole point of the prototype: prove the consumer-side contract
	// exercises every shape the protocol exposes.
	for _, want := range []string{
		"Workload: go.putnami.dev/example/api",
		"Databases (1)",
		"postgres://primary",
		"schemas: audit, iam",
		"go.putnami.dev/example/audit (manual)",
		"go.putnami.dev/example/iam (framework:database)",
		"Events:",
		"workspace.deploy.completed",
		"billing.invoice.issued",
		"Storage (1)",
		"audit-logs (retention: 90d)",
		"Secrets (1)",
		"jwks_signing_key",
		"Scheduled jobs (1)",
		"nightly-rollup",
		`"0 2 * * *"`,
		"jobs.NightlyRollup",
		"Runtime:",
		"api.example.com",
		"public",
		"max=100",
		"concurrency=80",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in plan output:\n%s", want, out)
		}
	}
}

func TestInfraPlan_MissingManifestSaysSo(t *testing.T) {
	root := t.TempDir()
	// Seed the workload but write no manifest — simulates a workload
	// that has never built or whose build was invalidated.
	seedWorkload(t, root, "apps/api", "go.putnami.dev/example/api", "")

	out := captureInfraStdout(t, func() {
		if err := InfraPlan(root, nil); err != nil {
			t.Fatalf("InfraPlan: %v", err)
		}
	})

	if !strings.Contains(out, "No aggregated manifest") {
		t.Errorf("expected missing-manifest message, got:\n%s", out)
	}
	if !strings.Contains(out, "putnami build") {
		t.Errorf("expected actionable hint, got:\n%s", out)
	}
}

func TestInfraPlan_InvalidManifestSurfacesDiagnostics(t *testing.T) {
	root := t.TempDir()
	// Aggregated manifest declares an unsupported protocol version.
	seedWorkload(t, root, "apps/api", "go.putnami.dev/example/api",
		`{"protocolVersion":99,"workload":"go.putnami.dev/example/api"}`)

	out := captureInfraStdout(t, func() {
		if err := InfraPlan(root, nil); err != nil {
			t.Fatalf("InfraPlan: %v", err)
		}
	})

	if !strings.Contains(out, "infra.invalid_protocol_version") {
		t.Errorf("expected protocol-version diagnostic, got:\n%s", out)
	}
}

func TestInfraPlan_NamedTargetFiltersToOneWorkload(t *testing.T) {
	root := t.TempDir()
	// RootProjectPaths doesn't expand globs, so list each project literally.
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"includes":["apps/api","apps/worker"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		path, name string
	}{
		{"apps/api", "go.putnami.dev/example/api"},
		{"apps/worker", "go.putnami.dev/example/worker"},
	} {
		dir := filepath.Join(root, p.path)
		if err := os.MkdirAll(filepath.Join(dir, ".gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"),
			[]byte(`{"name":"`+p.name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := captureInfraStdout(t, func() {
		if err := InfraPlan(root, []string{"go.putnami.dev/example/api"}); err != nil {
			t.Fatalf("InfraPlan: %v", err)
		}
	})

	if !strings.Contains(out, "go.putnami.dev/example/api") {
		t.Errorf("expected api in output, got:\n%s", out)
	}
	if strings.Contains(out, "worker") {
		t.Errorf("expected worker NOT in output (named target), got:\n%s", out)
	}
}

// Infra aggregation is over deployable workloads, and classification is what
// says which projects those are. An earlier version of the Go probe
// classified nothing, so every Go library fell through the empty-type default
// into this selection and was asked for infra it can never emit; a library must
// now drop out of the untargeted plan and be refused as a named target.
func TestInfraPlan_LibraryProjectIsNotAWorkload(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"includes":["apps/api","libs/core"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ path, name, projectType string }{
		{"apps/api", "go.putnami.dev/example/api", "application"},
		{"libs/core", "go.putnami.dev/example/core", "library"},
	} {
		dir := filepath.Join(root, p.path)
		if err := os.MkdirAll(filepath.Join(dir, ".gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"),
			[]byte(`{"name":"`+p.name+`","type":"`+p.projectType+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := captureInfraStdout(t, func() {
		if err := InfraPlan(root, nil); err != nil {
			t.Fatalf("InfraPlan: %v", err)
		}
	})
	if !strings.Contains(out, "go.putnami.dev/example/api") {
		t.Errorf("the application workload is missing from the plan, got:\n%s", out)
	}
	if strings.Contains(out, "go.putnami.dev/example/core") {
		t.Errorf("a library was aggregated as a workload, got:\n%s", out)
	}

	err := InfraPlan(root, []string{"go.putnami.dev/example/core"})
	if err == nil {
		t.Fatal("naming a library as an infra target succeeded; it is not a workload")
	}
	if !strings.Contains(err.Error(), "library") {
		t.Errorf("error = %v, want it to name the classification that refused the target", err)
	}
}

func TestInfraPlanJSON_EmitsOneEntryPerWorkload(t *testing.T) {
	root := t.TempDir()
	seedWorkload(t, root, "apps/api", "go.putnami.dev/example/api", aggregatedFixture)

	out := captureInfraStdout(t, func() {
		if err := InfraPlanJSON(root, nil); err != nil {
			t.Fatalf("InfraPlanJSON: %v", err)
		}
	})

	if !strings.Contains(out, `"workload":"go.putnami.dev/example/api"`) {
		t.Errorf("expected workload field, got:\n%s", out)
	}
	if !strings.Contains(out, `"status":"ok"`) {
		t.Errorf("expected ok status, got:\n%s", out)
	}
	// The manifest payload must round-trip — proves the consumer can hand
	// the deserialised AggregatedManifest straight to downstream code.
	if !strings.Contains(out, `"protocolVersion":2`) {
		t.Errorf("expected manifest payload, got:\n%s", out)
	}
}
