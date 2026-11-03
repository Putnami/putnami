package provider

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// liveSiteVariable and liveProjectVariable name the Jira Cloud site and the
// project TestLiveJira runs against. Unset, the test is skipped.
const (
	liveSiteVariable    = "PUTNAMI_JIRA_COLLAB_LIVE_SITE"
	liveProjectVariable = "PUTNAMI_JIRA_COLLAB_LIVE_PROJECT"
)

// liveProjectMarker must appear in the project's description: the test opens
// and deletes issues there, so it refuses a project that is not explicitly
// marked as a disposable test project.
const liveProjectMarker = "putnami-collaboration-live-test"

// TestLiveJira runs the shared tasks scenarios against a real Jira Cloud
// project with the credential JIRA_EMAIL and JIRA_API_TOKEN provide. When it
// ends, passed or failed, it deletes every issue the credential's account
// created in the project during the run. Run it with bin/live-test.
func TestLiveJira(t *testing.T) {
	site, project := os.Getenv(liveSiteVariable), os.Getenv(liveProjectVariable)
	if site == "" || project == "" {
		t.Skip(liveSiteVariable + " and " + liveProjectVariable + " name no project; the live Jira proof runs only on request (bin/live-test)")
	}
	settings := Settings{Site: site, Project: project}
	raw, _ := json.Marshal(settings)
	if failure := decodeSettings(raw, &Settings{}); failure != nil {
		t.Fatal(failure.Error.Message)
	}
	endpoint, err := jira.ResolveEndpoint(site, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	client := jira.New(endpoint, jira.NewCredential(os.Getenv), jira.Options{})
	ctx := context.Background()

	var about struct {
		Description string `json:"description"`
	}
	if err := client.Get(ctx, jira.Path("project", project), nil, &about); err != nil {
		t.Fatalf("read project %s: %s", project, err.Message)
	}
	if !strings.Contains(about.Description, liveProjectMarker) {
		t.Fatalf("the description of %s does not carry %q; the live proof runs only in a project marked as disposable", project, liveProjectMarker)
	}

	live := &liveCleanup{client: client, project: project, started: time.Now().Add(-time.Minute), tracked: map[string]bool{}}
	t.Cleanup(func() { live.run(t) })
	providertest.RunTasks(t, func(t *testing.T) providertest.Target {
		return providertest.Target{
			Handlers:      New().Handlers(),
			WorkspaceRoot: t.TempDir(),
			Settings:      raw,
			Settle:        90 * time.Second,
			Track:         live.track,
		}
	})
}

// liveCleanup deletes what a live run created.
type liveCleanup struct {
	client  *jira.Client
	project string
	started time.Time

	mu sync.Mutex
	// tracked holds the ids of the issues the scenarios reported.
	tracked map[string]bool
}

func (l *liveCleanup) track(_ string, ref collab.Ref) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tracked[ref.ID] = true
}

// run deletes the tracked issues and every issue the account created in the
// project since the run started, which covers a create whose answer was lost.
func (l *liveCleanup) run(t *testing.T) {
	ctx := context.Background()
	ids := map[string]bool{}
	l.mu.Lock()
	for id := range l.tracked {
		ids[id] = true
	}
	l.mu.Unlock()
	var found searchResult
	// A relative date: Jira reads an absolute one in the account's time zone.
	minutes := int(time.Since(l.started).Minutes()) + 1
	jql := "project = " + quote(l.project) + " AND creator = currentUser() AND created >= -" + strconv.Itoa(minutes) + "m ORDER BY key ASC"
	if err := l.client.Search(ctx, jira.Path("search", "jql"), searchRequest{JQL: jql, MaxResults: 100, Fields: []string{"summary"}}, &found); err != nil {
		t.Errorf("list the issues this run created: %s", err.Message)
	}
	for _, issue := range found.Issues {
		ids[issue.ID] = true
	}
	for id := range ids {
		if err := l.client.Write(ctx, "DELETE", jira.Path("issue", id), nil, nil); err != nil && err.Status != 404 {
			t.Errorf("delete issue %s: %s", id, err.Message)
		}
	}
	t.Logf("deleted %d issues of %s", len(ids), l.project)
}
