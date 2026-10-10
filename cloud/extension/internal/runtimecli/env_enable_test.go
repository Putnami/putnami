package runtimecli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The callers the fixture's control plane names, in the response's order.
const (
	enableWorkerCaller = "cpa-worker@cp.iam.gserviceaccount.com"
	enableAPICaller    = "control-api@cp.iam.gserviceaccount.com"
	enableConfigCaller = "config-api@cp.iam.gserviceaccount.com"
)

const enableHead = "0123456789abcdef0123456789abcdef01234567"

// enableRequest is one recorded request: what the command wrote, where.
type enableRequest struct {
	Method, Path, Query, Body string
	// Replay marks an acceptance the route replayed: it wrote nothing.
	Replay bool
}

// grantState is one Runtime grant row the fixture holds.
type grantState struct {
	Found, Enabled bool
	Revision       int64
}

// enableServer is a control-plane double for every provider `env enable`
// calls, routed by path the way the load balancer routes them. It records
// every request so a test can assert the exact writes.
type enableServer struct {
	mu       sync.Mutex
	requests []enableRequest
	// beforeRows and afterRows are the readiness rows the first and the
	// later readiness reads answer; afterRows nil answers beforeRows again.
	beforeRows, afterRows map[string]string
	// callers is the required_callers list; canManage answers the probe
	// when non-nil; acceptedRevision is the CAS input.
	callers          []map[string]string
	canManage        *bool
	acceptedRevision int64
	// acceptedSources maps each source revision accepted before to its
	// revision: the accept route replays those and appends any other.
	// appended counts the appends.
	acceptedSources map[string]int64
	appended        int
	// grants is keyed by "<kind> <caller>".
	grants map[string]grantState
	// Statuses to answer writes with; 0 means the success status.
	bindingStatus, acceptStatus, grantPutStatus int
	readinessReads                              int
}

func newEnableServer() *enableServer {
	return &enableServer{
		beforeRows: map[string]string{},
		callers: []map[string]string{
			{"caller": enableWorkerCaller, "kind": "operation"},
			{"caller": enableAPICaller, "kind": "operation"},
			{"caller": enableConfigCaller, "kind": "binding"},
		},
		grants: map[string]grantState{},
	}
}

