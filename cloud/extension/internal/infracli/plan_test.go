package infracli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/job"
)

const (
	apiName     = "go.putnami.dev/example/api"
	emptyName   = "go.putnami.dev/example/empty"
	workerName  = "go.putnami.dev/example/worker"
	libraryName = "go.putnami.dev/example/core"
)

// apiManifest is the aggregated manifest of the api workload. It declares every
// kind the plan prints; the core `infra plan` tests used the same document.
const apiManifest = `{
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

const emptyManifest = `{"protocolVersion":2,"workload":"go.putnami.dev/example/empty"}`

// workspace is a fixture workspace: the project references the job context
// lists, and the aggregated manifests on disk.
type workspace struct {
	root string
	// projects is in canonical project-id order, the order the job context
	// lists them in. It is not sorted by name.
	projects []job.ProjectRef
}

func (w workspace) project(name string) job.ProjectRef {
	for _, p := range w.projects {
		if p.Name == name {
			return p
		}
	}
	panic("fixture has no project " + name)
}

// newWorkspace writes three application workloads (one with every kind of
// requirement, one declaring nothing, one never built) and one library.
func newWorkspace(t *testing.T) workspace {
	t.Helper()
	root := t.TempDir()
	w := workspace{root: root}
	for _, p := range []struct {
		path, name, projectType, manifest string
	}{
		{"apps/worker", workerName, "application", ""},
		{"libs/core", libraryName, "library", apiManifest},
		{"apps/empty", emptyName, "application", emptyManifest},
		{"apps/api", apiName, "application", apiManifest},
	} {
		dir := filepath.Join(root, filepath.FromSlash(p.path))
		if err := os.MkdirAll(filepath.Join(dir, infra.AggregatedManifestDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if p.manifest != "" {
			manifest := filepath.Join(dir, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
			if err := os.WriteFile(manifest, []byte(p.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w.projects = append(w.projects, job.ProjectRef{Name: p.name, Path: p.path, FullPath: dir, Type: p.projectType})
	}
	return w
}

// plan runs the command and returns what it printed: one Stdout call is one
// line, as it is under the extension runtime. The runtime names the command in
// the result envelope through the command param.
func plan(params map[string]any, args []string, projects []job.ProjectRef) (string, error) {
	params = clicore.MergeParams(params, map[string]any{"command": "cloud infra"})
	var out strings.Builder
	err := Plan(params, args, projects, clicore.IO{
		Stdout: func(line string) { out.WriteString(line + "\n") },
		Stderr: func(string) {},
	})
	return out.String(), err
}

func TestPlanHumanOutputMatchesCoreInfraPlan(t *testing.T) {
	spectest.Proves(t, "cloud/cli-infra-plan", "plan-lists-workloads-from-job-context", "infra-plan-output-matches-core")
	w := newWorkspace(t)

	got, err := plan(map[string]any{}, nil, w.projects)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// The golden is the text the core `putnami infra plan` printed for this
	// workspace before the core deleted the command.
	golden, err := os.ReadFile(filepath.Join("testdata", "plan.human.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(golden), "{{root}}", w.root)
	if got != want {
		t.Fatalf("plan output differs from the core output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestPlanNamesOneWorkload(t *testing.T) {
	w := newWorkspace(t)

	got, err := plan(map[string]any{}, []string{workerName}, w.projects)
	if err != nil {
		t.Fatalf("plan %s: %v", workerName, err)
	}
	want := "Workload: " + workerName + "\n" + strings.Repeat("─", len("Workload: "+workerName)) + "\n" +
		"  No aggregated manifest at " + filepath.Join(w.root, "apps", "worker", ".gen", "requirements.json") + ".\n" +
		"  Run `putnami build` for this workload to emit one.\n"
	if got != want {
		t.Fatalf("plan %s =\n%s\nwant\n%s", workerName, got, want)
	}
}

// A flag the CLI forwards, such as the job context path, is not part of the
// project name.
func TestPlanIgnoresForwardedContextFlag(t *testing.T) {
	w := newWorkspace(t)

	got, err := plan(map[string]any{}, []string{"--putnamiContext", "/tmp/context.json", emptyName}, w.projects)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.HasPrefix(got, "Workload: "+emptyName+"\n") || strings.Contains(got, apiName) {
		t.Fatalf("plan output = %q, want only %s", got, emptyName)
	}
}

func TestPlanRefusesAnUnknownProjectAndALibrary(t *testing.T) {
	w := newWorkspace(t)
	for _, tc := range []struct {
		target, message string
	}{
		{"go.putnami.dev/example/missing", `project "go.putnami.dev/example/missing" not found in workspace`},
		{libraryName, `project "` + libraryName + `" is not an application workload (type="library")`},
	} {
		out, err := plan(map[string]any{}, []string{tc.target}, w.projects)
		if err == nil || err.Error() != tc.message {
			t.Fatalf("plan %s error = %v, want %q", tc.target, err, tc.message)
		}
		if !errors.Is(err, protocolcli.ErrNotFound) || clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("plan %s error %v is not a not-found usage error (exit %d)", tc.target, err, clicore.ExitCode(err))
		}
		if out != "" {
			t.Fatalf("plan %s printed %q before it failed", tc.target, out)
		}
	}
}

func TestPlanWithoutWorkloadsSaysSo(t *testing.T) {
	library := []job.ProjectRef{{Name: libraryName, Path: "libs/core", FullPath: t.TempDir(), Type: "library"}}

	got, err := plan(map[string]any{}, nil, library)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got != "No workloads found.\n" {
		t.Fatalf("plan output = %q, want the no-workloads line", got)
	}

	structured, err := plan(map[string]any{"output": "jsonl"}, nil, library)
	if err != nil {
		t.Fatalf("plan --output=jsonl: %v", err)
	}
	if data := envelopeData(t, structured); !reflect.DeepEqual(data, []any{}) {
		t.Fatalf("data = %#v, want []", data)
	}
}

func TestPlanOfAnEmptyWorkspacePlansNoWorkloads(t *testing.T) {
	empty := []job.ProjectRef{}

	got, err := plan(map[string]any{}, nil, empty)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got != "No workloads found.\n" {
		t.Fatalf("plan output = %q, want the no-workloads line", got)
	}

	structured, err := plan(map[string]any{"output": "jsonl"}, nil, empty)
	if err != nil {
		t.Fatalf("plan --output=jsonl: %v", err)
	}
	if data := envelopeData(t, structured); !reflect.DeepEqual(data, []any{}) {
		t.Fatalf("data = %#v, want []", data)
	}
}

func TestPlanReportsAnInvalidManifestInThePlan(t *testing.T) {
	spectest.Proves(t, "infra/infrastructure-requirements-contract", "a-manifest-is-read-strictly", "the-cloud-plan-prints-the-codes-of-an-invalid-manifest")
	w := newWorkspace(t)
	worker := w.project(workerName)
	path := filepath.Join(worker.FullPath, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
	if err := os.WriteFile(path, []byte(`{"protocolVersion":99,"workload":"`+workerName+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, diags := infra.LoadAggregatedManifest(path)
	if !diag.HasErrors(diags) {
		t.Fatal("the fixture manifest is valid; the test needs an invalid one")
	}

	got, err := plan(map[string]any{}, []string{workerName}, w.projects)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := "Workload: " + workerName + "\n" + strings.Repeat("─", len("Workload: "+workerName)) + "\n" +
		"  Manifest failed to parse or validate:\n"
	for _, d := range diags {
		want += fmt.Sprintf("    [%s] %s\n", d.Code, d.Message)
	}
	if got != want {
		t.Fatalf("plan output =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(got, "[infra.invalid_protocol_version]") {
		t.Fatalf("plan output has no protocol-version diagnostic:\n%s", got)
	}
}

func TestPlanStructuredOutputMatchesCoreInfraPlan(t *testing.T) {
	w := newWorkspace(t)
	// The order the core prints: workloads sorted by name.
	workloads := []job.ProjectRef{w.project(apiName), w.project(emptyName), w.project(workerName)}

	for _, mode := range []protocolcli.OutputMode{protocolcli.OutputJSON, protocolcli.OutputJSONL} {
		got, err := plan(map[string]any{"output": string(mode)}, nil, w.projects)
		if err != nil {
			t.Fatalf("plan --output=%s: %v", mode, err)
		}
		if want := coreStructuredOutput(t, workloads, mode); got != want {
			t.Fatalf("plan --output=%s differs from the core output\n--- got ---\n%s\n--- want ---\n%s", mode, got, want)
		}
	}

	// One named workload is an object, not an array of one.
	got, err := plan(map[string]any{"output": "jsonl"}, []string{workerName}, w.projects)
	if err != nil {
		t.Fatalf("plan %s: %v", workerName, err)
	}
	if want := coreStructuredOutput(t, []job.ProjectRef{w.project(workerName)}, protocolcli.OutputJSONL); got != want {
		t.Fatalf("plan %s =\n%s\nwant\n%s", workerName, got, want)
	}
	want := map[string]any{"workload": workerName, "status": "missing"}
	if data := envelopeData(t, got); !reflect.DeepEqual(data, want) {
		t.Fatalf("data = %#v, want %#v", data, want)
	}
}

// The manifest of an "ok" entry is the aggregated manifest document as written.
func TestPlanStructuredOutputCarriesTheManifest(t *testing.T) {
	w := newWorkspace(t)

	got, err := plan(map[string]any{"output": "json"}, []string{apiName}, w.projects)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var fixture any
	if err := json.Unmarshal([]byte(apiManifest), &fixture); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"workload": apiName, "status": "ok", "manifest": fixture}
	if data := envelopeData(t, got); !reflect.DeepEqual(data, want) {
		t.Fatalf("data = %#v, want %#v", data, want)
	}
}

