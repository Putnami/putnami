package provider

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// taskSession is one tasks call.
type taskSession struct {
	*session
	settings TaskSettings
	states   stateMap
}

func (p *Provider) taskHandler(run func(*taskSession, any) (any, *collab.Failure)) collab.Handler {
	return func(ctx context.Context, call collab.Call) (any, *collab.Failure) {
		var settings TaskSettings
		if failure := decodeSettings(call.Settings, &settings, taskSettingNames); failure != nil {
			return nil, failure
		}
		if settings.KeyScanPages < 0 || settings.KeyScanPages > 10 {
			return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.keyScanPages is between 1 and 10")
		}
		if settings.KeyScanPages == 0 {
			settings.KeyScanPages = 3
		}
		states, failure := parseStates(settings)
		if failure != nil {
			return nil, failure
		}
		ctx, cancel := p.budget(ctx)
		defer cancel()
		s, failure := p.open(ctx, settings)
		if failure != nil {
			return nil, failure
		}
		return run(&taskSession{session: s, settings: settings, states: states}, call.Input)
	}
}

// task renders an issue as a task.
func (s *taskSession) task(issue *glIssue) collab.Task {
	body, _ := splitMarker(deref(issue.Description))
	body = unescapeBody(body)
	state, providerState := s.states.stateOf(issue)
	key := "#" + strconv.Itoa(issue.IID)
	task := collab.Task{
		Ref:           s.project.ref(issue.IID),
		Key:           key,
		Revision:      issueRevision(issue),
		URL:           collabhelper.DisplayURL(issue.WebURL, collab.MaxURLLength),
		Title:         collabhelper.OneLine(issue.Title, collab.MaxTitleLength, key),
		Body:          collabhelper.BoundedBody(body, collab.MaxBodyBytes),
		State:         state,
		ProviderState: collabhelper.OneLine(providerState, collab.MaxLabelLength, string(state)),
		CreatedAt:     collabhelper.Timestamp(issue.CreatedAt),
		UpdatedAt:     collabhelper.Timestamp(issue.UpdatedAt),
	}
	task.Labels = collabhelper.BoundedLabels(s.states.visibleLabels(issue.labelNames()), collab.MaxLabels, collab.MaxLabelLength)
	task.Assignees = boundedLogins(issue.assigneeLogins())
	return task
}

// issueRevision covers everything a task shows and a caller can change:
// title, the whole description, state and its duplicate link, labels and
// assignees.
func issueRevision(issue *glIssue) string {
	labels := issue.labelNames()
	sort.Strings(labels)
	assignees := issue.assigneeLogins()
	sort.Strings(assignees)
	return revisionOf([]any{issue.Title, deref(issue.Description), issue.State, issue.duplicate(), labels, assignees})
}

// issue reads the issue a reference names. A reference another source issued
// is not found.
func (s *taskSession) issue(ref collab.Ref) (*glIssue, *collab.Failure) {
	iid, ours := s.project.iid(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, "task.missing", "%s", s.project.missing(ref))
	}
	return s.read(iid)
}

func (s *taskSession) read(iid int) (*glIssue, *collab.Failure) {
	var issue glIssue
	if _, err := s.client.Get(s.ctx, s.project.apiPath("issues", strconv.Itoa(iid)), nil, &issue); err != nil {
		return nil, fail(err, "read issue #%d of %s", iid, s.project.path)
	}
	return &issue, nil
}

func (s *taskSession) get(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskRefInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(issue)}, nil
}

// find lists the project's issues oldest first. GitLab filters on its state,
// the labels and the text; the provider then keeps the requested semantic
// states.
func (s *taskSession) find(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskFindInput](input)
	if failure != nil {
		return nil, failure
	}
	size := collab.DefaultPageSize
	cursor := ""
	if in.Page != nil {
		if in.Page.Size > 0 {
			size = in.Page.Size
		}
		cursor = in.Page.Cursor
	}
	gitlabState := "all"
	switch {
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, closedState):
		gitlabState = "opened"
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, func(state collab.TaskState) bool { return !closedState(state) }):
		gitlabState = "closed"
	}
	keep := func(issue *glIssue) bool {
		state, _ := s.states.stateOf(issue)
		if len(in.States) > 0 && !slices.Contains(in.States, state) {
			return false
		}
		names := issue.labelNames()
		for _, label := range in.Labels {
			if !slices.ContainsFunc(names, func(name string) bool { return strings.EqualFold(name, label) }) {
				return false
			}
		}
		return true
	}
	issues, next, failure := collect(s.listIssues(gitlabState, in.Labels, in.Query), keep, size, cursor)
	if failure != nil {
		return nil, failure
	}
	result := &collab.TaskListResult{Items: make([]collab.Task, 0, len(issues)), Page: collab.Page{Next: next}}
	for i := range issues {
		result.Items = append(result.Items, s.task(&issues[i]))
	}
	return result, nil
}

