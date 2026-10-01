package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
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
		s, failure := p.open(ctx, settings.repositorySettings)
		if failure != nil {
			return nil, failure
		}
		return run(&taskSession{session: s, settings: settings, states: states}, call.Input)
	}
}

// task renders an issue as a task. Parent is filled only by the operations
// that read it.
func (s *taskSession) task(issue *ghIssue) collab.Task {
	body, _ := splitMarker(deref(issue.Body))
	state, providerState := s.states.stateOf(issue)
	task := collab.Task{
		Ref:           s.repo.ref(issue.Number),
		Revision:      issueRevision(issue),
		URL:           displayURL(issue.HTMLURL),
		Title:         oneLine(issue.Title, collab.MaxTitleLength, "#"+strconv.Itoa(issue.Number)),
		Body:          boundedBody(body),
		State:         state,
		ProviderState: oneLine(providerState, collab.MaxLabelLength, string(state)),
		UpdatedAt:     timestamp(issue.UpdatedAt),
	}
	task.Labels = boundedLabels(s.states.visibleLabels(issue.labelNames()))
	task.Assignees = boundedLogins(issue.assigneeLogins())
	return task
}

// issueRevision covers everything a task shows and a caller can change:
// title, the whole body, state and its reason, labels and assignees.
func issueRevision(issue *ghIssue) string {
	labels := issue.labelNames()
	sort.Strings(labels)
	assignees := issue.assigneeLogins()
	sort.Strings(assignees)
	return revisionOf([]any{issue.Title, deref(issue.Body), issue.State, deref(issue.StateReason), labels, assignees})
}

// issue reads the issue a reference names. A reference another source issued,
// a number that is not an issue and a pull request are all not found.
func (s *taskSession) issue(ref collab.Ref) (*ghIssue, *collab.Failure) {
	number, ours := s.repo.number(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, "task.missing", "no task %s in source %s", ref.ID, ref.Source)
	}
	var issue ghIssue
	if _, err := s.client.Get(s.ctx, s.repo.path("issues", strconv.Itoa(number)), nil, &issue); err != nil {
		return nil, fail(err, "read issue #%d of %s", number, s.repo.display)
	}
	if issue.isPullRequest() {
		return nil, collab.Fail(collab.OutcomeNotFound, reasonPullRequest, "#%d of %s is a pull request, not a task", number, s.repo.display)
	}
	return &issue, nil
}

// parent reads the parent of an issue; nil when it has none.
func (s *taskSession) parent(number int) (*collab.Ref, *collab.Failure) {
	var parent ghIssue
	if _, err := s.client.Get(s.ctx, s.repo.path("issues", strconv.Itoa(number), "parent"), nil, &parent); err != nil {
		if err.Kind == github.Answered && err.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, fail(err, "read the parent of issue #%d", number)
	}
	if !s.repo.sameRepository(parent.RepositoryURL) {
		// A parent in another repository is issued by another source.
		owner, name, ok := repositoryOf(parent.RepositoryURL)
		if !ok {
			return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "GitHub named a parent of #%d outside any repository", number)
		}
		other, failure := parseRepository(owner+"/"+name, s.repo.host)
		if failure != nil {
			return nil, failure
		}
		ref := other.ref(parent.Number)
		return &ref, nil
	}
	ref := s.repo.ref(parent.Number)
	return &ref, nil
}

// repositoryOf reads owner and name from a repository API URL.
func repositoryOf(apiURL string) (string, string, bool) {
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "repos" {
			return parts[i+1], parts[i+2], true
		}
	}
	return "", "", false
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
	task := s.task(issue)
	if task.Parent, failure = s.parent(issue.Number); failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: task}, nil
}

