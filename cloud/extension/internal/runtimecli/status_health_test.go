package runtimecli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The exact bytes `putnami cloud env status --env prod` prints without
// --health.
const (
	statusGoldenHuman = "CD prod ← canary: gen 42 converged 2026-09-18T10:02Z (rs_ab12cd34…); ← stable: no move received yet\n" +
		"\n" +
		"PROJECT                         ENV      REVISION     HEALTH   LAST RUN  TRIGGER                    URL                    REASON\n" +
		"accounts/workloads/auth-server  prod     auth-00002   ready    Failed    channel-move acmes/prod#7  https://auth.example   revision auth-00003 did not become ready: container exited 1\n" +
		"payments/workloads/api          staging  api-00007    unknown  -         -                          -                      -\n" +
		"tasks/workloads/api             prod     tasks-00004  ready    Ready     cli                        https://tasks.example  -"
	statusGoldenProvenance = "CD prod ← canary: gen 42 converged 2026-09-18T10:02Z (rs_ab12cd34…); ← stable: no move received yet\n" +
		"\n" +
		"PROJECT                         COMMIT   TREE  RELEASE  REVISION     CONFIG VERSION\n" +
		"accounts/workloads/auth-server  abc1234  -     rel-2    auth-00002   rel-2\n" +
		"payments/workloads/api          -        -     rel-9    api-00007    -\n" +
		"tasks/workloads/api             -        -     rel-4    tasks-00004  -"
	statusGoldenStructured = `{"protocolVersion":2,"command":"cloud","status":"success","exitCode":0,"data":{"workspace_id":"ws-acme","deployments":[` +
		`{"project":"accounts/workloads/auth-server","environment":"prod","revision":"auth-00002","url":"https://auth.example","state":"ready","last_release_id":"rel-2","last_deployed_at":"2026-07-06T12:30:00Z","commit_sha":"abc1234def5678","config_version":"rel-2","last_run_status":"Failed","last_run_reason":"revision auth-00003 did not become ready:\n  container exited 1","trigger":{"kind":"channel-move","namespace":"acmes","channel":"prod","generation":7,"source_revision":"def5678"}},` +
		`{"project":"payments/workloads/api","environment":"staging","revision":"api-00007","last_release_id":"rel-9","last_deployed_at":"2026-07-06T09:00:00Z"},` +
		`{"project":"tasks/workloads/api","environment":"prod","revision":"tasks-00004","url":"https://tasks.example","state":"ready","last_release_id":"rel-4","last_run_status":"Ready"}],` +
		`"channel_follow":[{"environment":"prod","definition_revision":4,"channels":[{"channel":"canary","receipt":{"namespace":"acme","channel":"canary","generation":42,"release_set_id":"rs_ab12cd34ef567890","moved_at":"2026-09-18T10:02:30Z","received_at":"2026-09-18T10:02:31Z","disposition":"converged","release_ids":[],"settled":true}},{"channel":"stable"}]}]}}`
)

