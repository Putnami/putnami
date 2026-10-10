package runtimecli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// newControlTestCtx is a deployCtx bound to srv with a bearer that never needs
// a re-mint.
func newControlTestCtx(srv *httptest.Server) *deployCtx {
	return &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("token"),
			IO:           clicore.IO{Client: srv.Client()},
		},
	}
}

// answer is a test control plane that answers every request with status and
// body as JSON.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestDeployRequestRendersEveryRefusalShape pins the refusal text and exit code
// of the deploy submit for each refusal shape control-api declares.
func TestDeployRequestRendersEveryRefusalShape(t *testing.T) {
	const twoRejections = "deploy: preflight rejected 2 problem(s):\n" +
		"  - accounts/workloads/a-server: projects[0]: image not found\n" +
		"  - accounts/workloads/c-server: projects[2]: bundle not published"
	const quota = "deploy: ensure cloud run service: quota\n  GCP response body: {\"error\":{\"code\":429}}"
	rejections := `[{"index":0,"project":"accounts/workloads/a-server","error":"projects[0]: image not found"},` +
		`{"index":2,"project":"accounts/workloads/c-server","error":"projects[2]: bundle not published"}]`
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		exit     int
		conflict bool
	}{
		{"rejections", 400,
			`{"code":"control.deploy.rejected","error":"projects[0]: image not found","message":"projects[0]: image not found","rejections":` + rejections + `,"details":{"rejections":` + rejections + `}}`,
			twoRejections, clicore.ExitAPI, false},
		{"single rejection names only error", 400, `{"error":"app is required"}`, "deploy: app is required", clicore.ExitAPI, false},
		{"message wins over error", 422, `{"error":"coarse","message":"the specific detail"}`, "deploy: the specific detail", clicore.ExitAPI, false},
		{"manifest-support rejections are not a rejection list", 412,
			`{"error":"manifest declares infrastructure this control-plane cannot provision yet","code":"unsupported_manifest","rejections":[{"kind":"events","workload":"acme/events","message":"events not supported"}]}`,
			"deploy: manifest declares infrastructure this control-plane cannot provision yet", clicore.ExitAPI, false},
		{"gcp body", 500,
			`{"code":"control.cloud_failure","error":"internal","message":"ensure cloud run service: quota","gcp_response_body":"{\"error\":{\"code\":429}}","details":{"gcp_response_body":"{\"error\":{\"code\":429}}"}}`,
			quota, clicore.ExitAPI, false},
		{"internal error without gcp body", 500, `{"error":"internal","message":"record deployment run: db down"}`,
			"deploy: record deployment run: db down", clicore.ExitAPI, false},
		{"release conflict without a code", 409, `{"error":"release_conflict","message":"release id already used"}`,
			"deploy: release id already used", clicore.ExitAPI, false},
		{"release conflict", 409,
			`{"code":"control.deploy.release_conflict","error":"release_conflict","message":"release id already used","details":{"retryable":false}}`,
			"deploy: release id already used", clicore.ExitAPI, false},
		{"retryable release conflict", 409,
			`{"code":"control.deploy.release_conflict","error":"busy","message":"another release is running","details":{"retryable":true}}`,
			"deploy: another release is running", clicore.ExitAPI, true},
		{"unauthorized", 401, `{"error":"Authentication required"}`, "deploy: Authentication required", clicore.ExitAuth, false},
		{"body that is not JSON", 403, `<html>denied</html>`, "deploy: 403 Forbidden", clicore.ExitAPI, false},
		{"empty body", 500, ``, "deploy: 500 Internal Server Error", clicore.ExitAPI, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(answer(tc.status, tc.body))
			defer srv.Close()

			_, status, err := deployRequest(context.Background(), newControlTestCtx(srv), map[string]any{"release_id": "rel_test"})
			if err == nil {
				t.Fatal("want a refusal")
			}
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if err.Error() != tc.want {
				t.Errorf("error =\n%s\nwant\n%s", err.Error(), tc.want)
			}
			if code := clicore.ExitCode(err); code != tc.exit {
				t.Errorf("exit code = %d, want %d", code, tc.exit)
			}
			if got := isRetryableConflict(err); got != tc.conflict {
				t.Errorf("retryable conflict = %t, want %t", got, tc.conflict)
			}
		})
	}
}

