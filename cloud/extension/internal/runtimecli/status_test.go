package runtimecli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// captureIO records the human/structured output sinks so the render tests can
// assert on what reached stdout.
type captureIO struct {
	stdout []string
	stderr []string
}

func (c *captureIO) io(client *http.Client) clicore.IO {
	return clicore.IO{
		Client: client,
		Stdout: func(line string) { c.stdout = append(c.stdout, line) },
		Stderr: func(line string) { c.stderr = append(c.stderr, line) },
	}
}

const statusSample = `{"workspace_id":"ws-acme","deployments":[
	{"project":"accounts/workloads/auth-server","environment":"prod","revision":"auth-00002","url":"https://auth.example","state":"ready","last_release_id":"rel-2","last_deployed_at":"2026-07-06T12:30:00Z","commit_sha":"abc1234def5678","config_version":"rel-2","last_run_status":"Failed","last_run_reason":"revision auth-00003 did not become ready:\n  container exited 1","trigger":{"kind":"channel-move","namespace":"acmes","channel":"prod","generation":7,"source_revision":"def5678"}},
	{"project":"payments/workloads/api","environment":"staging","revision":"api-00007","url":"","state":"","last_release_id":"rel-9","last_deployed_at":"2026-07-06T09:00:00Z"},
	{"project":"tasks/workloads/api","environment":"prod","revision":"tasks-00004","url":"https://tasks.example","state":"ready","last_release_id":"rel-4","last_run_status":"Ready"}
]}`

// newStatusCtxFor builds a shared clicore.WorkspaceContext pointing at srv with
// the given output sink, bypassing the workspace-link/token resolution the entry
// point does (that is covered by the observability entry-point harness and the
// cli-core ResolveWorkspaceContext tests for the shared seam).
func newStatusCtxFor(srv *httptest.Server, cap *captureIO) *clicore.WorkspaceContext {
	return &clicore.WorkspaceContext{
		WorkspaceID:  "ws-acme",
		ControlPlane: srv.URL,
		AuthToken:    clicore.NewBearer("token"),
		IO:           cap.io(srv.Client()),
		RefreshAuth:  func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil },
	}
}

// TestStatusFetchHitsDeploymentsEndpoint pins the wire contract: the single GET
// targets /v1/workspaces/{workspace}/deployments and decodes the stable
// envelope.
func TestStatusFetchHitsDeploymentsEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	resp, err := statusFetch(newStatusCtxFor(srv, cap), context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/v1/workspaces/ws-acme/deployments" {
		t.Fatalf("path = %q, want /v1/workspaces/ws-acme/deployments", gotPath)
	}
	if resp.WorkspaceID != "ws-acme" || len(resp.Deployments) != 3 {
		t.Fatalf("resp = %+v, want ws-acme with 3 deployments", resp)
	}
}

// TestStatusHumanTableRendersColumns covers the human default: a compact table
// with the project/env/revision/health/last-run/trigger/url/reason columns, an
// empty state rendered as "unknown", and an empty url rendered as "-".
func TestStatusHumanTableRendersColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(cap.stdout) == 0 {
		t.Fatal("no stdout")
	}
	out := strings.Join(cap.stdout, "\n")
	for _, want := range []string{"PROJECT", "ENV", "REVISION", "HEALTH", "LAST RUN", "TRIGGER", "URL", "REASON",
		"accounts/workloads/auth-server", "auth-00002", "ready", "https://auth.example",
		"payments/workloads/api", "staging", "unknown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
	rows := statusRowsByProject(t, out)
	// A failed last run after a healthy serving release names the failure, the
	// channel move that opened it, and the reason — collapsed to one line.
	if got, want := rows["accounts/workloads/auth-server"], []string{"accounts/workloads/auth-server", "prod", "auth-00002", "ready", "Failed", "channel-move acmes/prod#7", "https://auth.example", "revision auth-00003 did not become ready: container exited 1"}; !slices.Equal(got, want) {
		t.Fatalf("failed-last-run row = %q, want %q\n%s", got, want, out)
	}
	// No run on record: every last-run cell and the empty URL render as "-".
	if got, want := rows["payments/workloads/api"], []string{"payments/workloads/api", "staging", "api-00007", "unknown", "-", "-", "-", "-"}; !slices.Equal(got, want) {
		t.Fatalf("no-last-run row = %q, want %q\n%s", got, want, out)
	}
	// A completed run no move opened reads "cli" with no reason.
	if got, want := rows["tasks/workloads/api"], []string{"tasks/workloads/api", "prod", "tasks-00004", "ready", "Ready", "cli", "https://tasks.example", "-"}; !slices.Equal(got, want) {
		t.Fatalf("cli-last-run row = %q, want %q\n%s", got, want, out)
	}
}

