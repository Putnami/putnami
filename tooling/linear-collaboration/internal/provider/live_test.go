package provider

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/tooling/linear-collaboration/internal/linear"
)

// liveTeamVariable names the key of the Linear team TestLiveLinear runs
// against. Unset, the test is skipped.
const liveTeamVariable = "PUTNAMI_LINEAR_COLLAB_LIVE_TEAM"

// liveTeamMarker must appear in that team's description: the test opens
// issues and labels there, so it refuses a team that is not explicitly marked
// as a disposable test team.
const liveTeamMarker = "putnami-collaboration-live-test"

// TestLiveLinear runs the shared task scenarios against a real Linear team
// with the key LINEAR_API_KEY holds. When it ends, passed or failed, it
// deletes every issue the run created and every team label whose name starts
// with the scenarios' pt- prefix. Run it with bin/live-test.
func TestLiveLinear(t *testing.T) {
	teamKey := os.Getenv(liveTeamVariable)
	if teamKey == "" {
		t.Skip(liveTeamVariable + " names no team; the live Linear proof runs only on request (bin/live-test)")
	}
	endpoint, err := linear.ResolveEndpoint(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	client := linear.New(endpoint, linear.NewCredential(os.Getenv), linear.Options{})
	ctx := context.Background()
	var about struct {
		Teams struct {
			Nodes []struct {
				ID          string `json:"id"`
				Key         string `json:"key"`
				Description string `json:"description"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := client.Query(ctx, "PutnamiLiveTeam", `query PutnamiLiveTeam($key: String!) { teams(filter: { key: { eqIgnoreCase: $key } }, first: 5) { nodes { id key description } } }`,
		map[string]any{"key": teamKey}, &about); err != nil {
		t.Fatalf("read team %s: %s", teamKey, err.Message)
	}
	if len(about.Teams.Nodes) != 1 {
		t.Fatalf("the organization has %d teams with key %s", len(about.Teams.Nodes), teamKey)
	}
	team := about.Teams.Nodes[0]
	if !strings.Contains(team.Description, liveTeamMarker) {
		t.Fatalf("team %s does not carry %q in its description; the live proof runs only in a team marked as disposable", teamKey, liveTeamMarker)
	}

	var mu sync.Mutex
	var created []string
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range created {
			if err := client.Mutate(ctx, "PutnamiLiveDeleteIssue", `mutation PutnamiLiveDeleteIssue($id: String!) { issueDelete(id: $id) { success } }`,
				map[string]any{"id": id}, nil); err != nil {
				t.Errorf("delete issue %s: %s", id, err.Message)
			}
		}
		var labels struct {
			IssueLabels struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"issueLabels"`
		}
		filter := map[string]any{"team": map[string]any{"id": map[string]any{"eq": team.ID}}, "name": map[string]any{"startsWith": "pt-"}}
		if err := client.Query(ctx, "PutnamiLiveLabels", `query PutnamiLiveLabels($filter: IssueLabelFilter!) { issueLabels(filter: $filter, first: 250) { nodes { id } } }`,
			map[string]any{"filter": filter}, &labels); err != nil {
			t.Errorf("list the scenario labels: %s", err.Message)
			return
		}
		for _, label := range labels.IssueLabels.Nodes {
			if err := client.Mutate(ctx, "PutnamiLiveDeleteLabel", `mutation PutnamiLiveDeleteLabel($id: String!) { issueLabelDelete(id: $id) { success } }`,
				map[string]any{"id": label.ID}, nil); err != nil {
				t.Errorf("delete label %s: %s", label.ID, err.Message)
			}
		}
	})
	providertest.RunTasks(t, func(t *testing.T) providertest.Target {
		return providertest.Target{
			Handlers:      New().Handlers(),
			WorkspaceRoot: t.TempDir(),
			Settings:      json.RawMessage(`{"team":` + quote(team.Key) + `}`),
			Settle:        30 * time.Second,
			Track: func(_ string, ref collab.Ref) {
				mu.Lock()
				defer mu.Unlock()
				created = append(created, ref.ID)
			},
		}
	})
}