func (s *enableServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, enableRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	prefix := "/v1/workspaces/" + doctorWorkspaceID
	path := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodGet && path == "/environments/prod/readiness":
		s.readinessReads++
		rows := s.beforeRows
		if s.readinessReads > 1 && s.afterRows != nil {
			rows = s.afterRows
		}
		callers := s.callers
		if callers == nil {
			callers = []map[string]string{}
		}
		document := map[string]any{"workspace": doctorWorkspaceID, "environment": "prod",
			"rows": enableRows(rows), "required_callers": callers, "accepted_revision": s.acceptedRevision}
		if s.canManage != nil && r.URL.Query().Get("probe") == "manage" {
			document["caller_can_manage"] = *s.canManage
		}
		writeJSON(w, http.StatusOK, document)
	case r.Method == http.MethodPost && path == "/distribution/bindings":
		if s.bindingStatus != 0 {
			writeJSON(w, s.bindingStatus, map[string]any{"code": "refused", "message": "binding refused"})
			return
		}
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		writeJSON(w, http.StatusCreated, map[string]any{"binding": map[string]any{
			"id": "b-" + in["protocol"], "protocol": in["protocol"], "namespace": in["namespace"], "workspace_id": doctorWorkspaceID,
		}})
	case r.Method == http.MethodPost && path == "/environment-definitions/accept":
		if s.acceptStatus != 0 {
			writeJSON(w, s.acceptStatus, map[string]any{"code": "unavailable", "message": "Source environment declaration is unavailable"})
			return
		}
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		revision, _ := in["sourceRevision"].(string)
		if revision == "" {
			revision = enableHead
		}
		expected, _ := in["expectedRevision"].(float64)
		accepted, replay := s.acceptedSources[revision]
		s.requests[len(s.requests)-1].Replay = replay
		if !replay {
			accepted = int64(expected) + 1
			if s.acceptedSources == nil {
				s.acceptedSources = map[string]int64{}
			}
			s.acceptedSources[revision] = accepted
			s.appended++
		}
		writeJSON(w, http.StatusOK, map[string]any{"workspace_id": doctorWorkspaceID, "revision": accepted,
			"digest": "sha256:" + strings.Repeat("a", 64), "source_revision": revision, "accepted_at": "2026-09-27T10:00:00Z"})
	case strings.HasPrefix(path, "/runtime/operation-grants/"), strings.HasPrefix(path, "/runtime/binding-grants/"):
		kind := "operation"
		if strings.HasPrefix(path, "/runtime/binding-grants/") {
			kind = "binding"
		}
		caller := path[strings.LastIndex(path, "/")+1:]
		key := kind + " " + caller
		state := s.grants[key]
		switch r.Method {
		case http.MethodGet:
			if !state.Found {
				writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "message": "no grant"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"caller_service_account": caller, "enabled": state.Enabled, "revision": state.Revision, "workspace_id": doctorWorkspaceID})
		case http.MethodPut:
			if s.grantPutStatus != 0 {
				writeJSON(w, s.grantPutStatus, map[string]any{"code": "refused", "message": "grant refused"})
				return
			}
			var in struct {
				Enabled          bool  `json:"enabled"`
				ExpectedRevision int64 `json:"expected_revision"`
			}
			_ = json.Unmarshal(body, &in)
			if in.ExpectedRevision != state.Revision {
				writeJSON(w, http.StatusConflict, map[string]any{"code": "conflict", "message": "revision moved"})
				return
			}
			state = grantState{Found: true, Enabled: in.Enabled, Revision: state.Revision + 1}
			s.grants[key] = state
			writeJSON(w, http.StatusOK, map[string]any{"caller_service_account": caller, "enabled": state.Enabled, "revision": state.Revision, "workspace_id": doctorWorkspaceID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "method"})
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "not_found", "message": "unrouted " + r.Method + " " + r.URL.Path})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// enableRows answers every server row: ok unless override names another
// status.
func enableRows(override map[string]string) []map[string]string {
	rows := make([]map[string]string, 0, len(envDoctorServerTitles))
	for _, id := range envDoctorOrder {
		title, server := envDoctorServerTitles[id]
		if !server {
			continue
		}
		status := EnvDoctorOK
		if value, ok := override[id]; ok {
			status = value
		}
		fix := ""
		if status != EnvDoctorOK {
			fix = "fix " + id
		}
		rows = append(rows, map[string]string{"id": id, "title": title, "status": status, "detail": "detail " + id, "fix": fix})
	}
	return rows
}

// freshWorkspace is the first-run fixture: every workspace-owned row
// missing, the identity rows as a never-deployed workspace answers them,
// and, after the writes, every workspace-owned row ok.
func freshWorkspace() *enableServer {
	server := newEnableServer()
	server.beforeRows = map[string]string{
		"put-binding": EnvDoctorMissing, "oci-binding": EnvDoctorMissing, "environment-definition": EnvDoctorMissing,
		"runtime-operation-grants": EnvDoctorMissing, "runtime-binding-grant-config": EnvDoctorMissing,
		"project-identity": EnvDoctorUnknown, "database-ownership": EnvDoctorUnknown,
	}
	server.afterRows = map[string]string{"project-identity": EnvDoctorUnknown, "database-ownership": EnvDoctorUnknown}
	yes := true
	server.canManage = &yes
	return server
}

func (s *enableServer) seen() []enableRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]enableRequest(nil), s.requests...)
}

// writes is every recorded request that is not a readiness read or an
// acceptance the route replayed.
func (s *enableServer) writes() []enableRequest {
	var out []enableRequest
	for _, request := range s.seen() {
		if request.Replay || request.Method == http.MethodGet && strings.HasSuffix(request.Path, "/readiness") {
			continue
		}
		out = append(out, request)
	}
	return out
}

