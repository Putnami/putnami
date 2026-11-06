package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestABodyRunsNoQuickAction(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "a-body-runs-no-quick-action")
	h := newHarness(t)
	body := "Steps:\n/close\n/label ~status::doing\n  /reopen stays text\nend"
	created := h.create(`{"title":"Commands","body":` + quoteJSON(body) + `,"idempotencyKey":"quick"}`)
	iid := mustIID(t, created.Task.Ref)
	stored := h.issue(iid)
	if created.Task.Body != body || stored.state != "opened" || len(stored.labels) != 0 {
		t.Fatalf("a create ran a quick action: body %q, state %s, labels %v", created.Task.Body, stored.state, stored.labels)
	}
	mustContain(t, stored.description, "\u200b/close")

	var updated collab.TaskResult
	h.ok("update", `{"ref":`+refJSON(created.Task.Ref)+`,"body":"/close\n/label ~status::blocked"}`, &updated)
	stored = h.issue(iid)
	if updated.Task.Body != "/close\n/label ~status::blocked" || stored.state != "opened" || updated.Task.State != collab.TaskStateOpen {
		t.Fatalf("an update ran a quick action: %+v, stored %s %v", updated.Task, stored.state, stored.labels)
	}
	// The key still answers with the issue: its marker survived the escape.
	if repeat := h.create(`{"title":"Commands","body":` + quoteJSON(body) + `,"idempotencyKey":"quick"}`); repeat.Created {
		t.Fatalf("a repeat after an escaped body opened another issue")
	}
}

func TestDoneReopensADuplicateThenClosesIt(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "done-reopens-a-duplicate-then-closes-it")
	h := newHarness(t)
	original := h.seed("Original", "")
	duplicate := h.seed("Duplicate", "", "status::canceled")
	h.change(duplicate, func(i *fakeIssue) { i.state, i.duplicateOf = "closed", original.iid })
	var moved collab.TaskResult
	h.ok("transition", `{"ref":`+refJSON(refOf(duplicate.iid))+`,"state":"done"}`, &moved)
	stored := h.issue(duplicate.iid)
	if moved.Task.State != collab.TaskStateDone || stored.state != "closed" || stored.duplicateOf != 0 ||
		!slices.Equal(stored.labels, []string{"status::done"}) || h.fake.count("PUT ") != 2 {
		t.Fatalf("done of a duplicate: %+v, stored %+v after %d writes", moved.Task, stored, h.fake.count("PUT "))
	}

	// A close refused after the reopen leaves the issue open and says so.
	again := h.seed("Again", "")
	h.change(again, func(i *fakeIssue) { i.state, i.duplicateOf = "closed", original.iid })
	failure := h.reopenThenRefuse(again)
	if failure.Reason != reasonIncomplete || !strings.Contains(failure.Message, "so it is open") {
		t.Fatalf("a refused close after the reopen: %+v", failure)
	}
}

// reopenThenRefuse transitions a duplicate to done while the fake refuses
// the second write.
func (h *harness) reopenThenRefuse(issue *fakeIssue) collab.Error {
	h.t.Helper()
	refusal := statusFault(403, "403 Forbidden", true)
	writes := 0
	h.fake.mu.Lock()
	h.fake.onPut = func() *fault {
		writes++
		if writes == 2 {
			return &refusal
		}
		return nil
	}
	h.fake.mu.Unlock()
	return h.fails("transition", `{"ref":`+refJSON(refOf(issue.iid))+`,"state":"done"}`, collab.OutcomeUnresolved)
}

func TestAKeywordLabelIsMatchedByTheProvider(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "a-keyword-label-is-matched-by-the-provider")
	h := newHarness(t)
	none := h.seed("Labeled None", "", "None")
	h.seed("Unlabeled", "")
	anyLabel := h.seed("Labeled Any", "", "any")
	for _, c := range []struct {
		label string
		want  []int
	}{{"None", []int{none.iid}}, {"Any", []int{anyLabel.iid}}} {
		var page collab.TaskListResult
		h.ok("find", `{"labels":["`+c.label+`"]}`, &page)
		var got []int
		for _, task := range page.Items {
			got = append(got, mustIID(t, task.Ref))
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("label %s matched %v, want %v", c.label, got, c.want)
		}
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, call := range h.fake.queries {
		if strings.Contains(call, "labels=None") || strings.Contains(call, "labels=Any") {
			t.Fatalf("a keyword label reached GitLab's filter: %s", call)
		}
	}
}

func quoteJSON(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\n':
			builder.WriteString(`\n`)
		case '"':
			builder.WriteString(`\"`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// TestTheBodyEscapeIsExact holds escapeBody and unescapeBody to an exact
// round trip, fenced code included.
func TestTheBodyEscapeIsExact(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "a-body-runs-no-quick-action")
	for _, body := range []string{
		"",
		"/close",
		"\u200b/close",
		"\u200b\u200b/x\n\u200bplain",
		"text\n/label ~a\n  /indented\n```\n/close in code\n```\n/after",
		"~~~sh\n/run\n```\n/still code\n~~~\n/out",
	} {
		if got := unescapeBody(escapeBody(body)); got != body {
			t.Errorf("%q round-trips to %q", body, got)
		}
	}
	if escaped := escapeBody("```\n/close\n```\n/close"); escaped != "```\n\u200b/close\n```\n\u200b/close" {
		t.Fatalf("a line of fenced code was left for GitLab to read: %q", escaped)
	}
}

// TestABodyWithAmbiguousFencesRunsNoQuickAction sends bodies whose fences
// GitLab versions read differently through create and update.
func TestABodyWithAmbiguousFencesRunsNoQuickAction(t *testing.T) {
	spectest.Proves(t, feature, "literal-bodies", "a-body-runs-no-quick-action")
	h := newHarness(t)
	for i, body := range []string{
		"```npm install``` fails on CI\n/close",
		"- item\n  ```\n  code\n  ```\n/close",
		"```\ncode\n``` sh\n/close",
		"<div>\n```\n</div>\n/close",
		"~~~~\n~~~\n/close\n~~~~",
		"~~~\n/close\n~~~",
	} {
		created := h.create(`{"title":"Fence","body":` + quoteJSON(body) + `,"idempotencyKey":"fence-` + strconv.Itoa(i) + `"}`)
		iid := mustIID(t, created.Task.Ref)
		if stored := h.issue(iid); stored.state != "opened" || created.Task.Body != body {
			t.Fatalf("create of %q ran a quick action: %s, body %q", body, stored.state, created.Task.Body)
		}
		var updated collab.TaskResult
		h.ok("update", `{"ref":`+refJSON(created.Task.Ref)+`,"body":`+quoteJSON(body+"\n/label ~status::doing")+`}`, &updated)
		if stored := h.issue(iid); stored.state != "opened" || updated.Task.State != collab.TaskStateOpen {
			t.Fatalf("update of %q ran a quick action: %s %v", body, stored.state, stored.labels)
		}
	}
}