// TestDeployRequestSendsTheBodyTheCLIBuilt pins the wire: the generated call
// sends the same JSON value the hand-written request sent, for every member
// the CLI builds.
func TestDeployRequestSendsTheBodyTheCLIBuilt(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/ws-acme/deploy" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"release_id":"rel_test","state":"Provisioning","async":true,"projects":[]}`))
	}))
	defer srv.Close()

	body := map[string]any{
		"environment": "prod",
		"release_id":  "rel_test",
		"publish_v2":  publishV2ControlRequest{EnvironmentDefinitionRevision: 3},
	}
	resp, status, err := deployRequest(context.Background(), newControlTestCtx(srv), body)
	if err != nil {
		t.Fatalf("deployRequest: %v", err)
	}
	if status != http.StatusAccepted || resp.State != "Provisioning" || !resp.Async {
		t.Fatalf("status=%d resp=%+v, want an async 202", status, resp)
	}
	want, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(received, want) {
		t.Fatalf("sent\n%s\nwant the value of\n%s", received, want)
	}
}

// TestDeployRequestRefusesAMemberTheContractDoesNotDeclare: a body member
// control-api does not declare fails before anything is sent.
func TestDeployRequestRefusesAMemberTheContractDoesNotDeclare(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := deployRequest(context.Background(), newControlTestCtx(srv), map[string]any{"release_id": "rel_test", "surprise": 1})
	if err == nil || !strings.Contains(err.Error(), "does not match the control-api contract") {
		t.Fatalf("err = %v, want a contract mismatch", err)
	}
	if calls != 0 {
		t.Fatalf("the server saw %d request(s), want none", calls)
	}
}

// TestDeployRequestReadsNullMembers: encoding/json writes a nil list as null,
// and the answer still decodes as it did before the generated client.
func TestDeployRequestReadsNullMembers(t *testing.T) {
	srv := httptest.NewServer(answer(http.StatusOK, `{"release_id":"rel_test","state":"Ready","projects":null,`+
		`"attempts":[{"attempt_number":1,"worker_revision":null}],`+
		`"timings":{"build_ms":null,"total_wall_clock_ms":12}}`))
	defer srv.Close()

	resp, status, err := deployRequest(context.Background(), newControlTestCtx(srv), map[string]any{"release_id": "rel_test"})
	if err != nil {
		t.Fatalf("deployRequest: %v", err)
	}
	if status != http.StatusOK || resp.State != "Ready" || len(resp.Attempts) != 1 {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
	if resp.Timings == nil || resp.Timings.BuildMS != nil || resp.Timings.TotalWallClockMS == nil || *resp.Timings.TotalWallClockMS != 12 {
		t.Fatalf("timings = %+v", resp.Timings)
	}
}

// TestDeployRequestNamesAnAnswerOutsideTheContract keeps the old text for a
// success answer that is not the declared JSON.
func TestDeployRequestNamesAnAnswerOutsideTheContract(t *testing.T) {
	srv := httptest.NewServer(answer(http.StatusOK, `{"state":`))
	defer srv.Close()

	ctx := newControlTestCtx(srv)
	_, _, err := deployRequest(context.Background(), ctx, map[string]any{"release_id": "rel_test"})
	if err == nil || err.Error() != "invalid JSON response from "+ctx.WorkspaceURL("/deploy") {
		t.Fatalf("err = %v, want the invalid JSON response error", err)
	}
}

// TestDeployRequestKeepsTheTransportCause: a failed connection is a
// transportError the --wait loop retries, and it names the cause.
func TestDeployRequestKeepsTheTransportCause(t *testing.T) {
	srv := httptest.NewServer(answer(http.StatusOK, `{}`))
	ctx := newControlTestCtx(srv)
	srv.Close()

	_, _, err := deployRequest(context.Background(), ctx, map[string]any{"release_id": "rel_test"})
	if err == nil {
		t.Fatal("want a transport error")
	}
	if !isTransientDeployError(err) {
		t.Fatalf("err = %v, want a transient transport error", err)
	}
	target := ctx.WorkspaceURL("/deploy")
	if want := "request failed for " + target + `: Post "` + target + `": `; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %q, want the prefix %q", err.Error(), want)
	}
}

// TestDeployRequestReportsACanceledCall keeps the cancel text and exit code.
func TestDeployRequestReportsACanceledCall(t *testing.T) {
	srv := httptest.NewServer(answer(http.StatusOK, `{}`))
	defer srv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := deployRequest(reqCtx, newControlTestCtx(srv), map[string]any{"release_id": "rel_test"})
	if err == nil || err.Error() != "deploy canceled by user" || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("err = %v, want the cancel error", err)
	}
}