func TestPlanRefusesAContextWithoutProjectTypes(t *testing.T) {
	spectest.Proves(t, "cloud/cli-infra-plan", "plan-lists-workloads-from-job-context", "infra-plan-refuses-untyped-context")
	w := newWorkspace(t)
	untyped := append([]job.ProjectRef{}, w.projects...)
	untyped[2].Type = ""

	for _, params := range []map[string]any{{}, {"output": "json"}} {
		out, err := plan(params, nil, untyped)
		if err == nil {
			t.Fatalf("plan %v succeeded on a project without a type; it must not read it as application", params)
		}
		if !strings.Contains(err.Error(), `"`+emptyName+`"`) || !strings.Contains(err.Error(), "upgrade the CLI") {
			t.Fatalf("error = %q, want it to name the project and say to upgrade the CLI", err)
		}
		if clicore.ExitCode(err) != clicore.ExitFailure {
			t.Fatalf("exit code = %d, want %d", clicore.ExitCode(err), clicore.ExitFailure)
		}
		if out != "" {
			t.Fatalf("plan printed %q before it refused the context", out)
		}
	}

	// A named target does not bypass the refusal.
	if _, err := plan(map[string]any{}, []string{apiName}, untyped); err == nil {
		t.Fatal("plan with a named target succeeded on a project without a type")
	}
}