// TestStatusWithoutHealthIsByteIdentical pins the human, provenance, and
// structured output of `putnami cloud env status` without --health, byte for
// byte, so the health columns, notes, and JSON key never leak into the
// default views.
func TestStatusWithoutHealthIsByteIdentical(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"human", map[string]any{"env": "prod"}, statusGoldenHuman},
		{"provenance", map[string]any{"env": "prod", "provenance": true}, statusGoldenProvenance},
		{"structured", map[string]any{"env": "prod", "output": "jsonl"}, statusGoldenStructured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureIO{}
			if err := runStatus(t, srv, cap, tc.params); err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got := strings.Join(cap.stdout, "\n"); got != tc.want {
				t.Fatalf("output changed:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

const logsPath = "/v1/workspaces/ws-acme/logs"

// statusHealthNow is the fixed clock of the --health tests: the read window is
// 10:02:30Z to 10:12:30Z (the sub-second part is dropped).
var statusHealthNow = time.Date(2026, 9, 18, 10, 12, 30, 500_000_000, time.UTC)

// prodErrors is one page of prod ERROR entries: three for auth-server under
// its folded deploy label, one for tasks under its raw app path, one without a
// service label, and two whose label names no prod row (a service with no
// deployment row, and a staging project's label).
const prodErrors = `{"entries":[
	{"timestamp":"2026-09-18T10:12:00Z","severity":17,"severityText":"ERROR","body":"{\"level\":\"error\",\"message\":\"db timeout\\n  at pool.go:12\"}","attributes":{"service":"accounts-workloads-auth-server","environment":"prod","workspace":"ws-acme"}},
	{"timestamp":"2026-09-18T10:11:00Z","severity":17,"body":"db timeout","attributes":{"service":"accounts-workloads-auth-server"}},
	{"timestamp":"2026-09-18T10:10:00Z","severity":21,"body":"  panic: assignment to entry in nil map  \n\ngoroutine 1 [running]:","attributes":{"service":"accounts-workloads-auth-server"}},
	{"timestamp":"2026-09-18T10:09:00Z","severity":17,"body":"{\"msg\":\"upstream answered 502\"}","attributes":{"service":"tasks/workloads/api"}},
	{"timestamp":"2026-09-18T10:08:00Z","severity":17,"body":"no label on this one"},
	{"timestamp":"2026-09-18T10:07:00Z","severity":17,"body":"x","attributes":{"service":"cloud-ci-worker"}},
	{"timestamp":"2026-09-18T10:06:00Z","severity":17,"body":"y","attributes":{"service":"payments-workloads-api"}}
]}`

// runStatusHealth runs statusRun with --health semantics on the fixed clock,
// with workspaceRoot as the git checkout.
func runStatusHealth(t *testing.T, srv *httptest.Server, cap *captureIO, params map[string]any, workspaceRoot string) error {
	t.Helper()
	ctx := newStatusCtxFor(srv, cap)
	ctx.IO.Now = func() time.Time { return statusHealthNow }
	return statusRun(ctx, context.Background(), params, workspaceRoot)
}

// gitRepositoryEnv names the variables that point git at a repository other
// than the one in its working directory: the output of
// `git rev-parse --local-env-vars`, plus GIT_NAMESPACE and the discovery
// variables.
var gitRepositoryEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_NAMESPACE",
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// hermeticGit isolates every git process of the test from the machine's
// configuration, and stops repository discovery above dir, so an empty
// directory is never read as part of an enclosing checkout. It unsets
// gitRepositoryEnv first: a test run by `git bisect run` or a hook inherits
// GIT_DIR, and without this `git init` in dir rewrites that repository.
func hermeticGit(t *testing.T, dir string) {
	t.Helper()
	for _, key := range gitRepositoryEnv {
		t.Setenv(key, "") // restores the inherited value at cleanup
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// statusGitFixture builds a checkout whose main branch has one commit and
// whose feature branch has one more, the shape of a pull-request head that
// was squash-merged: its exact commit is not on main.
func statusGitFixture(t *testing.T) (root, onMain, offMain string) {
	t.Helper()
	root = t.TempDir()
	hermeticGit(t, root)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "checkout", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "on main")
	onMain = gitIn(t, root, "rev-parse", "HEAD")
	gitIn(t, root, "checkout", "-q", "-b", "feature")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "pull request head")
	offMain = gitIn(t, root, "rev-parse", "HEAD")
	return root, onMain, offMain
}

// TestStatusHealthGroupsErrorsByServiceLabel is the human contract: one logs
// read for prod, entries attributed to a row by its folded or raw service
// label, the most frequent message as TOP ERROR, "-" on the staging row, and
// one note naming the entries no row could claim.
func TestStatusHealthGroupsErrorsByServiceLabel(t *testing.T) {
	root := t.TempDir()
	hermeticGit(t, root)
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath:    jsonReply(http.StatusOK, prodErrors),
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true}, root); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := strings.Join(cap.stdout, "\n")
	lines := strings.Split(out, "\n")
	wantNotes := []string{
		"CD prod ← canary: gen 42 converged 2026-09-18T10:02Z (rs_ab12cd34…); ← stable: no move received yet",
		"",
		"Errors prod: 1 entry without a service label; 2 entries from services matching no row: cloud-ci-worker (1), payments-workloads-api (1)",
		"On main: unknown (not a git checkout)",
		"",
	}
	if len(lines) < len(wantNotes) || !slices.Equal(lines[:len(wantNotes)], wantNotes) {
		t.Fatalf("notes = %q, want %q", lines[:min(len(lines), len(wantNotes))], wantNotes)
	}
	if got, want := statusCells(lines[len(wantNotes)]), []string{"PROJECT", "ENV", "REVISION", "HEALTH", "LAST RUN", "TRIGGER", "URL", "ERRORS (10m)", "TOP ERROR", "ON MAIN", "REASON"}; !slices.Equal(got, want) {
		t.Fatalf("header = %q, want the health columns between URL and REASON", got)
	}
	rows := statusRowsByProject(t, out)
	if got, want := rows["accounts/workloads/auth-server"], []string{"accounts/workloads/auth-server", "prod", "auth-00002", "ready", "Failed", "channel-move acmes/prod#7", "https://auth.example", "3", "db timeout", "unknown", "revision auth-00003 did not become ready: container exited 1"}; !slices.Equal(got, want) {
		t.Fatalf("auth-server row = %q, want %q\n%s", got, want, out)
	}
	if got, want := rows["payments/workloads/api"], []string{"payments/workloads/api", "staging", "api-00007", "unknown", "-", "-", "-", "-", "-", "-", "-"}; !slices.Equal(got, want) {
		t.Fatalf("staging row = %q, want %q\n%s", got, want, out)
	}
	if got, want := rows["tasks/workloads/api"], []string{"tasks/workloads/api", "prod", "tasks-00004", "ready", "Ready", "cli", "https://tasks.example", "1", "upstream answered 502", "unknown", "-"}; !slices.Equal(got, want) {
		t.Fatalf("tasks row = %q, want %q\n%s", got, want, out)
	}

	// --provenance appends the same three columns.
	cap = &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true, "provenance": true}, root); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out = strings.Join(cap.stdout, "\n")
	header := slices.IndexFunc(strings.Split(out, "\n"), func(line string) bool { return strings.HasPrefix(line, "PROJECT") })
	if got, want := statusCells(strings.Split(out, "\n")[header]), []string{"PROJECT", "COMMIT", "TREE", "RELEASE", "REVISION", "CONFIG VERSION", "ERRORS (10m)", "TOP ERROR", "ON MAIN"}; !slices.Equal(got, want) {
		t.Fatalf("provenance header = %q, want the health columns appended", got)
	}
	if got, want := statusRowsByProject(t, out)["accounts/workloads/auth-server"], []string{"accounts/workloads/auth-server", "abc1234", "-", "rel-2", "auth-00002", "rel-2", "3", "db timeout", "unknown"}; !slices.Equal(got, want) {
		t.Fatalf("provenance auth-server row = %q, want %q\n%s", got, want, out)
	}
}

