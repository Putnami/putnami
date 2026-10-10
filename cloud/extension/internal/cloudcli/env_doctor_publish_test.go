package cloudcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	runtimecli "go.putnami.dev/cloud/extension/internal/runtimecli"
)

func writeDoctorProject(t *testing.T, root, path string, manifest map[string]any, files ...string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "putnami.json"), manifest)
	for _, file := range files {
		target := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func publishOptions(layers ...string) map[string]any {
	options := map[string]any{}
	for _, layer := range layers {
		options[layer] = map[string]any{"namespace": "acme"}
	}
	return options
}

// TestEnvDoctorPublishAppliesTheProbeRules pins the publish-namespaces row
// against the probe's rules: a Config schema needs a Config namespace, a built
// migration bundle needs a migration namespace, and a workload with neither
// needs nothing.
func TestEnvDoctorPublishAppliesTheProbeRules(t *testing.T) {
	root := t.TempDir()
	writeDoctorProject(t, root, "shop/workloads/api", map[string]any{
		"name": "shop/workloads/api", "extensions": []string{"@putnami/go"},
		"options": publishOptions("@putnami/cloud:publish-config", "@putnami/cloud:publish-migration"),
	}, "shop/workloads/api/schema/config.json", "shop/workloads/api/.gen/migration-bundle/bundle.json")
	writeDoctorProject(t, root, "shop/workloads/declared", map[string]any{
		"name": "shop/workloads/declared", "options": publishOptions("@putnami/cloud:publish-migration"),
	})
	writeDoctorProject(t, root, "shop/workloads/web", map[string]any{"name": "shop-web"})
	workloads := []runtimecli.EnvDoctorWorkload{
		{Path: "shop/workloads/api", Name: "shop/workloads/api"},
		{Path: "shop/workloads/declared", Name: "shop/workloads/declared"},
		{Path: "shop/workloads/web", Name: "shop-web"},
	}

	row, out := envDoctorPublish(root, "acme", workloads)
	if row.ID != runtimecli.EnvDoctorRowPublishNamespaces || row.Status != runtimecli.EnvDoctorOK || row.Fix != "" {
		t.Fatalf("row = %+v", row)
	}
	if !strings.Contains(row.Detail, "3 workload(s) checked: 1 publish Config, 2 publish migrations") {
		t.Fatalf("detail = %q", row.Detail)
	}
	migrated := map[string]bool{}
	for _, workload := range out {
		migrated[workload.Path] = workload.Migrated
	}
	if !migrated["shop/workloads/api"] || !migrated["shop/workloads/declared"] || migrated["shop/workloads/web"] || len(out) != 3 {
		t.Fatalf("migrated = %v", migrated)
	}
}

// TestEnvDoctorPublishNamesEachUndeclaredNamespace is acceptance (a) for the
// row: every workload that would publish nothing is named, with the manifest
// change that fixes it.
func TestEnvDoctorPublishNamesEachUndeclaredNamespace(t *testing.T) {
	root := t.TempDir()
	writeDoctorProject(t, root, "shop/workloads/api", map[string]any{"name": "shop/workloads/api"},
		"shop/workloads/api/.gen/config-schema.json", "shop/workloads/api/.gen/migration-bundle/bundle.json")
	writeDoctorProject(t, root, "shop/workloads/staged", map[string]any{"name": "shop/workloads/staged"},
		".putnami/out/shop/workloads/staged/build/migration-bundle/bundle.json")
	writeDoctorProject(t, root, "shop/workloads/bad", map[string]any{
		"name": "shop/workloads/bad", "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "a/b"}},
	})
	workloads := []runtimecli.EnvDoctorWorkload{
		{Path: "shop/workloads/api"}, {Path: "shop/workloads/staged"}, {Path: "shop/workloads/bad"}, {Path: "shop/workloads/gone"},
	}

	row, out := envDoctorPublish(root, "", workloads)
	if row.Status != runtimecli.EnvDoctorMissing {
		t.Fatalf("row = %+v", row)
	}
	for _, want := range []string{
		"shop/workloads/api has a Config schema but declares no publish-config namespace",
		"shop/workloads/api builds a migration bundle but declares no publish-migration namespace",
		"shop/workloads/staged builds a migration bundle",
		"shop/workloads/bad: options.@putnami/cloud:publish-migration.namespace must be one canonical native path segment",
		"shop/workloads/gone/putnami.json could not be read",
	} {
		if !strings.Contains(row.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, row.Detail)
		}
	}
	for _, want := range []string{
		`in shop/workloads/api/putnami.json set "options": {"@putnami/cloud:publish-config": {"namespace": "<ns>"}, "@putnami/cloud:publish-migration": {"namespace": "<ns>"}}`,
		`in shop/workloads/staged/putnami.json set "options": {"@putnami/cloud:publish-migration": {"namespace": "<ns>"}}`,
		`in shop/workloads/bad/putnami.json set "options": {"@putnami/cloud:publish-migration": {"namespace": "<ns>"}}`,
		"restore shop/workloads/gone/putnami.json",
	} {
		if !strings.Contains(row.Fix, want) {
			t.Errorf("fix lacks %q:\n%s", want, row.Fix)
		}
	}
	if len(out) != 4 || !out[0].Migrated || !out[1].Migrated || out[2].Migrated || out[3].Migrated {
		t.Fatalf("workloads = %+v", out)
	}
}

