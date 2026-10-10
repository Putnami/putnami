package configcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// statusFakeServer serves several config apps: the app list, each app's
// schema, its resolved config per environment and its secret status per
// environment. An app with no schema answers 404 on both schema routes and on
// the secret status route, as config-api does.
type statusFakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	apps    []map[string]any
	appsErr int
	// schemas maps an app to the non-secret and secret fields it declares.
	schemas map[string][]map[string]any
	// configs maps "app/env" to the config the resolve answers.
	configs map[string]map[string]any
	// secretsSet maps "app/env" to the secret keys that are set.
	secretsSet map[string][]string
	// statusCodes maps "app/env" to a status the secret status route answers.
	statusCodes map[string]int
	// grants lists the secret grants; grantsErr makes the route refuse.
	grants    []map[string]any
	grantsErr int
	calls     []string
}

func (s *statusFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req.Method+" "+req.URL.Path)
	query := req.URL.Query()
	switch {
	case req.URL.Path == "/.well-known/openid-configuration":
		return jsonResponse(http.StatusOK, map[string]any{"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token"}), nil
	case req.URL.Path == "/token":
		return jsonResponse(http.StatusOK, tokenResponse(map[string]any{"scope_ref": map[string]any{"workspace_id": "ws-acme"}})), nil
	case req.URL.Path == "/v1/workspaces/ws-acme/config/apps":
		if s.appsErr != 0 {
			return jsonResponse(s.appsErr, map[string]any{"error": http.StatusText(s.appsErr)}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"apps": s.apps}), nil
	case req.URL.Path == "/v1/workspaces/ws-acme/config/secret-grants":
		if s.grantsErr != 0 {
			return jsonResponse(s.grantsErr, map[string]any{"error": http.StatusText(s.grantsErr)}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"grants": s.grants}), nil
	case req.URL.Path == "/api/schemas" && req.Method == http.MethodGet:
		return s.schema(query.Get("appName")), nil
	case strings.HasPrefix(req.URL.Path, "/api/schemas/") && req.Method == http.MethodGet:
		app, _ := url.PathUnescape(strings.TrimPrefix(req.URL.EscapedPath(), "/api/schemas/"))
		return s.schema(app), nil
	case req.URL.Path == "/api/configs/resolve" && req.Method == http.MethodPost:
		return s.resolve(body), nil
	case req.URL.Path == "/api/secrets/status" && req.Method == http.MethodGet:
		return s.secretStatus(query.Get("appName"), query.Get("environment")), nil
	default:
		s.t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		return jsonResponse(http.StatusTeapot, map[string]any{}), nil
	}
}

func (s *statusFakeServer) schema(app string) *http.Response {
	fields, ok := s.schemas[app]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "schema not registered"})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"appName": app, "version": "v1", "schemaHash": "sha256:" + app,
		"configs": []map[string]any{{"path": "app", "fields": fields}},
	})
}

func (s *statusFakeServer) resolve(body []byte) *http.Response {
	var request struct {
		AppName     string `json:"appName"`
		Environment string `json:"environment"`
	}
	_ = json.Unmarshal(body, &request)
	config := s.configs[request.AppName+"/"+request.Environment]
	return jsonResponse(http.StatusOK, map[string]any{"config": config, "resolved": len(config) > 0, "schemaMatch": true})
}

func (s *statusFakeServer) secretStatus(app, environment string) *http.Response {
	if code := s.statusCodes[app+"/"+environment]; code != 0 {
		return jsonResponse(code, map[string]any{"error": http.StatusText(code)})
	}
	fields, ok := s.schemas[app]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "schema not registered"})
	}
	set := map[string]bool{}
	for _, key := range s.secretsSet[app+"/"+environment] {
		set[key] = true
	}
	var keys []map[string]any
	for _, field := range fields {
		if field["sensitive"] != true {
			continue
		}
		key := "app." + fmt.Sprint(field["name"])
		status := "missing"
		if set[key] {
			status = "set"
		}
		keys = append(keys, map[string]any{"key": key, "path": "app", "field": field["name"], "required": field["required"] == true, "status": status})
	}
	return jsonResponse(http.StatusOK, map[string]any{"appName": app, "environment": environment, "keys": keys})
}

