package lifecycle

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeProjectGraphWorkspace writes a chain web -> util -> core and an unrelated
// docs project.
func makeProjectGraphWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	hometest.Temp(t)

	writeRawFile(t, filepath.Join(dir, "putnami.workspace.json"),
		`{"name":"graph-workspace","includes":["libs/core","libs/util","apps/web","apps/docs"]}`)
	writeRawFile(t, filepath.Join(dir, "libs", "core", "putnami.json"), `{"name":"core"}`)
	writeRawFile(t, filepath.Join(dir, "libs", "util", "putnami.json"), `{"name":"util","dependencies":["core"]}`)
	writeRawFile(t, filepath.Join(dir, "apps", "web", "putnami.json"), `{"name":"web","dependencies":["util"]}`)
	writeRawFile(t, filepath.Join(dir, "apps", "docs", "putnami.json"), `{"name":"docs"}`)

	workspace.InvalidateLoadCache(dir)
	t.Cleanup(func() { workspace.InvalidateLoadCache(dir) })
	return dir
}

// jsonlObjects decodes every non-empty line of output as one JSON object.
func jsonlObjects(t *testing.T, output string) []map[string]any {
	t.Helper()

	var objects []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			t.Fatalf("output line %q is not a JSON object: %v", line, err)
		}
		objects = append(objects, object)
	}
	return objects
}