// listIssues reads a page of the project's issue list, oldest first, from a
// creation millisecond when from is not negative. A label that
// carries a comma, or that GitLab's filter reads as a keyword, cannot be named
// in its label filter: the provider then filters on every label itself.
func (s *taskSession) listIssues(state string, labels []string, text string) fetchPage {
	return func(from int64, page int) ([]glIssue, bool, *collab.Failure) {
		query := url.Values{
			"state": {state}, "order_by": {"created_at"}, "sort": {"asc"},
			"per_page": {strconv.Itoa(gitlabPageSize)}, "page": {strconv.Itoa(page)},
		}
		if from >= 0 {
			query.Set("created_after", createdAfter(from))
		}
		if len(labels) > 0 && !slices.ContainsFunc(labels, unfilterable) {
			query.Set("labels", strings.Join(labels, ","))
		}
		if text != "" {
			query.Set("search", text)
			query.Set("in", "title,description")
		}
		var issues []glIssue
		header, err := s.client.Get(s.ctx, s.project.apiPath("issues"), query, &issues)
		if err != nil {
			return nil, false, fail(err, "list the issues of %s", s.project.path)
		}
		return issues, gitlab.HasNext(header), nil
	}
}

// unfilterable reports whether GitLab's label filter cannot name a label: a
// comma separates labels there, and None and Any mean "no label" and "any
// label".
func unfilterable(label string) bool {
	return strings.Contains(label, ",") || strings.EqualFold(label, "None") || strings.EqualFold(label, "Any")
}

// quickActionEscape goes before a body line that starts with a slash, so
// GitLab reads the line as text and runs no quick action from it.
const quickActionEscape = "\u200b"

// escapeBody escapes every line outside fenced code that starts with a slash,
// or with the escape itself, so unescapeBody restores the body exactly.
// GitLab runs no quick action inside fenced code, which stays as written.
func escapeBody(body string) string {
	return mapBodyLines(body, func(line string) string {
		if strings.HasPrefix(line, "/") || strings.HasPrefix(line, quickActionEscape) {
			return quickActionEscape + line
		}
		return line
	})
}

// unescapeBody removes the one escape escapeBody put before a line.
func unescapeBody(body string) string {
	return mapBodyLines(body, func(line string) string {
		rest, escaped := strings.CutPrefix(line, quickActionEscape)
		if escaped && (strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, quickActionEscape)) {
			return rest
		}
		return line
	})
}

// mapBodyLines applies change to every line of a body outside fenced code. A
// fence opens on a line that starts, after at most three spaces, with three
// backticks or tildes, and closes on the next such line of the same
// character; neither escaping nor unescaping changes a fence line.
func mapBodyLines(body string, change func(string) string) string {
	lines := strings.Split(body, "\n")
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			switch {
			case fence == "":
				fence = trimmed[:3]
			case strings.HasPrefix(trimmed, fence):
				fence = ""
			}
			continue
		}
		if fence == "" {
			lines[i] = change(line)
		}
	}
	return strings.Join(lines, "\n")
}

// createRequest is what a create's idempotency digest covers.
type createRequest struct {
	Title  string           `json:"title"`
	Body   string           `json:"body"`
	Labels []string         `json:"labels"`
	State  collab.TaskState `json:"state"`
}