// find lists issues oldest first. Without a query it reads the repository's
// issue list; with one it reads GitHub's issue search. Both show a new issue
// only seconds after its create was answered.
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
	githubState := "all"
	switch {
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, closedState):
		githubState = "open"
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, func(state collab.TaskState) bool { return !closedState(state) }):
		githubState = "closed"
	}
	keep := func(issue ghIssue) bool {
		if issue.isPullRequest() || (issue.RepositoryURL != "" && !s.repo.sameRepository(issue.RepositoryURL)) {
			return false
		}
		state, _ := s.states.stateOf(&issue)
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
	fetch := s.listIssues(githubState, in.Labels)
	if in.Query != "" {
		fetch = s.searchIssues(in.Query, githubState, in.Labels)
	}
	issues, next, failure := collect(fetch, func(issue ghIssue) int { return issue.Number }, keep, size, cursor)
	if failure != nil {
		return nil, failure
	}
	result := &collab.TaskListResult{Items: make([]collab.Task, 0, len(issues)), Page: collab.Page{Next: next}}
	for i := range issues {
		result.Items = append(result.Items, s.task(&issues[i]))
	}
	return result, nil
}

func closedState(state collab.TaskState) bool {
	return state == collab.TaskStateDone || state == collab.TaskStateCanceled
}

func (s *taskSession) listIssues(state string, labels []string) fetchPage[ghIssue] {
	return func(page int) ([]ghIssue, bool, *collab.Failure) {
		query := url.Values{
			"state": {state}, "sort": {"created"}, "direction": {"asc"},
			"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)},
		}
		if len(labels) > 0 && !slices.ContainsFunc(labels, func(label string) bool { return strings.Contains(label, ",") }) {
			query.Set("labels", strings.Join(labels, ","))
		}
		var issues []ghIssue
		header, err := s.client.Get(s.ctx, s.repo.path("issues"), query, &issues)
		if err != nil {
			return nil, false, fail(err, "list the issues of %s", s.repo.display)
		}
		return issues, github.HasNext(header), nil
	}
}

// maxSearchResults is how many results GitHub's search serves for one query;
// it refuses a page beyond them.
const maxSearchResults = 1000

// searchIssues pages GitHub's issue search. The caller's text cannot add a
// qualifier: every ':' in it is a plain separator. A traversal ends after the
// first maxSearchResults matches.
func (s *taskSession) searchIssues(text, state string, labels []string) fetchPage[ghIssue] {
	terms := []string{strings.ReplaceAll(text, ":", " "), "repo:" + s.repo.owner + "/" + s.repo.name, "is:issue"}
	if state != "all" {
		terms = append(terms, "is:"+state)
	}
	for _, label := range labels {
		if !strings.ContainsAny(label, `"`) {
			terms = append(terms, `label:"`+label+`"`)
		}
	}
	return s.search(strings.Join(terms, " "))
}

func (s *taskSession) search(q string) fetchPage[ghIssue] {
	return func(page int) ([]ghIssue, bool, *collab.Failure) {
		query := url.Values{
			"q": {q}, "sort": {"created"}, "order": {"asc"},
			"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)},
		}
		var result ghSearch
		header, err := s.client.Get(s.ctx, github.Path("search", "issues"), query, &result)
		if err != nil {
			return nil, false, fail(err, "search the issues of %s", s.repo.display)
		}
		return result.Items, github.HasNext(header) && page*githubPageSize < maxSearchResults, nil
	}
}

// createRequest is what a create's idempotency digest covers.
type createRequest struct {
	Title  string           `json:"title"`
	Body   string           `json:"body"`
	Labels []string         `json:"labels"`
	State  collab.TaskState `json:"state"`
}