// acceptHead records the default-branch head as accepted at revision, so an
// ok definition row replays it.
func (s *enableServer) acceptHead(revision int64) {
	s.acceptedRevision = revision
	s.acceptedSources = map[string]int64{enableHead: revision}
}

// acceptances is every acceptance request, replayed or not.
func (s *enableServer) acceptances() []enableRequest {
	var out []enableRequest
	for _, request := range s.seen() {
		if strings.HasSuffix(request.Path, "/environment-definitions/accept") {
			out = append(out, request)
		}
	}
	return out
}

func runEnvEnable(t *testing.T, root string, srv *httptest.Server, params map[string]any, args ...string) (*captureIO, error) {
	t.Helper()
	cap := &captureIO{}
	env := map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}
	if params == nil {
		params = map[string]any{}
	}
	err := Env(params, append([]string{"enable"}, args...), root, env, cap.io(srv.Client()), (&okPublish{}).publish)
	return cap, err
}

func decodeEnableResult(t *testing.T, lines []string) EnvEnableResult {
	t.Helper()
	var result EnvEnableResult
	if err := decodeResultData([]byte(strings.Join(lines, "\n")), &result); err != nil {
		t.Fatalf("decode %q: %v", lines, err)
	}
	return result
}

func stepsByRow(result EnvEnableResult) map[string][]EnvEnableStep {
	out := map[string][]EnvEnableStep{}
	for _, step := range result.Steps {
		out[step.Row] = append(out[step.Row], step)
	}
	return out
}

func grantURL(kind, caller string) string {
	return fmt.Sprintf("/v1/workspaces/%s/runtime/%s-grants/%s", doctorWorkspaceID, kind, caller)
}