// create opens an issue whose description records the idempotency key.
// Before opening one it looks for the key (findKey); after a write whose
// outcome is unknown it looks again, and answers unresolved rather than
// opening a second issue.
func (s *taskSession) create(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskCreateInput](input)
	if failure != nil {
		return nil, failure
	}
	state := in.State
	if state == "" {
		state = collab.TaskStateOpen
	}
	if failure := s.states.refuseLabels(in.Labels); failure != nil {
		return nil, failure
	}
	// An opened issue without a state label reads as open, so a create
	// applies a state label only for in_progress and blocked, which need
	// one; done and canceled are reached by closing the issue afterwards.
	labels := slices.Clone(in.Labels)
	switch state {
	case collab.TaskStateInProgress, collab.TaskStateBlocked:
		if s.states.primary(state) == "" {
			return nil, unmapped(state)
		}
		labels = append(labels, s.states.primary(state))
	case collab.TaskStateCanceled:
		if s.states.primary(state) == "" {
			return nil, unmapped(state)
		}
	}
	marker := newIdempotency("tasks.create", in.IdempotencyKey, createRequest{in.Title, in.Body, sortedLabels(in.Labels), state})

	existing, failure := s.findKey(marker, true)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		return s.replay(existing, marker, in.IdempotencyKey)
	}

	request := map[string]any{"title": in.Title, "description": withMarker(escapeBody(in.Body), marker)}
	if len(labels) > 0 {
		request["labels"] = strings.Join(labels, ",")
	}
	var created glIssue
	err := s.client.Write(s.ctx, http.MethodPost, s.project.apiPath("issues"), request, &created)
	if err == nil && created.IID <= 0 {
		err = &gitlab.Error{Kind: gitlab.Uncertain, Status: http.StatusCreated, Message: "GitLab acknowledged the issue without its iid"}
	}
	if err != nil {
		if err.Kind != gitlab.Uncertain {
			return nil, fail(err, "open an issue in %s", s.project.path)
		}
		var found *glIssue
		settled := s.reconcile(func() (bool, *collab.Failure) {
			issue, failure := s.findKey(marker, false)
			found = issue
			return issue != nil, failure
		})
		if !settled {
			return nil, unresolved("repeat tasks.create with the same idempotencyKey: the key is recorded in the issue description, so the repeat returns the issue if it was opened",
				"opening the issue in %s may have happened (%s), and reading %s back did not find it", s.project.path, err.Message, s.project.path)
		}
		created = *found
	}
	if closedState(state) {
		closed, failure := s.close(&created, state)
		if failure != nil {
			return nil, failure
		}
		created = *closed
	}
	return &collab.TaskCreateResult{Task: s.task(&created), Created: true}, nil
}

func sortedLabels(labels []string) []string {
	sorted := slices.Clone(labels)
	if sorted == nil {
		sorted = []string{}
	}
	sort.Strings(sorted)
	return sorted
}

// replay answers a create whose key an issue already carries.
func (s *taskSession) replay(existing *glIssue, marker idempotency, key string) (any, *collab.Failure) {
	_, recorded := splitMarker(deref(existing.Description))
	if recorded == nil || recorded.digest != marker.digest {
		return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
			"idempotency key %q already opened #%d of %s for a different request", key, existing.IID, s.project.path)
	}
	return &collab.TaskCreateResult{Task: s.task(existing), Created: false}, nil
}

// close finishes a create that asked for a closed state. The issue is open
// already: when the close fails, the answer is unresolved with reason
// gitlab.incomplete and names the issue; when its outcome is unknown, the
// issue is read back first.
func (s *taskSession) close(issue *glIssue, state collab.TaskState) (*glIssue, *collab.Failure) {
	goal, failure := s.states.targetOf(issue, state)
	if failure != nil {
		return nil, failure
	}
	reconcile := "read #" + strconv.Itoa(issue.IID) + " with tasks.get, then transition it to " + string(state) +
		"; a repeat of tasks.create with the same idempotencyKey returns the issue as it is and does not close it"
	closed, failure := s.write(issue, map[string]any{"state_event": "close", "labels": strings.Join(goal.labels, ",")},
		"close as "+string(state), func(current *glIssue) bool {
			reached, _ := s.states.stateOf(current)
			return reached == state
		})
	if failure == nil {
		return closed, nil
	}
	if failure.Outcome == collab.OutcomeUnresolved {
		failure.Error.Reconcile = reconcile
		return nil, failure
	}
	incomplete := unresolved(reconcile, "opened #%d in %s, and closing it as %s failed, so it is open: %s",
		issue.IID, s.project.path, state, failure.Error.Message)
	incomplete.Error.Reason = reasonIncomplete
	return nil, incomplete
}

