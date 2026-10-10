package runtimecli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const doctorWorkspaceID = "0b8f3c9e-6a1d-4f2e-9c7b-3d5e8a1f2b4c"

// doctorServer is a control-api double that records every request, so a test
// can assert the command made exactly one GET.
type doctorServer struct {
	mu       sync.Mutex
	requests []*http.Request
	status   int
	body     string
}

func (s *doctorServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(r.Context()))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)
	_, _ = w.Write([]byte(s.body))
}

func (s *doctorServer) seen() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// serverRowsJSON answers every server row with status, in the server's
// response shape.
func serverRowsJSON(t *testing.T, status string, override map[string]string) string {
	t.Helper()
	rows := make([]map[string]string, 0, len(envDoctorServerTitles))
	for _, id := range envDoctorOrder {
		title, server := envDoctorServerTitles[id]
		if !server {
			continue
		}
		rowStatus := status
		if value, ok := override[id]; ok {
			rowStatus = value
		}
		fix := ""
		if rowStatus != EnvDoctorOK {
			fix = "fix " + id
		}
		rows = append(rows, map[string]string{"id": id, "title": title, "status": rowStatus, "detail": "detail " + id, "fix": fix})
	}
	data, err := json.Marshal(map[string]any{"workspace": doctorWorkspaceID, "environment": "prod", "rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeDoctorFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	switch typed := value.(type) {
	case string:
		data = []byte(typed)
	default:
		var err error
		if data, err = json.Marshal(typed); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorWorkspace is a linked checkout whose prod environment selects two
// workloads, one named differently from its path.
func doctorWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeDoctorFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
		"registries": map[string]any{"oci": map[string]any{"publish": "oci.putnami.dev/acme"}},
		"options": map[string]any{"@putnami/cloud": map[string]any{"workspace": map[string]any{
			"workspace_id": doctorWorkspaceID, "control_plane_url": "https://control.invalid",
		}}},
	})
	writeDoctorFile(t, filepath.Join(root, "putnami.ci.json"), map[string]any{
		"distribution": map[string]any{"namespace": "acme", "memberAttribution": true},
		"envs": map[string]any{"prod": map[string]any{"channel": "canary", "workloads": []any{
			map[string]any{"select": []string{"shop/workloads/api", "shop/workloads/web"}},
		}}},
	})
	writeDoctorFile(t, filepath.Join(root, "shop/workloads/api/putnami.json"), map[string]any{"name": "shop/workloads/api"})
	writeDoctorFile(t, filepath.Join(root, "shop/workloads/web/putnami.json"), map[string]any{"name": "shop-web"})
	return root
}

// okPublish answers the publish-namespaces row ok and marks the api
// workload migrated, recording what it was given.
type okPublish struct {
	namespace string
	workloads []EnvDoctorWorkload
}

func (p *okPublish) publish(_ string, namespace string, workloads []EnvDoctorWorkload) (EnvDoctorRow, []EnvDoctorWorkload) {
	p.namespace, p.workloads = namespace, workloads
	out := append([]EnvDoctorWorkload(nil), workloads...)
	for i := range out {
		out[i].Migrated = out[i].Path == "shop/workloads/api"
	}
	return EnvDoctorRow{ID: EnvDoctorRowPublishNamespaces, Title: EnvDoctorPublishNamespacesTitle, Status: EnvDoctorOK, Detail: "2 checked"}, out
}

func runEnvDoctor(t *testing.T, root string, srv *httptest.Server, params map[string]any, env map[string]string, publish EnvDoctorPublish, args ...string) (*captureIO, error) {
	t.Helper()
	cap := &captureIO{}
	if env == nil {
		env = map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}
	}
	if params == nil {
		params = map[string]any{}
	}
	err := Env(params, append([]string{"doctor"}, args...), root, env, cap.io(srv.Client()), publish)
	return cap, err
}

// decodeDoctorNode reads the status node an env doctor envelope carries.
func decodeDoctorNode(t *testing.T, lines []string) clicore.StatusNode {
	t.Helper()
	var node clicore.StatusNode
	if err := decodeResultData([]byte(strings.Join(lines, "\n")), &node); err != nil {
		t.Fatalf("decode %q: %v", lines, err)
	}
	return node
}

