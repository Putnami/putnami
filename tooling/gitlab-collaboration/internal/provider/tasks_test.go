package provider

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func mustIID(t *testing.T, ref collab.Ref) int {
	t.Helper()
	if ref.Source != source {
		t.Fatalf("ref %+v is not from %s", ref, source)
	}
	iid, err := strconv.Atoi(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return iid
}

func TestTheRevisionCoversWhatATaskShows(t *testing.T) {
	spectest.Proves(t, feature, "content-revision", "the-revision-covers-what-a-task-shows")
	h := newHarness(t)
	issue := h.seed("Revisioned", "Body", "bug")
	ref := refOf(issue.iid)
	seen := map[string]string{"initial": h.get(ref).Revision}
	changes := []struct {
		name string
		edit func(*fakeIssue)
	}{
		{"title", func(i *fakeIssue) { i.title = "Renamed" }},
		{"description", func(i *fakeIssue) { i.description = "Other body" }},
		{"labels", func(i *fakeIssue) { i.labels = []string{"bug", "feature"} }},
		{"state label", func(i *fakeIssue) { i.labels = []string{"bug", "feature", "status::doing"} }},
		{"assignees", func(i *fakeIssue) { i.assignees = []int64{9} }},
		{"state", func(i *fakeIssue) { i.state = "closed" }},
		{"duplicate", func(i *fakeIssue) { i.duplicateOf = 1 }},
	}
	for _, change := range changes {
		h.change(issue, change.edit)
		revision := h.get(ref).Revision
		for name, other := range seen {
			if other == revision {
				t.Fatalf("after a change of %s the revision is the one of %s", change.name, name)
			}
		}
		seen[change.name] = revision
	}
	// A comment moves updated_at, never the revision.
	before := h.get(ref)
	h.change(issue, func(i *fakeIssue) { i.updated = i.updated.Add(time.Hour) })
	if after := h.get(ref); after.Revision != before.Revision || after.UpdatedAt == before.UpdatedAt {
		t.Fatalf("a comment moved the revision: %s then %s", before.Revision, after.Revision)
	}
}

func TestAStaleRevisionIsAConflictBeforeAnyWrite(t *testing.T) {
	spectest.Proves(t, feature, "content-revision", "a-stale-revision-is-a-conflict-before-any-write")
	h := newHarness(t)
	issue := h.seed("Revisioned", "")
	ref := refJSON(refOf(issue.iid))
	stale := h.get(refOf(issue.iid)).Revision
	h.change(issue, func(i *fakeIssue) { i.title = "Changed elsewhere" })
	current := h.get(refOf(issue.iid)).Revision
	for _, request := range []struct{ operation, arguments string }{
		{"update", `{"ref":` + ref + `,"expectedRevision":"` + stale + `","title":"Mine"}`},
		{"transition", `{"ref":` + ref + `,"expectedRevision":"` + stale + `","state":"done"}`},
		{"assign", `{"ref":` + ref + `,"expectedRevision":"` + stale + `","assignees":["alice"]}`},
	} {
		failure := h.fails(request.operation, request.arguments, collab.OutcomeConflict)
		if failure.Reason != collab.ReasonRevisionConflict || failure.Current != current || failure.Retryable {
			t.Fatalf("%s: %+v", request.operation, failure)
		}
	}
	if writes := h.fake.writes(); writes != 0 {
		t.Fatalf("a stale revision sent %d writes", writes)
	}
}

func TestFindFiltersByStateLabelAndText(t *testing.T) {
	spectest.Proves(t, feature, "paged-find", "find-filters-by-state-label-and-text")
	h := newHarness(t)
	open := h.seed("Find the needle", "", "team")
	doing := h.seed("Doing", "has a needle too", "team", "status::doing")
	done := h.seed("Done", "", "team")
	canceled := h.seed("Canceled", "", "team", "status::canceled")
	elsewhere := h.seed("Elsewhere", "needle", "other")
	odd := h.seed("Comma", "", "a,b")
	h.change(done, func(i *fakeIssue) { i.state = "closed" })
	h.change(canceled, func(i *fakeIssue) { i.state = "closed" })
	h.change(odd, func(i *fakeIssue) { i.labels = []string{"a,b", "team"} })

	find := func(arguments string) []int {
		var page collab.TaskListResult
		h.ok("find", arguments, &page)
		var iids []int
		for _, task := range page.Items {
			iids = append(iids, mustIID(t, task.Ref))
		}
		return iids
	}
	for _, c := range []struct {
		arguments string
		want      []int
	}{
		{`{"labels":["team"]}`, []int{open.iid, doing.iid, done.iid, canceled.iid, odd.iid}},
		{`{"labels":["team"],"states":["open"]}`, []int{open.iid, odd.iid}},
		{`{"labels":["team"],"states":["in_progress","done"]}`, []int{doing.iid, done.iid}},
		{`{"labels":["team"],"states":["canceled"]}`, []int{canceled.iid}},
		{`{"labels":["team"],"states":["blocked"]}`, nil},
		{`{"labels":["team"],"query":"needle"}`, []int{open.iid, doing.iid}},
		{`{"query":"NEEDLE","states":["open"]}`, []int{open.iid, elsewhere.iid}},
		{`{"labels":["a,b"]}`, []int{odd.iid}},
		{`{"labels":["team","absent"]}`, nil},
	} {
		if got := find(c.arguments); !slices.Equal(got, c.want) {
			t.Errorf("find %s matched %v, want %v", c.arguments, got, c.want)
		}
	}
	h.fake.mu.Lock()
	calls := slices.Clone(h.fake.calls)
	h.fake.mu.Unlock()
	if !slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "GET /api/v4/projects/") }) {
		t.Fatalf("find never listed the project: %v", calls)
	}
}