// newStatusFake is a workspace with three projects: api sets everything in
// prod and misses a required secret and a required key in staging; worker has
// only optional gaps; legacy has values but no published schema.
func newStatusFake(t *testing.T) *statusFakeServer {
	return &statusFakeServer{
		t: t,
		apps: []map[string]any{
			{"name": "*", "environments": []string{"prod"}},
			{"name": "worker", "environments": []string{"*"}},
			{"name": "api", "environments": []string{"staging", "prod", "*"}},
			{"name": "legacy", "environments": []string{"prod"}},
		},
		schemas: map[string][]map[string]any{
			"api": {
				{"name": "url", "type": "string", "required": true},
				{"name": "ttl", "type": "duration", "required": true, "default": "1h"},
				{"name": "token", "type": "string", "required": true, "sensitive": true},
				{"name": "webhook", "type": "string", "sensitive": true},
			},
			"worker": {
				{"name": "queue", "type": "string"},
				{"name": "signing", "type": "string", "sensitive": true},
			},
		},
		configs: map[string]map[string]any{
			"api/prod": {"app": map[string]any{"url": "https://api"}},
		},
		secretsSet: map[string][]string{
			"api/prod": {"app.token", "app.webhook"},
		},
	}
}

func statusTestIO(t *testing.T, fake http.RoundTripper) (clicore.IO, *[]string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	return ioctx, stdout
}

func TestSecretsStatusReadsEveryProjectAndNeverAValue(t *testing.T) {
	fake := newStatusFake(t)
	ioctx, stdout := statusTestIO(t, fake)
	err := runSecrets(ioctx, []string{"status"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("secrets status = %v, want a failing exit", err)
	}
	out := strings.Join(*stdout, "\n")
	for _, want := range []string{
		"secrets  failing  1 required secret missing in 1 project",
		"secrets set       2 of 5 (40%)",
		"required missing  1",
		"optional missing  2",
		"  failing   api        1 required secret missing in staging",
		"  ok          prod     2 of 2 secrets set",
		"  failing     staging  1 required secret missing: app.token",
		"                       fix: putnami cloud secrets set api app.token --env staging --from-stdin",
		"  degraded    prod     no published schema, so no secret can be checked",
		"                       fix: putnami cloud config publish legacy --env prod",
		"  ok        worker     0 of 1 secrets set, 1 optional missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "/secrets/resolve") || strings.Contains(call, "/configs/resolve") || strings.HasSuffix(call, "/api/secrets") {
			t.Errorf("secrets status read values: %s", call)
		}
	}
}

func TestSecretsStatusNamedProjectListsMissingKeys(t *testing.T) {
	fake := newStatusFake(t)
	ioctx, stdout := statusTestIO(t, fake)
	if err := runSecrets(ioctx, []string{"status", "api"}); err == nil {
		t.Fatal("a missing required secret did not fail the status")
	}
	out := strings.Join(*stdout, "\n")
	for _, want := range []string{
		"secrets api  failing  1 required secret missing in staging",
		"  failing  staging        1 required secret missing: app.token",
		"  failing    app.token    required, not set",
		"                          fix: putnami cloud secrets set api app.token --env staging --from-stdin",
		"  ok         app.webhook  optional, not set",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "worker") {
		t.Errorf("named status read another project:\n%s", out)
	}
}

func TestSecretsStatusUnknownProjectReadsTheLinkedEnvironment(t *testing.T) {
	fake := newStatusFake(t)
	ioctx, stdout := statusTestIO(t, fake)
	if err := runSecrets(ioctx, []string{"status", "ghost", "--json"}); err != nil {
		t.Fatalf("status of a project with no schema = %v, want a degraded node", err)
	}
	var data map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &data)
	children, _ := data["children"].([]any)
	if data["id"] != "secrets.ghost" || data["state"] != "degraded" || len(children) != 1 {
		t.Fatalf("data = %v", data)
	}
	if prod, _ := children[0].(map[string]any); prod["id"] != "secrets.ghost.prod" || prod["fix"] != "putnami cloud config publish ghost --env prod" {
		t.Fatalf("prod = %v", prod)
	}
}

