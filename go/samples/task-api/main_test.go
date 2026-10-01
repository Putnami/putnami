package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/events"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	protofeatures "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/infra"
)

type taskGraphMigrationSource struct{}

func (taskGraphMigrationSource) Kind() migration.Kind { return migration.KindSQL }
func (taskGraphMigrationSource) Namespace() string    { return "tasks" }
func (taskGraphMigrationSource) InfraDatabases() []infra.Database {
	return []infra.Database{{Name: "default", Engine: infra.EnginePostgres, Schemas: []string{"public"}}}
}

type taskGraphMigrationPlugin struct{}

func (*taskGraphMigrationPlugin) Name() string { return "task-migrations" }
func (*taskGraphMigrationPlugin) MigrationSources() []migration.Source {
	return []migration.Source{taskGraphMigrationSource{}}
}

// migrationPlacement is where the conformance run mounts the migration plugin
// inside the native feature, so add/move/remove is expressed as composition.
type migrationPlacement int

const (
	migrationOnFeature migrationPlacement = iota
	migrationOnChildModule
	migrationAbsent
)

func describeTaskDesignGraph(t *testing.T, placement migrationPlacement) *protofeatures.DesignGraph {
	t.Helper()
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	broker := events.NewMemoryBroker()
	tasks := buildTaskFeature(server, broker)
	switch placement {
	case migrationOnFeature:
		tasks.Use(&taskGraphMigrationPlugin{})
	case migrationOnChildModule:
		tasks.Use(app.NewModule("storage").Use(&taskGraphMigrationPlugin{}))
	case migrationAbsent:
	}

	application := app.New("tasks-api")
	application.Use(server)
	application.Use(tasks)
	application.Provide(inject.ProvideInstance(broker))
	application.Provide(inject.ProvideInstance(events.NewPublisher(TaskCreatedTopic, broker)))
	application.Provide(inject.ProvideInstance(logger.New("design-test", logger.LevelError)))
	output := t.TempDir()
	if err := application.Describe(output, []string{"design"}); err != nil {
		t.Fatalf("describe native task feature: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(protofeatures.DesignGraphArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := protofeatures.ParseDesignGraph(data)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func designGraphHasEdge(graph *protofeatures.DesignGraph, from, to string, kind protofeatures.DesignEdgeKind) bool {
	for _, edge := range graph.Edges {
		if edge.From == from && edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func TestTaskFeatureGraphTracksNativeAddMoveAndRemove(t *testing.T) {
	const (
		migrationID   = "data.migration:sql:tasks"
		schemaID      = "data.schema:default:public"
		featureModule = "module:tasks-api/tasks"
		childModule   = "module:tasks-api/tasks/storage"
	)
	baseline := describeTaskDesignGraph(t, migrationOnFeature)
	wantKinds := map[protofeatures.DesignNodeKind]bool{
		protofeatures.DesignNodeAPIOperation:  false,
		protofeatures.DesignNodeService:       false,
		protofeatures.DesignNodeEventTopic:    false,
		protofeatures.DesignNodeEventHandler:  false,
		protofeatures.DesignNodeDataMigration: false,
		protofeatures.DesignNodeDataSchema:    false,
	}
	for _, node := range baseline.Nodes {
		if _, wanted := wantKinds[node.Kind]; wanted {
			wantKinds[node.Kind] = true
		}
	}
	for kind, present := range wantKinds {
		if !present {
			t.Errorf("native task graph has no %s node", kind)
		}
	}
	if !designGraphHasEdge(baseline, featureModule, migrationID, protofeatures.DesignEdgeContains) {
		t.Fatal("feature module does not contain its native migration")
	}
	if !designGraphHasEdge(baseline, migrationID, schemaID, protofeatures.DesignEdgeWrites) {
		t.Fatal("native migration is not related to the schema it writes")
	}

	moved := describeTaskDesignGraph(t, migrationOnChildModule)
	if designGraphHasEdge(moved, featureModule, migrationID, protofeatures.DesignEdgeContains) {
		t.Fatal("moved migration retained its stale feature-module edge")
	}
	if !designGraphHasEdge(moved, childModule, migrationID, protofeatures.DesignEdgeContains) {
		t.Fatal("moved migration was not attributed to its native child module")
	}
	// Containment follows the move; the write target is a fact of the source
	// itself and must survive it unchanged.
	if !designGraphHasEdge(moved, migrationID, schemaID, protofeatures.DesignEdgeWrites) {
		t.Fatal("moved migration lost the schema it writes")
	}

	removed := describeTaskDesignGraph(t, migrationAbsent)
	for _, node := range removed.Nodes {
		if node.ID == migrationID || node.Kind == protofeatures.DesignNodeDataSchema {
			t.Fatalf("removed native data declaration remained in graph: %#v", node)
		}
	}
	for _, edge := range removed.Edges {
		if edge.From == migrationID || edge.To == migrationID || edge.From == schemaID || edge.To == schemaID {
			t.Fatalf("removed native data declaration left a stale edge: %#v", edge)
		}
	}
}

// helper to do HTTP via httptest.
func doRequest(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// buildTestHTTPServer creates an httptest.Server wired with the task handler.
func buildTestHTTPServer(t *testing.T) (*httptest.Server, *TaskStore) {
	t.Helper()
	store := NewTaskStore()
	broker := events.NewMemoryBroker()
	broker.Start(context.TODO())
	publisher := events.NewPublisher(TaskCreatedTopic, broker)
	log := logger.New("test", logger.LevelError)

	handler := NewTaskHandler(store, publisher, log)

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	server.GET("/tasks", handler.List)
	server.POST("/tasks", handler.Create)
	server.POST("/tasks/batch", handler.BatchGet)
	server.GET("/tasks/{id}", handler.Get)
	server.PUT("/tasks/{id}", handler.Update)
	server.DELETE("/tasks/{id}", handler.Delete)

	return server.TestServer(), store
}

func TestCreateTask(t *testing.T) {
	ts, _ := buildTestHTTPServer(t)
	defer ts.Close()

	resp := doRequest(t, ts, "POST", "/tasks", `{"title":"Write docs","description":"Update the README"}`)
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var task Task
	json.NewDecoder(resp.Body).Decode(&task)

	if task.Title != "Write docs" {
		t.Errorf("expected title 'Write docs', got %q", task.Title)
	}
	if task.Status != "pending" {
		t.Errorf("expected status 'pending', got %q", task.Status)
	}
	if task.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestCreateTaskValidation(t *testing.T) {
	ts, _ := buildTestHTTPServer(t)
	defer ts.Close()

	resp := doRequest(t, ts, "POST", "/tasks", `{"description":"no title"}`)
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateTaskRejectsInvalidFieldType(t *testing.T) {
	ts, store := buildTestHTTPServer(t)
	defer ts.Close()

	resp := doRequest(t, ts, "POST", "/tasks", `{"title":123}`)
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if got := len(store.List()); got != 0 {
		t.Fatalf("invalid typed body created %d tasks, want 0", got)
	}
}

func TestBatchGetConcurrentLookup(t *testing.T) {
	ts, store := buildTestHTTPServer(t)
	defer ts.Close()

	a := store.Create(CreateTaskInput{Title: "A"})
	b := store.Create(CreateTaskInput{Title: "B"})

	body := `{"ids":["` + a.ID + `","missing","` + b.ID + `"]}`
	resp := doRequest(t, ts, "POST", "/tasks/batch", body)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var results []batchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results (ordered), got %d", len(results))
	}
	// MapBounded preserves input order regardless of completion order.
	if !results[0].Found || results[0].ID != a.ID {
		t.Errorf("result[0] = %+v, want found %s", results[0], a.ID)
	}
	if results[1].Found {
		t.Errorf("result[1] (missing id) should not be found, got %+v", results[1])
	}
	if !results[2].Found || results[2].ID != b.ID {
		t.Errorf("result[2] = %+v, want found %s", results[2], b.ID)
	}
}

func TestListTasks(t *testing.T) {
	ts, _ := buildTestHTTPServer(t)
	defer ts.Close()

	// Create two tasks.
	doRequest(t, ts, "POST", "/tasks", `{"title":"Task 1"}`).Body.Close()
	doRequest(t, ts, "POST", "/tasks", `{"title":"Task 2"}`).Body.Close()

	resp := doRequest(t, ts, "GET", "/tasks", "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tasks []Task
	json.NewDecoder(resp.Body).Decode(&tasks)

	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestGetTask(t *testing.T) {
	ts, store := buildTestHTTPServer(t)
	defer ts.Close()

	created := store.Create(CreateTaskInput{Title: "Test task"})

	resp := doRequest(t, ts, "GET", "/tasks/"+created.ID, "")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var task Task
	json.NewDecoder(resp.Body).Decode(&task)

	if task.ID != created.ID {
		t.Errorf("expected ID %q, got %q", created.ID, task.ID)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	ts, _ := buildTestHTTPServer(t)
	defer ts.Close()

	resp := doRequest(t, ts, "GET", "/tasks/nonexistent", "")
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUpdateTask(t *testing.T) {
	ts, store := buildTestHTTPServer(t)
	defer ts.Close()

	created := store.Create(CreateTaskInput{Title: "Original"})

	resp := doRequest(t, ts, "PUT", "/tasks/"+created.ID, `{"title":"Updated","status":"completed"}`)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var task Task
	json.NewDecoder(resp.Body).Decode(&task)

	if task.Title != "Updated" {
		t.Errorf("expected title 'Updated', got %q", task.Title)
	}
	if task.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", task.Status)
	}
}

func TestDeleteTask(t *testing.T) {
	ts, store := buildTestHTTPServer(t)
	defer ts.Close()

	created := store.Create(CreateTaskInput{Title: "To delete"})

	resp := doRequest(t, ts, "DELETE", "/tasks/"+created.ID, "")
	defer resp.Body.Close()

	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Verify it's gone.
	resp2 := doRequest(t, ts, "GET", "/tasks/"+created.ID, "")
	defer resp2.Body.Close()

	if resp2.StatusCode != 404 {
		t.Errorf("expected 404 after delete, got %d", resp2.StatusCode)
	}
}
