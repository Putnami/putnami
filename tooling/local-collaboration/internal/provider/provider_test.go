package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
)

// The provider is driven through collab.Serve with the request a routed call
// carries, so every answer here passed the same request validation and result
// validation a real call does.

type harness struct {
	t         *testing.T
	workspace string
	settings  string
	provider  *Provider
}

func newHarness(t *testing.T, settings string) *harness {
	t.Helper()
	clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	return &harness{t: t, workspace: t.TempDir(), settings: settings, provider: &Provider{Now: func() time.Time { return clock }}}
}

func (h *harness) call(contract, operation, arguments string) collab.Response {
	h.t.Helper()
	request := extension.ToolCallRequest{
		Name:          "local-collaboration." + contract + "." + operation,
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: h.workspace,
		Provider:      &extension.ToolProviderCall{Contract: contract, Version: 1, Operation: operation},
	}
	if h.settings != "" {
		request.Provider.Settings = json.RawMessage(h.settings)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		h.t.Fatal(err)
	}
	var out bytes.Buffer
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), &out, h.provider.Handlers()); err != nil {
		h.t.Fatal(err)
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		h.t.Fatal(err)
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		h.t.Fatalf("invalid response %s: %v", result.Content[0].Text, diags)
	}
	return *response
}

func (h *harness) ok(contract, operation, arguments string, into any) {
	h.t.Helper()
	response := h.call(contract, operation, arguments)
	if response.Outcome != collab.OutcomeOK {
		h.t.Fatalf("%s.%s %s: %s %+v", contract, operation, arguments, response.Outcome, response.Error)
	}
	if err := json.Unmarshal(response.Result, into); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fails(contract, operation, arguments string, outcome collab.Outcome) collab.Error {
	h.t.Helper()
	response := h.call(contract, operation, arguments)
	if response.Outcome != outcome {
		h.t.Fatalf("%s.%s %s: outcome %s, want %s (%+v)", contract, operation, arguments, response.Outcome, outcome, response.Error)
	}
	return *response.Error
}

func refJSON(ref collab.Ref) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

func TestTasks_CreateReadFindAndPage(t *testing.T) {
	h := newHarness(t, "")
	var empty collab.TaskListResult
	h.ok("tasks", "find", `{}`, &empty)
	if empty.Items == nil || len(empty.Items) != 0 || empty.Page.Next != "" {
		t.Fatalf("an empty store: %+v", empty)
	}
	var first collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"Write the provider guide","labels":["docs"],"idempotencyKey":"k1"}`, &first)
	if !first.Created || first.Task.Ref.ID != "T-1" || first.Task.Revision != "r1" || first.Task.State != collab.TaskStateOpen ||
		!strings.HasPrefix(first.Task.Ref.Source, "local:") || first.Task.UpdatedAt != "2026-09-24T08:00:00Z" {
		t.Fatalf("created %+v", first)
	}
	for i := 2; i <= 5; i++ {
		var created collab.TaskCreateResult
		h.ok("tasks", "create", `{"title":"task `+strconv.Itoa(i)+`","state":"blocked","idempotencyKey":"k`+strconv.Itoa(i)+`"}`, &created)
	}
	var got collab.TaskResult
	h.ok("tasks", "get", `{"ref":`+refJSON(first.Task.Ref)+`}`, &got)
	if got.Task.Title != "Write the provider guide" {
		t.Fatalf("get %+v", got)
	}

	var ids []string
	cursor := ""
	for {
		arguments := `{"page":{"size":2}}`
		if cursor != "" {
			arguments = `{"page":{"size":2,"cursor":"` + cursor + `"}}`
		}
		var page collab.TaskListResult
		h.ok("tasks", "find", arguments, &page)
		if len(page.Items) > 2 {
			t.Fatalf("page of %d", len(page.Items))
		}
		for _, task := range page.Items {
			ids = append(ids, task.Ref.ID)
		}
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	if strings.Join(ids, ",") != "T-1,T-2,T-3,T-4,T-5" {
		t.Fatalf("pages yielded %v", ids)
	}
	var filtered collab.TaskListResult
	h.ok("tasks", "find", `{"states":["open"],"labels":["docs"],"query":"GUIDE"}`, &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].Ref.ID != "T-1" {
		t.Fatalf("filtered %+v", filtered)
	}
	h.ok("tasks", "find", `{"labels":["missing"]}`, &filtered)
	if len(filtered.Items) != 0 {
		t.Fatalf("a label nobody carries matched %+v", filtered)
	}
	h.fails("tasks", "find", `{"page":{"cursor":"x"}}`, collab.OutcomeInvalid)
}

func TestTasks_IdempotencyReplaysAndRefusesReuse(t *testing.T) {
	h := newHarness(t, "")
	var first, replay collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"once","idempotencyKey":"retry-me"}`, &first)
	h.ok("tasks", "create", `{"title":"once","idempotencyKey":"retry-me"}`, &replay)
	if replay.Created || replay.Task.Ref != first.Task.Ref {
		t.Fatalf("a retried create made a second task: %+v then %+v", first, replay)
	}
	failure := h.fails("tasks", "create", `{"title":"something else","idempotencyKey":"retry-me"}`, collab.OutcomeConflict)
	if failure.Reason != collab.ReasonIdempotencyMismatch {
		t.Fatalf("reason %q", failure.Reason)
	}
	var all collab.TaskListResult
	h.ok("tasks", "find", `{}`, &all)
	if len(all.Items) != 1 {
		t.Fatalf("%d tasks after retries", len(all.Items))
	}
}