// TestDeployStatusRequestRendersRefusals pins the status read's refusal text
// for each answer shape a control plane sends.
func TestDeployStatusRequestRendersRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"not found", 404, `{"error":"not_found","message":"no deploy found for this release","workspace_id":"ws-acme","release_id":"rel_x"}`,
			"deploy status: no deploy found for this release"},
		{"gcp body", 500, `{"code":"control.cloud_failure","error":"internal","message":"read run: x","gcp_response_body":"gcp says no","details":{"gcp_response_body":"gcp says no"}}`,
			"deploy status: read run: x\n  GCP response body: gcp says no"},
		{"error only", 403, `{"error":"forbidden"}`, "deploy status: forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(answer(tc.status, tc.body))
			defer srv.Close()

			_, status, err := deployStatusRequest(context.Background(), newControlTestCtx(srv), "rel_x")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if status != tc.status || clicore.ExitCode(err) != clicore.ExitAPI {
				t.Fatalf("status=%d exit=%d", status, clicore.ExitCode(err))
			}
		})
	}
}

// TestDeployStatusFetchReadsTheStatusAnswer reads the release path's status
// answer, members the poll does not read included.
func TestDeployStatusFetchReadsTheStatusAnswer(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		answer(http.StatusOK, `{"release_id":"rel/x","state":"Ready","projects":[{"name":"a","status":"Ready"}],`+
			`"migrations":[{"project":"a","status":"applied"}],"trigger":{"kind":"cli"}}`)(w, r)
	}))
	defer srv.Close()

	resp, status, err := deployStatusFetch(context.Background(), newControlTestCtx(srv), "rel/x")
	if err != nil {
		t.Fatalf("deployStatusFetch: %v", err)
	}
	if path != "/v1/workspaces/ws-acme/deploy/rel%2Fx" {
		t.Errorf("path = %q, want the escaped release id", path)
	}
	if status != http.StatusOK || resp.State != "Ready" || len(resp.Projects) != 1 || len(resp.Migrations) != 1 || resp.Trigger == nil {
		t.Fatalf("status=%d resp=%+v", status, resp)
	}
}

func TestDeployRefusalEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not JSON", `bad gateway`, `bad gateway`},
		{"not an object", `["x"]`, `["x"]`},
		{"already an envelope", `{"code":"http.bad_request","message":"bad"}`, `{"code":"http.bad_request","message":"bad"}`},
		{"error becomes message", `{"error":"bad"}`, `{"error":"bad","message":"bad"}`},
		{"non-string message", `{"message":42}`, `{"message":"42"}`},
		{"coded body keeps its code", `{"code":"conflict","error":"deploy_in_progress","message":"m"}`,
			`{"code":"conflict","error":"deploy_in_progress","message":"m"}`},
		{"a code-less body gets no code", `{"error":"e","rejections":[{"error":"e"}]}`, `{"error":"e","message":"e","rejections":[{"error":"e"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deployRefusalEnvelope([]byte(tc.body))
			if string(got) == tc.want {
				return
			}
			if !json.Valid([]byte(tc.want)) || !sameJSON(got, []byte(tc.want)) {
				t.Fatalf("envelope = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestWithoutNullMembers(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"a":1}`, `{"a":1}`},
		{`not json null`, `not json null`},
		{`{"a":null} {"b":1}`, `{"a":null} {"b":1}`},
		{`{"a":null,"b":[{"c":null,"d":"null"}],"e":12345678901234567890}`, `{"b":[{"d":"null"}],"e":12345678901234567890}`},
	}
	for _, tc := range cases {
		if got := string(withoutNullMembers([]byte(tc.body))); got != tc.want {
			t.Errorf("withoutNullMembers(%s) = %s, want %s", tc.body, got, tc.want)
		}
	}
}

func TestDeployCallFailureKeepsTheOldTexts(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := deployCallFailure(canceled, "t", nil, errors.New("x")); err.Error() != "deploy canceled by user" {
		t.Errorf("canceled: %v", err)
	}
	read := &deployExchange{read: errors.New("unexpected EOF")}
	if err := deployCallFailure(context.Background(), "t", read, errors.New("x")); err.Error() != "read response: unexpected EOF" || isTransientDeployError(err) {
		t.Errorf("read: %v", err)
	}
	if err := deployCallFailure(context.Background(), "t", &deployExchange{}, errors.New("boom")); err.Error() != "request failed for t: boom" || isTransientDeployError(err) {
		t.Errorf("other: %v", err)
	}
}
