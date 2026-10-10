package runtimecli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// deployStatusSample is a finished release the fake control plane echoes for the
// deploy-status tests: one Ready project, one Failed project (state=Partial).
const deployStatusSample = `{"release_id":"rel_x","state":"Partial","projects":[
	{"name":"accounts/workloads/auth-server","status":"Ready","action":"roll","url":"https://auth.example","revision":"auth-00002"},
	{"name":"config/server","status":"Failed","action":"roll","detail":"migrate job failed"}
]}`

// newDeployStatusCtxFor builds a deployCtx pointing at srv with the given output
// sink, bypassing the workspace-link/token resolution the entry point does.
func newDeployStatusCtxFor(srv *httptest.Server, cap *captureIO) *deployCtx {
	return &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("token"),
			IO:           cap.io(srv.Client()),
			RefreshAuth:  func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil },
		},
		environment: "prod",
	}
}

// runDeployStatus drives the fetch+render+exit path through a deployCtx wired to
// srv so the render tests exercise the same code DeployStatus runs after resolving
// workspace/auth. Params select human vs structured output and --strict.
func runDeployStatus(t *testing.T, srv *httptest.Server, cap *captureIO, params map[string]any) error {
	t.Helper()
	ctx := newDeployStatusCtxFor(srv, cap)
	// Mirror DeployStatus's typed read path: read the reply into
	// deployStatusResponse and print its status node.
	resp, status, err := deployStatusFetch(context.Background(), ctx, "rel_x")
	if err != nil {
		if status == http.StatusUnauthorized {
			return clicore.NewError(err.Error(), clicore.ExitAuth)
		}
		return err
	}
	return clicore.WriteStatus(params, ctx.IO, DeployStatusNodeFrom("rel_x", resp))
}

// TestDeployStatus_RendersTableAndExitsNonZeroOnFailure: the human default renders
// the release and one check per workload, and a Partial release fails the
// command with exit 1, the rule every status command shares.
func TestDeployStatus_RendersTableAndExitsNonZeroOnFailure(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(deployStatusSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	err := runDeployStatus(t, srv, cap, map[string]any{})
	if err == nil {
		t.Fatal("Partial release must exit non-zero")
	}
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit code = %d, want ExitFailure", clicore.ExitCode(err))
	}
	if gotPath != "/v1/workspaces/ws-acme/deploy/rel_x" {
		t.Fatalf("path = %q, want /v1/workspaces/ws-acme/deploy/rel_x", gotPath)
	}
	out := strings.Join(cap.stdout, "\n")
	for _, want := range []string{
		"release rel_x  failing  partial, 1 of 2 workloads serving",
		"ok       accounts/workloads/auth-server  ready, revision auth-00002",
		"failing  config/server                   failed, migrate job failed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

// TestDeployStatus_ProvisioningIsDegraded: a still-Provisioning release is
// degraded. It exits 0 by default and 1 under --strict, so a CI gate that asks
// for the terminal outcome never reads an in-flight release as green. The
// report names the command to check again.
func TestDeployStatus_ProvisioningIsDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"release_id":"rel_x","state":"Provisioning","environment":"prod","projects":[
			{"name":"accounts/workloads/auth-server","status":"provisioning","action":"roll"}
		]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runDeployStatus(t, srv, cap, map[string]any{}); err != nil {
		t.Fatalf("a degraded release exits 0 without --strict: %v", err)
	}
	out := strings.Join(cap.stdout, "\n")
	if !strings.Contains(out, "release rel_x  degraded  provisioning, environment prod") ||
		!strings.Contains(out, "next: putnami cloud deploy status rel_x") {
		t.Fatalf("report:\n%s", out)
	}
	err := runDeployStatus(t, srv, &captureIO{}, map[string]any{"strict": true})
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("strict exit code = %d (%v), want ExitFailure", clicore.ExitCode(err), err)
	}
}