func TestPlanRefusesAContextWithoutWorkspaceProjects(t *testing.T) {
	out, err := plan(map[string]any{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "the job context lists none") {
		t.Fatalf("error = %v, want a refusal that says the job context lists no workspace project", err)
	}
	if out != "" {
		t.Fatalf("plan printed %q before it refused the context", out)
	}
}

// coreStructuredOutput re-implements the structured path the core's deleted
// `putnami infra plan` had: `InfraPlanJSON` (one JSON object per workload) and
// the dispatcher's `structuredPayload` (one document, an array of several, or
// [] under the data member of one result envelope). It proves the envelope
// framing and the object, array or [] rule. The command name differs because
// this command is `cloud infra`.
func coreStructuredOutput(t *testing.T, workloads []job.ProjectRef, mode protocolcli.OutputMode) string {
	t.Helper()
	var captured bytes.Buffer
	encoder := json.NewEncoder(&captured)
	for _, p := range workloads {
		entry := map[string]any{"workload": p.Name}
		path := filepath.Join(p.FullPath, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			entry["status"] = "missing"
			_ = encoder.Encode(entry)
			continue
		}
		manifest, diags := infra.LoadAggregatedManifest(path)
		if diag.HasErrors(diags) {
			entry["status"] = "invalid"
			entry["diagnostics"] = diags
			_ = encoder.Encode(entry)
			continue
		}
		entry["status"] = "ok"
		entry["manifest"] = manifest
		_ = encoder.Encode(entry)
	}

	decoder := json.NewDecoder(&captured)
	var documents []any
	for {
		var document any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode the core documents: %v", err)
		}
		documents = append(documents, document)
	}
	var data any = documents
	switch {
	case len(documents) == 1:
		data = documents[0]
	case documents == nil:
		data = []any{}
	}
	var out strings.Builder
	if _, err := protocolcli.WriteResultV2(&out, mode, protocolcli.NewResultV2("cloud infra", data, nil)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// envelopeData decodes one result envelope and returns its data member.
func envelopeData(t *testing.T, output string) any {
	t.Helper()
	var envelope struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Data    any    `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode the result envelope %q: %v", output, err)
	}
	if envelope.Status != "success" {
		t.Fatalf("envelope status = %q, want success: %s", envelope.Status, output)
	}
	return envelope.Data
}