// TestStatusHealthIsOneBoundedLogsReadPerEnvironment pins the request: one GET
// for the whole environment at level error over the last 10 minutes, a full
// page, and no service filter.
func TestStatusHealthIsOneBoundedLogsReadPerEnvironment(t *testing.T) {
	var queries []url.Values
	var mu sync.Mutex
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, r.URL.Query())
			mu.Unlock()
			jsonReply(http.StatusOK, prodErrors)(w, r)
		},
	}))
	defer srv.Close()

	if err := runStatusHealth(t, srv, &captureIO{}, map[string]any{"env": "prod", "health": true}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("logs requests = %d, want exactly 1 for the environment", len(queries))
	}
	want := url.Values{
		"environment": {"prod"},
		"level":       {"error"},
		"from":        {"2026-09-18T10:02:30Z"},
		"to":          {"2026-09-18T10:12:30Z"},
		"limit":       {"1000"},
	}
	if got := queries[0]; got.Encode() != want.Encode() {
		t.Fatalf("logs query = %q, want %q", got.Encode(), want.Encode())
	}
}

// TestStatusHealthCountIsHonestAtThePageCap pins the cap: the read follows the
// cursor with the same window for statusHealthMaxPages pages, then stops.
// Every count renders "N+", the note says the counts are lower bounds, and the
// JSON says capped.
func TestStatusHealthCountIsHonestAtThePageCap(t *testing.T) {
	var queries []url.Values
	var mu sync.Mutex
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, r.URL.Query())
			page := len(queries)
			mu.Unlock()
			jsonReply(http.StatusOK, fmt.Sprintf(`{"entries":[{"body":"db timeout","attributes":{"service":"accounts-workloads-auth-server"}}],"nextCursor":"c%d"}`, page))(w, r)
		},
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(queries) != statusHealthMaxPages {
		t.Fatalf("logs requests = %d, want the %d-page cap", len(queries), statusHealthMaxPages)
	}
	for page, query := range queries {
		if query.Get("from") != "2026-09-18T10:02:30Z" || query.Get("to") != "2026-09-18T10:12:30Z" {
			t.Fatalf("page %d window = %s..%s, want every page on the first page's window", page+1, query.Get("from"), query.Get("to"))
		}
		if wantCursor := map[int]string{0: "", 1: "c1", 2: "c2"}[page]; query.Get("cursor") != wantCursor {
			t.Fatalf("page %d cursor = %q, want %q", page+1, query.Get("cursor"), wantCursor)
		}
	}
	out := strings.Join(cap.stdout, "\n")
	if !strings.Contains(out, "\nErrors prod: the read stopped after 3 entries, so counts are lower bounds\n") {
		t.Fatalf("missing the capped note:\n%s", out)
	}
	rows := statusRowsByProject(t, out)
	if got := rows["accounts/workloads/auth-server"][7]; got != "3+" {
		t.Fatalf("auth-server errors = %q, want 3+", got)
	}
	if got := rows["tasks/workloads/api"][7]; got != "0+" {
		t.Fatalf("tasks errors = %q, want 0+: a capped zero is a lower bound too", got)
	}

	cap = &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true, "output": "json"}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	health := decodeStatusHealth(t, cap)
	if health.Logs == nil || !health.Logs.Capped || health.Logs.Pages != statusHealthMaxPages || health.Logs.Entries != 3 || health.Logs.Stopped != "" {
		t.Fatalf("logs = %+v, want capped after %d pages and 3 entries", health.Logs, statusHealthMaxPages)
	}
}