// create opens an issue whose body records the idempotency key. Before
// opening one it looks for the key (findKey); after a write whose outcome is
// unknown it looks again, and answers unresolved rather than opening a
// second issue.
func (s *taskSession) create(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskCreateInput](input)
	if failure != nil {
		return nil, failure
	}
	state := in.State
	if state == "" {
		state = collab.TaskStateOpen
	}
	if failure := s.states.refuseStateLabels(in.Labels); failure != nil {
		return nil, failure
	}
	// An open issue without a state label reads as open, so a create applies
	// a state label only for in_progress and blocked, which need one.
	labels := slices.Clone(in.Labels)
	if state == collab.TaskStateInProgress || state == collab.TaskStateBlocked {
		primary := s.states.primary(state)
		if primary == "" {
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped,
				"no label means %s in this binding: add settings.states.%s", state, state)
		}
		labels = append(labels, primary)
	}
	marker := newIdempotency("tasks.create", in.IdempotencyKey, createRequest{in.Title, in.Body, in.Labels, state})

	existing, failure := s.findKey(marker, true)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		return s.replay(existing, marker, in.IdempotencyKey)
	}

	request := map[string]any{"title": in.Title, "body": withMarker(in.Body, marker)}
	if len(labels) > 0 {
		request["labels"] = labels
	}
	var created ghIssue
	err := s.client.Write(s.ctx, http.MethodPost, s.repo.path("issues"), request, &created)
	if err == nil && created.Number <= 0 {
		err = &github.Error{Kind: github.Uncertain, Status: http.StatusCreated, Message: "GitHub acknowledged the issue without its number"}
	}
	if err != nil {
		if err.Kind != github.Uncertain {
			return nil, fail(err, "open an issue in %s", s.repo.display)
		}
		var found *ghIssue
		settled := s.reconcile(func() (bool, *collab.Failure) {
			issue, failure := s.findKey(marker, false)
			found = issue
			return issue != nil, failure
		})
		if !settled {
			return nil, unresolved("repeat tasks.create with the same idempotencyKey: the key is recorded in the issue body, so the repeat returns the issue if it was opened",
				"opening the issue in %s may have happened (%s), and reading %s back did not find it", s.repo.display, err.Message, s.repo.display)
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

// replay answers a create whose key an issue already carries.
func (s *taskSession) replay(existing *ghIssue, marker idempotency, key string) (any, *collab.Failure) {
	_, recorded := splitMarker(deref(existing.Body))
	if recorded == nil || recorded.digest != marker.digest {
		return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
			"idempotency key %q already opened #%d of %s for a different request", key, existing.Number, s.repo.display)
	}
	return &collab.TaskCreateResult{Task: s.task(existing), Created: false}, nil
}

// close finishes a create that asked for a closed state. The issue is open
// already: when the close fails, the answer is unresolved with reason
// github.incomplete and names the issue; when its outcome is unknown, the
// issue is read back first.
func (s *taskSession) close(issue *ghIssue, state collab.TaskState) (*ghIssue, *collab.Failure) {
	goal, failure := s.states.targetOf(issue, state)
	if failure != nil {
		return nil, failure
	}
	reconcile := "read #" + strconv.Itoa(issue.Number) + " with tasks.get, then transition it to " + string(state) +
		"; a repeat of tasks.create with the same idempotencyKey returns the issue as it is and does not close it"
	closed, failure := s.write(issue, map[string]any{"state": goal.state, "state_reason": goal.stateReason, "labels": goal.labels},
		"close as "+string(state), func(current *ghIssue) bool {
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
		issue.Number, s.repo.display, state, failure.Error.Message)
	incomplete.Error.Reason = reasonIncomplete
	return nil, incomplete
}

// Bounds of the reads by number one key lookup makes.
const (
	// maxKeyReads bounds the reads by number of one key lookup. A lookup that
	// needs more cannot tell whether its key opened an issue.
	maxKeyReads = 100
	// listLagBound is how long after its create GitHub's issue lists may still
	// leave an issue out. A missing number below a listed issue opened longer
	// ago than that is not read: an issue there would be listed.
	listLagBound = 30 * time.Minute
	// notGivenRun is how many numbers in a row above the newest listed one
	// must be held by nothing before the lookup takes them as not given yet.
	// The issues endpoint answers 404 for a discussion too, so GitHub is asked
	// which of those numbers a discussion holds before any counts.
	notGivenRun = 3
)

// findKey looks for the issue a create key opened. GitHub's issue lists and
// its search show a new issue only seconds after its create was answered,
// while a read by number shows it at once. So the lookup reads the newest
// page of the repository's issues, then reads by number every issue that
// page may not show yet (unlisted). When deep is set, it then reads the most
// recent issues the account opened, and the search index, which also reaches
// older ones.
func (s *taskSession) findKey(marker idempotency, deep bool) (*ghIssue, *collab.Failure) {
	login, failure := s.authenticated()
	if failure != nil {
		return nil, failure
	}
	newest, more, served, failure := s.newestIssues(1, "")
	if failure != nil {
		return nil, failure
	}
	if issue := carrying(newest, marker, login); issue != nil {
		return issue, nil
	}
	if issue, failure := s.unlisted(newest, more, served, marker, login); issue != nil || failure != nil {
		return issue, failure
	}
	if !deep {
		return nil, nil
	}
	for page := 1; page <= s.settings.KeyScanPages; page++ {
		issues, more, _, failure := s.newestIssues(page, login)
		if failure != nil {
			return nil, failure
		}
		if issue := carrying(issues, marker, login); issue != nil {
			return issue, nil
		}
		if !more {
			return nil, nil
		}
	}
	return s.searchKey(marker, login)
}

// searchKey looks for a marker's key in GitHub's issue search, which matches
// the key as a whole word of the body of the issues the account opened.
func (s *taskSession) searchKey(marker idempotency, login string) (*ghIssue, *collab.Failure) {
	var result ghSearch
	query := url.Values{"q": {marker.key + " repo:" + s.repo.owner + "/" + s.repo.name + " author:" + login + " is:issue in:body"}, "per_page": {"20"}}
	if _, err := s.client.Get(s.ctx, github.Path("search", "issues"), query, &result); err != nil {
		failure := fail(err, "look for idempotency key in the issue search of %s", s.repo.display)
		failure.Error.Reason = reasonKeyUnverifiable
		return nil, failure
	}
	return carrying(result.Items, marker, login), nil
}

// newestIssues reads one page of the repository's issues and pull requests,
// newest first, of every account or only of creator. It reports whether a
// next page exists and GitHub's clock when it answered (zero when GitHub did
// not say).
func (s *taskSession) newestIssues(page int, creator string) ([]ghIssue, bool, time.Time, *collab.Failure) {
	query := url.Values{
		"state": {"all"}, "sort": {"created"}, "direction": {"desc"},
		"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)},
	}
	if creator != "" {
		query.Set("creator", creator)
	}
	var issues []ghIssue
	header, err := s.client.Get(s.ctx, s.repo.path("issues"), query, &issues)
	if err != nil {
		return nil, false, time.Time{}, fail(err, "look for idempotency key among the issues of %s", s.repo.display)
	}
	served, _ := http.ParseTime(header.Get("Date"))
	return issues, github.HasNext(header), served, nil
}

// unlisted reads by number every issue the newest page of the repository's
// issues may not show yet, and returns the one that carries a marker's key.
// Those are every number above the page's newest one, until notGivenRun
// numbers in a row are held by nothing — neither an issue, a pull request, a
// deleted or moved issue, nor a discussion — and every number missing below
// its newest one, down to the newest listed issue opened more than
// listLagBound before GitHub's clock (served): down to the page's oldest when
// none was, or to the first number when the page is the whole list.
func (s *taskSession) unlisted(newest []ghIssue, more bool, served time.Time, marker idempotency, login string) (*ghIssue, *collab.Failure) {
	listed := make(map[int]bool, len(newest))
	top, floor := 0, 0
	for _, issue := range newest {
		listed[issue.Number] = true
		top = max(top, issue.Number)
		if more && (floor == 0 || issue.Number < floor) {
			floor = issue.Number
		}
	}
	if !served.IsZero() {
		settled := served.Add(-listLagBound)
		for _, issue := range newest {
			if created, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil && created.Before(settled) {
				floor = max(floor, issue.Number)
			}
		}
	}
	scan := &numberScan{s: s}
	// misses counts the numbers in a row held by nothing; absent holds the
	// numbers after them that answered 404 and that GitHub was not asked
	// about yet.
	misses := 0
	var absent []int
	for number := top + 1; misses < notGivenRun; number++ {
		issue, answer, failure := scan.read(number)
		if failure != nil {
			return nil, failure
		}
		if answer != numberAbsent {
			misses, absent = 0, absent[:0]
			if issue != nil && carries(issue, marker, login) {
				return issue, nil
			}
			continue
		}
		if absent = append(absent, number); misses+len(absent) < notGivenRun {
			continue
		}
		held, failure := scan.discussions(absent)
		if failure != nil {
			return nil, failure
		}
		for _, candidate := range absent {
			misses++
			if held[candidate] {
				misses = 0
			}
		}
		absent = absent[:0]
	}
	for number := top - 1; number > floor; number-- {
		if listed[number] {
			continue
		}
		issue, _, failure := scan.read(number)
		if failure != nil {
			return nil, failure
		}
		if issue != nil && carries(issue, marker, login) {
			return issue, nil
		}
	}
	return nil, nil
}

// numberScan reads numbers for one key lookup, at most maxKeyReads, and
// counts what they hold.
type numberScan struct {
	s                                   *taskSession
	reads, unlisted, gone, moved, other int
}

// read reads one number. Past maxKeyReads it answers why the lookup cannot
// finish.
func (n *numberScan) read(number int) (*ghIssue, numberAnswer, *collab.Failure) {
	if n.reads == maxKeyReads {
		return nil, 0, n.exhausted()
	}
	n.reads++
	issue, answer, err := readNumber(n.s.ctx, n.s.client, n.s.repo, number)
	if err != nil {
		return nil, 0, fail(err, "look for idempotency key in issue #%d of %s", number, n.s.repo.display)
	}
	switch answer {
	case numberIssue:
		n.unlisted++
	case numberDeleted:
		n.gone++
	case numberMoved:
		n.moved++
	case numberAbsent:
		n.other++
	}
	return issue, answer, nil
}

// discussions asks GitHub, in one query, which of numbers a discussion holds.
// The issues endpoint answers 404 for each of them, so each is a discussion,
// an item the credential cannot read, or a number not given yet. A number is
// held by nothing only when GitHub says that no discussion holds it or that
// the repository takes no discussions; when it says neither, the lookup
// cannot finish, and nothing is created. The query counts as one read.
func (n *numberScan) discussions(numbers []int) (map[int]bool, *collab.Failure) {
	if n.reads == maxKeyReads {
		return nil, n.exhausted()
	}
	n.reads++
	s := n.s
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { hasDiscussionsEnabled")
	for _, number := range numbers {
		query.WriteString(" " + discussionAlias(number) + ": discussion(number: " + strconv.Itoa(number) + ") { number }")
	}
	query.WriteString(" } }")
	var data struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	errs, err := s.client.Query(s.ctx, query.String(), map[string]any{"owner": s.repo.owner, "name": s.repo.name}, &data)
	if err != nil {
		failure := fail(err, "ask whether discussions hold numbers %s of %s", numberList(numbers), s.repo.display)
		failure.Error.Reason = reasonKeyUnverifiable
		return nil, failure
	}
	if data.Repository == nil {
		return nil, n.undecided(numbers[0], fieldError(errs, "repository"))
	}
	// Only an explicit false says the repository takes no discussions; a
	// missing or null member leaves each number to its own answer.
	enabled := true
	if raw, ok := data.Repository["hasDiscussionsEnabled"]; ok {
		var answered *bool
		if json.Unmarshal(raw, &answered) == nil && answered != nil {
			enabled = *answered
		}
	}
	held := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		alias := discussionAlias(number)
		var discussion *struct {
			Number int `json:"number"`
		}
		raw, answered := data.Repository[alias]
		if answered && json.Unmarshal(raw, &discussion) != nil {
			return nil, n.undecided(number, fieldError(errs, "repository", alias))
		}
		switch {
		case discussion != nil && discussion.Number == number:
			held[number] = true
		case discussion == nil && !enabled:
			// The repository takes no discussions.
		case discussion == nil && answered && fieldError(errs, "repository", alias).Type == "NOT_FOUND":
			// No discussion holds the number.
		default:
			return nil, n.undecided(number, fieldError(errs, "repository", alias))
		}
	}
	return held, nil
}

// undecided is the answer of a lookup GitHub did not tell whether a
// discussion holds a number: nothing was created.
func (n *numberScan) undecided(number int, reported github.GraphQLError) *collab.Failure {
	s := n.s
	if reported.Type == "FORBIDDEN" {
		return collab.Fail(collab.OutcomeDenied, reasonKeyUnverifiable,
			"checking the idempotency key needs to know whether #%d of %s is a discussion or a number GitHub has not given yet, and the credential cannot read the repository's discussions (%s); nothing was created: give the credential read access to discussions",
			number, s.repo.display, reported.Message)
	}
	said := "GitHub named no discussion and no error for it"
	if reported.Type != "" || reported.Message != "" {
		said = "GitHub answered " + reported.Type + ": " + reported.Message
	}
	return collab.Fail(collab.OutcomeUnavailable, reasonKeyUnverifiable,
		"checking the idempotency key needs to know whether #%d of %s is a discussion or a number GitHub has not given yet, and %s; nothing was created",
		number, s.repo.display, said)
}

// discussionAlias names the query field that reads the discussion a number
// may hold.
func discussionAlias(number int) string { return "n" + strconv.Itoa(number) }

// fieldError is the error GitHub reported for the field at path, or the zero
// error.
func fieldError(errs []github.GraphQLError, path ...string) github.GraphQLError {
	for _, reported := range errs {
		if reported.At(path...) {
			return reported
		}
	}
	return github.GraphQLError{}
}

func numberList(numbers []int) string {
	names := make([]string, len(numbers))
	for i, number := range numbers {
		names[i] = "#" + strconv.Itoa(number)
	}
	return strings.Join(names, ", ")
}

// exhausted is the answer of a lookup that needs more than maxKeyReads
// reads. Issues GitHub does not list yet are listed within seconds, so a
// retry can succeed. Deleted, moved and unreadable numbers stay what they
// are: a retry meets them again until a newer issue is listed above them and
// ages past listLagBound.
func (n *numberScan) exhausted() *collab.Failure {
	if n.unlisted > 0 {
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonKeyUnverifiable,
			"checking the idempotency key needs more than %d reads by number in %s, and %d of the numbers read are issues or pull requests GitHub does not list yet; nothing was created: retry once it lists them",
			maxKeyReads, n.s.repo.display, n.unlisted))
	}
	return collab.Fail(collab.OutcomeUnavailable, reasonKeyUnverifiable,
		"checking the idempotency key needs more than %d reads by number in %s: %d of the numbers read are deleted issues, %d moved to another repository and %d not issues this credential can read (discussions or hidden items), and none is an issue GitHub does not list yet; nothing was created. A repeat meets the same numbers until GitHub lists an issue or pull request newer than them and %d minutes pass after it was opened",
		maxKeyReads, n.s.repo.display, n.gone, n.moved, n.other, int(listLagBound.Minutes()))
}