// findKey looks for the issue a create key opened among the newest issues
// the credential's account opened in the project. GitLab's lists read its
// database, so an issue is listed as soon as its create was answered. When
// deep is set, the lookup reads settings.keyScanPages pages, then asks
// GitLab's issue search, which reaches older issues; otherwise it reads the
// newest page only.
func (s *taskSession) findKey(marker idempotency, deep bool) (*glIssue, *collab.Failure) {
	me, failure := s.authenticated()
	if failure != nil {
		return nil, failure
	}
	pages := 1
	if deep {
		pages = s.settings.KeyScanPages
	}
	for page := 1; page <= pages; page++ {
		query := url.Values{
			"author_id": {strconv.FormatInt(me.ID, 10)}, "state": {"all"}, "order_by": {"created_at"}, "sort": {"desc"},
			"per_page": {strconv.Itoa(gitlabPageSize)}, "page": {strconv.Itoa(page)},
		}
		issues, more, failure := s.keyedList(query)
		if failure != nil {
			return nil, failure
		}
		if issue := carrying(issues, marker, me); issue != nil {
			return issue, nil
		}
		if !more {
			return nil, nil
		}
	}
	if !deep {
		return nil, nil
	}
	query := url.Values{
		"author_id": {strconv.FormatInt(me.ID, 10)}, "state": {"all"}, "search": {marker.searchWord()}, "in": {"description"},
		"per_page": {"20"},
	}
	issues, _, failure := s.keyedList(query)
	if failure != nil {
		return nil, failure
	}
	return carrying(issues, marker, me), nil
}

func (s *taskSession) keyedList(query url.Values) ([]glIssue, bool, *collab.Failure) {
	var issues []glIssue
	header, err := s.client.Get(s.ctx, s.project.apiPath("issues"), query, &issues)
	if err != nil {
		failure := fail(err, "look for idempotency key among the issues of %s", s.project.path)
		if failure.Outcome == collab.OutcomeUnavailable {
			failure.Error.Reason = reasonKeyUnverifiable
		}
		return nil, false, failure
	}
	return issues, gitlab.HasNext(header), nil
}

// carrying returns the first issue the account opened whose description
// carries a marker's key. An issue someone else opened never answers for a
// key, even when it copies the marker.
func carrying(issues []glIssue, marker idempotency, me *glUser) *glIssue {
	for i := range issues {
		issue := &issues[i]
		if issue.Author == nil || issue.Author.ID != me.ID {
			continue
		}
		if _, recorded := splitMarker(deref(issue.Description)); recorded != nil && recorded.key == marker.key {
			return issue
		}
	}
	return nil
}

// write applies a patch to an issue and returns the issue as GitLab answers
// it. When the outcome is unknown, it reads the issue back and accepts it
// when applied reports that the patch is in place.
func (s *taskSession) write(issue *glIssue, patch map[string]any, operation string, applied func(*glIssue) bool) (*glIssue, *collab.Failure) {
	path := s.project.apiPath("issues", strconv.Itoa(issue.IID))
	var updated glIssue
	err := s.client.Write(s.ctx, http.MethodPut, path, patch, &updated)
	if err == nil {
		return &updated, nil
	}
	if err.Kind != gitlab.Uncertain {
		return nil, fail(err, "%s #%d of %s", operation, issue.IID, s.project.path)
	}
	var current glIssue
	if s.reconcile(func() (bool, *collab.Failure) {
		if _, err := s.client.Get(s.ctx, path, nil, &current); err != nil {
			return false, fail(err, "read #%d back", issue.IID)
		}
		return applied(&current), nil
	}) {
		return &current, nil
	}
	return nil, unresolved("read the task with tasks.get and compare it with the request before any retry",
		"the %s of #%d in %s may have happened (%s), and reading it back did not show it", operation, issue.IID, s.project.path, err.Message)
}

func (s *taskSession) checkRevision(issue *glIssue, expected string) *collab.Failure {
	if current := issueRevision(issue); expected != "" && current != expected {
		return conflictAt(current, collab.ReasonRevisionConflict, "task #%d of %s is at revision %s, not %s", issue.IID, s.project.path, current, expected)
	}
	return nil
}