// TestStatusHealthLaterPageFailureCapsTheCount: a page after the first that
// fails keeps what was read, as a lower bound, and says why the read stopped.
func TestStatusHealthLaterPageFailureCapsTheCount(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "" {
				jsonReply(http.StatusOK, `{"entries":[{"body":"db timeout","attributes":{"service":"accounts-workloads-auth-server"}}],"nextCursor":"c1"}`)(w, r)
				return
			}
			jsonReply(http.StatusBadGateway, `{"error":"Cloud Logging entries:list failed"}`)(w, r)
		},
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := strings.Join(cap.stdout, "\n")
	if !strings.Contains(out, "\nErrors prod: the read stopped after 1 entry (502 Bad Gateway), so counts are lower bounds\n") {
		t.Fatalf("missing the stopped note:\n%s", out)
	}
	if got := statusRowsByProject(t, out)["accounts/workloads/auth-server"][7]; got != "1+" {
		t.Fatalf("auth-server errors = %q, want 1+", got)
	}
}

// TestStatusHealthNeverFailsTheCommand pins the degradation contract of the
// logs read: a machine token's 403, an older control plane's 404, a backend
// failure, an undecodable body, and a transport failure each print
// `Errors prod: unavailable (<reason>)`, render "-" in the error cells, keep
// ON MAIN, and leave the command green with its table.
func TestStatusHealthNeverFailsTheCommand(t *testing.T) {
	cases := []struct {
		name  string
		reply http.HandlerFunc
		want  string
	}{
		{"machine token refused", jsonReply(http.StatusForbidden, `{"error":"Forbidden","message":"machine tokens cannot read logs"}`), "Errors prod: unavailable (403 Forbidden)"},
		{"route absent on an older control plane", nil, "Errors prod: unavailable (404 Not Found)"},
		{"telemetry backend failure", jsonReply(http.StatusBadGateway, `{"error":"Upstream telemetry credential/backend failure"}`), "Errors prod: unavailable (502 Bad Gateway)"},
		{"undecodable body", jsonReply(http.StatusOK, `{"entries":"nope"`), "Errors prod: unavailable (invalid response)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			hermeticGit(t, root)
			routes := map[string]http.HandlerFunc{
				summaryPath: jsonReply(http.StatusOK, statusSample),
				prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
			}
			if tc.reply != nil {
				routes[logsPath] = tc.reply
			}
			srv := httptest.NewServer(statusRoutes(routes))
			defer srv.Close()

			cap := &captureIO{}
			if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true}, root); err != nil {
				t.Fatalf("Status failed on a degraded logs read: %v", err)
			}
			out := strings.Join(cap.stdout, "\n")
			if !strings.Contains(out, "\n\n"+tc.want+"\nOn main: unknown (not a git checkout)\n\nPROJECT") {
				t.Fatalf("output misses %q above the table:\n%s", tc.want, out)
			}
			if got := statusRowsByProject(t, out)["tasks/workloads/api"][7:10]; !slices.Equal(got, []string{"-", "-", "unknown"}) {
				t.Fatalf("degraded tasks cells = %q, want - - unknown", got)
			}

			cap = &captureIO{}
			if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true, "output": "json"}, root); err != nil {
				t.Fatalf("Status: %v", err)
			}
			health := decodeStatusHealth(t, cap)
			if health.Logs != nil || health.Unavailable != strings.TrimSuffix(strings.TrimPrefix(tc.want, "Errors prod: unavailable ("), ")") {
				t.Fatalf("health = %+v, want null logs and the unavailable reason", health)
			}
			for _, service := range health.Services {
				if service.Errors != nil || service.TopError != nil {
					t.Fatalf("service %s = %+v, want null errors when the read failed", service.Project, service)
				}
			}
		})
	}

	// A transport failure is the same degradation, with a short reason and no URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ctx := newStatusCtxFor(srv, &captureIO{})
	ctx.ControlPlane = "http://127.0.0.1:1"
	read := readStatusErrors(ctx, context.Background(), "prod", statusHealthNow)
	if !strings.HasPrefix(read.Unavailable, "request failed: ") || strings.Contains(read.Unavailable, "http://") || read.Pages != 0 {
		t.Fatalf("transport failure = %+v, want a short request-failed reason", read)
	}
}