// numberAnswer is what a number of a repository holds.
type numberAnswer int

const (
	// numberIssue: an issue or pull request of the repository.
	numberIssue numberAnswer = iota + 1
	// numberDeleted: an issue GitHub deleted (410).
	numberDeleted
	// numberMoved: an issue moved to another repository.
	numberMoved
	// numberAbsent: GitHub answers 404 — a number it has not given yet, a
	// discussion, or an item the credential cannot read.
	numberAbsent
)

// readNumber reads the issue or pull request that has a number in a
// repository. A read by number shows an issue as soon as its create was
// answered. It returns the issue only for numberIssue; GitHub redirects a
// read of a moved issue to its new repository.
func readNumber(ctx context.Context, client *github.Client, repo repository, number int) (*ghIssue, numberAnswer, *github.Error) {
	var issue ghIssue
	_, err := client.Get(ctx, repo.path("issues", strconv.Itoa(number)), nil, &issue)
	switch {
	case err == nil && issue.RepositoryURL != "" && !repo.sameRepository(issue.RepositoryURL):
		return nil, numberMoved, nil
	case err == nil:
		return &issue, numberIssue, nil
	case err.Kind == github.Answered && err.Status == http.StatusNotFound:
		return nil, numberAbsent, nil
	case err.Kind == github.Answered && err.Status == http.StatusGone:
		return nil, numberDeleted, nil
	}
	return nil, 0, err
}