func TestTasks_ExpectedRevisionIsComparedAtomically(t *testing.T) {
	h := newHarness(t, "")
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, &created)
	ref := refJSON(created.Task.Ref)

	var updated collab.TaskResult
	h.ok("tasks", "update", `{"ref":`+ref+`,"expectedRevision":"r1","title":"renamed","body":"b","labels":["x"]}`, &updated)
	if updated.Task.Revision != "r2" || updated.Task.Title != "renamed" || updated.Task.Body != "b" {
		t.Fatalf("updated %+v", updated)
	}
	failure := h.fails("tasks", "update", `{"ref":`+ref+`,"expectedRevision":"r1","title":"stale"}`, collab.OutcomeConflict)
	if failure.Current != "r2" || failure.Retryable {
		t.Fatalf("conflict %+v", failure)
	}
	var same collab.TaskResult
	h.ok("tasks", "update", `{"ref":`+ref+`,"title":"renamed"}`, &same)
	if same.Task.Revision != "r2" {
		t.Errorf("an update that changes nothing moved the revision to %s", same.Task.Revision)
	}
	var moved collab.TaskResult
	h.ok("tasks", "transition", `{"ref":`+ref+`,"state":"done","expectedRevision":"r2"}`, &moved)
	if moved.Task.State != collab.TaskStateDone || moved.Task.Revision != "r3" {
		t.Fatalf("moved %+v", moved)
	}
	h.fails("tasks", "transition", `{"ref":`+ref+`,"state":"open","expectedRevision":"r2"}`, collab.OutcomeConflict)

	foreign := `{"source":"local:elsewhere","id":"T-1"}`
	h.fails("tasks", "get", `{"ref":`+foreign+`}`, collab.OutcomeNotFound)
	h.fails("tasks", "update", `{"ref":`+foreign+`,"title":"x"}`, collab.OutcomeNotFound)
	h.fails("tasks", "get", `{"ref":{"source":"`+created.Task.Ref.Source+`","id":"T-9"}}`, collab.OutcomeNotFound)
}