// statusRowsByProject splits the rendered table back into cells on the
// two-space column separator, keyed by project. Every cell in the sample is free
// of double spaces, so the split is exact. The CD header lines above the table
// are skipped: parsing starts after the PROJECT column header.
func statusRowsByProject(t *testing.T, table string) map[string][]string {
	t.Helper()
	rows := map[string][]string{}
	lines := strings.Split(table, "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "PROJECT") })
	if start < 0 {
		t.Fatalf("no table header in:\n%s", table)
	}
	for _, line := range lines[start+1:] {
		var cells []string
		for _, cell := range strings.Split(line, "  ") {
			if cell = strings.TrimSpace(cell); cell != "" {
				cells = append(cells, cell)
			}
		}
		if len(cells) > 0 {
			rows[cells[0]] = cells
		}
	}
	return rows
}

// TestStatusEmptyDeploymentsMessage: zero deployments prints a clear line and
// exits 0.
func TestStatusEmptyDeploymentsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws-acme","deployments":[]}`))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(cap.stdout) != 1 || !strings.Contains(cap.stdout[0], "No deployments") {
		t.Fatalf("stdout = %q, want a No-deployments line", cap.stdout)
	}
}

// TestStatusStructuredEmitsStableObject: --output=jsonl (and --json) emit the
// shared result envelope with the stable {workspace_id, deployments:[...]} data
// object, not per-row lines.
func TestStatusStructuredEmitsStableObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusSample))
	}))
	defer srv.Close()

	for _, mode := range []map[string]any{{"output": "jsonl"}, {"output": "json"}, {"json": true}} {
		cap := &captureIO{}
		if err := runStatus(t, srv, cap, mode); err != nil {
			t.Fatalf("Status %v: %v", mode, err)
		}
		joined := strings.Join(cap.stdout, "\n")
		var obj deploymentsSummary
		if err := decodeResultData([]byte(joined), &obj); err != nil {
			t.Fatalf("structured output for %v is not the stable object: %v\n%s", mode, err, joined)
		}
		if obj.WorkspaceID != "ws-acme" || len(obj.Deployments) != 3 {
			t.Fatalf("structured object = %+v, want ws-acme with 3 deployments", obj)
		}
		if obj.Deployments[0].Project != "accounts/workloads/auth-server" {
			t.Fatalf("deployment[0] project = %q", obj.Deployments[0].Project)
		}
		// The config_version rides the structured object unchanged by the
		// --provenance flag (structured output is flag-independent).
		if obj.Deployments[0].ConfigVersion != "rel-2" {
			t.Fatalf("deployment[0] config_version = %q, want rel-2", obj.Deployments[0].ConfigVersion)
		}
		// The last-run fields ride the structured object verbatim: the
		// reason keeps its line break, and the trigger keeps every field.
		failed := obj.Deployments[0]
		if failed.LastRunStatus != "Failed" || failed.LastRunReason != "revision auth-00003 did not become ready:\n  container exited 1" {
			t.Fatalf("deployment[0] last run = (%q, %q), want the server's exact status and reason", failed.LastRunStatus, failed.LastRunReason)
		}
		if failed.Trigger == nil || failed.Trigger.Kind != "channel-move" || failed.Trigger.Generation != 7 || failed.Trigger.SourceRevision != "def5678" {
			t.Fatalf("deployment[0] trigger = %+v, want the server's channel-move trigger", failed.Trigger)
		}
	}
}