// TestEnvEnableFreshWorkspaceMakesExactlyTheWorkspaceOwnedWrites is the
// first acceptance case: on a workspace with none of the prerequisites, one
// run activates both bindings, accepts the definition at the default-branch
// head with expectedRevision 0, reads then enables every required grant at
// its revision, and nothing else. The readiness read comes first with the
// manage probe, and again after the writes.
func TestEnvEnableFreshWorkspaceMakesExactlyTheWorkspaceOwnedWrites(t *testing.T) {
	server := freshWorkspace()
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json"})
	if err != nil {
		t.Fatalf("a converged environment must exit 0: %v", err)
	}
	result := decodeEnableResult(t, cap.stdout)
	if !result.Ready || result.Applied != 6 || result.Failed != 0 || result.Workspace != doctorWorkspaceID || result.Environment != "prod" {
		t.Fatalf("result = ready %v, applied %d, failed %d (%s/%s)", result.Ready, result.Applied, result.Failed, result.Workspace, result.Environment)
	}
	if !result.Readiness.Ready || result.Readiness.Missing != 0 {
		t.Fatalf("readiness after = %+v", result.Readiness)
	}

	want := []enableRequest{
		{Method: http.MethodPost, Path: "/v1/workspaces/" + doctorWorkspaceID + "/distribution/bindings", Body: `{"idempotency_key":"acme-put","namespace":"acme","protocol":"put"}`},
		{Method: http.MethodPost, Path: "/v1/workspaces/" + doctorWorkspaceID + "/distribution/bindings", Body: `{"idempotency_key":"acme-oci","namespace":"acme","protocol":"oci"}`},
		{Method: http.MethodPost, Path: "/v1/workspaces/" + doctorWorkspaceID + "/environment-definitions/accept", Body: `{"expectedRevision":0}`},
		{Method: http.MethodGet, Path: grantURL("operation", enableWorkerCaller)},
		{Method: http.MethodPut, Path: grantURL("operation", enableWorkerCaller), Body: `{"enabled":true,"expected_revision":0}`},
		{Method: http.MethodGet, Path: grantURL("operation", enableAPICaller)},
		{Method: http.MethodPut, Path: grantURL("operation", enableAPICaller), Body: `{"enabled":true,"expected_revision":0}`},
		{Method: http.MethodGet, Path: grantURL("binding", enableConfigCaller)},
		{Method: http.MethodPut, Path: grantURL("binding", enableConfigCaller), Body: `{"enabled":true,"expected_revision":0}`},
	}
	got := server.writes()
	if len(got) != len(want) {
		t.Fatalf("writes = %d %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].Method != want[i].Method || got[i].Path != want[i].Path || strings.TrimSpace(got[i].Body) != want[i].Body {
			t.Errorf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	reads := server.seen()
	if reads[0].Method != http.MethodGet || !strings.HasSuffix(reads[0].Path, "/readiness") || !strings.Contains(reads[0].Query, "probe=manage") {
		t.Errorf("first request = %+v, want the readiness read with the manage probe", reads[0])
	}
	last := reads[len(reads)-1]
	if !strings.HasSuffix(last.Path, "/readiness") || strings.Contains(last.Query, "probe") {
		t.Errorf("last request = %+v, want the readiness read without a probe", last)
	}
	if server.readinessReads != 2 {
		t.Errorf("readiness reads = %d, want 2", server.readinessReads)
	}

	steps := stepsByRow(result)
	for row, status := range map[string]string{
		"put-binding": EnvEnableApplied, "oci-binding": EnvEnableApplied, "environment-definition": EnvEnableApplied,
		"oci-registry": EnvEnableSkipped, "ci-envs": EnvEnableSkipped, "publish-namespaces": EnvEnableSkipped,
		"project-identity": EnvEnableReported, "database-ownership": EnvEnableReported,
	} {
		if len(steps[row]) != 1 || steps[row][0].Status != status {
			t.Errorf("%s steps = %+v, want one %s", row, steps[row], status)
		}
	}
	if len(steps["runtime-operation-grants"]) != 2 || len(steps["runtime-binding-grant-config"]) != 1 {
		t.Errorf("grant steps = %+v / %+v, want one per caller", steps["runtime-operation-grants"], steps["runtime-binding-grant-config"])
	}
	if detail := steps["environment-definition"][0].Detail; !strings.Contains(detail, enableHead) || !strings.Contains(detail, "default-branch head") {
		t.Errorf("acceptance detail = %q, want the resolved head", detail)
	}
	if fix := steps["project-identity"][0].Fix; fix != "fix project-identity" {
		t.Errorf("a reported step lost its fix: %+v", steps["project-identity"][0])
	}
	// Objects owned outside <db>_owner are re-owned by the platform's
	// bootstrap: the command prints the row's fix and never runs it.
	if step := steps["database-ownership"][0]; step.Fix != "fix database-ownership" || step.Action != "not written by this command" {
		t.Errorf("database-ownership step = %+v, want reported with its fix", step)
	}
}

// TestEnvEnableSecondRunWritesNothing is the idempotence case: every
// workspace-owned row ok and the default-branch head already accepted, so
// the command reads twice, asks the accept route once, and the route
// replays: nothing is appended and nothing else is written.
func TestEnvEnableSecondRunWritesNothing(t *testing.T) {
	server := freshWorkspace()
	server.beforeRows = map[string]string{"project-identity": EnvDoctorUnknown}
	server.afterRows = nil
	server.acceptHead(3)
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json"})
	if err != nil {
		t.Fatalf("an enabled environment must exit 0: %v", err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("a second run wrote: %+v", writes)
	}
	if replays := server.acceptances(); len(replays) != 1 || !replays[0].Replay {
		t.Fatalf("acceptances = %+v, want one replay", replays)
	}
	result := decodeEnableResult(t, cap.stdout)
	if !result.Ready || result.Applied != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	for _, step := range result.Steps {
		if step.Status == EnvEnableApplied || step.Status == EnvEnableFailed {
			t.Errorf("step %+v on an enabled environment", step)
		}
	}
	if detail := stepsByRow(result)["environment-definition"][0].Detail; !strings.Contains(detail, "already accepted as revision 3") {
		t.Fatalf("detail = %q, want the replayed revision", detail)
	}
}

// TestEnvEnableAcceptsWhenTheRowIsOKButTheSourceMoved pins the fix for the
// ok row: it is ok once any accepted definition declares the environment,
// so a changed putnami.ci.json left it ok and nothing was accepted. Now a
// default-branch head, or a --source-revision, that differs from the
// accepted source revision is accepted; the same one is not.
func TestEnvEnableAcceptsWhenTheRowIsOKButTheSourceMoved(t *testing.T) {
	older, other := strings.Repeat("c", 40), strings.Repeat("d", 40)
	for _, c := range []struct {
		name     string
		accepted map[string]int64
		params   map[string]any
		appended int
		status   string
		detail   string
	}{
		{"the default-branch head moved", map[string]int64{older: 3}, map[string]any{}, 1, EnvEnableApplied, "accepted revision 4 at source revision " + enableHead + " (the default-branch head; expected revision 3)"},
		{"--source-revision differs", map[string]int64{enableHead: 3}, map[string]any{"source-revision": other}, 1, EnvEnableApplied, "accepted revision 4 at source revision " + other + " (--source-revision; expected revision 3)"},
		{"--source-revision is the accepted one", map[string]int64{other: 3}, map[string]any{"source-revision": other}, 0, EnvEnableSkipped, "source revision " + other + " (--source-revision) is already accepted as revision 3"},
		{"--source-revision is an older accepted one", map[string]int64{other: 1, enableHead: 3}, map[string]any{"source-revision": other}, 0, EnvEnableSkipped, "source revision " + other + " (--source-revision) is already accepted as revision 1; the latest accepted revision stays 3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := freshWorkspace()
			server.beforeRows = map[string]string{"project-identity": EnvDoctorUnknown}
			server.afterRows = nil
			server.acceptedRevision = 3
			server.acceptedSources = c.accepted
			srv := httptest.NewServer(server)
			defer srv.Close()

			params := map[string]any{"output": "json"}
			for key, value := range c.params {
				params[key] = value
			}
			cap, err := runEnvEnable(t, doctorWorkspace(t), srv, params)
			if err != nil {
				t.Fatalf("exit: %v", err)
			}
			if server.appended != c.appended {
				t.Fatalf("appended = %d, want %d", server.appended, c.appended)
			}
			acceptances := server.acceptances()
			if len(acceptances) != 1 || !strings.Contains(acceptances[0].Body, `"expectedRevision":3`) {
				t.Fatalf("acceptances = %+v, want one expecting revision 3", acceptances)
			}
			step := stepsByRow(decodeEnableResult(t, cap.stdout))["environment-definition"][0]
			if step.Status != c.status || step.Detail != c.detail {
				t.Fatalf("step = %+v, want %s %q", step, c.status, c.detail)
			}
		})
	}
}

// TestEnvEnableFailsWhenTheRowIsOKButAcceptFails pins the exit code that
// asking accept on every run brings: an enabled environment whose accept
// call fails (Source cannot read the head, the head's putnami.ci.json is
// invalid, a concurrent acceptance won) now fails the step and exits 1,
// where the run used to skip the step and exit 0.
func TestEnvEnableFailsWhenTheRowIsOKButAcceptFails(t *testing.T) {
	server := freshWorkspace()
	server.beforeRows = map[string]string{"project-identity": EnvDoctorUnknown}
	server.afterRows = nil
	server.acceptedRevision = 3
	server.acceptedSources = map[string]int64{strings.Repeat("c", 40): 3}
	server.acceptStatus = http.StatusServiceUnavailable
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json"})
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v), want failure", clicore.ExitCode(err), err)
	}
	if server.appended != 0 {
		t.Fatalf("appended = %d, want 0", server.appended)
	}
	if acceptances := server.acceptances(); len(acceptances) != 1 {
		t.Fatalf("acceptances = %+v, want one", acceptances)
	}
	if len(cap.stdout) != 0 {
		t.Fatalf("a failed run printed a success result: %q", cap.stdout)
	}
	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	result := decodeEnableResult(t, envelope.stdout)
	step := stepsByRow(result)["environment-definition"][0]
	if result.Failed != 1 || step.Status != EnvEnableFailed || !strings.Contains(step.Detail, "503") {
		t.Fatalf("failed = %d, step = %+v, want one failed step naming the 503", result.Failed, step)
	}
}

