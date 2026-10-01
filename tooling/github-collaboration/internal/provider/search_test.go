package provider

import (
	"strconv"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestASearchTraversalEndsAtGitHubsThousandResults(t *testing.T) {
	spectest.Proves(t, feature, "honest-failures", "a-search-traversal-ends-at-its-thousand-results")
	h := newHarness(t)
	h.fake.mu.Lock()
	for i := 0; i < maxSearchResults+50; i++ {
		h.fake.newIssue("Widespread "+strconv.Itoa(i), "", nil)
	}
	h.fake.mu.Unlock()
	seen := 0
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("the traversal did not end")
		}
		arguments := `{"query":"widespread","page":{"size":100}}`
		if cursor != "" {
			arguments = `{"query":"widespread","page":{"size":100,"cursor":` + strconv.Quote(cursor) + `}}`
		}
		var page collab.TaskListResult
		h.ok("tasks", "find", arguments, &page)
		seen += len(page.Items)
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	if seen != maxSearchResults {
		t.Fatalf("the traversal returned %d tasks, want the %d GitHub's search serves", seen, maxSearchResults)
	}
}
