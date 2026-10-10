package sourcecli

import (
	"strings"
	"testing"
	"time"

	"go.putnami.dev/client"
	sourceapiclient "go.putnami.dev/cloud/clients/source-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestSourceStatusNodeReadsTheConnectedRepository(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	ioctx.Stdout, ioctx.Stderr = func(string) {}, func(string) {}
	node := SourceStatusNode(map[string]any{}, clicore.EnvGet(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT"), ioctx.Env, ioctx)
	if node.State != clicore.StatusOK || node.Detail != "acme/app, 0 open pull requests" {
		t.Fatalf("node = %+v, want ok acme/app", node)
	}
	want := "GET /v1/workspaces/ws-acme/source/github\nGET /v1/workspaces/ws-acme/source/github/pull-requests"
	if got := requestMethods(fake.requests); got != want {
		t.Fatalf("requests = %q, want the source view, then the open pull requests", got)
	}
}

func TestSourceStatusNodeIsUnknownWithoutALinkedWorkspace(t *testing.T) {
	fake := &bindingsFakeServer{}
	ioctx := newSourceTestIO(t, fake)
	ioctx.Stdout, ioctx.Stderr = func(string) {}, func(string) {}
	node := SourceStatusNode(map[string]any{}, t.TempDir(), ioctx.Env, ioctx)
	if node.State != clicore.StatusUnknown || node.Fix != "putnami cloud source status" {
		t.Fatalf("node = %+v, want unknown with the source status fix", node)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %q, want none without a workspace", requestMethods(fake.requests))
	}
}

// TestSourceStatusCountsOpenAndStalePullRequests: the pull request read
// follows the cursor, counts a pull request with no update in 14 days as
// stale, and never changes the state.
func TestSourceStatusCountsOpenAndStalePullRequests(t *testing.T) {
	now := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	pull := func(number int, updated time.Time) map[string]any {
		return map[string]any{"number": number, "state": "open", "updated_at": updated.Format(time.RFC3339)}
	}
	fake := &bindingsFakeServer{pullPages: [][]map[string]any{
		{pull(1, now.Add(-time.Hour)), pull(2, now.Add(-15*24*time.Hour))},
		{pull(3, now.Add(-30*24*time.Hour))},
	}}
	ioctx := newSourceTestIO(t, fake)
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	if err := runSource(ioctx, []string{"status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{"source  ok  acme/app, 3 open pull requests (2 stale)", "open pull requests", "permissions  every required permission granted"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output misses %q:\n%s", want, out)
		}
	}

	// A refused read leaves the counts out and the state as it is.
	refused := &bindingsFakeServer{pullStatus: 503}
	ioctx = newSourceTestIO(t, refused)
	ioctx.Stdout, ioctx.Stderr = func(string) {}, func(string) {}
	node := SourceStatusNode(map[string]any{}, clicore.EnvGet(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT"), ioctx.Env, ioctx)
	if node.State != clicore.StatusOK || node.Detail != "acme/app" || len(node.Metrics) != 0 {
		t.Fatalf("node = %+v, want ok without pull request counts", node)
	}
}

// TestSourceStatusPullRequestReadIsBounded: past the page bound the counts
// are lower bounds and the metrics are estimated.
func TestSourceStatusPullRequestReadIsBounded(t *testing.T) {
	pages := make([][]map[string]any, sourcePullRequestPages+2)
	for i := range pages {
		pages[i] = []map[string]any{{"number": i + 1, "state": "open"}}
	}
	fake := &bindingsFakeServer{pullPages: pages}
	ioctx := newSourceTestIO(t, fake)
	ioctx.Stdout, ioctx.Stderr = func(string) {}, func(string) {}
	node := SourceStatusNode(map[string]any{}, clicore.EnvGet(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT"), ioctx.Env, ioctx)
	if !strings.Contains(node.Detail, "at least 5 open pull requests") || len(node.Metrics) != 2 || !node.Metrics[0].Estimated || node.Metrics[0].Value != 5 {
		t.Fatalf("node = %+v", node)
	}
	if reads := strings.Count(requestMethods(fake.requests), "/pull-requests"); reads != sourcePullRequestPages {
		t.Fatalf("pull request reads = %d, want %d", reads, sourcePullRequestPages)
	}
}

func TestSourceStatusNodeFrom(t *testing.T) {
	connected, owner, repo := true, "acme", "app"
	binding := client.Some(sourceapiclient.Binding{Owner: &owner, Repo: &repo})
	health := func(code string, healthy bool, gaps int) *sourceapiclient.SourceGitHubHealth {
		out := &sourceapiclient.SourceGitHubHealth{Code: &code, Healthy: &healthy}
		if gaps > 0 {
			permissionGaps := make([]sourceapiclient.SourceGitHubPermissionGap, gaps)
			for i := range permissionGaps {
				action := "grant checks: write to the GitHub App"
				permissionGaps[i].Action = &action
			}
			out.PermissionGaps = &permissionGaps
		}
		return out
	}
	repository := func(code string, healthy bool, gaps int) sourceapiclient.SourceGitHubRepository {
		return sourceapiclient.SourceGitHubRepository{Connected: &connected, Binding: binding, Health: health(code, healthy, gaps)}
	}
	for _, tc := range []struct {
		name     string
		view     sourceapiclient.SourceGitHubRepository
		state    clicore.StatusState
		detail   string
		fix      string
		children []clicore.StatusState
	}{
		{
			name:     "not connected",
			view:     sourceapiclient.SourceGitHubRepository{},
			state:    clicore.StatusDegraded,
			detail:   "no GitHub repository connected",
			fix:      "putnami cloud source connect",
			children: []clicore.StatusState{clicore.StatusDegraded},
		},
		{
			name:     "healthy",
			view:     repository("connected", true, 0),
			state:    clicore.StatusOK,
			detail:   "acme/app",
			children: []clicore.StatusState{clicore.StatusOK, clicore.StatusOK, clicore.StatusOK},
		},
		{
			name:     "suspended",
			view:     repository("installation_suspended", false, 0),
			state:    clicore.StatusFailing,
			detail:   "acme/app, GitHub App installation suspended",
			fix:      "unsuspend the Putnami GitHub App in the GitHub organization or account settings",
			children: []clicore.StatusState{clicore.StatusOK, clicore.StatusFailing, clicore.StatusUnknown},
		},
		{
			name:     "uninstalled",
			view:     repository("installation_uninstalled", false, 0),
			state:    clicore.StatusFailing,
			detail:   "acme/app, GitHub App uninstalled",
			fix:      "putnami cloud source connect --replace",
			children: []clicore.StatusState{clicore.StatusOK, clicore.StatusFailing, clicore.StatusUnknown},
		},
		{
			name:     "permission gaps",
			view:     repository("permission_update_required", false, 2),
			state:    clicore.StatusDegraded,
			detail:   "acme/app, 2 permission update(s) required",
			fix:      "putnami cloud source connect --replace",
			children: []clicore.StatusState{clicore.StatusOK, clicore.StatusOK, clicore.StatusDegraded},
		},
		{
			name:     "permissions not checked",
			view:     repository("permission_check_failed", false, 0),
			state:    clicore.StatusUnknown,
			detail:   "acme/app, the GitHub App permissions could not be checked",
			children: []clicore.StatusState{clicore.StatusOK, clicore.StatusOK, clicore.StatusUnknown},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := SourceStatusNodeFrom(SourceStatusFacts{Repository: tc.view})
			if node.ID != "source" || node.State != tc.state || node.Detail != tc.detail || node.Fix != tc.fix {
				t.Fatalf("node = %+v, want state %s, detail %q, fix %q", node, tc.state, tc.detail, tc.fix)
			}
			if len(node.Children) != len(tc.children) {
				t.Fatalf("children = %+v, want %d", node.Children, len(tc.children))
			}
			for i, child := range node.Children {
				if child.State != tc.children[i] {
					t.Fatalf("child %s = %s, want %s", child.ID, child.State, tc.children[i])
				}
			}
		})
	}
	gaps := SourceStatusNodeFrom(SourceStatusFacts{Repository: repository("permission_update_required", false, 1)})
	if detail := gaps.Children[2].Detail; detail != "1 permission update(s) required: grant checks: write to the GitHub App" {
		t.Fatalf("permissions detail = %q", detail)
	}
}

// TestSourceStatusExitsByTheSharedRule: a broken installation fails the
// command; a degraded one exits 0 unless --strict.
func TestSourceStatusExitsByTheSharedRule(t *testing.T) {
	ioctx := clicore.IO{Stdout: func(string) {}, Stderr: func(string) {}}
	connected, owner, repo, code, healthy := true, "acme", "app", "installation_suspended", false
	failing := SourceStatusNodeFrom(SourceStatusFacts{Repository: sourceapiclient.SourceGitHubRepository{
		Connected: &connected, Binding: client.Some(sourceapiclient.Binding{Owner: &owner, Repo: &repo}),
		Health: &sourceapiclient.SourceGitHubHealth{Code: &code, Healthy: &healthy},
	}})
	if err := clicore.WriteStatus(map[string]any{}, ioctx, failing); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("failing exit = %v", err)
	}
	degraded := SourceStatusNodeFrom(SourceStatusFacts{})
	if err := clicore.WriteStatus(map[string]any{}, ioctx, degraded); err != nil {
		t.Fatalf("degraded exit = %v", err)
	}
	if err := clicore.WriteStatus(map[string]any{"strict": true}, ioctx, degraded); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("strict degraded exit = %v", err)
	}
}