// doctorControl is a readiness double that records every request.
type doctorControl struct {
	mu     sync.Mutex
	calls  []string
	status string
}

func (c *doctorControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.calls = append(c.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	c.mu.Unlock()
	ids := []string{"put-binding", "oci-binding", "environment-definition", "runtime-operation-grants",
		"runtime-binding-grant-config", "database-ownership", "project-identity"}
	rows := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		fix := ""
		if c.status != runtimecli.EnvDoctorOK {
			fix = "fix " + id
		}
		rows = append(rows, map[string]string{"id": id, "title": id, "status": c.status, "detail": id, "fix": fix})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"workspace": "w", "environment": "prod", "rows": rows})
}

// TestRunMainDispatchesEnvDoctor pins the verb end to end through the
// standalone entry point: the exit code follows the rows, and the JSON
// envelope carries the doctor node, one child per row, either way.
func TestRunMainDispatchesEnvDoctor(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   int
	}{{runtimecli.EnvDoctorOK, ExitSuccess}, {runtimecli.EnvDoctorMissing, 1}} {
		t.Run(tc.status, func(t *testing.T) {
			root := t.TempDir()
			writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
				"registries": map[string]any{"oci": map[string]any{"publish": "oci.putnami.dev/acme"}},
				"options": map[string]any{"@putnami/cloud": map[string]any{"workspace": map[string]any{
					"workspace_id": "0b8f3c9e-6a1d-4f2e-9c7b-3d5e8a1f2b4c", "control_plane_url": "https://control.invalid",
				}}},
			})
			writeJSONFile(t, filepath.Join(root, "putnami.ci.json"), map[string]any{
				"distribution": map[string]any{"namespace": "acme", "memberAttribution": true},
				"envs":         map[string]any{"prod": map[string]any{"channel": "canary", "workloads": []any{map[string]any{"select": "shop/workloads/api"}}}},
			})
			writeDoctorProject(t, root, "shop/workloads/api", map[string]any{
				"name": "shop/workloads/api", "options": publishOptions("@putnami/cloud:publish-migration"),
			})
			control := &doctorControl{status: tc.status}
			srv := httptest.NewServer(control)
			defer srv.Close()

			var stdout []string
			code := RunMain([]string{"env", "doctor", "--output=json"}, IO{
				Env: map[string]string{
					"HOME": t.TempDir(), "PUTNAMI_WORKSPACE_ROOT": root,
					"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL,
				},
				Stdout: func(line string) { stdout = append(stdout, line) },
				Client: srv.Client(),
			})
			if code != tc.code {
				t.Fatalf("code = %d, want %d; stdout %q", code, tc.code, stdout)
			}
			var node clicore.StatusNode
			if !decodeResultData(t, []byte(strings.Join(stdout, "\n")), &node) {
				t.Fatalf("stdout is not one result envelope: %q", stdout)
			}
			if node.ID != "env.doctor" || len(node.Children) != 10 || (node.State == clicore.StatusOK) != (tc.code == ExitSuccess) {
				t.Fatalf("node = %+v", node)
			}
			if len(control.calls) != 1 || !strings.HasPrefix(control.calls[0], "GET /v1/workspaces/0b8f3c9e-6a1d-4f2e-9c7b-3d5e8a1f2b4c/environments/prod/readiness?") ||
				!strings.Contains(control.calls[0], "migrated=shop%2Fworkloads%2Fapi") {
				t.Fatalf("calls = %q", control.calls)
			}
		})
	}
}

// TestEnvDoctorStrictIsABooleanFlag pins that the cloud CLI itself declares
// --strict boolean, so `env doctor --strict staging` checks staging rather
// than reading "staging" as the flag's value.
func TestEnvDoctorStrictIsABooleanFlag(t *testing.T) {
	if !isBooleanFlag("strict") {
		t.Fatal("strict is not a registered boolean flag")
	}
	params := parseFlags([]string{"doctor", "--strict", "staging"})
	if params["strict"] != true {
		t.Fatalf("strict = %#v, want true", params["strict"])
	}
}