// TestStatusHealthNeverReMintsTheSession: the logs read runs beside the
// summary, so a 401 on it degrades instead of re-minting the shared session.
func TestStatusHealthNeverReMintsTheSession(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath:    jsonReply(http.StatusUnauthorized, `{"error":"Authentication required"}`),
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newStatusCtxFor(srv, cap)
	var remints atomic.Int32
	ctx.RefreshAuth = func() (clicore.Bearer, error) {
		remints.Add(1)
		return clicore.NewBearer("fresh"), nil
	}
	if err := statusRun(ctx, context.Background(), map[string]any{"env": "prod", "health": true}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := remints.Load(); got != 0 {
		t.Fatalf("re-mints = %d, want 0: the logs read must never re-mint the session", got)
	}
	if out := strings.Join(cap.stdout, "\n"); !strings.Contains(out, "\nErrors prod: unavailable (401 Unauthorized)\n") {
		t.Fatalf("missing the 401 degradation:\n%s", out)
	}
}

// TestStatusHealthReadsLogsBesideTheSummaryAndHeader proves the three reads
// overlap: no handler answers until all three requests have arrived. A serial
// implementation would never send the later requests and time out.
func TestStatusHealthReadsLogsBesideTheSummaryAndHeader(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(3)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	barrier := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			arrived.Done()
			select {
			case <-all:
			case <-time.After(5 * time.Second):
				t.Errorf("%s waited alone: the summary, header, and logs reads are serial", r.URL.Path)
			}
			jsonReply(http.StatusOK, body)(w, r)
		}
	}
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: barrier(statusSample),
		prodCDPath:  barrier(prodChannelFollow),
		logsPath:    barrier(prodErrors),
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := statusRowsByProject(t, strings.Join(cap.stdout, "\n"))["accounts/workloads/auth-server"][7]; got != "3" {
		t.Fatalf("auth-server errors = %q, want 3", got)
	}
}