// update changes title, description or labels. The description keeps its
// idempotency marker, and labels replace the task's labels while its state
// labels stay. expectedRevision is compared with the issue read just before
// the write: GitLab has no conditional issue write, so a writer between the
// two is not detected.
func (s *taskSession) update(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskUpdateInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	if failure := s.checkRevision(issue, in.ExpectedRevision); failure != nil {
		return nil, failure
	}
	patch := map[string]any{}
	title, body, labels := issue.Title, deref(issue.Description), issue.labelNames()
	if in.Title != nil && *in.Title != issue.Title {
		title = *in.Title
		patch["title"] = title
	}
	if in.Body != nil {
		if joined := rejoin(deref(issue.Description), escapeBody(*in.Body)); joined != deref(issue.Description) {
			body = joined
			patch["description"] = body
		}
	}
	if in.Labels != nil {
		if failure := s.states.refuseLabels(*in.Labels); failure != nil {
			return nil, failure
		}
		wanted := append(slices.Clone(*in.Labels), s.states.stateLabels(issue.labelNames())...)
		if !sameLabels(wanted, issue.labelNames()) {
			labels = wanted
			patch["labels"] = strings.Join(labels, ",")
		}
	}
	if len(patch) == 0 {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	updated, failure := s.write(issue, patch, "update", func(current *glIssue) bool {
		return current.Title == title && deref(current.Description) == body && sameLabels(current.labelNames(), labels)
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// transition moves an issue to a semantic state: the state labels are
// swapped, and the issue is closed for done and canceled and reopened
// otherwise, in one request. A task already in the state is left as it is,
// except an opened issue that reads as the state through a label other than
// the state's first: it gets the first label.
func (s *taskSession) transition(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskTransitionInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	if failure := s.checkRevision(issue, in.ExpectedRevision); failure != nil {
		return nil, failure
	}
	if s.states.settled(issue, in.State) {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	goal, failure := s.states.targetOf(issue, in.State)
	if failure != nil {
		return nil, failure
	}
	reopened := in.State == collab.TaskStateDone && issue.duplicate()
	if reopened {
		// A duplicate link keeps a closed issue canceled; reopening the issue
		// clears it, and the write below closes it again.
		if issue, failure = s.write(issue, map[string]any{"state_event": "reopen"}, "reopen", func(current *glIssue) bool {
			return !current.closed()
		}); failure != nil {
			return nil, failure
		}
	}
	patch := map[string]any{"labels": strings.Join(goal.labels, ",")}
	switch {
	case goal.closed && !issue.closed():
		patch["state_event"] = "close"
	case !goal.closed && issue.closed():
		patch["state_event"] = "reopen"
	}
	updated, failure := s.write(issue, patch, "transition to "+string(in.State), func(current *glIssue) bool {
		return s.states.settled(current, in.State)
	})
	if failure != nil && reopened && failure.Outcome != collab.OutcomeUnresolved {
		incomplete := unresolved("read the task with tasks.get, then transition it to done",
			"reopened #%d of %s to clear its duplicate link, and closing it failed, so it is open: %s", issue.IID, s.project.path, failure.Error.Message)
		incomplete.Error.Reason = reasonIncomplete
		return nil, incomplete
	}
	if failure != nil {
		return nil, failure
	}
	if !s.states.settled(updated, in.State) {
		state, providerState := s.states.stateOf(updated)
		return nil, unresolved("read the task with tasks.get before any retry",
			"GitLab accepted the transition of #%d to %s and reports it as %s", issue.IID, in.State, describeState(state, providerState))
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// assign replaces an issue's assignees, named by username. "@me" names the
// account of the credential. A username GitLab does not know is invalid and
// nothing is written; GitLab keeps only the users it can assign, and the
// answer lists the assignees the issue actually has. An assignee is
// information, never an exclusive claim.
func (s *taskSession) assign(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskAssignInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	if failure := s.checkRevision(issue, in.ExpectedRevision); failure != nil {
		return nil, failure
	}
	usernames := make([]string, 0, len(in.Assignees))
	ids := make([]int64, 0, len(in.Assignees))
	for _, name := range in.Assignees {
		user, failure := s.user(name)
		if failure != nil {
			return nil, failure
		}
		if slices.Contains(ids, user.ID) {
			continue
		}
		usernames = append(usernames, user.Username)
		ids = append(ids, user.ID)
	}
	if sameLabels(usernames, issue.assigneeLogins()) {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	sent := ids
	if len(sent) == 0 {
		// GitLab reads 0 as "no assignee".
		sent = []int64{0}
	}
	updated, failure := s.write(issue, map[string]any{"assignee_ids": sent}, "assignment", func(current *glIssue) bool {
		return sameLabels(current.assigneeLogins(), usernames)
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// user resolves a username, or "@me", to a GitLab user.
func (s *taskSession) user(name string) (*glUser, *collab.Failure) {
	if name == "@me" {
		return s.authenticated()
	}
	var users []glUser
	if _, err := s.client.Get(s.ctx, "/users", url.Values{"username": {strings.TrimPrefix(name, "@")}}, &users); err != nil {
		return nil, fail(err, "look up GitLab user %s", name)
	}
	for i := range users {
		if strings.EqualFold(users[i].Username, strings.TrimPrefix(name, "@")) && users[i].ID > 0 {
			return &users[i], nil
		}
	}
	return nil, collab.Fail(collab.OutcomeInvalid, reasonUnknownUser, "GitLab knows no user %s on %s; nothing was assigned", name, s.project.host)
}
