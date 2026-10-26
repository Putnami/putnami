package compose

import (
	"encoding/json"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
)

// A TypeScript project declares runsWith in its package.json, which its
// provider reports: its putnami.json authors none, and the job context carries
// the resolved list in the workspace membership. The composition reads that
// list, so the project composes its provider.
func TestNewWorkspace_AProviderReportedRunsWithComposesItsProvider(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", "runs-with-closure-served-behind-stable-proxies",
		"a-provider-reported-runs-with-composes-its-provider")
	var refs []job.ProjectRef
	if err := json.Unmarshal([]byte(`[
		{"id":"/ts/app","name":"app","path":"ts/app","fullPath":"/ws/ts/app","config":{"name":"app"},"type":"application","runsWith":["provider"]},
		{"id":"/ts/provider","name":"provider","path":"ts/provider","fullPath":"/ws/ts/provider","config":{"name":"provider"},"type":"application"}
	]`), &refs); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(t.TempDir(), nil, refs)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	app := ws.ProjectByID("/ts/app")
	if app == nil || app.Config.RunsWith != nil {
		t.Fatalf("app = %+v, want a project whose putnami.json authors no runsWith", app)
	}

	plan, err := PlanFor(ws, app)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if got, want := plan.ProjectIDs(), []string{"/ts/provider", "/ts/app"}; !slices.Equal(got, want) {
		t.Errorf("members = %v, want %v: the provider the app's package.json names, then the app", got, want)
	}
}
