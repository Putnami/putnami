package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pdb "go.putnami.dev/protocol/database"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The composition tests run real serve steps. The workload is this test binary
// re-executed in a fixture mode, so each member is a process that binds a real
// port, announces it with the runtime protocol's typed ready event, and answers
// HTTP — no toolchain, no install, no fixed port.
const (
	fixtureModeEnv   = "PUTNAMI_COMPOSE_FIXTURE"
	fixtureMemberEnv = "PUTNAMI_COMPOSE_FIXTURE_MEMBER"
	fixtureRecordEnv = "PUTNAMI_COMPOSE_FIXTURE_RECORD"
)

// Fixture modes.
const (
	fixtureServe       = "serve"        // bind, announce, serve
	fixtureNeverReady  = "never-ready"  // log, then wait without announcing
	fixtureExitEarly   = "exit-early"   // log, then exit 3
	fixtureIgnoreTerm  = "ignore-term"  // used by the reap tests: survive SIGTERM
	fixtureRecordFirst = "record-first" // serve, after recording what its first dependency answers
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fixtureModeEnv); mode != "" {
		os.Exit(runFixtureWorkload(mode))
	}
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-compose-test-")
	if err == nil {
		_ = os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		_ = os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	_ = dir.Remove()
	os.Exit(code)
}

// fixtureRecord is what a fixture member writes about the environment it was
// started with: never a value of CONFIG_DATA, only its shape.
type fixtureRecord struct {
	Member          string   `json:"member"`
	Port            string   `json:"port"`
	NodeEnv         string   `json:"nodeEnv"`
	ConfigSections  []string `json:"configSections"`
	ServiceKeys     []string `json:"serviceKeys,omitempty"`
	DependencyReply string   `json:"dependencyReply,omitempty"`
}

// fixtureEvent is one runtime protocol v2 line a fixture member prints.
type fixtureEvent struct {
	V       int                     `json:"v"`
	Type    string                  `json:"type"`
	Level   string                  `json:"level,omitempty"`
	Message string                  `json:"message,omitempty"`
	Data    *runtimeproto.ReadyData `json:"data,omitempty"`
}

func emitLog(level, message string) {
	emit(fixtureEvent{V: 2, Type: "log", Level: level, Message: message})
}

func emit(event fixtureEvent) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
}

func runFixtureWorkload(mode string) int {
	member := os.Getenv(fixtureMemberEnv)
	emitLog("info", "fixture "+member+" starting in "+mode)
	parentGone := func() bool { return os.Getppid() == 1 }

	switch mode {
	case fixtureExitEarly:
		emitLog("error", "fixture "+member+" cannot start")
		return 3
	case fixtureNeverReady:
		for !parentGone() {
			time.Sleep(50 * time.Millisecond)
		}
		return 0
	case fixtureIgnoreTerm:
		signal.Ignore(syscall.SIGTERM)
		for !parentGone() {
			time.Sleep(50 * time.Millisecond)
		}
		return 0
	}

	record := fixtureRecord{Member: member, Port: os.Getenv("PORT"), NodeEnv: os.Getenv("NODE_ENV")}
	var document map[string]json.RawMessage
	if raw := os.Getenv(ConfigDataEnv); raw != "" && json.Unmarshal([]byte(raw), &document) == nil {
		for section := range document {
			record.ConfigSections = append(record.ConfigSections, section)
		}
		sort.Strings(record.ConfigSections)
		var clients clientsSection
		if json.Unmarshal(document[SectionClients], &clients) == nil {
			for key, binding := range clients.Services {
				record.ServiceKeys = append(record.ServiceKeys, key)
				if mode == fixtureRecordFirst && record.DependencyReply == "" {
					record.DependencyReply = fetch(binding.URL + "/whoami")
				}
			}
			sort.Strings(record.ServiceKeys)
		}
	}
	if path := os.Getenv(fixtureRecordEnv); path != "" {
		data, _ := json.Marshal(record)
		_ = os.WriteFile(path, data, 0o600)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:"+record.Port)
	if err != nil {
		emitLog("error", err.Error())
		return 1
	}
	port := listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, member+" host="+r.Host)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	emit(fixtureEvent{V: 2, Type: "ready", Data: &runtimeproto.ReadyData{
		Target:    runtimeproto.ReadyTargetServer,
		Endpoints: []runtimeproto.ReadyEndpoint{{Scheme: runtimeproto.ReadySchemeHTTP, Host: "127.0.0.1", Port: port}},
	}})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-stop:
			_ = server.Close()
			return 0
		case <-time.After(50 * time.Millisecond):
			if parentGone() {
				return 0
			}
		}
	}
}