// doctorMetric is the value of one metric of the doctor node.
func doctorMetric(t *testing.T, node clicore.StatusNode, id string) int {
	t.Helper()
	for _, metric := range node.Metrics {
		if metric.ID == id {
			return int(metric.Value)
		}
	}
	t.Fatalf("node has no metric %s: %+v", id, node.Metrics)
	return 0
}

// doctorChildIDs is the row id of each child, in order.
func doctorChildIDs(node clicore.StatusNode) []string {
	ids := make([]string, 0, len(node.Children))
	for _, child := range node.Children {
		ids = append(ids, strings.TrimPrefix(child.ID, "env.doctor."))
	}
	return ids
}

// doctorChild is the child of one row.
func doctorChild(t *testing.T, node clicore.StatusNode, id string) clicore.StatusNode {
	t.Helper()
	for _, child := range node.Children {
		if child.ID == "env.doctor."+id {
			return child
		}
	}
	t.Fatalf("node has no row %s", id)
	return clicore.StatusNode{}
}

func rowIDs(rows []EnvDoctorRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// TestEnvDoctorReadyExitsZeroAfterOneGET is acceptance (b) on the CLI side:
// every row ok, exit 0, and the one request is a GET that names the selected
// workloads the way the route reads them.
func TestEnvDoctorReadyExitsZeroAfterOneGET(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, nil)}
	srv := httptest.NewServer(server)
	defer srv.Close()
	publish := &okPublish{}

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"}, nil, publish.publish)
	if err != nil {
		t.Fatalf("a ready environment must exit 0: %v", err)
	}
	node := decodeDoctorNode(t, cap.stdout)
	if node.State != clicore.StatusOK || node.Title != "env doctor prod" ||
		doctorMetric(t, node, "prerequisites_missing") != 0 || doctorMetric(t, node, "prerequisites_unchecked") != 0 {
		t.Fatalf("node = %+v", node)
	}
	if got := doctorChildIDs(node); !slices.Equal(got, envDoctorOrder) {
		t.Fatalf("rows = %v, want the issue order %v", got, envDoctorOrder)
	}
	for _, child := range node.Children {
		if child.State != clicore.StatusOK {
			t.Errorf("row %s = %s (%s)", child.ID, child.State, child.Detail)
		}
	}
	if publish.namespace != "acme" || len(publish.workloads) != 2 || publish.workloads[1].Name != "shop-web" {
		t.Fatalf("publish saw namespace %q and %+v", publish.namespace, publish.workloads)
	}

	requests := server.seen()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want exactly one read", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", request.Method)
	}
	if want := "/v1/workspaces/" + doctorWorkspaceID + "/environments/prod/readiness"; request.URL.Path != want {
		t.Fatalf("path = %s, want %s", request.URL.Path, want)
	}
	if got := request.URL.Query().Get("workloads"); got != "shop/workloads/api=shop/workloads/api,shop/workloads/web=shop-web" {
		t.Fatalf("workloads query = %q", got)
	}
	if got := request.URL.Query().Get("migrated"); got != "shop/workloads/api" {
		t.Fatalf("migrated query = %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer user-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestEnvDoctorDeployedOnceWithOneUnknownRowExitsZero is acceptance (b) with
// one server row the control plane could not check, here database-ownership,
// and every other row ok. An unknown row is
// counted and printed with its fix, but only a missing row fails the command.
func TestEnvDoctorDeployedOnceWithOneUnknownRowExitsZero(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, map[string]string{
		"database-ownership": EnvDoctorUnknown,
	})}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"}, nil, (&okPublish{}).publish)
	if err != nil {
		t.Fatalf("a deployed environment with one unchecked row must exit 0: %v", err)
	}
	node := decodeDoctorNode(t, cap.stdout)
	if node.State != clicore.StatusUnknown || doctorMetric(t, node, "prerequisites_missing") != 0 || doctorMetric(t, node, "prerequisites_unchecked") != 1 {
		t.Fatalf("node = %+v; want unknown, 0 missing, 1 unchecked", node)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.Join(cap.stdout, "\n")), &raw); err != nil {
		t.Fatal(err)
	}
	if status, _ := raw["status"].(string); status != "success" {
		t.Fatalf("envelope status = %q, want success: %v", status, raw)
	}

	cap, err = runEnvDoctor(t, doctorWorkspace(t), srv, nil, nil, (&okPublish{}).publish)
	if err != nil {
		t.Fatalf("human view: %v", err)
	}
	text := strings.Join(cap.stdout, "\n")
	if !strings.Contains(text, "unknown  database-ownership") || !strings.Contains(text, "fix: fix database-ownership") {
		t.Fatalf("the unchecked row and its fix must print:\n%s", text)
	}
	if first := cap.stdout[0]; !strings.HasPrefix(first, "env doctor prod  unknown  9 of 10 prerequisites ok, 1 could not be checked") {
		t.Fatalf("header = %q", first)
	}
}