// TestEnvEnableGrantsReadTheRevisionBeforeWriting pins the compare-and-set:
// a disabled grant is enabled at the revision read, an enabled one is
// skipped without a write, and an absent one starts at 0.
func TestEnvEnableGrantsReadTheRevisionBeforeWriting(t *testing.T) {
	server := freshWorkspace()
	server.acceptHead(1)
	server.beforeRows = map[string]string{"runtime-operation-grants": EnvDoctorMissing, "runtime-binding-grant-config": EnvDoctorUnknown, "project-identity": EnvDoctorUnknown}
	server.afterRows = map[string]string{"project-identity": EnvDoctorUnknown}
	server.grants["operation "+enableWorkerCaller] = grantState{Found: true, Enabled: false, Revision: 7}
	server.grants["operation "+enableAPICaller] = grantState{Found: true, Enabled: true, Revision: 2}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json"})
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	got := server.writes()
	want := []enableRequest{
		{Method: http.MethodGet, Path: grantURL("operation", enableWorkerCaller)},
		{Method: http.MethodPut, Path: grantURL("operation", enableWorkerCaller), Body: `{"enabled":true,"expected_revision":7}`},
		{Method: http.MethodGet, Path: grantURL("operation", enableAPICaller)},
		{Method: http.MethodGet, Path: grantURL("binding", enableConfigCaller)},
		{Method: http.MethodPut, Path: grantURL("binding", enableConfigCaller), Body: `{"enabled":true,"expected_revision":0}`},
	}
	if len(got) != len(want) {
		t.Fatalf("writes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Method != want[i].Method || got[i].Path != want[i].Path || strings.TrimSpace(got[i].Body) != want[i].Body {
			t.Errorf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	steps := stepsByRow(decodeEnableResult(t, cap.stdout))
	operation := steps["runtime-operation-grants"]
	if len(operation) != 2 || operation[0].Status != EnvEnableApplied || !strings.Contains(operation[0].Detail, "expected revision 7") ||
		operation[1].Status != EnvEnableSkipped || !strings.Contains(operation[1].Detail, "revision 2") {
		t.Errorf("operation steps = %+v", operation)
	}
}

// TestEnvEnablePartialFailureReportsEachStep: one refused write fails its
// own step, the other steps still run, and the command exits 1 with the
// steps on the failure envelope.
func TestEnvEnablePartialFailureReportsEachStep(t *testing.T) {
	server := freshWorkspace()
	server.acceptStatus = http.StatusServiceUnavailable
	server.afterRows = map[string]string{"environment-definition": EnvDoctorMissing, "project-identity": EnvDoctorUnknown, "database-ownership": EnvDoctorUnknown}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, nil)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v), want failure", clicore.ExitCode(err), err)
	}
	if want := "environment prod is not enabled: 1 step(s) failed; 1 missing, 2 could not be checked"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if writes := server.writes(); len(writes) != 9 {
		t.Fatalf("writes = %d, want every step attempted: %+v", len(writes), writes)
	}
	text := strings.Join(cap.stdout, "\n")
	for _, want := range []string{
		"Enable prod of workspace " + doctorWorkspaceID,
		"applied   put-binding",
		"failed    environment-definition",
		"control-api refused: 503 Service Unavailable: Source environment declaration is unavailable",
		"fix: fix environment-definition",
		"5 applied, 1 failed",
		"Environment prod of workspace " + doctorWorkspaceID,
		"missing  environment-definition",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("human output lacks %q:\n%s", want, text)
		}
	}

	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	result := decodeEnableResult(t, envelope.stdout)
	if result.Ready || result.Applied != 5 || result.Failed != 1 || len(result.Readiness.Rows) != len(envDoctorOrder) {
		t.Fatalf("envelope data = ready %v, applied %d, failed %d, rows %d", result.Ready, result.Applied, result.Failed, len(result.Readiness.Rows))
	}
}