// carrying returns the first issue that carries a marker's key.
func carrying(issues []ghIssue, marker idempotency, login string) *ghIssue {
	for i := range issues {
		if carries(&issues[i], marker, login) {
			return &issues[i]
		}
	}
	return nil
}

// carries reports whether the account login opened an issue whose body
// carries a marker's key. An issue someone else opened never answers for a
// key, even when it copies the marker.
func carries(issue *ghIssue, marker idempotency, login string) bool {
	if issue.isPullRequest() || issue.User == nil || !strings.EqualFold(issue.User.Login, login) {
		return false
	}
	_, recorded := splitMarker(deref(issue.Body))
	return recorded != nil && recorded.key == marker.key
}

// write applies a patch to an issue and returns the issue as GitHub answers
// it. When the outcome is unknown, it reads the issue back and accepts it
// when applied reports that the patch is in place.
func (s *taskSession) write(issue *ghIssue, patch map[string]any, operation string, applied func(*ghIssue) bool) (*ghIssue, *collab.Failure) {
	path := s.repo.path("issues", strconv.Itoa(issue.Number))
	var updated ghIssue
	err := s.client.Write(s.ctx, http.MethodPatch, path, patch, &updated)
	if err == nil {
		return &updated, nil
	}
	if err.Kind != github.Uncertain {
		return nil, fail(err, "%s #%d of %s", operation, issue.Number, s.repo.display)
	}
	var current ghIssue
	if s.reconcile(func() (bool, *collab.Failure) {
		if _, err := s.client.Get(s.ctx, path, nil, &current); err != nil {
			return false, fail(err, "read #%d back", issue.Number)
		}
		return applied(&current), nil
	}) {
		return &current, nil
	}
	return nil, unresolved("read the task with tasks.get and compare it with the request before any retry",
		"the %s of #%d in %s may have happened (%s), and reading it back did not show it", operation, issue.Number, s.repo.display, err.Message)
}