// TestStatusHealthStructuredOutput pins the additive health key: every field
// the columns and notes read, while the existing keys keep their shapes.
func TestStatusHealthStructuredOutput(t *testing.T) {
	root := t.TempDir()
	hermeticGit(t, root)
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		logsPath:    jsonReply(http.StatusOK, prodErrors),
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatusHealth(t, srv, cap, map[string]any{"env": "prod", "health": true, "output": "json"}, root); err != nil {
		t.Fatalf("Status: %v", err)
	}
	joined := strings.Join(cap.stdout, "\n")
	var raw map[string]json.RawMessage
	if err := decodeResultData([]byte(joined), &raw); err != nil {
		t.Fatalf("decode: %v\n%s", err, joined)
	}
	for _, key := range []string{"workspace_id", "deployments", "channel_follow", "health"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("structured object misses %q: %s", key, joined)
		}
	}
	var summary deploymentsSummary
	if err := decodeResultData([]byte(joined), &summary); err != nil || len(summary.Deployments) != 3 {
		t.Fatalf("summary keys changed: (%+v, %v)", summary, err)
	}

	health := decodeStatusHealth(t, cap)
	if health.Environment != "prod" || health.Window != "10m" || health.From != "2026-09-18T10:02:30Z" || health.To != "2026-09-18T10:12:30Z" || health.Level != "error" {
		t.Fatalf("health window = %+v, want prod, 10m, 10:02:30Z..10:12:30Z, error", health)
	}
	if health.MainRef != "" || health.MainRefUnavailable != "not a git checkout" {
		t.Fatalf("main ref = (%q, %q), want the not-a-checkout reason", health.MainRef, health.MainRefUnavailable)
	}
	wantLogs := statusLogsSummary{Entries: 7, Pages: 1, Unlabeled: 1, Unmatched: []statusUnmatchedService{{Service: "cloud-ci-worker", Errors: 1}, {Service: "payments-workloads-api", Errors: 1}}}
	if health.Logs == nil || health.Logs.Entries != wantLogs.Entries || health.Logs.Pages != 1 || health.Logs.Capped || health.Logs.Unlabeled != 1 || !slices.Equal(health.Logs.Unmatched, wantLogs.Unmatched) {
		t.Fatalf("logs = %+v, want %+v", health.Logs, wantLogs)
	}
	if len(health.Services) != 2 {
		t.Fatalf("services = %+v, want the two prod rows only", health.Services)
	}
	auth, tasks := health.Services[0], health.Services[1]
	if auth.Project != "accounts/workloads/auth-server" || auth.CommitSHA != "abc1234def5678" || auth.OnMain != "unknown" || auth.OnMainReason != "not a git checkout" ||
		auth.Errors == nil || *auth.Errors != 3 || auth.TopError == nil || *auth.TopError != (statusTopError{Message: "db timeout", Count: 2}) {
		t.Fatalf("auth-server = %+v (top %+v), want 3 errors, db timeout x2, unknown", auth, auth.TopError)
	}
	if tasks.Project != "tasks/workloads/api" || tasks.OnMain != "unknown" || tasks.OnMainReason != "no commit recorded" ||
		tasks.Errors == nil || *tasks.Errors != 1 || tasks.TopError == nil || tasks.TopError.Message != "upstream answered 502" {
		t.Fatalf("tasks = %+v, want 1 error and no commit", tasks)
	}
	var rawHealth map[string]json.RawMessage
	if err := json.Unmarshal(raw["health"], &rawHealth); err != nil {
		t.Fatal(err)
	}
	var rawServices []map[string]json.RawMessage
	if err := json.Unmarshal(rawHealth["services"], &rawServices); err != nil || string(rawServices[1]["errors"]) != "1" {
		t.Fatalf("services = %s, want errors as a number", rawHealth["services"])
	}
}

// statusCells splits one rendered table line on the two-space separator.
func statusCells(line string) []string {
	var cells []string
	for _, cell := range strings.Split(line, "  ") {
		if cell = strings.TrimSpace(cell); cell != "" {
			cells = append(cells, cell)
		}
	}
	return cells
}

func decodeStatusHealth(t *testing.T, cap *captureIO) statusHealthReport {
	t.Helper()
	var result struct {
		Health *statusHealthReport `json:"health"`
	}
	joined := strings.Join(cap.stdout, "\n")
	if err := decodeResultData([]byte(joined), &result); err != nil || result.Health == nil {
		t.Fatalf("no health key (%v): %s", err, joined)
	}
	return *result.Health
}