func TestPagesCoverTheProjectExactlyOnce(t *testing.T) {
	spectest.Proves(t, feature, "paged-find", "pages-cover-the-project-exactly-once")
	h := newHarness(t)
	var want []int
	for i := 0; i < 230; i++ {
		issue := h.seed(fmt.Sprintf("Issue %d", i), "", "paged")
		want = append(want, issue.iid)
		if i%7 == 0 {
			h.seed("Unlabeled", "")
		}
	}
	// An issue imported with an older creation time sorts before the others
	// whatever its iid.
	imported := h.seed("Imported", "", "paged")
	h.change(imported, func(i *fakeIssue) { i.created = i.created.Add(-48 * time.Hour) })
	want = append([]int{imported.iid}, want...)

	traverse := func(sizes []int, during func(pages int)) []int {
		var iids []int
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 400 {
				t.Fatalf("the traversal does not end")
			}
			size := sizes[pages%len(sizes)]
			arguments := `{"labels":["paged"],"page":{"size":` + strconv.Itoa(size) + `}}`
			if cursor != "" {
				arguments = `{"labels":["paged"],"page":{"size":` + strconv.Itoa(size) + `,"cursor":"` + cursor + `"}}`
			}
			var page collab.TaskListResult
			h.ok("find", arguments, &page)
			if len(page.Items) > size {
				t.Fatalf("a page of %d carried %d", size, len(page.Items))
			}
			for _, task := range page.Items {
				iids = append(iids, mustIID(t, task.Ref))
			}
			if during != nil {
				during(pages)
			}
			if cursor = page.Page.Next; cursor == "" {
				return iids
			}
		}
	}
	if got := traverse([]int{100}, nil); !slices.Equal(got, want) {
		t.Fatalf("pages of 100 listed %d issues, want %d in creation order", len(got), len(want))
	}
	if got := traverse([]int{7, 50, 1}, nil); !slices.Equal(got, want) {
		t.Fatalf("pages of changing sizes listed %d issues, want %d", len(got), len(want))
	}
	// Issues opened and deleted during a traversal move no other issue
	// across a page boundary.
	var added []int
	got := traverse([]int{30}, func(pages int) {
		if pages == 1 {
			h.fake.mu.Lock()
			h.fake.issues[want[100]].deleted = true
			h.fake.mu.Unlock()
		}
		if pages == 2 {
			added = append(added, h.seed("Late", "", "paged").iid)
		}
	})
	expected := append(slices.Delete(slices.Clone(want), 100, 101), added...)
	if !slices.Equal(got, expected) {
		t.Fatalf("a traversal with concurrent changes listed %d issues, want %d", len(got), len(expected))
	}
	for _, cursor := range []string{"p1.n3", "t1.n0", "t1.i", "t1.n1-2"} {
		h.fails("find", `{"page":{"cursor":"`+cursor+`"}}`, collab.OutcomeInvalid)
	}
}

// TestAFindReadsAtMostFivePagesPerAnswer bounds the GitLab pages one answer
// reads: a sparse match answers with what it found and a cursor.
func TestAFindReadsAtMostFivePagesPerAnswer(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 520; i++ {
		h.seed("Noise", "", "noise")
	}
	last := h.seed("Needle", "", "noise", "status::doing")
	var page collab.TaskListResult
	h.ok("find", `{"states":["in_progress"]}`, &page)
	if len(page.Items) != 0 || page.Page.Next == "" {
		t.Fatalf("the first answer read past five pages: %+v", page.Page)
	}
	h.ok("find", `{"states":["in_progress"],"page":{"cursor":"`+page.Page.Next+`"}}`, &page)
	if len(page.Items) != 1 || mustIID(t, page.Items[0].Ref) != last.iid || page.Page.Next != "" {
		t.Fatalf("the resumed answer: %+v", page)
	}
}