func fetch(url string) string {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "error: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return strconv.Itoa(resp.StatusCode) + " " + string(body)
}

// fixtureProject is one workspace project of a composition fixture.
type fixtureProject struct {
	id       string
	name     string
	runsWith []string
	mode     string
	// requirements, when set, is written as the project's infra/requirements.json.
	requirements string
}

// compositionFixture is a temporary workspace whose serve steps run the fixture
// workload, and the fake engine preparation that hands them back.
type compositionFixture struct {
	root     string
	ws       *workspace.Workspace
	projects map[string]*workspace.Project
	jobs     []*jobs.ScheduledJob
	prepared []string
}

func newCompositionFixture(t *testing.T, specs ...fixtureProject) *compositionFixture {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fixture := &compositionFixture{root: root, projects: map[string]*workspace.Project{}}
	var projects []*workspace.Project
	for _, spec := range specs {
		path := strings.TrimPrefix(spec.id, "/")
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		if spec.requirements != "" {
			writeFile(t, filepath.Join(root, path, "infra", "requirements.json"), spec.requirements)
		}
		project := &workspace.Project{ID: spec.id, Name: spec.name, Path: path, RunsWith: spec.runsWith, Config: &wsproto.ProjectConfig{}}
		projects = append(projects, project)
		fixture.projects[spec.id] = project
		mode := spec.mode
		if mode == "" {
			mode = fixtureServe
		}
		fixture.jobs = append(fixture.jobs, &jobs.ScheduledJob{
			Project:   project,
			Extension: &extension.ExtensionDescription{Name: "@putnami/fixture-serve", Path: root},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/fixture-serve",
				Name:          "serve~serve",
				Kind:          "command",
				Command:       executable,
				TimeoutMs:     -1,
				Env: map[string]string{
					fixtureModeEnv:   mode,
					fixtureMemberEnv: spec.id,
					fixtureRecordEnv: filepath.Join(root, path, "record.json"),
				},
			},
		})
	}
	fixture.ws = workspace.NewWorkspace(root, &wsproto.Config{}, projects)
	fixture.ws.Name = "compose-fixture"
	return fixture
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *compositionFixture) deps(provider databaseProvider) upDeps {
	return upDeps{
		loadWorkspace: func(string) (*workspace.Workspace, error) { return f.ws, nil },
		prepare: func(_ context.Context, _ Options, projectIDs []string) (*preparation, error) {
			f.prepared = append([]string(nil), projectIDs...)
			return &preparation{ws: f.ws, withheld: f.jobs}, nil
		},
		provider: func() databaseProvider { return provider },
		reap:     func(root string) []ReapedLease { return reapOrphans(root, reapDeps{killDelay: 100 * time.Millisecond}) },
	}
}

func (f *compositionFixture) record(t *testing.T, id string) fixtureRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, strings.TrimPrefix(id, "/"), "record.json"))
	if err != nil {
		t.Fatalf("member %s left no record: %v", id, err)
	}
	var record fixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode record of %s: %v", id, err)
	}
	return record
}

// fakeIsolator is a database provider that isolates, recording every
// statement it was asked for.
type fakeIsolator struct {
	created  []string
	dropped  []string
	existing map[string]bool
	dropErr  error
	listErr  error
}

const fakePassword = "compose-fixture-password"

func (f *fakeIsolator) Provision() (pdb.Connection, string, error) {
	return pdb.Connection{Host: "127.0.0.1", Port: 55432, Database: "putnami_test", User: "putnami", Password: fakePassword}, "fakedigest", nil
}

func (f *fakeIsolator) CreateDatabase(_ context.Context, _ string, name string) error {
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	f.created = append(f.created, name)
	f.existing[name] = true
	return nil
}

func (f *fakeIsolator) DropDatabase(_ context.Context, _ string, name string) error {
	f.dropped = append(f.dropped, name)
	if f.dropErr != nil {
		return f.dropErr
	}
	delete(f.existing, name)
	return nil
}

func (f *fakeIsolator) ListDatabases(_ context.Context, _ string, prefix string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var names []string
	for name := range f.existing {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// sharedServer is a database provider that does not isolate.
type sharedServer struct{}

func (sharedServer) Provision() (pdb.Connection, string, error) {
	return pdb.Connection{Host: "127.0.0.1", Port: 55432, Database: "shared", User: "putnami", Password: fakePassword}, "shareddigest", nil
}