// TestReleaseExitStrict_Mapping pins the terminal verdict mapping directly: Ready
// exits 0; Partial/Failed and any non-terminal state (Provisioning / blank) are
// non-zero.
func TestReleaseExitStrict_Mapping(t *testing.T) {
	if err := releaseExitStrict("Ready", "rel_x"); err != nil {
		t.Errorf("Ready must exit 0, got %v", err)
	}
	for _, state := range []string{"Partial", "Failed", "Provisioning", ""} {
		if err := releaseExitStrict(state, "rel_x"); err == nil {
			t.Errorf("state %q must exit non-zero under the strict verdict", state)
		} else if clicore.ExitCode(err) != clicore.ExitAPI {
			t.Errorf("state %q exit code = %d, want ExitAPI", state, clicore.ExitCode(err))
		}
	}
}

// TestDeployStatus_StructuredEmitsTheNode: --output=jsonl / json / --json all
// emit the shared result envelope with the release node as data, on failure
// too.
func TestDeployStatus_StructuredEmitsTheNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(deployStatusSample))
	}))
	defer srv.Close()

	for _, mode := range []map[string]any{{"output": "jsonl"}, {"output": "json"}, {"json": true}} {
		cap := &captureIO{}
		err := runDeployStatus(t, srv, cap, mode)
		if len(cap.stdout) != 0 {
			t.Fatalf("structured failure printed %q before the envelope", cap.stdout)
		}
		envelope := &captureIO{}
		clicore.WriteErrorResult(err, mode, envelope.io(nil))
		joined := strings.Join(envelope.stdout, "\n")
		var node clicore.StatusNode
		if err := decodeResultData([]byte(joined), &node); err != nil {
			t.Fatalf("structured output for %v is not a single object: %v\n%s", mode, err, joined)
		}
		if node.ID != "deploy" || node.Title != "release rel_x" || node.State != clicore.StatusFailing || len(node.Children) != 2 {
			t.Fatalf("structured node = %+v", node)
		}
	}
}

// TestDeployStatus_Non200MapsToError: a non-200 maps to a clear error, ExitAuth for
// 401 and ExitAPI otherwise.
func TestDeployStatus_Non200MapsToError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"not-found", http.StatusNotFound, `{"error":"release not found"}`, clicore.ExitAPI},
		{"unauthorized", http.StatusUnauthorized, `{"error":"Authentication required"}`, clicore.ExitAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			cap := &captureIO{}
			ctx := newDeployStatusCtxFor(srv, cap)
			ctx.RefreshAuth = func() (clicore.Bearer, error) {
				return clicore.Bearer{}, clicore.NewError("session is not refreshable", clicore.ExitAuth)
			}
			err := runDeployStatus(t, srv, cap, map[string]any{})
			if err == nil {
				t.Fatalf("want an error for %d", tc.status)
			}
			if clicore.ExitCode(err) != tc.code {
				t.Fatalf("exit code = %d, want %d (err=%v)", clicore.ExitCode(err), tc.code, err)
			}
		})
	}
}

// TestDeployStatus_NoReleaseID: a missing release-id is an ExitUsage error that
// names where release ids are printed. The parent CLI's --putnamiContext file
// is never read as the id.
func TestDeployStatus_NoReleaseID(t *testing.T) {
	for _, args := range [][]string{nil, {"--putnamiContext", "/tmp/context.json"}} {
		err := DeployStatus(map[string]any{}, args, t.TempDir(), map[string]string{}, clicore.IO{})
		if clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("args %q: exit code = %d (%v), want ExitUsage", args, clicore.ExitCode(err), err)
		}
		if !strings.Contains(err.Error(), "usage:") || !strings.Contains(err.Error(), "putnami cloud env status") {
			t.Fatalf("error %q should name the usage and where ids are printed", err.Error())
		}
	}
}