func jsonlIDs(t *testing.T, output string) []string {
	t.Helper()

	var ids []string
	for _, object := range jsonlObjects(t, output) {
		id, _ := object["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestProjectsOwner_NamesTheProjectsThatOwnAPath(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "project-graph-queries", "owner-names-the-projects-that-own-a-path")
	dir := makeProjectGraphWorkspace(t)

	output, err := captureStdout(t, func() error {
		return ProjectsOwner(dir, nil, []string{"libs/util/src/index.ts"}, "", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsOwner: %v", err)
	}
	for _, want := range []string{"Owners of libs/util/src/index.ts", "/libs/util", "1 project"} {
		if !strings.Contains(output, want) {
			t.Errorf("owner output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "/libs/core") || strings.Contains(output, "/apps/web") {
		t.Errorf("owner output names a project that does not own the path:\n%s", output)
	}

	output, err = captureStdout(t, func() error {
		return ProjectsOwner(dir, nil, []string{"elsewhere/file.txt"}, "", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsOwner(unowned path): %v", err)
	}
	if !strings.Contains(output, "No project owns elsewhere/file.txt") {
		t.Errorf("an unowned path is not reported:\n%s", output)
	}

	for _, args := range [][]string{nil, {"--bogus"}, {"a", "b"}} {
		if _, err := captureStdout(t, func() error { return ProjectsOwner(dir, nil, args, "", nil) }); !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("ProjectsOwner(%v) error = %v, want a usage error", args, err)
		}
	}
}

func TestProjectsDeps_ListsDirectAndTransitiveEdgesBothWays(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "project-graph-queries", "deps-lists-direct-and-transitive-edges-both-ways")
	dir := makeProjectGraphWorkspace(t)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"direct dependencies", []string{"web"}, []string{"/libs/util"}},
		{"transitive dependencies", []string{"web", "--transitive"}, []string{"/libs/core", "/libs/util"}},
		{"direct dependents", []string{"core", "--dependents"}, []string{"/libs/util"}},
		{"transitive dependents", []string{"--transitive", "--dependents", "/libs/core"}, []string{"/apps/web", "/libs/util"}},
		{"none", []string{"docs", "--transitive"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := captureStdout(t, func() error {
				return ProjectsDeps(dir, nil, tt.args, "jsonl", nil)
			})
			if err != nil {
				t.Fatalf("ProjectsDeps(%v): %v", tt.args, err)
			}
			if got := jsonlIDs(t, output); !sameStrings(got, tt.want) {
				t.Errorf("ProjectsDeps(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}

	output, err := captureStdout(t, func() error {
		return ProjectsDeps(dir, nil, []string{"web", "--transitive"}, "", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsDeps(text): %v", err)
	}
	for _, want := range []string{"Project:   /apps/web", "transitive dependencies", "/libs/core", "/libs/util", "2 projects"} {
		if !strings.Contains(output, want) {
			t.Errorf("deps output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "/apps/web ") {
		t.Errorf("deps lists the project itself:\n%s", output)
	}

	output, err = captureStdout(t, func() error {
		return ProjectsDeps(dir, nil, []string{"docs"}, "", nil)
	})
	if err != nil || !strings.Contains(output, "(no direct dependencies)") {
		t.Errorf("a project with no edges is not reported (err %v):\n%s", err, output)
	}

	if _, err := captureStdout(t, func() error { return ProjectsDeps(dir, nil, []string{"missing"}, "", nil) }); !errors.Is(err, cmderr.ErrNotFound) {
		t.Errorf("ProjectsDeps(unknown project) error = %v, want a not-found error", err)
	}
	for _, args := range [][]string{nil, {"--dependents"}, {"web", "--bogus"}, {"web", "core"}} {
		if _, err := captureStdout(t, func() error { return ProjectsDeps(dir, nil, args, "", nil) }); !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("ProjectsDeps(%v) error = %v, want a usage error", args, err)
		}
	}
}

// scopedIndex reports every dependency hop as carrying the build task only, so
// the projects the walk reaches are task-scoped.
type scopedIndex struct{}

func (scopedIndex) TasksReadingPath(string, string) []string              { return nil }
func (scopedIndex) TasksReachedFrom([]string) []string                    { return []string{"build"} }
func (scopedIndex) TasksOfExtension(string) []string                      { return nil }
func (scopedIndex) TasksOfManifestChange(string, string) ([]string, bool) { return nil, false }
func (scopedIndex) ReadsAsExtensionRuntime(string, string) bool           { return false }

func TestProjectsWhy_PrintsTheImpactPathAndTaskScope(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "project-graph-queries", "why-prints-the-impact-path-and-task-scope")
	dir := makeProjectGraphWorkspace(t)

	output, err := captureStdout(t, func() error {
		return ProjectsWhy(t.Context(), dir, nil, []string{"core", "web"}, "", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsWhy: %v", err)
	}
	for _, want := range []string{
		"A change to /libs/core impacts /apps/web",
		"Path:   /libs/core → /libs/util → /apps/web",
		"/libs/core → /libs/util (dependency)",
		"/libs/util → /apps/web (dependency)",
		"Tasks:  all",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("why output missing %q:\n%s", want, output)
		}
	}

	// A walk over a task index reaches the project task-scoped.
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	answer := whyAnswerFor(ws, ws.ProjectByName("core"), ws.ProjectByName("web"), scopedIndex{})
	if !answer.Impacted || len(answer.Path) != 3 {
		t.Fatalf("task-indexed answer = %+v, want an impacted three-project path", answer)
	}
	if !sameStrings(answer.TaskScope, []string{"build"}) {
		t.Errorf("task scope = %v, want [build]", answer.TaskScope)
	}
	output, err = captureStdout(t, func() error { return printWhy(answer, "") })
	if err != nil {
		t.Fatalf("printWhy: %v", err)
	}
	if !strings.Contains(output, "Tasks:  build (task-scoped)") {
		t.Errorf("why output does not name the task scope:\n%s", output)
	}

	output, err = captureStdout(t, func() error {
		return ProjectsWhy(t.Context(), dir, nil, []string{"docs", "docs"}, "", nil)
	})
	if err != nil || !strings.Contains(output, "are the same project") {
		t.Errorf("the same project is not reported (err %v):\n%s", err, output)
	}
}

func TestProjectsWhy_ReportsWhenNoPathExists(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "project-graph-queries", "why-reports-when-no-path-exists")
	dir := makeProjectGraphWorkspace(t)

	for _, pair := range [][]string{{"web", "core"}, {"docs", "web"}} {
		output, err := captureStdout(t, func() error {
			return ProjectsWhy(t.Context(), dir, nil, pair, "", nil)
		})
		if err != nil {
			t.Fatalf("ProjectsWhy(%v) must succeed when there is no path: %v", pair, err)
		}
		if !strings.Contains(output, "does not impact") || strings.Contains(output, "Path:") {
			t.Errorf("ProjectsWhy(%v) output:\n%s", pair, output)
		}
	}

	output, err := captureStdout(t, func() error {
		return ProjectsWhy(t.Context(), dir, nil, []string{"web", "core"}, "jsonl", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsWhy(jsonl): %v", err)
	}
	objects := jsonlObjects(t, output)
	if len(objects) != 1 || objects[0]["impacted"] != false {
		t.Fatalf("no-path jsonl = %v, want one object with impacted false", objects)
	}
	if path, _ := objects[0]["path"].([]any); len(path) != 0 {
		t.Errorf("no-path jsonl names a path: %v", path)
	}

	if _, err := captureStdout(t, func() error {
		return ProjectsWhy(t.Context(), dir, nil, []string{"core", "missing"}, "", nil)
	}); !errors.Is(err, cmderr.ErrNotFound) {
		t.Errorf("ProjectsWhy(unknown project) error = %v, want a not-found error", err)
	}
	for _, args := range [][]string{nil, {"core"}, {"core", "web", "util"}, {"core", "--bogus"}} {
		if _, err := captureStdout(t, func() error {
			return ProjectsWhy(t.Context(), dir, nil, args, "", nil)
		}); !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("ProjectsWhy(%v) error = %v, want a usage error", args, err)
		}
	}
}

func TestProjectGraphQueries_PrintJSONL(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "project-graph-queries", "each-query-prints-jsonl")
	dir := makeProjectGraphWorkspace(t)

	output, err := captureStdout(t, func() error {
		return ProjectsOwner(dir, nil, []string{"apps/web/src/app.ts"}, "jsonl", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsOwner(jsonl): %v", err)
	}
	owners := jsonlObjects(t, output)
	if len(owners) != 1 || owners[0]["id"] != "/apps/web" || owners[0]["name"] != "web" || owners[0]["path"] != "apps/web" {
		t.Errorf("owner jsonl = %v, want one /apps/web object", owners)
	}

	output, err = captureStdout(t, func() error {
		return ProjectsDeps(dir, nil, []string{"core", "--dependents"}, "jsonl", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsDeps(jsonl): %v", err)
	}
	deps := jsonlObjects(t, output)
	if len(deps) != 1 || deps[0]["id"] != "/libs/util" || deps[0]["direction"] != "dependents" || deps[0]["transitive"] != false {
		t.Errorf("deps jsonl = %v, want one /libs/util dependent object", deps)
	}

	output, err = captureStdout(t, func() error {
		return ProjectsWhy(t.Context(), dir, nil, []string{"core", "web"}, "jsonl", nil)
	})
	if err != nil {
		t.Fatalf("ProjectsWhy(jsonl): %v", err)
	}
	why := jsonlObjects(t, output)
	if len(why) != 1 {
		t.Fatalf("why jsonl = %v, want one object", why)
	}
	object := why[0]
	if object["impacted"] != true {
		t.Errorf("why jsonl impacted = %v, want true", object["impacted"])
	}
	for _, key := range []string{"from", "to", "path", "edges", "taskScope"} {
		if _, ok := object[key]; !ok {
			t.Errorf("why jsonl lacks %q: %v", key, object)
		}
	}
	if path, _ := object["path"].([]any); len(path) != 3 {
		t.Errorf("why jsonl path = %v, want three projects", object["path"])
	}
	edges, _ := object["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("why jsonl edges = %v, want two", object["edges"])
	}
	if first, _ := edges[0].(map[string]any); first["from"] != "/libs/core" || first["to"] != "/libs/util" || first["kind"] != "dependency" {
		t.Errorf("why jsonl first edge = %v", edges[0])
	}
	if scope, ok := object["taskScope"].([]any); !ok || len(scope) != 0 {
		t.Errorf("why jsonl taskScope = %v, want an empty list", object["taskScope"])
	}
}

// makeExtensionGraphWorkspace writes app -> lib and an in-workspace extension
// project that every project runs under. Nothing declares a dependency on the
// extension, so the only path from it to a consumer is the impact-only
// extension-consumer edge.
func makeExtensionGraphWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	hometest.Temp(t)

	writeRawFile(t, filepath.Join(dir, "putnami.workspace.json"),
		`{"name":"extension-graph","includes":["packages/app","packages/lib","packages/ext"],"extensions":["./packages/ext"]}`)
	writeRawFile(t, filepath.Join(dir, "packages", "app", "putnami.json"), `{"name":"app","type":"application","dependencies":["lib"]}`)
	writeRawFile(t, filepath.Join(dir, "packages", "lib", "putnami.json"), `{"name":"lib","type":"library"}`)
	writeRawFile(t, filepath.Join(dir, "packages", "ext", "putnami.json"), `{"name":"ext","type":"library"}`)

	workspace.InvalidateLoadCache(dir)
	t.Cleanup(func() { workspace.InvalidateLoadCache(dir) })
	return dir
}

// projects why walks the edges --impacted widens through, the impact-only
// extension-consumer edge included: a change to an extension reaches every
// project that runs under it, scoped to the extension's tasks, and never the
// other way.
func TestProjectsWhy_CrossesAnExtensionConsumerEdge(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "why-impacted-walks-the-edges-impacted-widens-through")
	dir := makeExtensionGraphWorkspace(t)

	why := func(from, to string) whyAnswer {
		t.Helper()
		output, err := captureStdout(t, func() error {
			return ProjectsWhy(t.Context(), dir, nil, []string{from, to}, "jsonl", nil)
		})
		if err != nil {
			t.Fatalf("ProjectsWhy(%s, %s): %v", from, to, err)
		}
		var answer whyAnswer
		if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &answer); err != nil {
			t.Fatalf("ProjectsWhy(%s, %s) printed %q, want one answer: %v", from, to, output, err)
		}
		return answer
	}

	answer := why("ext", "app")
	if !answer.Impacted {
		t.Fatalf("ext -> app = %+v, want impacted: app runs under ext", answer)
	}
	if want := []whyEdge{{From: "/packages/ext", To: "/packages/app", Kind: "extension-consumer"}}; len(answer.Edges) != 1 || answer.Edges[0] != want[0] {
		t.Errorf("ext -> app edges = %+v, want %+v", answer.Edges, want)
	}
	if !sameStrings(answer.TaskScope, []string{"/packages/ext"}) {
		t.Errorf("ext -> app taskScope = %v, want the extension's tasks only", answer.TaskScope)
	}

	// lib reaches app through a dependency edge, which carries every task.
	if full := why("lib", "app"); !full.Impacted || full.TaskScope == nil || len(full.TaskScope) != 0 {
		t.Errorf("lib -> app = %+v, want impacted with an empty task scope", full)
	}

	// The extension edge is one-directional.
	if reverse := why("app", "ext"); reverse.Impacted || len(reverse.Edges) != 0 {
		t.Errorf("app -> ext = %+v, want not impacted and no edge", reverse)
	}
}