// TestEnvEnableRefusesWithoutManageBeforeAnyWrite: the readiness probe says
// the session lacks platform.workspace.manage, so the command refuses in one
// line after that one read.
func TestEnvEnableRefusesWithoutManageBeforeAnyWrite(t *testing.T) {
	server := freshWorkspace()
	no := false
	server.canManage = &no
	srv := httptest.NewServer(server)
	defer srv.Close()

	_, err := runEnvEnable(t, doctorWorkspace(t), srv, nil)
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("exit = %d (%v), want auth", clicore.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "does not hold platform.workspace.manage in workspace "+doctorWorkspaceID) {
		t.Fatalf("error = %q", err)
	}
	if requests := server.seen(); len(requests) != 1 {
		t.Fatalf("requests = %+v, want the one readiness read", requests)
	}
}

// TestEnvEnableStopsAtTheFirstRefusedWrite: a control plane too old to
// answer the probe does not refuse early; the first 403 stops the run, and
// no later write is attempted.
func TestEnvEnableStopsAtTheFirstRefusedWrite(t *testing.T) {
	server := freshWorkspace()
	server.canManage = nil
	server.bindingStatus = http.StatusForbidden
	srv := httptest.NewServer(server)
	defer srv.Close()

	_, err := runEnvEnable(t, doctorWorkspace(t), srv, nil)
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("exit = %d (%v), want auth", clicore.ExitCode(err), err)
	}
	if writes := server.writes(); len(writes) != 1 || writes[0].Method != http.MethodPost {
		t.Fatalf("writes = %+v, want the one refused activation", writes)
	}
	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	steps := stepsByRow(decodeEnableResult(t, envelope.stdout))
	if step := steps["put-binding"]; len(step) != 1 || step[0].Status != EnvEnableFailed || !strings.Contains(step[0].Detail, "403") {
		t.Fatalf("put-binding step = %+v", step)
	}
}