// TestStatusHealthNeedsAnEnvironment: the logs read is per environment, so
// --health without --env is a usage error raised before any request.
func TestStatusHealthNeedsAnEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request %s sent for a usage error", r.URL.Path)
	}))
	defer srv.Close()
	err := runStatus(t, srv, &captureIO{}, map[string]any{"health": true})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--env") {
		t.Fatalf("err = %v, want a usage error naming --env", err)
	}
}

// TestStatusErrorMessageNormalization pins the TOP ERROR grouping key and the
// deterministic tie-break.
func TestStatusErrorMessageNormalization(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"message":"db timeout\n  at pool.go:12","level":"error"}`, "db timeout"},
		{`{"msg":"  upstream 502 "}`, "upstream 502"},
		{`{"message":"","msg":"falls back to msg"}`, "falls back to msg"},
		{`{"message":42}`, `{"message":42}`},
		{`{"error":"no message field"}`, `{"error":"no message field"}`},
		{"  panic: nil map  \r\ngoroutine 1", "panic: nil map"},
		{"{not json", "{not json"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := statusErrorMessage(tc.body); got != tc.want {
			t.Errorf("statusErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}

	for range 20 {
		if top := statusTopMessage(map[string]int{"b": 2, "a": 2, "c": 1, "": 5}); top == nil || *top != (statusTopError{Message: "a", Count: 2}) {
			t.Fatalf("top = %+v, want a x2: the highest count, ties to the first message, empty messages never", top)
		}
	}
	if top := statusTopMessage(map[string]int{"": 3}); top != nil {
		t.Fatalf("top = %+v, want none when every message is empty", top)
	}

	long := strings.Repeat("x", statusTopErrorWidth+10)
	health := &statusHealthReport{Environment: "prod", Services: []statusServiceHealth{{Project: "p", OnMain: "yes", Errors: new(int), TopError: &statusTopError{Message: long, Count: 1}}}}
	if cell := statusHealthCells(health, deploymentSummaryEntry{Project: "p", Environment: "prod"})[1]; len([]rune(cell)) != statusTopErrorWidth || !strings.HasSuffix(cell, "…") {
		t.Fatalf("top error cell = %q, want it cut to %d characters", cell, statusTopErrorWidth)
	}
}

// TestStatusHealthAmbiguousLabelIsNotAttributed: a label that names two rows
// (two projects folding to the same value) is reported unmatched, never
// credited to either.
func TestStatusHealthAmbiguousLabelIsNotAttributed(t *testing.T) {
	resp := &deploymentsSummary{Deployments: []deploymentSummaryEntry{
		{Project: "apps/api", Environment: "prod"},
		{Project: "apps-api", Environment: "prod"},
	}}
	read := statusErrorsRead{Pages: 1, Entries: []statusErrorEntry{{Service: "apps-api", Message: "boom"}, {Service: "apps/api", Message: "raw"}}}
	health := buildStatusHealth(resp, "prod", read, statusMainAnswers{})
	if *health.Services[0].Errors != 1 || *health.Services[1].Errors != 0 {
		t.Fatalf("errors = (%d, %d), want the raw label credited and the ambiguous folded one not", *health.Services[0].Errors, *health.Services[1].Errors)
	}
	if !slices.Equal(health.Logs.Unmatched, []statusUnmatchedService{{Service: "apps-api", Errors: 1}}) {
		t.Fatalf("unmatched = %+v, want the ambiguous label", health.Logs.Unmatched)
	}
}

// TestStatusServiceLabelMirrorsTheProvisioner pins the fold on the vectors of
// the provisioner's own TestCloudRunServiceLabel, plus the 63-character cut.
func TestStatusServiceLabelMirrorsTheProvisioner(t *testing.T) {
	cases := []struct{ in, want string }{
		{"apps/intelligence-server", "apps-intelligence-server"},
		{"shop/workloads/api", "shop-workloads-api"},
		{"echo", "echo"},
		{"Has.Dots_And-CAPS", "has-dots_and-caps"},
		{"", ""},
		{"/leading/trailing/", "leading-trailing"},
		{strings.Repeat("a", 62) + "/b", strings.Repeat("a", 62)},
	}
	for _, tc := range cases {
		if got := statusServiceLabel(tc.in); got != tc.want {
			t.Errorf("statusServiceLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStatusOnMainTriState answers ON MAIN against a real checkout: yes for a
// commit on main, no for a pull-request head that is not (a squash merge reads
// like this), unknown for a commit the checkout lacks, a row with no commit,
// and a value that is not a commit id. Git runs once for the main ref and once
// per distinct commit.
func TestStatusOnMainTriState(t *testing.T) {
	root, onMain, offMain := statusGitFixture(t)
	missing := strings.Repeat("d", 40)
	rows := []deploymentSummaryEntry{
		{Project: "a", CommitSHA: onMain},
		{Project: "b", CommitSHA: onMain[:7]},
		{Project: "c", CommitSHA: offMain},
		{Project: "d", CommitSHA: missing},
		{Project: "e", CommitSHA: onMain},
		{Project: "f"},
		{Project: "g", CommitSHA: "--output=/tmp/x"},
	}
	var processes atomic.Int32
	run := statusGitRun
	statusGitRun = func(ctx context.Context, dir string, args ...string) (string, error) {
		processes.Add(1)
		return run(ctx, dir, args...)
	}
	t.Cleanup(func() { statusGitRun = run })

	check := func(t *testing.T, wantRef string, want map[string]statusOnMainAnswer) {
		t.Helper()
		processes.Store(0)
		answers := statusOnMain(context.Background(), root, rows)
		if answers.Ref != wantRef {
			t.Fatalf("main ref = %q (%q), want %q", answers.Ref, answers.RefUnavailable, wantRef)
		}
		for _, row := range rows {
			if got := answers.answer(row.CommitSHA); got != want[row.Project] {
				t.Errorf("project %s (%q) = %+v, want %+v", row.Project, row.CommitSHA, got, want[row.Project])
			}
		}
		// One for-each-ref, then one merge-base per distinct valid commit:
		// onMain, its short form, offMain, and missing.
		if got := processes.Load(); got != 5 {
			t.Fatalf("git processes = %d, want 5", got)
		}
	}
	notACommit := statusOnMainAnswer{State: "unknown", Reason: "not a commit id"}
	noCommit := statusOnMainAnswer{State: "unknown", Reason: "no commit recorded"}
	absent := statusOnMainAnswer{State: "unknown", Reason: "commit not in the local checkout"}
	yes, no := statusOnMainAnswer{State: "yes"}, statusOnMainAnswer{State: "no"}

	t.Run("main without origin/main", func(t *testing.T) {
		check(t, "main", map[string]statusOnMainAnswer{"a": yes, "b": yes, "c": no, "d": absent, "e": yes, "f": noCommit, "g": notACommit})
	})
	t.Run("origin/main wins over main", func(t *testing.T) {
		gitIn(t, root, "update-ref", "refs/remotes/origin/main", offMain)
		defer gitIn(t, root, "update-ref", "-d", "refs/remotes/origin/main")
		check(t, "origin/main", map[string]statusOnMainAnswer{"a": yes, "b": yes, "c": yes, "d": absent, "e": yes, "f": noCommit, "g": notACommit})
	})
	t.Run("no main ref", func(t *testing.T) {
		gitIn(t, root, "branch", "-m", "main", "trunk")
		defer gitIn(t, root, "branch", "-m", "trunk", "main")
		processes.Store(0)
		answers := statusOnMain(context.Background(), root, rows)
		if answers.Ref != "" || answers.RefUnavailable != "no origin/main or main ref" {
			t.Fatalf("main ref = (%q, %q), want none", answers.Ref, answers.RefUnavailable)
		}
		if got := answers.answer(offMain); got != (statusOnMainAnswer{State: "unknown", Reason: "no origin/main or main ref"}) {
			t.Fatalf("answer = %+v, want unknown for want of a main ref", got)
		}
		if got := processes.Load(); got != 1 {
			t.Fatalf("git processes = %d, want 1: no merge-base without a main ref", got)
		}
	})
	t.Run("no commit to check runs no git", func(t *testing.T) {
		processes.Store(0)
		answers := statusOnMain(context.Background(), root, []deploymentSummaryEntry{{Project: "f"}})
		if got := processes.Load(); got != 0 || answers.RefUnavailable != "" {
			t.Fatalf("git processes = %d (%+v), want none", got, answers)
		}
	})
}