// TestEnvDoctorNotReadyExitsOneAndTheEnvelopeCarriesTheRows pins the exit
// contract: a missing row exits 1, the human view prints each fix and ends
// with the counts, and the structured failure envelope still carries every
// row.
func TestEnvDoctorNotReadyExitsOneAndTheEnvelopeCarriesTheRows(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, map[string]string{
		"put-binding": EnvDoctorMissing, "database-ownership": EnvDoctorUnknown,
	})}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, nil, nil, nil)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v), want %d", clicore.ExitCode(err), err, clicore.ExitFailure)
	}
	if want := "env doctor prod is failing: 7 of 10 prerequisites ok, 1 missing, 2 could not be checked"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	text := strings.Join(cap.stdout, "\n")
	if !strings.HasPrefix(text, "env doctor prod  failing  7 of 10 prerequisites ok") {
		t.Fatalf("heading = %q", text)
	}
	for _, want := range []string{
		"failing  put-binding",
		"fix: fix put-binding",
		"unknown  publish-namespaces",
		"fix: run `putnami cloud env doctor` from the Putnami CLI",
		"ok       oci-registry",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("human output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "fix: fix oci-binding") {
		t.Errorf("an ok row printed a fix:\n%s", text)
	}

	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	node := decodeDoctorNode(t, envelope.stdout)
	if node.State != clicore.StatusFailing || doctorMetric(t, node, "prerequisites_missing") != 1 ||
		doctorMetric(t, node, "prerequisites_unchecked") != 2 || len(node.Children) != len(envDoctorOrder) {
		t.Fatalf("envelope data = %+v", node)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.Join(envelope.stdout, "\n")), &raw); err != nil {
		t.Fatal(err)
	}
	if code, _ := raw["exitCode"].(float64); int(code) != clicore.ExitFailure {
		t.Fatalf("envelope exitCode = %v, want %d: %v", raw["exitCode"], clicore.ExitFailure, raw)
	}
}

// TestEnvDoctorStructuredFailurePrintsNoText pins that --output=json leaves
// stdout to the one envelope the dispatcher writes.
func TestEnvDoctorStructuredFailurePrintsNoText(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorMissing, nil)}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"}, nil, (&okPublish{}).publish)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit = %d (%v)", clicore.ExitCode(err), err)
	}
	if len(cap.stdout) != 0 {
		t.Fatalf("structured failure printed %q before the envelope", cap.stdout)
	}
}