func TestSecretsStatusKeepsTheNodeWhenOneEnvironmentFails(t *testing.T) {
	fake := newStatusFake(t)
	fake.statusCodes = map[string]int{"api/prod": http.StatusServiceUnavailable}
	ioctx, stdout := statusTestIO(t, fake)
	_ = runSecrets(ioctx, []string{"status"})
	out := strings.Join(*stdout, "\n")
	if !strings.Contains(out, "  unknown     prod     Service Unavailable") || !strings.Contains(out, "  failing     staging") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSecretsStatusNodeIsUnknownWhenTheAppListFails(t *testing.T) {
	fake := newStatusFake(t)
	fake.appsErr = http.StatusServiceUnavailable
	ioctx, _ := statusTestIO(t, fake)
	params := clicore.MergeParams(clicore.ParseFlags(nil))
	node := SecretsStatusNode(params, clicore.EnvGet(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT"), ioctx.Env, ioctx)
	if node.ID != "secrets" || node.State != clicore.StatusUnknown || node.Detail == "" {
		t.Fatalf("node = %+v", node)
	}
	config := ConfigStatusNode(params, clicore.EnvGet(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT"), ioctx.Env, ioctx)
	if config.ID != "config" || config.State != clicore.StatusUnknown {
		t.Fatalf("config node = %+v", config)
	}
}

func TestStatusNodesAreUnknownWithoutALink(t *testing.T) {
	ioctx, _ := statusTestIO(t, newStatusFake(t))
	for _, node := range []clicore.StatusNode{
		SecretsStatusNode(map[string]any{}, t.TempDir(), ioctx.Env, ioctx),
		ConfigStatusNode(map[string]any{}, t.TempDir(), ioctx.Env, ioctx),
	} {
		if node.State != clicore.StatusUnknown {
			t.Errorf("%s without a link = %+v", node.ID, node)
		}
	}
}

func TestStatusRejectsTwoProjects(t *testing.T) {
	ioctx, _ := statusTestIO(t, newStatusFake(t))
	for _, run := range []func(clicore.IO, []string) error{runSecrets, runConfig} {
		if err := run(ioctx, []string{"status", "a", "b"}); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Errorf("two projects = %v, want a usage error", err)
		}
	}
}

func TestConfigStatusReadsEveryProject(t *testing.T) {
	fake := newStatusFake(t)
	ioctx, stdout := statusTestIO(t, fake)
	err := runConfig(ioctx, []string{"status"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("config status = %v, want a failing exit", err)
	}
	out := strings.Join(*stdout, "\n")
	for _, want := range []string{
		"config  failing  1 required key missing in 1 project",
		"projects          3",
		"keys declared     5",
		"keys set          3 of 5 (60%)",
		"required missing  1",
		"optional missing  1",
		"  failing   api        1 required key missing",
		"  ok          prod     2 of 2 keys set",
		"  failing     staging  1 required key missing: app.url",
		"                       fix: putnami cloud config publish api --env staging",
		"  degraded    prod     no published schema, so no key can be checked",
		"  ok        worker     0 of 1 keys set, 1 optional missing",
		"  ok          prod     0 of 1 keys set, 1 optional missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "/secrets") {
			t.Errorf("config status read secrets: %s", call)
		}
	}
}

func TestConfigStatusNamedProjectListsMissingKeys(t *testing.T) {
	ioctx, stdout := statusTestIO(t, newStatusFake(t))
	err := runConfig(ioctx, []string{"status", "api"})
	if err == nil || err.Error() != "config api is failing: 1 required key missing" {
		t.Fatalf("status = %v, want a failing exit", err)
	}
	out := strings.Join(*stdout, "\n")
	for _, want := range []string{
		"keys set          3 of 4 (75%)",
		"  ok       prod       2 of 2 keys set",
		"  failing  staging    1 required key missing: app.url",
		"                      fix: putnami cloud config publish api --env staging",
		"  failing    app.url  required, no value and no default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
}

func TestWithinBudgetLeavesTheRestUnread(t *testing.T) {
	projects := make([]statusProject, 0, 120)
	for index := range 120 {
		projects = append(projects, statusProject{Name: fmt.Sprintf("p%02d", index), Environments: []string{"prod", "staging"}})
	}
	read, unread := withinBudget(projects, func(project statusProject) int { return 1 + len(project.Environments) })
	if len(read) != 80 || len(unread) != 40 || unread[0] != "p80" {
		t.Fatalf("read %d, unread %d (%v)", len(read), len(unread), unread[:1])
	}
	node := ConfigStatusFrom(nil, unread)
	if node.State != clicore.StatusUnknown || node.Children[0].Fix != "putnami cloud config status p80" ||
		node.Children[0].Detail != "40 projects past the 240-call budget of a status: p80, p81, p82 and 37 more" {
		t.Fatalf("node = %+v", node)
	}
	if node.Metrics[0].Value != 40 {
		t.Fatalf("projects metric = %v, want the unread projects counted", node.Metrics[0].Value)
	}
}

func TestSecretsStatusFromFoldsEachState(t *testing.T) {
	node := SecretsStatusFrom([]SecretsEnvironment{
		{Project: "a", Environment: "prod", Keys: []SecretKeyState{{Key: "k", Required: true, Set: true}}},
		{Project: "b", Environment: "prod", Err: errors.New("config-api timed out")},
	}, nil)
	if node.State != clicore.StatusUnknown || node.Detail != "2 projects, 1 check not read" {
		t.Fatalf("node = %+v", node)
	}
	if got := node.Children[1].Children[0]; got.ID != "secrets.b.prod" || got.Detail != "config-api timed out" {
		t.Fatalf("unread environment = %+v", got)
	}
	clean := SecretsStatusFrom([]SecretsEnvironment{{Project: "a", Environment: "prod", Keys: []SecretKeyState{{Key: "k", Required: true, Set: true}}}}, nil)
	if clean.State != clicore.StatusOK || clean.Detail != "1 project, every required secret set" {
		t.Fatalf("clean = %+v", clean)
	}
	if empty := SecretsStatusFrom(nil, nil); empty.State != clicore.StatusOK || empty.Detail != "no project has config or secrets" {
		t.Fatalf("empty = %+v", empty)
	}
	if none := SecretsProjectStatusFrom("a", []SecretsEnvironment{{Project: "a", Environment: "prod"}}); none.Detail != "no secret declared" || none.Children[0].Detail != "no secret declared" {
		t.Fatalf("no secret = %+v", none)
	}
}

func TestConfigStatusFromFoldsEachState(t *testing.T) {
	node := ConfigStatusFrom([]ConfigEnvironment{
		{Project: "a", Environment: "prod", Keys: []ConfigKeyState{{Key: "k", Required: true, Status: "default"}}},
		{Project: "a", Environment: "staging", Keys: []ConfigKeyState{{Key: "k", Required: true, Status: "set"}}},
	}, nil)
	if node.State != clicore.StatusOK || node.Detail != "1 project, every required key set" || node.Children[0].Detail != "2 of 2 keys set in 2 environments" {
		t.Fatalf("node = %+v", node)
	}
	optional := ConfigStatusFrom([]ConfigEnvironment{{Project: "a", Environment: "prod", Keys: []ConfigKeyState{{Key: "k", Status: "missing"}}}}, nil)
	if optional.State != clicore.StatusOK || optional.Detail != "1 project, every required key set, 1 optional missing" {
		t.Fatalf("optional = %+v", optional)
	}
	unread := ConfigProjectStatusFrom("a", []ConfigEnvironment{{Project: "a", Environment: "prod", Err: errors.New("boom")}})
	if unread.State != clicore.StatusUnknown || unread.Detail != "1 of 1 environments not read" {
		t.Fatalf("unread = %+v", unread)
	}
	empty := ConfigProjectStatusFrom("a", []ConfigEnvironment{{Project: "a", Environment: "prod"}})
	if empty.State != clicore.StatusOK || empty.Detail != "no config key declared" {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestConfigShowDeclaredListsConfigAndSecretKeys(t *testing.T) {
	home := t.TempDir()
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	fake := &configFakeServer{t: t}
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	if err := runConfig(ioctx, []string{"apps/auth-server", "--declared"}); err != nil {
		t.Fatalf("show --declared: %v", err)
	}
	want := []string{
		"Declared keys for apps/auth-server/prod:",
		"  database.host                            config  set      required",
		"  database.password                        secret  set      required",
		"  database.user                            config  missing  required",
		"  session.cookieSecret                     secret  missing  required",
		"  session.ttl                              config  set      required",
	}
	if got := strings.Join(*stdout, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("output =\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	for _, request := range fake.requests {
		if _, ok := request["includeSecrets"]; ok {
			t.Fatalf("--declared asked for secret values: %v", request)
		}
	}
}

// TestLeftoverAppsAreNamedNotRead: an app under a name no project of the
// checkout has is one degraded check, and its keys do not count.
func TestLeftoverAppsAreNamedNotRead(t *testing.T) {
	projects := []statusProject{{Name: "auth/server"}, {Name: "apps/auth-server"}, {Name: "apps/server"}}
	kept, leftover, shared := splitLeftoverApps(projects, map[string]bool{"apps/auth-server": true}, nil)
	if len(kept) != 1 || kept[0].Name != "apps/auth-server" || strings.Join(leftover, ",") != "auth/server,apps/server" || shared != nil {
		t.Fatalf("kept = %v, leftover = %v, shared = %v", kept, leftover, shared)
	}
	if all, none, _ := splitLeftoverApps(projects, nil, nil); len(all) != 3 || none != nil {
		t.Fatalf("without a checkout every app is kept: %v, %v", all, none)
	}
	node := withLeftoverApps(SecretsStatusFrom([]SecretsEnvironment{
		{Project: "apps/auth-server", Environment: "prod", Keys: []SecretKeyState{{Key: "k", Required: true, Set: true}}},
	}, nil), "secrets", leftover, nil, nil)
	check, _ := node.Child("secrets.leftover")
	if node.State != clicore.StatusDegraded || node.Detail != "1 project, every required secret set, 2 config apps with no project" ||
		check.Detail != "2 config apps under a name no project of this checkout has: auth/server, apps/server" || check.Fix != "" {
		t.Fatalf("node = %+v", node)
	}
}

// TestSharedAppsAreNotLeftovers: an app another project reads through an
// active secret grant, such as source/github-app, is named in the facts, not
// judged, and not a leftover. Without the grants, a leftover may be shared,
// so the check is unknown.
func TestSharedAppsAreNotLeftovers(t *testing.T) {
	projects := []statusProject{{Name: "auth/server"}, {Name: "source/github-app"}, {Name: "apps/source-api"}}
	checkout := map[string]bool{"apps/source-api": true}
	kept, leftover, shared := splitLeftoverApps(projects, checkout, map[string]bool{"source/github-app": true})
	if len(kept) != 1 || strings.Join(leftover, ",") != "auth/server" || strings.Join(shared, ",") != "source/github-app" {
		t.Fatalf("kept = %v, leftover = %v, shared = %v", kept, leftover, shared)
	}
	base := SecretsStatusFrom([]SecretsEnvironment{
		{Project: "apps/source-api", Environment: "prod", Keys: []SecretKeyState{{Key: "k", Required: true, Set: true}}},
	}, nil)
	node := withLeftoverApps(base, "secrets", nil, shared, nil)
	if node.State != clicore.StatusOK || node.Facts["shared_apps"] != "source/github-app" {
		t.Fatalf("shared only: %+v", node)
	}
	node = withLeftoverApps(base, "secrets", []string{"auth/server", "source/github-app"}, nil, errors.New("config-api: forbidden"))
	check, _ := node.Child("secrets.leftover")
	if node.State != clicore.StatusUnknown || check.State != clicore.StatusUnknown ||
		check.Detail != "2 config apps under a name no project of this checkout has: auth/server, source/github-app; secret grants unreadable, so some may be shared: config-api: forbidden" {
		t.Fatalf("grants unreadable: %+v", check)
	}
}

// TestWorkspaceStatusSetsSharedAppsAside: against the checkout's projects, an
// app read through an active secret grant is named in shared_apps and is not
// a leftover, while a revoked grant shares nothing.
func TestWorkspaceStatusSetsSharedAppsAside(t *testing.T) {
	fake := newStatusFake(t)
	// api in prod alone, so the node is not failing and prints its envelope.
	fake.apps[2] = map[string]any{"name": "api", "environments": []string{"prod"}}
	fake.apps = append(fake.apps, map[string]any{"name": "github-app", "environments": []string{"prod"}})
	fake.grants = []map[string]any{{"appName": "github-app", "active": true}, {"appName": "legacy", "active": false}}
	ioctx, stdout := statusTestIO(t, fake)
	for _, name := range []string{"api", "worker"} {
		dir := filepath.Join(ioctx.Env["PUTNAMI_WORKSPACE_ROOT"], name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runConfig(ioctx, []string{"status", "--json"}); err != nil && len(*stdout) == 0 {
		t.Fatalf("config status = %v", err)
	}
	var data map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &data)
	facts, _ := data["facts"].(map[string]any)
	var leftover map[string]any
	for _, child := range data["children"].([]any) {
		if node, _ := child.(map[string]any); node["id"] == "config.leftover" {
			leftover = node
		}
	}
	leftoverFacts, _ := leftover["facts"].(map[string]any)
	if facts["shared_apps"] != "github-app" || leftover["state"] != "degraded" || leftoverFacts["apps"] != "legacy" {
		t.Fatalf("facts = %v, leftover = %v", facts, leftover)
	}

	fake.grantsErr = http.StatusForbidden
	*stdout = nil
	_ = runConfig(ioctx, []string{"status", "--json"})
	decodeJSON(t, strings.Join(*stdout, "\n"), &data)
	for _, child := range data["children"].([]any) {
		if node, _ := child.(map[string]any); node["id"] == "config.leftover" {
			if node["state"] != "unknown" || !strings.Contains(node["detail"].(string), "secret grants unreadable") {
				t.Fatalf("grants refused: %v", node)
			}
			return
		}
	}
	t.Fatalf("no leftover check when the grants are refused: %v", data)
}

// TestNoLeftoverCandidateSkipsTheGrants: when every app has a project in the
// checkout, the status reads no secret grant, so a refused grant route changes
// nothing.
func TestNoLeftoverCandidateSkipsTheGrants(t *testing.T) {
	fake := newStatusFake(t)
	fake.apps = []map[string]any{{"name": "api", "environments": []string{"prod"}}}
	fake.grantsErr = http.StatusForbidden
	ioctx, stdout := statusTestIO(t, fake)
	dir := filepath.Join(ioctx.Env["PUTNAMI_WORKSPACE_ROOT"], "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"api"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runConfig(ioctx, []string{"status", "--json"}); err != nil && len(*stdout) == 0 {
		t.Fatalf("config status = %v", err)
	}
	var data map[string]any
	decodeJSON(t, strings.Join(*stdout, "\n"), &data)
	for _, child := range data["children"].([]any) {
		if node, _ := child.(map[string]any); node["id"] == "config.leftover" {
			t.Fatalf("leftover check with no candidate: %v", node)
		}
	}
	for _, call := range fake.calls {
		if strings.HasSuffix(call, "/config/secret-grants") {
			t.Fatalf("grants read with no leftover candidate: %v", fake.calls)
		}
	}
}