// TestDeployStatus_RefusesAReleaseSet: an rs_ id is a release set, not a
// release; the usage error says so and names where releases are printed.
func TestDeployStatus_RefusesAReleaseSet(t *testing.T) {
	err := DeployStatus(map[string]any{}, []string{"rs_ab12cd34ef567890"}, t.TempDir(), map[string]string{}, clicore.IO{})
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d (%v), want ExitUsage", clicore.ExitCode(err), err)
	}
	for _, want := range []string{"is a release set", "not a release", "putnami cloud env status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err.Error(), want)
		}
	}
}

// TestDeployStatusNodeFrom pins the fold: a held workload makes a Ready
// release degraded, and the counts and the wall clock are metrics.
func TestDeployStatusNodeFrom(t *testing.T) {
	traffic := 100
	wall := int64(95500)
	node := DeployStatusNodeFrom("cm_1", &deployStatusResponse{releaseResponse: releaseResponse{
		ReleaseID: "cm_1", State: "Ready", Environment: "prod",
		Projects: []releaseProject{
			{Name: "api", Status: "Ready", Action: "roll", CandidateRevision: "api-2", ServingRevision: "api-2", TrafficPercent: &traffic},
			{Name: "web", Status: "Ready", Action: "roll", CandidateRevision: "web-3", ServingRevision: "web-2", HoldReason: "candidate web-3 receives no traffic"},
		},
		Timings: &releaseTimings{TotalWallClockMS: &wall},
	}})
	if node.State != clicore.StatusDegraded || node.Detail != "ready, environment prod, 1 of 2 workloads serving" {
		t.Fatalf("node = %+v", node)
	}
	if node.Children[0].State != clicore.StatusOK || node.Children[0].Detail != "ready, revision api-2, traffic 100%" {
		t.Fatalf("api = %+v", node.Children[0])
	}
	if node.Children[1].State != clicore.StatusDegraded || !strings.Contains(node.Children[1].Detail, "held: candidate web-3 receives no traffic") {
		t.Fatalf("web = %+v", node.Children[1])
	}
	metrics := map[string]float64{}
	for _, metric := range node.Metrics {
		metrics[metric.ID] = metric.Value
	}
	if metrics["workloads_requested"] != 2 || metrics["workloads_serving"] != 1 || metrics["workloads_held"] != 1 || metrics["total_wall_clock"] != 95.5 {
		t.Fatalf("metrics = %+v", node.Metrics)
	}
}

// TestRenderRelease_StructuredOmitsFollowUp: the deploy-time render must keep the
// Partial/Failed follow-up guidance out of structured output too — only the result
// map reaches --output.
func TestRenderRelease_StructuredOmitsFollowUp(t *testing.T) {
	cap := &captureIO{}
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{WorkspaceID: "ws-acme", IO: clicore.IO{}}, environment: "prod"}
	targets := []deployTarget{
		{app: "accounts/workloads/auth-server", version: "1.0.0"},
		{app: "config/server", version: "1.0.0"},
	}
	resp := &deployResponse{
		ReleaseID: "rel_x",
		State:     "Failed",
		Projects: []releaseProject{
			{Name: "accounts/workloads/auth-server", Status: "Ready", Action: "roll"},
			{Name: "config/server", Status: "Failed", Action: "roll"},
		},
	}

	// Structured mode: no follow-up text, single object.
	_ = renderRelease(map[string]any{"output": "jsonl"}, ctx, cap.io(nil), "rel_x", targets, resp)
	structured := strings.Join(cap.stdout, "\n")
	if strings.Contains(structured, "Next steps") || strings.Contains(structured, "deploy status rel_x") {
		t.Fatalf("structured output leaked follow-up text:\n%s", structured)
	}

	// Human mode: the follow-up commands are present.
	cap.stdout = nil
	_ = renderRelease(map[string]any{}, ctx, cap.io(nil), "rel_x", targets, resp)
	human := strings.Join(cap.stdout, "\n")
	for _, want := range []string{
		"putnami cloud logs config/server --env prod --since deploy:last --level error",
		"putnami cloud deploy status rel_x",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output missing %q:\n%s", want, human)
		}
	}
}
