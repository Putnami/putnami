package workspace

import (
	"reflect"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// verdictPolicyWorkspace holds a library and its importer, an extension and
// its consumer, and a project that declares every repository file an input,
// as a documentation check does.
func verdictPolicyWorkspace(t *testing.T) *Workspace {
	t.Helper()
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/libs/http", Name: "http", Path: "libs/http"},
		{ID: "/libs/api", Name: "api", Path: "libs/api", Dependencies: []string{"/libs/http"}},
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
		{ID: "/tools/docs", Name: "docs", Path: "tools/docs", Config: declaredInputConfig(t, "docs", "validate", "git:**", "**/*.json")},
	})
}

// A putnami.verdict-policy.json file at the workspace root, in a scope
// directory, in a project or in an extension project selects nothing by
// itself: no task of the index reads it, a declared input or a workspace
// input that does not name it leaves it out, and owning it keeps no project
// whole, an extension project included. Without a task index, owning it
// selects nothing either.
func TestTraceChangeImpact_AVerdictPolicySelectsNothingByItself(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "a-verdict-policy-selects-only-the-tasks-that-name-it",
		"a-verdict-policy-alone-selects-no-project")
	ws := verdictPolicyWorkspace(t)
	// The extension declares a task, so a whole extension reaches its consumer.
	index := fakeTaskIndex{extensionTasks: map[string][]string{"/go/extension": {"/go/extension#build~describe"}}}
	options := map[string]ChangeImpactOptions{
		"a task index":                  {Tasks: index},
		"no task index":                 {},
		"workspace inputs by file type": {Tasks: index, WorkspaceInputPatterns: []string{"*.json", "**/*.json", "**"}},
		"no task index, inputs by type": {WorkspaceInputPatterns: []string{"*.json", "**/*.json"}},
	}
	for _, file := range []string{
		"putnami.verdict-policy.json",
		"libs/putnami.verdict-policy.json",
		"libs/http/putnami.verdict-policy.json",
		"go/extension/putnami.verdict-policy.json",
		"tools/docs/putnami.verdict-policy.json",
	} {
		for name, opts := range options {
			r := TraceChangeImpact(ws, []string{file}, opts)
			if len(r.Projects) != 0 {
				t.Errorf("%s with %s selected %v, want nothing", file, name, ownerProjectIDs(r.Projects))
			}
		}
	}
	// The same declarations select every other file they match, so the cases
	// above prove a rule and not a declaration that matches nothing.
	for _, tc := range []struct {
		file string
		opts ChangeImpactOptions
		want []string
	}{
		{file: "libs/http/settings.json", opts: ChangeImpactOptions{}, want: []string{"/libs/http", "/libs/api", "/tools/docs"}},
		{file: "go/extension/settings.json", opts: ChangeImpactOptions{Tasks: index}, want: []string{"/go/extension", "/apps/web"}},
		{file: "settings.json", opts: ChangeImpactOptions{WorkspaceInputPatterns: []string{"*.json"}}, want: []string{"/libs/http", "/libs/api", "/go/extension", "/apps/web", "/tools/docs"}},
	} {
		if got := ownerProjectIDs(TraceChangeImpact(ws, []string{tc.file}, tc.opts).Projects); !slices.Equal(got, tc.want) {
			t.Errorf("%s selected %v, want %v", tc.file, got, tc.want)
		}
	}
}

// A task that names the file reads it: the index answers for it, and a
// declared input or a workspace input that names it claims it as any other
// file.
func TestTraceChangeImpact_AVerdictPolicySelectsTheTasksThatNameIt(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "a-verdict-policy-selects-only-the-tasks-that-name-it",
		"a-task-that-names-a-verdict-policy-is-selected")
	ws := verdictPolicyWorkspace(t)
	index := fakeTaskIndex{reading: map[string][]string{
		"/libs/http|libs/http/putnami.verdict-policy.json": {"validate~verdict-policy"},
	}}
	r := TraceChangeImpact(ws, []string{"libs/http/putnami.verdict-policy.json"}, ChangeImpactOptions{Tasks: index})
	if got, want := ownerProjectIDs(r.Projects), []string{"/libs/http"}; !slices.Equal(got, want) {
		t.Fatalf("a policy a task names selected %v, want %v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/libs/http"), []string{"validate~verdict-policy"}; !slices.Equal(got, want) {
		t.Errorf("scoped /libs/http to %v, want %v", got, want)
	}

	named := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/tools/gate", Name: "gate", Path: "tools/gate", Config: declaredInputConfig(t, "gate", "validate", "../../**/putnami.verdict-policy.json")},
		{ID: "/libs/http", Name: "http", Path: "libs/http"},
	})
	for _, tc := range []struct {
		name string
		ws   *Workspace
		opts ChangeImpactOptions
		want []string
	}{
		{name: "a declared input", ws: named, opts: ChangeImpactOptions{}, want: []string{"/tools/gate"}},
		{name: "a workspace input", ws: ws, opts: ChangeImpactOptions{WorkspaceInputPatterns: []string{"**/putnami.verdict-policy.json"}},
			want: []string{"/libs/http", "/libs/api", "/go/extension", "/apps/web", "/tools/docs"}},
	} {
		got := ownerProjectIDs(TraceChangeImpact(tc.ws, []string{"libs/putnami.verdict-policy.json"}, tc.opts).Projects)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s that names the policy selected %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A verdict policy is no changed file of its project: a step that reads the
// changed files does not get it. The project still owns it by path, so
// `putnami projects owner` names that project.
func TestOwnedChangedFiles_LeavesOutAVerdictPolicy(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "a-verdict-policy-selects-only-the-tasks-that-name-it",
		"a-verdict-policy-is-no-changed-file-of-its-project")
	ws := verdictPolicyWorkspace(t)
	got := OwnedChangedFiles(ws, []string{"libs/http/putnami.verdict-policy.json", "libs/http/http.go"})
	if want := map[string][]string{"/libs/http": {"http.go"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnedChangedFiles = %v, want %v", got, want)
	}
	if got := OwnedChangedFiles(ws, []string{"libs/http/putnami.verdict-policy.json"}); got != nil {
		t.Errorf("OwnedChangedFiles of a policy alone = %v, want nil", got)
	}
	if owners := ownerProjectIDs(ProjectOwnersForPath(ws, "libs/http/putnami.verdict-policy.json")); !slices.Equal(owners, []string{"/libs/http"}) {
		t.Errorf("the owners of a policy = %v, want [/libs/http]", owners)
	}
}