// TestTasks_ConcurrentCreatesAndUpdatesNeverLoseAWrite races writers on one
// store: every create gets its own identifier, and of concurrent updates
// against one revision exactly one wins.
func TestTasks_ConcurrentCreatesAndUpdatesNeverLoseAWrite(t *testing.T) {
	h := newHarness(t, "")
	const writers = 12
	var wg sync.WaitGroup
	responses := make([]collab.Response, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i] = h.call("tasks", "create", `{"title":"t`+strconv.Itoa(i)+`","idempotencyKey":"c`+strconv.Itoa(i)+`"}`)
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, response := range responses {
		var created collab.TaskCreateResult
		if response.Outcome != collab.OutcomeOK || json.Unmarshal(response.Result, &created) != nil {
			t.Fatalf("a concurrent create failed: %+v", response)
		}
		if seen[created.Task.Ref.ID] {
			t.Fatalf("two creates got %s", created.Task.Ref.ID)
		}
		seen[created.Task.Ref.ID] = true
	}

	var target collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"contended","idempotencyKey":"target"}`, &target)
	ref := refJSON(target.Task.Ref)
	outcomes := make([]collab.Outcome, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = h.call("tasks", "update", `{"ref":`+ref+`,"expectedRevision":"r1","title":"winner `+strconv.Itoa(i)+`"}`).Outcome
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, outcome := range outcomes {
		switch outcome {
		case collab.OutcomeOK:
			wins++
		case collab.OutcomeConflict:
		default:
			t.Fatalf("unexpected outcome %s", outcome)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent updates against one revision succeeded; exactly one may", wins)
	}
}

func TestProposals_OneOpenProposalPerExactIdentity(t *testing.T) {
	h := newHarness(t, `{"repository":"acme/app"}`)
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic","headCommit":"abcdef1"},"title":"first","draft":true}`, &created)
	if !created.Created || created.Proposal.Change.Repository != "acme/app" || created.Proposal.State != collab.ProposalStateDraft {
		t.Fatalf("created %+v", created)
	}
	var updated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"repository":"acme/app","base":"main","head":"topic","headCommit":"1234567"},"title":"second","draft":false,"expectedRevision":"r1"}`, &updated)
	if updated.Created || updated.Proposal.Ref != created.Proposal.Ref || updated.Proposal.Revision != "r2" ||
		updated.Proposal.State != collab.ProposalStateOpen || updated.Proposal.Change.HeadCommit != "1234567" {
		t.Fatalf("updated %+v", updated)
	}
	var repeated collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"second"}`, &repeated)
	if repeated.Created || repeated.Proposal.Revision != "r2" {
		t.Fatalf("a repeated upsert: %+v", repeated)
	}
	failure := h.fails("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"stale","expectedRevision":"r1"}`, collab.OutcomeConflict)
	if failure.Current != "r2" {
		t.Errorf("conflict %+v", failure)
	}
	h.fails("proposals", "upsert", `{"change":{"base":"main","head":"other"},"title":"x","expectedRevision":"r1"}`, collab.OutcomeConflict)

	var other collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"repository":"acme/lib","base":"main","head":"topic"},"title":"elsewhere"}`, &other)
	if !other.Created || other.Proposal.Ref == created.Proposal.Ref {
		t.Fatalf("another repository shares a proposal: %+v", other)
	}
	var found collab.ProposalListResult
	h.ok("proposals", "find", `{"change":{"base":"main","head":"topic"}}`, &found)
	if len(found.Items) != 1 || found.Items[0].Ref != created.Proposal.Ref {
		t.Fatalf("find matched %+v", found)
	}
	h.ok("proposals", "find", `{"change":{"repository":"acme/app","base":"main","head":"topic"},"states":["merged"]}`, &found)
	if len(found.Items) != 0 {
		t.Fatalf("a state filter matched %+v", found)
	}
}

func TestProposals_ConcurrentUpsertsCreateOne(t *testing.T) {
	h := newHarness(t, "")
	const writers = 10
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.call("proposals", "upsert", `{"change":{"base":"main","head":"race"},"title":"t"}`)
		}()
	}
	wg.Wait()
	var found collab.ProposalListResult
	h.ok("proposals", "find", `{"change":{"base":"main","head":"race"}}`, &found)
	if len(found.Items) != 1 {
		t.Fatalf("%d proposals for one identity", len(found.Items))
	}
}

func TestProposals_StatusDisclosesNoHostedChecksAndReviewsReplay(t *testing.T) {
	h := newHarness(t, "")
	var created collab.ProposalUpsertResult
	h.ok("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"t"}`, &created)
	ref := refJSON(created.Proposal.Ref)
	var review, replay collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"approve","body":"ok","commit":"abcdef1","idempotencyKey":"rv1"}`, &review)
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"approve","body":"ok","commit":"abcdef1","idempotencyKey":"rv1"}`, &replay)
	if !review.Created || replay.Created || replay.Review.Ref != review.Review.Ref {
		t.Fatalf("review %+v, replay %+v", review, replay)
	}
	h.fails("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"other","idempotencyKey":"rv1"}`, collab.OutcomeConflict)
	h.fails("proposals", "review", `{"ref":{"source":"local:elsewhere","id":"P-1"},"verdict":"comment","body":"x","idempotencyKey":"rv2"}`, collab.OutcomeNotFound)

	var status collab.ProposalStatusResult
	h.ok("proposals", "status", `{"ref":`+ref+`}`, &status)
	if status.Checks.State != collab.ChecksStateUnsupported || !strings.Contains(status.Checks.Detail, "no hosted checks") {
		t.Fatalf("checks %+v", status.Checks)
	}
	if len(status.Reviews) != 1 || status.Reviews[0].Verdict != collab.ReviewVerdictApprove {
		t.Fatalf("reviews %+v", status.Reviews)
	}
	h.fails("proposals", "status", `{"ref":{"source":"local:elsewhere","id":"P-1"}}`, collab.OutcomeNotFound)
	if response := h.call("proposals", "merge", `{"ref":`+ref+`,"expectedHeadCommit":"abcdef1"}`); response.Outcome != collab.OutcomeUnsupported {
		t.Fatalf("merge answered %s; this provider offers no merge", response.Outcome)
	}
}