func (s *taskSession) checkRevision(issue *ghIssue, expected string) *collab.Failure {
	if current := issueRevision(issue); expected != "" && current != expected {
		return conflictAt(current, collab.ReasonRevisionConflict, "task #%d of %s is at revision %s, not %s", issue.Number, s.repo.display, current, expected)
	}
	return nil
}

// update changes title, body or labels. The body keeps its idempotency
// marker, and labels replace the task's labels while its state labels stay.
// expectedRevision is compared with the issue read just before the write:
// GitHub has no conditional issue write, so a writer between the two is not
// detected.
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
	title, body, labels := issue.Title, deref(issue.Body), issue.labelNames()
	if in.Title != nil && *in.Title != issue.Title {
		title = *in.Title
		patch["title"] = title
	}
	if in.Body != nil {
		if joined := rejoin(deref(issue.Body), *in.Body); joined != deref(issue.Body) {
			body = joined
			patch["body"] = body
		}
	}
	if in.Labels != nil {
		if failure := s.states.refuseStateLabels(*in.Labels); failure != nil {
			return nil, failure
		}
		wanted := append(slices.Clone(*in.Labels), s.states.stateLabels(issue.labelNames())...)
		if !sameLabels(wanted, issue.labelNames()) {
			labels = wanted
			patch["labels"] = labels
		}
	}
	if len(patch) == 0 {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	updated, failure := s.write(issue, patch, "update", func(current *ghIssue) bool {
		return current.Title == title && deref(current.Body) == body && sameLabels(current.labelNames(), labels)
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// transition moves an issue to a semantic state: open states by label,
// done and canceled by closing it with the matching reason. A task already in
// the state is left as it is, except an open issue that reads as the state
// through a label other than the state's first: it gets the first label.
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
	patch := map[string]any{"state": goal.state, "labels": goal.labels}
	if goal.stateReason != "" {
		patch["state_reason"] = goal.stateReason
	}
	updated, failure := s.write(issue, patch, "transition to "+string(in.State), func(current *ghIssue) bool {
		return s.states.settled(current, in.State)
	})
	if failure != nil {
		return nil, failure
	}
	if !s.states.settled(updated, in.State) {
		state, providerState := s.states.stateOf(updated)
		return nil, unresolved("read the task with tasks.get before any retry",
			"GitHub accepted the transition of #%d to %s and reports it as %s", issue.Number, in.State, describeState(state, providerState))
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// assign replaces an issue's assignees. "@me" names the account of the
// credential. GitHub drops a login it cannot assign; the answer lists the
// assignees the issue actually has. An assignee is information, never an
// exclusive claim.
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
	assignees := make([]string, 0, len(in.Assignees))
	for _, login := range in.Assignees {
		if login == "@me" {
			me, failure := s.authenticated()
			if failure != nil {
				return nil, failure
			}
			login = me
		}
		assignees = append(assignees, login)
	}
	if sameLabels(assignees, issue.assigneeLogins()) {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	updated, failure := s.write(issue, map[string]any{"assignees": assignees}, "assignment", func(current *ghIssue) bool {
		return sameLabels(current.assigneeLogins(), assignees)
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// link sets or removes an issue's parent through GitHub sub-issues. The
// parent must be an issue of the same repository.
func (s *taskSession) link(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskLinkInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	current, failure := s.parent(issue.Number)
	if failure != nil {
		return nil, failure
	}
	var goal *collab.Ref
	var path string
	var method string
	switch {
	case in.Unlink && current == nil:
		return s.linked(issue, nil)
	case in.Unlink:
		number, ours := s.repo.number(*current)
		if !ours {
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonRepository,
				"the parent of #%d is in another repository, which this binding does not serve", issue.Number)
		}
		method, path = http.MethodDelete, s.repo.path("issues", strconv.Itoa(number), "sub_issue")
	default:
		if *in.Parent == in.Ref {
			return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "a task cannot be its own parent")
		}
		parent, failure := s.issue(*in.Parent)
		if failure != nil {
			return nil, failure
		}
		if current != nil && *current == *in.Parent {
			return s.linked(issue, current)
		}
		goal = in.Parent
		method, path = http.MethodPost, s.repo.path("issues", strconv.Itoa(parent.Number), "sub_issues")
	}
	body := map[string]any{"sub_issue_id": issue.ID}
	if method == http.MethodPost {
		body["replace_parent"] = true
	}
	err := s.client.Write(s.ctx, method, path, body, nil)
	if err != nil && err.Kind != github.Uncertain {
		return nil, fail(err, "change the parent of #%d in %s", issue.Number, s.repo.display)
	}
	var now *collab.Ref
	if !s.reconcile(func() (bool, *collab.Failure) {
		parent, failure := s.parent(issue.Number)
		now = parent
		return failure == nil && sameParent(parent, goal), failure
	}) {
		if err == nil {
			// GitHub acknowledged the change: that answer establishes the
			// parent even while reading it back does not show it yet.
			return s.linked(issue, goal)
		}
		return nil, unresolved("read the task with tasks.get before any retry",
			"the parent change of #%d may have happened (%s), and reading it back did not show it", issue.Number, err.Message)
	}
	return s.linked(issue, now)
}

func sameParent(a, b *collab.Ref) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// linked answers a link with the issue as it is now and its parent.
func (s *taskSession) linked(issue *ghIssue, parent *collab.Ref) (any, *collab.Failure) {
	fresh, failure := s.issue(s.repo.ref(issue.Number))
	if failure != nil {
		return nil, failure
	}
	task := s.task(fresh)
	task.Parent = parent
	return &collab.TaskResult{Task: task}, nil
}