// TestEnvDoctorJSONShapeIsStable pins the --output data keys, the row order
// and the statuses scripts read.
func TestEnvDoctorJSONShapeIsStable(t *testing.T) {
	result := envDoctorMerge("ws-acme", "prod",
		[]EnvDoctorRow{{ID: EnvDoctorRowOCIRegistry, Title: "OCI publish registry", Status: EnvDoctorOK, Detail: "images publish to oci.putnami.dev/acme"}},
		[]EnvDoctorRow{{ID: "put-binding", Title: "Put namespace binding", Status: EnvDoctorMissing, Detail: "no active put binding", Fix: "activate it"}},
	)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"workspace":"ws-acme","environment":"prod","ready":false,"missing":1,"unchecked":8,"rows":[` +
		`{"id":"put-binding","title":"Put namespace binding","status":"missing","detail":"no active put binding","fix":"activate it"},` +
		`{"id":"oci-registry","title":"OCI publish registry","status":"ok","detail":"images publish to oci.putnami.dev/acme","fix":""},` +
		`{"id":"oci-binding","title":"OCI namespace binding","status":"unknown","detail":"no check answered this row","fix":"upgrade the Putnami CLI and control-api, then run ` + "`putnami cloud env doctor`" + ` again"},`
	if !strings.HasPrefix(string(data), want) {
		t.Fatalf("JSON =\n%s\nwant prefix\n%s", data, want)
	}
}

// TestEnvDoctorMergeKeepsANewerServerRow pins forward compatibility: a row
// this CLI does not know follows the known ones and still decides readiness;
// a duplicate id keeps its first answer.
func TestEnvDoctorMergeKeepsANewerServerRow(t *testing.T) {
	server := make([]EnvDoctorRow, 0, len(envDoctorOrder)+2)
	for _, id := range envDoctorOrder {
		server = append(server, EnvDoctorRow{ID: id, Status: EnvDoctorOK})
	}
	server = append(server,
		EnvDoctorRow{ID: "future-check", Status: EnvDoctorMissing, Fix: "do the new thing"},
		EnvDoctorRow{ID: "put-binding", Status: EnvDoctorMissing},
	)
	result := envDoctorMerge("ws", "prod", nil, server)
	ids := rowIDs(result.Rows)
	if want := append(append([]string(nil), envDoctorOrder...), "future-check"); !slices.Equal(ids, want) {
		t.Fatalf("rows = %v, want %v", ids, want)
	}
	if result.Ready || result.Missing != 1 || result.Unchecked != 0 {
		t.Fatalf("a missing row this CLI does not know must keep the environment not ready: %+v", result)
	}
	if result.Rows[0].Status != EnvDoctorOK {
		t.Fatalf("the first put-binding answer must win: %+v", result.Rows[0])
	}
}

// TestEnvDoctorServerFailureDegradesOnlyServerRows pins the failure contract:
// a control-api that cannot answer turns the server rows unknown with the
// reason, and the checkout rows still report what the checkout says. Nothing
// is missing, so the environment is unknown: the command exits 0, and 1
// under --strict.
func TestEnvDoctorServerFailureDegradesOnlyServerRows(t *testing.T) {
	server := &doctorServer{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"}, nil, (&okPublish{}).publish)
	if err != nil {
		t.Fatalf("an unanswered server leaves rows unknown, which fails only under --strict: %v", err)
	}
	node := decodeDoctorNode(t, cap.stdout)
	if node.State != clicore.StatusUnknown || doctorMetric(t, node, "prerequisites_missing") != 0 ||
		doctorMetric(t, node, "prerequisites_unchecked") != len(envDoctorServerTitles) {
		t.Fatalf("node = %+v", node)
	}
	for _, id := range envDoctorOrder {
		child := doctorChild(t, node, id)
		_, server := envDoctorServerTitles[id]
		switch {
		case server && (child.State != clicore.StatusUnknown || !strings.Contains(child.Detail, "control-api did not answer the readiness read") || child.Fix == ""):
			t.Errorf("server row %s = %+v", id, child)
		case !server && child.State != clicore.StatusOK:
			t.Errorf("checkout row %s = %+v", id, child)
		}
	}
	_, err = runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json", "strict": true}, nil, (&okPublish{}).publish)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("strict exit = %d (%v), want %d", clicore.ExitCode(err), err, clicore.ExitFailure)
	}
}

// TestEnvDoctorUnresolvedSessionMakesNoCall pins that a session the CLI cannot
// resolve degrades the server rows without any request.
func TestEnvDoctorUnresolvedSessionMakesNoCall(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, nil)}
	srv := httptest.NewServer(server)
	defer srv.Close()

	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"},
		map[string]string{"PUTNAMI_CLOUD_TOKEN": "two words"}, (&okPublish{}).publish)
	if err != nil {
		t.Fatalf("unchecked rows alone must not fail the command: %v", err)
	}
	if got := len(server.seen()); got != 0 {
		t.Fatalf("requests = %d, want none", got)
	}
	node := decodeDoctorNode(t, cap.stdout)
	if node.State != clicore.StatusUnknown {
		t.Fatalf("state = %s without a server answer", node.State)
	}
	if got := doctorMetric(t, node, "prerequisites_unchecked"); got != len(envDoctorServerTitles) {
		t.Fatalf("unchecked = %d, want every server row", got)
	}
	put := node.Children[0]
	if put.ID != "env.doctor.put-binding" || put.State != clicore.StatusUnknown || !strings.Contains(put.Detail, "workspace session could not be resolved") {
		t.Fatalf("put-binding = %+v", put)
	}
}

// TestEnvDoctorPicksTheEnvironment pins the default order: the positional,
// then --env, then the linked environment, then prod.
func TestEnvDoctorPicksTheEnvironment(t *testing.T) {
	cases := []struct {
		name   string
		link   string
		params map[string]any
		args   []string
		want   string
	}{
		{name: "default", want: "prod"},
		{name: "linked", link: "staging", want: "staging"},
		{name: "flag", link: "staging", params: map[string]any{"env": "qa"}, want: "qa"},
		{name: "positional", link: "staging", params: map[string]any{"env": "qa"}, args: []string{"--putnamiContext", "ctx", "--env", "qa", "preview"}, want: "preview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, nil)}
			srv := httptest.NewServer(server)
			defer srv.Close()
			root := doctorWorkspace(t)
			if tc.link != "" {
				writeDoctorFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
					"options": map[string]any{"@putnami/cloud": map[string]any{"workspace": map[string]any{
						"workspace_id": doctorWorkspaceID, "control_plane_url": "https://control.invalid", "environment": tc.link,
					}}},
				})
			}
			_, _ = runEnvDoctor(t, root, srv, tc.params, nil, nil, tc.args...)
			requests := server.seen()
			if len(requests) != 1 {
				t.Fatalf("requests = %d", len(requests))
			}
			if want := "/environments/" + tc.want + "/readiness"; !strings.HasSuffix(requests[0].URL.Path, want) {
				t.Fatalf("path = %s, want suffix %s", requests[0].URL.Path, want)
			}
		})
	}
}

func TestEnvRefusesUsageErrorsBeforeAnyCall(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, nil)}
	srv := httptest.NewServer(server)
	defer srv.Close()
	root := doctorWorkspace(t)
	env := map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}
	for name, args := range map[string][]string{
		"unknown subcommand": {"inspect"},
		"two environments":   {"doctor", "prod", "staging"},
		"malformed env":      {"doctor", "prod env"},
		"env too long":       {"doctor", strings.Repeat("a", 65)},
	} {
		t.Run(name, func(t *testing.T) {
			cap := &captureIO{}
			err := Env(map[string]any{}, args, root, env, cap.io(srv.Client()), nil)
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit = %d (%v), want usage", clicore.ExitCode(err), err)
			}
		})
	}
	if got := len(server.seen()); got != 0 {
		t.Fatalf("requests = %d, want none", got)
	}
}

func TestEnvHelpListsDoctor(t *testing.T) {
	for _, params := range []map[string]any{{}, {"output": "json"}} {
		cap := &captureIO{}
		if err := Env(params, nil, t.TempDir(), map[string]string{}, cap.io(nil), nil); err != nil {
			t.Fatal(err)
		}
		if text := strings.Join(cap.stdout, "\n"); !strings.Contains(text, "cloud env doctor [") {
			t.Fatalf("help = %q", text)
		}
	}
	cap := &captureIO{}
	if err := Env(map[string]any{}, []string{"help"}, t.TempDir(), map[string]string{}, cap.io(nil), nil); err != nil || len(cap.stdout) == 0 {
		t.Fatalf("help: %v %q", err, cap.stdout)
	}
}

func TestEnvDoctorWithoutALinkFails(t *testing.T) {
	cap := &captureIO{}
	err := Env(map[string]any{}, []string{"doctor"}, t.TempDir(), map[string]string{}, cap.io(nil), nil)
	if err == nil || clicore.ExitCode(err) == clicore.ExitSuccess {
		t.Fatalf("an unlinked workspace must fail: %v", err)
	}
}

func TestEnvDoctorQueryEncodesNamesAndMigrations(t *testing.T) {
	list, migrated := envDoctorQuery([]EnvDoctorWorkload{
		{Path: "a/workloads/api", Name: "a/workloads/api", Migrated: true},
		{Path: "a/workloads/web", Name: "a-web"},
		{Path: "a/workloads/job"},
	})
	// A name equal to the path still goes out: a bare path tells the route
	// the manifest name is not known.
	if list != "a/workloads/api=a/workloads/api,a/workloads/web=a-web,a/workloads/job" || migrated != "a/workloads/api" {
		t.Fatalf("list = %q, migrated = %q", list, migrated)
	}
	if list, migrated := envDoctorQuery(nil); list != "" || migrated != "" {
		t.Fatalf("empty selection encoded %q %q", list, migrated)
	}
}

// TestEnvDoctorReadyHumanViewIsOneBlock pins the ready human view.
func TestEnvDoctorReadyHumanViewIsOneBlock(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, nil)}
	srv := httptest.NewServer(server)
	defer srv.Close()
	cap, err := runEnvDoctor(t, doctorWorkspace(t), srv, nil, nil, (&okPublish{}).publish)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(cap.stdout, "\n")
	if !strings.HasPrefix(text, "env doctor prod  ok  10 of 10 prerequisites ok\n") || strings.Contains(text, "fix:") {
		t.Fatalf("ready view:\n%s", text)
	}
}

func TestRenderEnvDoctorIndentsMultiLineFixes(t *testing.T) {
	text := renderEnvDoctor(EnvDoctorResult{Environment: "prod", Missing: 1, Rows: []EnvDoctorRow{
		{ID: "ci-envs", Status: EnvDoctorMissing, Detail: "line one\nline two", Fix: "first\nsecond\n"},
	}})
	want := "Environment prod of workspace (unresolved)\n\n" +
		"missing  ci-envs  line one line two\n" +
		"                  fix: first\n" +
		"                       second\n" +
		"\nnot ready: 1 missing, 0 could not be checked\n"
	if text != want {
		t.Fatalf("render =\n%q\nwant\n%q", text, want)
	}
}

// --- checkout rows ---

func checkoutRow(t *testing.T, rows []EnvDoctorRow, id string) EnvDoctorRow {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no %s row in %+v", id, rows)
	return EnvDoctorRow{}
}

func TestEnvDoctorOCIRegistryNeedsAHostAndANamespace(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		status   string
		detail   string
	}{
		"absent":       {manifest: "", status: EnvDoctorMissing, detail: "names no registries.oci.publish"},
		"host only":    {manifest: `{"registries":{"oci":{"publish":"oci.putnami.dev"}}}`, status: EnvDoctorMissing, detail: "oci.putnami.dev names no namespace"},
		"empty segs":   {manifest: `{"registries":{"oci":{"publish":"oci.putnami.dev//"}}}`, status: EnvDoctorMissing, detail: "names no namespace"},
		"unparseable":  {manifest: `{`, status: EnvDoctorMissing, detail: "names no registries.oci.publish"},
		"host and ns":  {manifest: `{"registries":{"oci":{"publish":"oci.putnami.dev/acme"}}}`, status: EnvDoctorOK, detail: "images publish to oci.putnami.dev/acme"},
		"nested path":  {manifest: `{"registries":{"oci":{"publish":"oci.putnami.dev/acme/images"}}}`, status: EnvDoctorOK, detail: "oci.putnami.dev/acme/images"},
		"trimmed path": {manifest: `{"registries":{"oci":{"publish":"  oci.putnami.dev/acme "}}}`, status: EnvDoctorOK, detail: "images publish to oci.putnami.dev/acme"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if tc.manifest != "" {
				writeDoctorFile(t, filepath.Join(root, "putnami.workspace.json"), tc.manifest)
			}
			row := envDoctorOCIRegistry(root, "acme")
			if row.Status != tc.status || !strings.Contains(row.Detail, tc.detail) {
				t.Fatalf("row = %+v", row)
			}
			if tc.status == EnvDoctorOK && row.Fix != "" {
				t.Fatalf("an ok row carries a fix: %q", row.Fix)
			}
			if tc.status != EnvDoctorOK && !strings.Contains(row.Fix, `"publish": "oci.putnami.dev/acme"`) {
				t.Fatalf("fix = %q", row.Fix)
			}
		})
	}
	if fix := envDoctorOCIRegistry(t.TempDir(), "").Fix; !strings.Contains(fix, "oci.putnami.dev/<ns>") {
		t.Fatalf("without a namespace the fix must use a placeholder: %q", fix)
	}
}

func TestEnvDoctorCIEnvsNamesEachProblem(t *testing.T) {
	type ci = map[string]any
	selectEnv := func(attribution bool, selects ...any) ci {
		rules := make([]any, 0, len(selects))
		for _, sel := range selects {
			rules = append(rules, ci{"select": sel})
		}
		return ci{
			"distribution": ci{"namespace": "acme", "memberAttribution": attribution},
			"envs":         ci{"prod": ci{"channel": "canary", "workloads": rules}},
		}
	}
	cases := map[string]struct {
		document  any
		status    string
		detail    string
		workloads []string
	}{
		"no file":         {document: nil, status: EnvDoctorMissing, detail: "no readable putnami.ci.json"},
		"not json":        {document: "{", status: EnvDoctorMissing, detail: "does not parse"},
		"no env":          {document: ci{"envs": ci{"staging": ci{}}}, status: EnvDoctorMissing, detail: "declares no envs.prod"},
		"no attribution":  {document: selectEnv(false, "shop/workloads/api"), status: EnvDoctorMissing, detail: "memberAttribution is not true", workloads: []string{"shop/workloads/api"}},
		"glob selector":   {document: selectEnv(true, "*/workloads/*"), status: EnvDoctorMissing, detail: `"*/workloads/*" is not an exact workload path`},
		"no manifest":     {document: selectEnv(true, "shop/workloads/gone"), status: EnvDoctorMissing, detail: "shop/workloads/gone has no readable putnami.json"},
		"bad select type": {document: selectEnv(true, 7), status: EnvDoctorMissing, detail: "neither a string nor a list"},
		"empty selection": {document: selectEnv(true), status: EnvDoctorMissing, detail: "envs.prod selects no workload"},
		"string and list": {
			document:  selectEnv(true, "shop/workloads/api", []string{"shop/workloads/web", "shop/workloads/api"}),
			status:    EnvDoctorOK,
			detail:    "prod follows canary and selects 2 workload(s); member attribution is on",
			workloads: []string{"shop/workloads/api", "shop/workloads/web"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeDoctorFile(t, filepath.Join(root, "shop/workloads/api/putnami.json"), `{"name":"shop/workloads/api"}`)
			writeDoctorFile(t, filepath.Join(root, "shop/workloads/web/putnami.json"), `{"name":"shop-web"}`)
			if tc.document != nil {
				writeDoctorFile(t, filepath.Join(root, "putnami.ci.json"), tc.document)
			}
			row, _, workloads := envDoctorCIEnvs(root, "prod")
			if row.Status != tc.status || !strings.Contains(row.Detail, tc.detail) {
				t.Fatalf("row = %+v", row)
			}
			if tc.status != EnvDoctorOK && (!strings.Contains(row.Fix, `"memberAttribution": true`) || !strings.Contains(row.Fix, `"prod": {"channel"`)) {
				t.Fatalf("fix = %q", row.Fix)
			}
			paths := make([]string, 0, len(workloads))
			for _, workload := range workloads {
				paths = append(paths, workload.Path)
			}
			if !slices.Equal(paths, tc.workloads) {
				t.Fatalf("workloads = %v, want %v", paths, tc.workloads)
			}
		})
	}
}

func TestEnvDoctorCIEnvsReportsAReadError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "putnami.ci.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	row, _, _ := envDoctorCIEnvs(root, "prod")
	if row.Status != EnvDoctorMissing || !strings.Contains(row.Detail, "could not be read") {
		t.Fatalf("row = %+v", row)
	}
}

func TestEnvDoctorCheckoutWithoutWorkloadsOrHookIsUnknown(t *testing.T) {
	rows, workloads, namespace := envDoctorCheckout(t.TempDir(), "prod", (&okPublish{}).publish)
	if row := checkoutRow(t, rows, EnvDoctorRowPublishNamespaces); row.Status != EnvDoctorUnknown || row.Fix == "" || len(workloads) != 0 || namespace != "" {
		t.Fatalf("no selection: %+v %v %q", row, workloads, namespace)
	}
	rows, workloads, namespace = envDoctorCheckout(doctorWorkspace(t), "prod", nil)
	if row := checkoutRow(t, rows, EnvDoctorRowPublishNamespaces); row.Status != EnvDoctorUnknown || !strings.Contains(row.Detail, "cannot read") || len(workloads) != 2 || namespace != "acme" {
		t.Fatalf("no hook: %+v %v %q", row, workloads, namespace)
	}
}

// TestEnvDoctorCheckoutWritesNothing pins the read-only contract for the
// checkout rows: the tree is byte-identical after the checks.
func TestEnvDoctorCheckoutWritesNothing(t *testing.T) {
	root := doctorWorkspace(t)
	before := snapshotTree(t, root)
	envDoctorCheckout(root, "prod", (&okPublish{}).publish)
	envDoctorCheckout(root, "missing-env", nil)
	if after := snapshotTree(t, root); !mapsEqual(before, after) {
		t.Fatalf("the checkout changed:\nbefore %v\nafter  %v", before, after)
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[path] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// TestEnvDoctorServerRowsArePrintableAndJudged pins that a server string
// cannot drive the terminal and that an unknown status never reads ok.
func TestEnvDoctorServerRowsArePrintableAndJudged(t *testing.T) {
	row := envDoctorServerRow(EnvDoctorRow{
		ID: "put-binding", Title: "Put\x1b[2J binding", Status: "ok",
		Detail: "active\x1b]0;title\x07 namespace", Fix: "",
	})
	if row.Title != "Put[2J binding" || row.Detail != "active]0;title namespace" || row.Status != EnvDoctorOK {
		t.Fatalf("row = %+v", row)
	}
	fix := envDoctorServerRow(EnvDoctorRow{ID: "x", Status: EnvDoctorMissing, Fix: "line one\r\n\tline two"})
	if fix.Fix != "line one\n\tline two" {
		t.Fatalf("fix = %q", fix.Fix)
	}
	for _, status := range []string{"passed", "OK", ""} {
		judged := envDoctorServerRow(EnvDoctorRow{ID: "put-binding", Status: status, Detail: "d"})
		if judged.Status != EnvDoctorUnknown || !strings.Contains(judged.Detail, "the server answered status") || judged.Fix == "" {
			t.Fatalf("status %q = %+v", status, judged)
		}
	}
}

// TestEnvDoctorStrictFailsOnAnUnknownRow pins the CI gate: the same answer
// that exits 0 by default (nothing missing, one row unknown) exits 1 under
// --strict, and the envelope still carries the rows.
func TestEnvDoctorStrictFailsOnAnUnknownRow(t *testing.T) {
	server := &doctorServer{status: http.StatusOK, body: serverRowsJSON(t, EnvDoctorOK, map[string]string{
		"database-ownership": EnvDoctorUnknown,
	})}
	srv := httptest.NewServer(server)
	defer srv.Close()

	if _, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json"}, nil, (&okPublish{}).publish); err != nil {
		t.Fatalf("default mode: unknown rows alone must not fail the command: %v", err)
	}
	_, err := runEnvDoctor(t, doctorWorkspace(t), srv, map[string]any{"output": "json", "strict": true}, nil, (&okPublish{}).publish)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("strict exit = %d (%v), want %d", clicore.ExitCode(err), err, clicore.ExitFailure)
	}
	envelope := &captureIO{}
	clicore.WriteErrorResult(err, map[string]any{"output": "json"}, envelope.io(nil))
	node := decodeDoctorNode(t, envelope.stdout)
	if node.State != clicore.StatusUnknown || doctorMetric(t, node, "prerequisites_missing") != 0 || doctorMetric(t, node, "prerequisites_unchecked") != 1 {
		t.Fatalf("node = %+v; want unknown, 0 missing, 1 unchecked", node)
	}
}
