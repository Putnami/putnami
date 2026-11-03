package provider

import (
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestCallerTextCannotChangeTheJQL(t *testing.T) {
	spectest.Proves(t, feature, "jql-find", "caller-text-cannot-change-the-jql")
	s := &session{settings: Settings{Project: "ENG"}}
	jql, ok := s.findJQL(&collab.TaskFindInput{
		Query:  `x" OR project = OTHER OR summary ~ "y AND (z) \ -w*`,
		Labels: []string{`a"b`, `c\`},
		States: []collab.TaskState{collab.TaskStateOpen, collab.TaskStateDone, collab.TaskStateCanceled, collab.TaskStateBlocked},
	})
	want := `project = "ENG" AND statusCategory IN (1, 2, 3) AND labels = "a\"b" AND labels = "c\\" AND ` +
		`(summary ~ "x or project = other or summary y and z w" OR description ~ "x or project = other or summary y and z w") ORDER BY key ASC`
	if !ok || jql != want {
		t.Fatalf("jql\n%s\nwant\n%s", jql, want)
	}
	query, err := parseJQL(jql, time.Now())
	if err != nil || query.project != "ENG" || len(query.labels) != 2 || query.labels[0] != `a"b` || query.labels[1] != `c\` {
		t.Fatalf("the JQL reads as %+v, %v", query, err)
	}
	if _, ok := s.findJQL(&collab.TaskFindInput{Query: `"?*!`}); ok {
		t.Fatal("a text of operators only searched")
	}

	h := newHarness(t)
	h.seed("Other project", "")
	h.fake.mu.Lock()
	h.fake.project = "OTHER"
	h.fake.newIssue("x or project = other", "", nil, fakeAccount)
	h.fake.project = "ENG"
	h.fake.mu.Unlock()
	var page collab.TaskListResult
	h.ok("find", `{"query":"x\" OR project = OTHER OR summary ~ \"y"}`, &page)
	if len(page.Items) != 0 {
		t.Fatalf("the text reached another project: %+v", page.Items)
	}
	h.fails("find", `{"labels":["putnami-idempotency-abc"]}`, collab.OutcomeInvalid)
}

func TestFindFiltersByStateLabelAndText(t *testing.T) {
	spectest.Proves(t, feature, "jql-find", "find-filters-by-state-label-and-text")
	h := newHarness(t)
	open := h.seed("Open needle", "", "team")
	done := h.seed("Done one", "a needle inside", "team")
	canceled := h.seed("Canceled one", "", "team")
	h.seed("Unlabeled needle", "")
	h.setStatus(done, "Done", "Done")
	h.setStatus(canceled, "Done", "Duplicate")
	ids := func(arguments string) string {
		t.Helper()
		var page collab.TaskListResult
		h.ok("find", arguments, &page)
		keys := make([]string, 0, len(page.Items))
		for _, task := range page.Items {
			keys = append(keys, task.Key)
		}
		return strings.Join(keys, ",")
	}
	for arguments, want := range map[string]string{
		`{"labels":["team"]}`:                              open.key() + "," + done.key() + "," + canceled.key(),
		`{"labels":["TEAM"],"states":["done"]}`:            done.key(),
		`{"labels":["team"],"states":["canceled"]}`:        canceled.key(),
		`{"labels":["team"],"states":["open","canceled"]}`: open.key() + "," + canceled.key(),
		`{"labels":["team"],"query":"NEEDLE"}`:             open.key() + "," + done.key(),
		`{"query":"needle"}`:                               open.key() + "," + done.key() + ",ENG-4",
	} {
		if got := ids(arguments); got != want {
			t.Fatalf("find %s answered %s, want %s", arguments, got, want)
		}
	}
}

func TestPagesCoverTheProjectExactlyOnce(t *testing.T) {
	spectest.Proves(t, feature, "jql-find", "pages-cover-the-project-exactly-once")
	h := newHarness(t)
	for i := 0; i < 230; i++ {
		h.seed("Issue "+strconv.Itoa(i), "")
	}
	first := h.issue("10000")
	seen := map[string]int{}
	cursor := ""
	sizes := []int{7, 100, 1, 33}
	for pages := 0; ; pages++ {
		if pages > 300 {
			t.Fatal("the traversal did not end")
		}
		arguments := `{"page":{"size":` + strconv.Itoa(sizes[pages%len(sizes)]) + `}}`
		if cursor != "" {
			arguments = `{"page":{"size":` + strconv.Itoa(sizes[pages%len(sizes)]) + `,"cursor":"` + cursor + `"}}`
		}
		var page collab.TaskListResult
		h.ok("find", arguments, &page)
		for _, task := range page.Items {
			seen[task.Key]++
		}
		if pages == 2 {
			// During the traversal one issue is deleted and one created.
			h.fake.mu.Lock()
			h.fake.issues["10150"].deleted = true
			h.fake.newIssue("Late", "", nil, fakeAccount)
			h.fake.mu.Unlock()
		}
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	for i := 0; i < 230; i++ {
		key := "ENG-" + strconv.Itoa(i+1)
		if i == 150 {
			continue
		}
		if seen[key] != 1 {
			t.Fatalf("%s was listed %d times", key, seen[key])
		}
	}
	if seen["ENG-231"] != 1 || seen[first.key()] != 1 {
		t.Fatalf("the issue created during the traversal was listed %d times", seen["ENG-231"])
	}
	h.fails("find", `{"page":{"size":5,"cursor":"p2.n9"}}`, collab.OutcomeInvalid)
}

func TestCursorsCarryJiraTokens(t *testing.T) {
	for _, token := range []string{"", "abc=", "a&b c", strings.Repeat("x", 600)} {
		at := cursor{last: 42, token: token}
		parsed, failure := parseCursor(at.String())
		if failure != nil || parsed != at {
			t.Fatalf("cursor %q read back as %+v, %v", at.String(), parsed, failure)
		}
	}
	if _, failure := paged(&collab.TaskListResult{}, cursor{last: 1, token: strings.Repeat("&", 800)}); failure == nil {
		t.Fatal("a cursor beyond the contract's bound was issued")
	}
}

// TestAFullPageEndsAtAJiraPageBoundary: a page that fills with the last issue
// of a Jira page answers without reading the next one.
func TestAFullPageEndsAtAJiraPageBoundary(t *testing.T) {
	h := newHarness(t)
	for range jiraPageSize + 1 {
		h.seed("Paged", "")
	}
	var page collab.TaskListResult
	h.ok("find", `{"page":{"size":100}}`, &page)
	if len(page.Items) != jiraPageSize || page.Page.Next == "" || h.fake.count("POST /rest/api/2/search/jql") != 1 {
		t.Fatalf("a full page read %d search pages: %d items, next %q", h.fake.count("POST /rest/api/2/search/jql"), len(page.Items), page.Page.Next)
	}
	h.ok("find", `{"page":{"size":100,"cursor":"`+page.Page.Next+`"}}`, &page)
	if len(page.Items) != 1 || page.Page.Next != "" {
		t.Fatalf("the next page carried %d items, next %q", len(page.Items), page.Page.Next)
	}
}