// traverse walks every page of a find, asking for the sizes in turn, and
// calls during after each page.
func (h *harness) traverse(arguments string, sizes []int, during func(page int)) []int {
	h.t.Helper()
	var iids []int
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 500 {
			h.t.Fatalf("the traversal does not end")
		}
		size := strconv.Itoa(sizes[pages%len(sizes)])
		request := strings.TrimSuffix(arguments, "}")
		if request != "{" {
			request += ","
		}
		request += `"page":{"size":` + size
		if cursor != "" {
			request += `,"cursor":"` + cursor + `"`
		}
		request += "}}"
		var page collab.TaskListResult
		h.ok("find", request, &page)
		for _, task := range page.Items {
			iids = append(iids, mustIID(h.t, task.Ref))
		}
		if during != nil {
			during(pages)
		}
		if cursor = page.Page.Next; cursor == "" {
			return iids
		}
	}
}

func TestATraversalSurvivesTiesAndClosedIssues(t *testing.T) {
	spectest.Proves(t, feature, "paged-find", "a-traversal-survives-ties-and-closed-issues")

	// Issues created in one millisecond, as an import makes them: GitLab
	// lists them by id descending, and by microsecond within a millisecond.
	h := newHarness(t)
	var tied []*fakeIssue
	for i := 0; i < 7; i++ {
		tied = append(tied, h.seed("Tied", "", "tie"))
	}
	at := tied[0].created
	for i, issue := range tied {
		h.change(issue, func(f *fakeIssue) { f.created = at })
		if i == 5 {
			h.change(issue, func(f *fakeIssue) { f.created = at.Add(300 * time.Microsecond) })
		}
		if i == 6 {
			h.change(issue, func(f *fakeIssue) { f.created = at.Add(200 * time.Microsecond) })
		}
	}
	for _, sizes := range [][]int{{1}, {2}, {3, 1}, {100}} {
		got := h.traverse(`{"labels":["tie"]}`, sizes, nil)
		slices.Sort(got)
		if len(got) != len(tied) || len(slices.Compact(slices.Clone(got))) != len(tied) {
			t.Fatalf("pages of %v listed %v of %d tied issues", sizes, got, len(tied))
		}
	}

	// Closing issues during a traversal filtered on open moves the rest
	// onto pages the cursor already passed.
	h = newHarness(t)
	var open []int
	for i := 0; i < 150; i++ {
		open = append(open, h.seed("Open", "", "walk").iid)
	}
	got := h.traverse(`{"labels":["walk"],"states":["open"]}`, []int{100}, func(page int) {
		if page == 0 {
			h.fake.mu.Lock()
			for _, iid := range open[:60] {
				h.fake.issues[iid].state = "closed"
			}
			h.fake.mu.Unlock()
		}
	})
	if !slices.Equal(got, open) {
		t.Fatalf("closing 60 issues mid-traversal listed %d of %d", len(got), len(open))
	}

	// Ties larger than the ids a cursor carries end, and list every issue
	// once: past 48 the cursor counts them.
	for _, c := range []struct{ tied, later, size int }{{70, 0, 60}, {120, 0, 100}, {250, 10, 100}, {49, 3, 10}} {
		h = newHarness(t)
		var want []int
		var at time.Time
		for i := 0; i < c.tied; i++ {
			issue := h.seed("Tied", "", "many")
			if i == 0 {
				at = issue.created
			}
			h.change(issue, func(f *fakeIssue) { f.created = at })
			want = append(want, issue.iid)
		}
		for i := 0; i < c.later; i++ {
			want = append(want, h.seed("Later", "", "many").iid)
		}
		got := h.traverse(`{"labels":["many"]}`, []int{c.size}, nil)
		sorted := slices.Clone(got)
		slices.Sort(sorted)
		if !slices.Equal(sorted, want) {
			t.Fatalf("a tie of %d with %d later issues in pages of %d listed %d issues, want each of %d once", c.tied, c.later, c.size, len(got), len(want))
		}
	}

	// An issue closed between two GitLab reads of one answer hides no other.
	h = newHarness(t)
	var doing []int
	for i := 0; i < 250; i++ {
		labels := []string{"walk"}
		if i%2 == 0 {
			labels = append(labels, "status::doing")
		}
		issue := h.seed("Walk", "", labels...)
		if i%2 == 0 {
			doing = append(doing, issue.iid)
		}
	}
	reads := 0
	h.fake.mu.Lock()
	h.fake.onList = func() {
		if reads++; reads == 2 {
			for iid := 1; iid <= 30; iid++ {
				h.fake.issues[iid].state = "closed"
			}
		}
	}
	h.fake.mu.Unlock()
	got = h.traverse(`{"labels":["walk"],"states":["in_progress"]}`, []int{100}, nil)
	if !slices.Equal(got, doing) {
		t.Fatalf("closing issues between reads of one answer listed %d of %d", len(got), len(doing))
	}
}