// TestStatusProvenanceTableRendersColumns covers the --provenance human view: the
// per-workload provenance chain PROJECT | COMMIT | RELEASE | REVISION | CONFIG
// VERSION, the commit rendered as a short sha, and an absent commit/config
// version rendered as "-".
func TestStatusProvenanceTableRendersColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{"provenance": true}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := strings.Join(cap.stdout, "\n")
	for _, want := range []string{"PROJECT", "COMMIT", "TREE", "RELEASE", "REVISION", "CONFIG VERSION",
		"accounts/workloads/auth-server", "rel-2", "auth-00002",
		"payments/workloads/api", "api-00007", "rel-9"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provenance table missing %q:\n%s", want, out)
		}
	}
	// The commit renders as the 7-char short sha, never the full value.
	if !strings.Contains(out, "abc1234") || strings.Contains(out, "abc1234def5678") {
		t.Fatalf("commit not rendered as short sha:\n%s", out)
	}
	// The delivery row has no commit and no config version: the trailing config
	// version cell renders as "-" (the line ends with it), and the pinned row's
	// trailing cell is its config version rel-2.
	var deliveryLine, identityLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "payments/workloads/api") {
			deliveryLine = line
		}
		if strings.HasPrefix(line, "accounts/workloads/auth-server") {
			identityLine = line
		}
	}
	if !strings.HasSuffix(deliveryLine, "-") {
		t.Fatalf("absent config version not rendered as trailing -:\n%q", deliveryLine)
	}
	if !strings.HasSuffix(identityLine, "rel-2") {
		t.Fatalf("pinned config version not in trailing cell:\n%q", identityLine)
	}
}

// TestStatusNon200MapsToError: a non-200 maps to a clear error naming the fix
// (status:), ExitAuth for 401 and ExitAPI otherwise. Through control-api's
// generated client, the message is the status line, not the provider's
// free-text body: the generated client withholds it, the same disclosed
// trade-off the CD header and log-follow reads accept.
func TestStatusNon200MapsToError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   int
		want   string
	}{
		{"forbidden", http.StatusForbidden, `{"error":"missing platform.workspace.read"}`, clicore.ExitAPI, "403 Forbidden"},
		{"unauthorized", http.StatusUnauthorized, `{"error":"Authentication required"}`, clicore.ExitAuth, "401 Unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			cap := &captureIO{}
			ctx := newStatusCtxFor(srv, cap)
			ctx.RefreshAuth = func() (clicore.Bearer, error) {
				return clicore.Bearer{}, clicore.NewError("session is not refreshable", clicore.ExitAuth)
			}
			_, err := statusFetch(ctx, context.Background())
			if err == nil {
				t.Fatalf("want an error for %d", tc.status)
			}
			if clicore.ExitCode(err) != tc.code {
				t.Fatalf("exit code = %d, want %d (err=%v)", clicore.ExitCode(err), tc.code, err)
			}
			if !strings.HasPrefix(err.Error(), "status:") {
				t.Fatalf("error %q should be prefixed with status:", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.want)
			}
		})
	}
}

// runStatus drives the same post-resolve path the Status entry point runs
// (statusRun) through a shared WorkspaceContext wired to srv. Params select
// human vs structured output and the --env header selection. The workspace
// root is an empty directory, so no test reads the ambient git checkout.
func runStatus(t *testing.T, srv *httptest.Server, cap *captureIO, params map[string]any) error {
	t.Helper()
	return statusRun(newStatusCtxFor(srv, cap), context.Background(), params, t.TempDir())
}

func decodeResultData(data []byte, dest any) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	payload, ok := probe["data"]
	if !ok {
		payload = data
	}
	return json.Unmarshal(payload, dest)
}

// The --workspace override (an explicit --workspace must win over the
// linked workspace, so status queries — and mints auth for — the caller's
// workspace) is now enforced by the shared seam. Its unit coverage moved to
// clicore.TestResolveWorkspaceID_FlagOverridesLink when statusWorkspaceID folded
// into clicore.ResolveWorkspaceID; the entry-point harness exercises the full
// resolver for all three commands.

// TestStatusTriggerRendersUnknownWhenTheLedgerWasUnreadable pins that an
// absent trigger reads "cli" (a person submitted the run) only when the server
// resolved the ledger; when it could not, the cell reads "unknown".
func TestStatusTriggerRendersUnknownWhenTheLedgerWasUnreadable(t *testing.T) {
	row := deploymentSummaryEntry{Project: "tasks/workloads/api", LastRunStatus: "Failed"}
	if got := statusTrigger(row, false); got != "cli" {
		t.Errorf("resolved ledger, no trigger: %q, want cli", got)
	}
	if got := statusTrigger(row, true); got != "unknown" {
		t.Errorf("unreadable ledger, no trigger: %q, want unknown", got)
	}
	if got := statusTrigger(deploymentSummaryEntry{Project: "tasks/workloads/api"}, true); got != "-" {
		t.Errorf("no last run: %q, want -", got)
	}
	moved := row
	moved.Trigger = &deployTrigger{Kind: "channel-move", Namespace: "acme", Channel: "canary", Generation: 7}
	if got := statusTrigger(moved, true); got != "channel-move acme/canary#7" {
		t.Errorf("resolved trigger: %q, want the move", got)
	}
}
