package compose

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	pdb "go.putnami.dev/protocol/database"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestConfigData_ServicesAndDatabasesNeverLeakIntoOutput(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "injected-configuration-never-reaches-output",
		"the-document-carries-urls-and-bindings-and-the-status-carries-section-names-only")
	provider := &Member{
		Project:    &workspace.Project{ID: "/provider", Name: "go.acme.dev/provider"},
		ProxyURL:   "http://127.0.0.1:41001",
		serviceIDs: []string{"items"},
	}
	consumer := &Member{
		Project:  &workspace.Project{ID: "/consumer", Name: "consumer"},
		ProxyURL: "http://127.0.0.1:41002",
		runsWith: []*Member{provider},
		Databases: []DatabaseBinding{{
			Datasource: "default",
			Schema:     "app",
			Database:   "compose_0123456789abcdef__consumer_default",
			connection: &pdb.Connection{Host: "127.0.0.1", Port: 55432, Database: "compose_0123456789abcdef__consumer_default", User: "putnami", Password: fakePassword},
		}},
	}
	plan := &Plan{Target: consumer, Members: []*Member{provider, consumer}}

	document, sections, err := configData(consumer, plan)
	if err != nil {
		t.Fatalf("configData: %v", err)
	}
	if !slices.Equal(sections, []string{SectionClients, SectionDatabase}) {
		t.Errorf("sections = %v", sections)
	}
	var decoded struct {
		Clients struct {
			Services map[string]struct {
				URL string `json:"url"`
			} `json:"services"`
		} `json:"clients"`
		Database pdb.Binding `json:"database"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("CONFIG_DATA is not JSON: %v", err)
	}
	for _, key := range []string{"items", "go.acme.dev/provider"} {
		if decoded.Clients.Services[key].URL != provider.ProxyURL {
			t.Errorf("clients.services[%q].url = %q, want the provider proxy URL", key, decoded.Clients.Services[key].URL)
		}
	}
	bound := decoded.Database.Databases["default"]
	if decoded.Database.ProtocolVersion != pdb.ProtocolVersion || bound.Engine != pdb.EnginePostgres || bound.Schema != "app" ||
		bound.Connection == nil || bound.Connection.Database != consumer.Databases[0].Database || bound.Connection.Password != fakePassword {
		t.Errorf("database section = %+v", decoded.Database)
	}

	// A member that runs with nobody and declares no database gets no document.
	empty, emptySections, err := configData(provider, plan)
	if err != nil {
		t.Fatalf("configData(provider): %v", err)
	}
	if len(emptySections) != 0 || string(empty) != "{}" {
		t.Errorf("provider document = %s, sections %v; want an empty document", empty, emptySections)
	}
	if env := memberEnv(true, nil); slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, ConfigDataEnv+"=") }) {
		t.Errorf("an empty document still set %s: %v", ConfigDataEnv, env)
	}

	// The status a composition prints carries section and datasource names only.
	proxyA := &proxy{member: "/provider", port: 41001}
	proxyB := &proxy{member: "/consumer", port: 41002}
	c := &Composition{
		id:   "0123456789abcdef",
		plan: plan,
		runtimes: []*memberRuntime{
			newMemberRuntime(provider, proxyA, memberEnv(true, empty), nil, false),
			newMemberRuntime(consumer, proxyB, memberEnv(false, document), sections, true),
		},
		databases: &databaseSet{isolation: IsolationDatabase},
	}
	status, err := json.Marshal(c.Status())
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	for _, secret := range []string{fakePassword, "55432", ConfigDataEnv, "compose_0123456789abcdef__consumer_default"} {
		if strings.Contains(string(status), secret) {
			t.Errorf("status leaks %q: %s", secret, status)
		}
	}
	if !strings.Contains(string(status), `"configSections":["clients","database"]`) || !strings.Contains(string(status), `"databases":["default"]`) {
		t.Errorf("status lacks the section and datasource names: %s", status)
	}
	composeErr := newError(CodeReadyTimeout, "/consumer", PhaseReadiness, "no typed ready event")
	if strings.Contains(composeErr.Error(), fakePassword) {
		t.Errorf("an error message carries the password: %s", composeErr.Error())
	}
}

func TestConfigData_RefusesAnEnvironmentThatAlreadyCarriesConfigData(t *testing.T) {
	member := &Member{Project: &workspace.Project{ID: "/app"}, ServeJob: &jobs.ScheduledJob{
		JobDef: &extension.JobDefinition{Env: map[string]string{ConfigDataEnv: "{}"}},
	}}
	if err := checkConfigDataConflict(member); err == nil || composeError(t, err).Code != CodeConfigDataConflict {
		t.Fatalf("a manifest-declared %s was accepted: %v", ConfigDataEnv, err)
	}
	t.Setenv(ConfigDataEnv, `{"clients":{}}`)
	member.ServeJob.JobDef.Env = nil
	if err := checkConfigDataConflict(member); err == nil || composeError(t, err).Code != CodeConfigDataConflict {
		t.Fatalf("an inherited %s was accepted: %v", ConfigDataEnv, err)
	}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{}, []*workspace.Project{member.Project})
	fixture := &compositionFixture{ws: ws, projects: map[string]*workspace.Project{"/app": member.Project}}
	_, err := up(t.Context(), Options{WorkspaceRoot: ws.Root, Target: member.Project}, fixture.deps(nil))
	if err == nil || composeError(t, err).Code != CodeConfigDataConflict {
		t.Fatalf("Up with an inherited %s = %v, want compose.config_data_conflict", ConfigDataEnv, err)
	}
}

func TestProxy_503UntilBackendKnown_ThenForwardsAndKeepsHost(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"the-proxy-answers-unavailable-until-the-backend-is-known")
	var seenHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		_, _ = io.WriteString(w, "backend "+r.URL.Path+"?"+r.URL.RawQuery)
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)
	backendPort, _ := strconv.Atoi(backendURL.Port())

	p, err := startProxy("/app", 0)
	if err != nil {
		t.Fatalf("startProxy: %v", err)
	}
	defer func() { _ = p.close(t.Context()) }()

	resp, err := http.Get(p.URL() + "/readyz")
	if err != nil {
		t.Fatalf("GET before backend: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != backendNotReadyBody {
		t.Fatalf("before the backend is known: %d %s", resp.StatusCode, body)
	}

	p.setBackend(backendPort)
	request, _ := http.NewRequest(http.MethodGet, p.URL()+"/items?limit=2", nil)
	request.Host = "consumer.example:3911"
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "backend /items?limit=2" {
		t.Fatalf("forwarded response: %d %s", resp.StatusCode, body)
	}
	if seenHost != "consumer.example:3911" {
		t.Errorf("backend saw Host %q, want the caller's", seenHost)
	}

	// A backend that went away while its port is still recorded is
	// unavailable, not a transport error.
	backend.Close()
	resp, err = http.Get(p.URL() + "/readyz")
	if err != nil {
		t.Fatalf("GET after backend loss: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != backendUnreachableBody {
		t.Fatalf("after the backend left: %d %s", resp.StatusCode, body)
	}

	p.setBackend(0)
	resp, err = http.Get(p.URL() + "/")
	if err != nil {
		t.Fatalf("GET after restart began: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a restarting backend answered %d", resp.StatusCode)
	}
}

func TestDefaultTargetProxyPort(t *testing.T) {
	withPort := func(port any) *workspace.Project {
		return &workspace.Project{Config: &wsproto.ProjectConfig{Options: map[string]map[string]any{"serve": {"port": port}}}}
	}
	cases := []struct {
		project *workspace.Project
		want    int
	}{
		{nil, 3000},
		{&workspace.Project{}, 3000},
		{withPort(float64(3911)), 3911},
		{withPort(3906), 3906},
		{withPort("3803"), 3803},
		{withPort(float64(39.5)), 3000},
		{withPort(0), 3000},
		{withPort(70000), 3000},
	}
	for _, tc := range cases {
		if got := DefaultTargetProxyPort(tc.project); got != tc.want {
			t.Errorf("DefaultTargetProxyPort(%+v) = %d, want %d", tc.project, got, tc.want)
		}
	}
}

func TestDatabaseName_SlugsTheProjectAndStaysWithinTheIdentifierLimit(t *testing.T) {
	if got := databaseName("0123456789abcdef", "/Go/samples/api", "default"); got != "compose_0123456789abcdef__go_samples_api_default" {
		t.Errorf("databaseName = %q", got)
	}
	long := databaseName("0123456789abcdef", "/typescript/samples/06-database", "default")
	if len(long) != maxDatabaseNameBytes || !strings.HasPrefix(long, "compose_0123456789abcdef__typescript_samples_06_databa_") {
		t.Errorf("long databaseName = %q (%d bytes)", long, len(long))
	}
	if again := databaseName("0123456789abcdef", "/typescript/samples/06-database", "default"); again != long {
		t.Errorf("databaseName is not deterministic: %q then %q", long, again)
	}
	if cut := databaseName("0123456789abcdef", strings.Repeat("a", 36), "dé"); len(cut) > maxDatabaseNameBytes || !strings.HasPrefix(cut, databasePrefix("0123456789abcdef")) {
		t.Errorf("truncated name = %q", cut)
	}
}

func TestDatabaseName_KeepsDatasourcesDistinctPastTheCut(t *testing.T) {
	const id = "59a9f92748a2dfb8"
	const project = "/identity/workloads/identity-api"
	seen := map[string]string{}
	for _, datasource := range []string{"platform_iam", "runtime_placement", "runtime_profiles"} {
		name := databaseName(id, project, datasource)
		if len(name) > maxDatabaseNameBytes || !utf8.ValidString(name) || !strings.HasPrefix(name, databasePrefix(id)) {
			t.Errorf("databaseName(%q) = %q (%d bytes)", datasource, name, len(name))
		}
		if other, ok := seen[name]; ok {
			t.Errorf("datasources %q and %q share database %q", other, datasource, name)
		}
		seen[name] = datasource
	}
}