// TestEnvEnableSourceRevision: --source-revision is sent as given; a value
// that is not a full commit id is a usage error before any call.
func TestEnvEnableSourceRevision(t *testing.T) {
	server := freshWorkspace()
	server.beforeRows = map[string]string{"environment-definition": EnvDoctorMissing, "project-identity": EnvDoctorUnknown}
	server.acceptedRevision = 4
	server.afterRows = map[string]string{"project-identity": EnvDoctorUnknown}
	srv := httptest.NewServer(server)
	defer srv.Close()
	sha := strings.Repeat("b", 40)

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json", "source-revision": sha})
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	writes := server.writes()
	if len(writes) != 1 || strings.TrimSpace(writes[0].Body) != `{"expectedRevision":4,"sourceRevision":"`+sha+`"}` {
		t.Fatalf("writes = %+v, want one acceptance at revision 4 of %s", writes, sha)
	}
	if detail := stepsByRow(decodeEnableResult(t, cap.stdout))["environment-definition"][0].Detail; !strings.Contains(detail, "revision 5 at source revision "+sha+" (--source-revision") {
		t.Fatalf("detail = %q", detail)
	}

	for _, bad := range []string{"main", "HEAD", strings.Repeat("b", 39), "../x"} {
		before := len(server.seen())
		_, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"source-revision": bad})
		if clicore.ExitCode(err) != clicore.ExitUsage {
			t.Errorf("--source-revision %q: exit = %d (%v), want usage", bad, clicore.ExitCode(err), err)
		}
		if len(server.seen()) != before {
			t.Errorf("--source-revision %q reached the control plane", bad)
		}
	}
}

