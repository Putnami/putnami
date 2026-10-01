package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// taskScopeFixtureWorkspace is a library, an application that imports it, and an
// in-workspace extension with a real manifest, so the MCP server's own
// extension discovery can build the task index. Its `build` pipeline carries the
// cross-project `^describe` reference and reads sources without test files;
// `test` reads the test files and nothing references it.
func taskScopeFixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json",
		`{"name":"fixture","includes":["packages/app","packages/lib","packages/ext"],"extensions":["./packages/ext"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	write("packages/lib/lib.ts", "export const lib = 1\n")
	write("packages/lib/lib_test.ts", "export const test = 1\n")
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/ext/putnami.json", `{"name":"ext","type":"library"}`)
	write("packages/ext/putnami.extension.json", `{
	  "name":"ext","version":"1.0.0","cliContract":4,
	  "commands":{
	    "build":{"run":[
	      {"id":"describe","task":"describe","dependsOn":["^describe"]},
	      {"id":"compile","task":"compile","dependsOn":["describe"]}
	    ]},
	    "test":{"run":[{"id":"test","task":"test"}]}
	  },
	  "tasks":{
	    "describe":{"kind":"command","command":"true","cache":false,
	      "inputs":{"sources":{"from":"project","files":["**/*.ts","!**/*_test.ts"]}}},
	    "compile":{"kind":"command","command":"true","cache":false,
	      "inputs":{"sources":{"from":"project","files":["**/*.ts","!**/*_test.ts"]}}},
	    "test":{"kind":"command","command":"true","cache":false,
	      "inputs":{"tests":{"from":"project","files":["**/*_test.ts"]}}}
	  }}`)
	workspace.InvalidateLoadCache(dir)
	recordWorkspaceIndex(t, dir)
	return dir
}

// `impacted` and `why_impacted` name the same task scopes, because both read
// the one propagation over the one task index. A test file selects its own
// project's `test` and reaches no dependent; a source file reaches the
// importer's `build~describe` and what sits behind it.
func TestToolImpactedAndWhyImpactedNameTheTaskScope(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "why-impacted-walks-the-edges-impacted-widens-through")
	dir := taskScopeFixtureWorkspace(t)
	initFixtureGitRepo(t, dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	// A test file of the library: its own `test~test`, and no dependent.
	if err := os.WriteFile(filepath.Join(dir, "packages", "lib", "lib_test.ts"), []byte("export const test = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	answer := impactedFor(t, srv)
	if got, want := projectRefIDs(answer.Projects), []string{"/packages/lib"}; !slices.Equal(got, want) {
		t.Fatalf("a test file selected %v, want %v", got, want)
	}
	if got, want := scopeOf(answer, "/packages/lib"), []string{"test~test"}; !slices.Equal(got, want) {
		t.Errorf("impacted scoped /packages/lib to %v, want %v", got, want)
	}

	// A source file of the library: the importer is reached through the task
	// edge, and why_impacted names the same scope impacted plans with.
	if err := os.WriteFile(filepath.Join(dir, "packages", "lib", "lib.ts"), []byte("export const lib = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	answer = impactedFor(t, srv)
	if got, want := projectRefIDs(answer.Projects), []string{"/packages/app", "/packages/lib"}; !slices.Equal(got, want) {
		t.Fatalf("a source file selected %v, want %v", got, want)
	}
	want := []string{"build~compile", "build~describe"}
	if got := scopeOf(answer, "/packages/app"); !slices.Equal(got, want) {
		t.Errorf("impacted scoped /packages/app to %v, want %v", got, want)
	}
	args, err := json.Marshal(whyImpactedArgs{From: "lib", To: "/packages/app"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := srv.toolWhyImpacted(context.Background(), args)
	if err != nil {
		t.Fatalf("toolWhyImpacted: %v", err)
	}
	reply := decodeWhyImpacted(t, out)
	if !reply.Impacted {
		t.Fatalf("why_impacted denied a pair impacted lists: %q", reply.Message)
	}
	if !slices.Equal(reply.TaskScope, want) {
		t.Errorf("why_impacted taskScope = %v, want %v: the scope impacted plans with", reply.TaskScope, want)
	}
}

func impactedFor(t *testing.T, srv *Server) impactedAnswer {
	t.Helper()
	out, err := srv.toolImpacted(context.Background(), json.RawMessage(`{"baseline":"main"}`))
	if err != nil {
		t.Fatalf("toolImpacted: %v", err)
	}
	return out.(impactedAnswer)
}

func projectRefIDs(refs []projectRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	slices.Sort(ids)
	return ids
}

func scopeOf(answer impactedAnswer, id string) []string {
	for _, reason := range answer.Reasons {
		if reason.Project == id {
			return reason.TaskScope
		}
	}
	return nil
}
