package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// liveProjectVariable and liveHostVariable name the GitLab project
// TestLiveGitLab runs against and its instance (gitlab.com unless set).
// Without a project, the test is skipped.
const (
	liveProjectVariable = "PUTNAMI_GITLAB_COLLAB_LIVE_PROJECT"
	liveHostVariable    = "PUTNAMI_GITLAB_COLLAB_LIVE_HOST"
)

// liveProjectMarker must appear in the project's description: the test opens
// and closes issues there, so it refuses a project that is not explicitly
// marked as a disposable test project.
const liveProjectMarker = "putnami-collaboration-live-test"

// TestLiveGitLab runs the shared tasks scenarios against a real GitLab
// project with the credential glab or GITLAB_TOKEN provides. When it ends,
// passed or failed, it deletes every issue the credential's account opened in
// the project during the run, or closes it when the account may not delete
// issues. Run it with bin/live-test.
func TestLiveGitLab(t *testing.T) {
	project, host := os.Getenv(liveProjectVariable), os.Getenv(liveHostVariable)
	if project == "" {
		t.Skip(liveProjectVariable + " names no project; the live GitLab proof runs only on request (bin/live-test)")
	}
	settings := TaskSettings{Project: project, Host: host, States: StateLabels{
		InProgress: []string{"status::in-progress"}, Blocked: []string{"status::blocked"}, Canceled: []string{"status::canceled"},
	}, StateLabelPrefix: "status::"}
	raw, _ := json.Marshal(settings)
	parsed, failure := parseProject(project, host)
	if failure != nil {
		t.Fatal(failure.Error.Message)
	}
	endpoint, err := gitlab.ResolveEndpoint(parsed.host, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	client := gitlab.New(endpoint, gitlab.NewCredential(endpoint.Host, os.Getenv, gitlab.GlabConfigToken), gitlab.Options{})
	ctx := context.Background()

	var about struct {
		Description string `json:"description"`
	}
	if _, err := client.Get(ctx, parsed.apiPath(), nil, &about); err != nil {
		t.Fatalf("read project %s: %s", project, err.Message)
	}
	if !strings.Contains(about.Description, liveProjectMarker) {
		t.Fatalf("the description of %s does not carry %q; the live proof runs only in a project marked as disposable", project, liveProjectMarker)
	}

	live := &liveCleanup{client: client, project: parsed, started: time.Now().Add(-time.Minute), tracked: map[string]bool{}}
	t.Cleanup(func() { live.run(t) })
	providertest.RunTasks(t, func(t *testing.T) providertest.Target {
		return providertest.Target{
			Handlers:      New().Handlers(),
			WorkspaceRoot: t.TempDir(),
			Settings:      raw,
			Settle:        30 * time.Second,
			Track:         live.track,
		}
	})
}

// liveCleanup removes what a live run opened.
type liveCleanup struct {
	client  *gitlab.Client
	project project
	started time.Time

	mu sync.Mutex
	// tracked holds the iids of the issues the scenarios reported.
	tracked map[string]bool
}

func (l *liveCleanup) track(_ string, ref collab.Ref) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tracked[ref.ID] = true
}

// run removes the tracked issues and every issue the account opened in the
// project since the run started, which covers a create whose answer was lost.
func (l *liveCleanup) run(t *testing.T) {
	ctx := context.Background()
	iids := map[string]bool{}
	l.mu.Lock()
	for iid := range l.tracked {
		iids[iid] = true
	}
	l.mu.Unlock()
	var me glUser
	if _, err := l.client.Get(ctx, "/user", nil, &me); err != nil {
		t.Errorf("read the account: %s", err.Message)
	}
	var opened []glIssue
	query := url.Values{"author_id": {strconv.FormatInt(me.ID, 10)}, "created_after": {l.started.UTC().Format(time.RFC3339)},
		"state": {"all"}, "per_page": {"100"}}
	if _, err := l.client.Get(ctx, l.project.apiPath("issues"), query, &opened); err != nil {
		t.Errorf("list the issues this run opened: %s", err.Message)
	}
	for _, issue := range opened {
		iids[strconv.Itoa(issue.IID)] = true
	}
	deleted, closed := 0, 0
	for iid := range iids {
		path := l.project.apiPath("issues", iid)
		err := l.client.Write(ctx, http.MethodDelete, path, nil, nil)
		switch {
		case err == nil || err.Status == http.StatusNotFound:
			deleted++
		case err.Status == http.StatusForbidden:
			if err := l.client.Write(ctx, http.MethodPut, path, map[string]any{"state_event": "close"}, nil); err != nil {
				t.Errorf("close issue #%s: %s", iid, err.Message)
			}
			closed++
		default:
			t.Errorf("delete issue #%s: %s", iid, err.Message)
		}
	}
	t.Logf("deleted %d and closed %d issues of %s", deleted, closed, l.project.path)
}