// TestEnvEnableDoesNotWriteOnAGuess pins the two cases where a write would
// be a guess: a row the server could not check is reported, not written;
// and a grant row without a required caller from the server fails with the
// fix, because the CLI carries no service-account email of its own.
func TestEnvEnableDoesNotWriteOnAGuess(t *testing.T) {
	server := freshWorkspace()
	server.acceptHead(1)
	server.beforeRows = map[string]string{"put-binding": EnvDoctorUnknown, "runtime-operation-grants": EnvDoctorMissing, "project-identity": EnvDoctorUnknown}
	server.afterRows = map[string]string{"put-binding": EnvDoctorUnknown, "runtime-operation-grants": EnvDoctorMissing, "project-identity": EnvDoctorUnknown}
	server.callers = nil
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvEnable(t, doctorWorkspace(t), srv, map[string]any{"output": "json"})
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v), want failure", clicore.ExitCode(err), err)
	}
	if want := "environment prod is not enabled: 1 step(s) failed; 1 missing, 2 could not be checked"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("wrote on a guess: %+v", writes)
	}
	if len(cap.stdout) != 0 {
		t.Fatalf("structured failure printed %q before the envelope", cap.stdout)
	}
	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	steps := stepsByRow(decodeEnableResult(t, envelope.stdout))
	if step := steps["put-binding"]; len(step) != 1 || step[0].Status != EnvEnableReported || !strings.Contains(step[0].Detail, "could not be checked") {
		t.Errorf("put-binding step = %+v", step)
	}
	if step := steps["runtime-operation-grants"]; len(step) != 1 || step[0].Status != EnvEnableFailed || !strings.Contains(step[0].Detail, "names no required operation caller") || step[0].Fix != "fix runtime-operation-grants" {
		t.Errorf("operation grants step = %+v", step)
	}
}

// TestEnvEnableWithoutANamespaceFailsTheBindings: putnami.ci.json without a
// distribution.namespace cannot name what to bind, so the binding steps
// fail with the ci-envs fix and nothing is activated.
func TestEnvEnableWithoutANamespaceFailsTheBindings(t *testing.T) {
	server := freshWorkspace()
	server.acceptHead(1)
	server.beforeRows = map[string]string{"put-binding": EnvDoctorMissing, "project-identity": EnvDoctorUnknown}
	srv := httptest.NewServer(server)
	defer srv.Close()
	root := doctorWorkspace(t)
	writeDoctorFile(t, root+"/putnami.ci.json", map[string]any{
		"distribution": map[string]any{"memberAttribution": true},
		"envs":         map[string]any{"prod": map[string]any{"channel": "canary", "workloads": []any{map[string]any{"select": []string{"shop/workloads/api"}}}}},
	})

	_, err := runEnvEnable(t, root, srv, map[string]any{"output": "json"})
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v), want failure", clicore.ExitCode(err), err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("activated without a namespace: %+v", writes)
	}
	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	if step := stepsByRow(decodeEnableResult(t, envelope.stdout))["put-binding"]; len(step) != 1 || step[0].Status != EnvEnableFailed || !strings.Contains(step[0].Detail, "distribution.namespace") {
		t.Fatalf("put-binding step = %+v", step)
	}
}

// TestEnvEnableNeedsTheControlPlane: without a readiness answer there is
// nothing to converge from, so the command fails before any write.
func TestEnvEnableNeedsTheControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"code": "unavailable"})
	}))
	defer srv.Close()
	_, err := runEnvEnable(t, doctorWorkspace(t), srv, nil)
	if clicore.ExitCode(err) != clicore.ExitFailure || !strings.Contains(err.Error(), "cannot be enabled: control-api did not answer the readiness read") {
		t.Fatalf("exit = %d (%v)", clicore.ExitCode(err), err)
	}
}

// TestEnvEnableUsage pins the positional rules and the help entry.
func TestEnvEnableUsage(t *testing.T) {
	server := freshWorkspace()
	srv := httptest.NewServer(server)
	defer srv.Close()
	for name, args := range map[string][]string{
		"two environments": {"prod", "staging"},
		"malformed env":    {"prod env"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runEnvEnable(t, doctorWorkspace(t), srv, nil, args...); clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit = %d (%v), want usage", clicore.ExitCode(err), err)
			}
		})
	}
	if got := len(server.seen()); got != 0 {
		t.Fatalf("requests = %d, want none", got)
	}
	cap := &captureIO{}
	if err := Env(map[string]any{}, nil, t.TempDir(), map[string]string{}, cap.io(nil), nil); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(cap.stdout, "\n"); !strings.Contains(text, "cloud env enable [") {
		t.Fatalf("help = %q", text)
	}
}