// TestProposals_EveryAnswerReportsNoLabelsAndNoAssignees: the store keeps
// neither, so every answer about a proposal says it has none ([]), never
// that the provider does not report them (absent), and nothing is stored.
func TestProposals_EveryAnswerReportsNoLabelsAndNoAssignees(t *testing.T) {
	h := newHarness(t, "")
	created := h.call("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"t"}`).Result
	var upserted collab.ProposalUpsertResult
	if err := json.Unmarshal(created, &upserted); err != nil {
		t.Fatal(err)
	}
	raw := map[string]json.RawMessage{
		"upsert": created,
		"repeat": h.call("proposals", "upsert", `{"change":{"base":"main","head":"topic"},"title":"t"}`).Result,
		"status": h.call("proposals", "status", `{"ref":`+refJSON(upserted.Proposal.Ref)+`}`).Result,
		"find":   h.call("proposals", "find", `{"change":{"base":"main","head":"topic"}}`).Result,
	}
	for name, result := range raw {
		if !strings.Contains(string(result), `"labels":[]`) || !strings.Contains(string(result), `"assignees":[]`) {
			t.Errorf("%s answered %s", name, result)
		}
	}
	stored, err := os.ReadFile(filepath.Join(h.workspace, filepath.FromSlash(DefaultRoot), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), `"labels"`) || strings.Contains(string(stored), `"assignees"`) {
		t.Fatalf("the store kept labels or assignees: %s", stored)
	}
}

func TestSettings(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "store")
	h := newHarness(t, `{"root":`+strconv.Quote(absolute)+`}`)
	var created collab.TaskCreateResult
	h.ok("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, &created)
	if _, err := os.Stat(filepath.Join(absolute, "state.json")); err != nil {
		t.Fatalf("the absolute root was not used: %v", err)
	}
	relative := newHarness(t, `{"root":"custom/store"}`)
	relative.ok("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, &created)
	if _, err := os.Stat(filepath.Join(relative.workspace, "custom", "store", "state.json")); err != nil {
		t.Fatalf("the relative root was not resolved against the workspace: %v", err)
	}
	defaults := newHarness(t, "")
	defaults.ok("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, &created)
	if _, err := os.Stat(filepath.Join(defaults.workspace, filepath.FromSlash(DefaultRoot), "state.json")); err != nil {
		t.Fatalf("the default root was not used: %v", err)
	}
	unknown := newHarness(t, `{"token":"x"}`)
	if failure := unknown.fails("tasks", "find", `{}`, collab.OutcomeInvalid); failure.Reason != "settings.invalid" {
		t.Fatalf("an unknown setting: %+v", failure)
	}
	rootless := newHarness(t, "")
	rootless.workspace = ""
	rootless.fails("tasks", "find", `{}`, collab.OutcomeInvalid)
}

func TestAStoreThatCannotBeWrittenIsUnavailable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, `{"root":`+strconv.Quote(filepath.Join(file, "store"))+`}`)
	if failure := h.fails("tasks", "create", `{"title":"t","idempotencyKey":"k"}`, collab.OutcomeUnavailable); failure.Reason != "store.unwritable" {
		t.Fatalf("failure %+v", failure)
	}
	broken := newHarness(t, "")
	root := filepath.Join(broken.workspace, filepath.FromSlash(DefaultRoot))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if failure := broken.fails("tasks", "find", `{}`, collab.OutcomeUnavailable); failure.Reason != "store.unreadable" {
		t.Fatalf("failure %+v", failure)
	}
}

func TestHelpers(t *testing.T) {
	if nextRevision("garbage") != "r1" || nextRevision("r9") != "r10" {
		t.Error("nextRevision")
	}
	if ordinal("T") != 0 || ordinal("T-x") != 0 || ordinal("P-12") != 12 {
		t.Error("ordinal")
	}
	if New().timestamp() == "" {
		t.Error("timestamp")
	}
	if (&refused{failure: collab.Fail(collab.OutcomeConflict, "x", "message")}).Error() != "message" {
		t.Error("refused")
	}
}
