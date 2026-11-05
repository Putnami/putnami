package provider

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// An idempotency record is two things a keyed create writes in the create
// call itself: the label putnami-idempotency-<sha256 of the project and the
// key>, which JQL finds, and the issue property putnami.idempotency holding
// the digest of the request, which tells a repeat from a reuse.
type idempotency struct {
	label  string
	digest string
}

// createRequest is what a create's idempotency digest covers.
type createRequest struct {
	Title  string           `json:"title"`
	Body   string           `json:"body"`
	Labels []string         `json:"labels"`
	State  collab.TaskState `json:"state"`
}

type recorded struct {
	Digest string `json:"digest"`
}

func newIdempotency(project, key string, request createRequest) idempotency {
	// Labels are a set: absent, empty and reordered labels are one request.
	request.Labels = slices.Sorted(slices.Values(request.Labels))
	if request.Labels == nil {
		request.Labels = []string{}
	}
	return idempotency{label: reservedLabelPrefix + collabhelper.Digest([]string{"tasks.create", project, key}), digest: collabhelper.Digest(request)}
}

// recordOf reads the request digest an issue's property records; empty when
// it records none.
func recordOf(issue *jiraIssue) string {
	var record recorded
	if raw, ok := issue.Properties[idempotencyProperty]; ok && json.Unmarshal(raw, &record) == nil {
		return record.Digest
	}
	return ""
}

// Bounds of one key lookup.
const (
	// indexedAge is how old an issue is, by Jira's clock, when the lookup
	// takes it as shown by Jira's search. The search trails a create by
	// seconds; the margin covers an index that falls minutes behind.
	indexedAge = "-30m"
	// maxRecentPages bounds the search pages of 100 that list the issues
	// opened within indexedAge. A project busier than that cannot tell
	// whether a key opened an issue.
	maxRecentPages = 5
	// maxKeyReads bounds the reads by key of one lookup. A lookup that needs
	// more cannot tell whether its key opened an issue.
	maxKeyReads = 50
	// notGivenRun is how many keys in a row above the newest one the search
	// shows must hold nothing before the lookup takes them as not given yet.
	notGivenRun = 3
)

// findKey looks for the issue a create key opened, among the issues the
// credential's account created. Jira's search shows a new issue only seconds
// after its create was answered, and not in key order, while a read by key
// shows it at once. So the lookup searches the label, then takes as its floor
// the newest key neither created nor updated within indexedAge: an issue
// moved into the project takes a new key but keeps its creation date, and
// the move updates it. It lists the issues above the floor, and reads by key
// every key that list misses and every key above its newest one.
func (s *session) findKey(marker idempotency) (*jiraIssue, *collab.Failure) {
	account, failure := s.myself()
	if failure != nil {
		return nil, failure
	}
	unverifiable := func(err *jira.Error, format string, args ...any) *collab.Failure {
		failure := fail(err, format, args...)
		if failure.Outcome == collab.OutcomeUnavailable {
			failure.Error.Reason = reasonKeyUnverifiable
		}
		return failure
	}
	found, _, err := s.search("labels = "+quote(marker.label)+" AND creator = currentUser() ORDER BY key ASC", "", 10)
	if err != nil {
		return nil, unverifiable(err, "look for idempotency key in the search of %s", s.settings.Site)
	}
	if issue := carrying(found, marker, account); issue != nil {
		return issue, nil
	}
	project := "project = " + quote(s.settings.Project)
	older, _, err := s.search(project+" AND created <= "+quote(indexedAge)+" AND updated <= "+quote(indexedAge)+" ORDER BY key DESC", "", 1)
	if err != nil {
		return nil, unverifiable(err, "look for idempotency key among the issues of project %s", s.settings.Project)
	}
	floor := 0
	recent := project + " ORDER BY key ASC"
	if len(older) > 0 {
		floor = keyNumber(older[0].Key)
		recent = project + " AND key > " + quote(older[0].Key) + " ORDER BY key ASC"
	}
	listed := map[int]bool{}
	top := floor
	for page, token := 0, ""; ; page++ {
		if page == maxRecentPages {
			return nil, retryable(collab.Fail(collab.OutcomeUnavailable, reasonKeyUnverifiable,
				"checking the idempotency key needs to list more than %d issues project %s created or updated within %s; nothing was created: retry once fewer are listed",
				maxRecentPages*jiraPageSize, s.settings.Project, strings.TrimPrefix(indexedAge, "-")))
		}
		issues, next, err := s.search(recent, token, jiraPageSize)
		if err != nil {
			return nil, unverifiable(err, "look for idempotency key among the recent issues of project %s", s.settings.Project)
		}
		for i := range issues {
			number := keyNumber(issues[i].Key)
			listed[number] = true
			top = max(top, number)
		}
		if token = next; token == "" || len(issues) == 0 {
			break
		}
	}
	reads := 0
	readKey := func(number int) (*jiraIssue, bool, *collab.Failure) {
		if reads == maxKeyReads {
			return nil, false, retryable(collab.Fail(collab.OutcomeUnavailable, reasonKeyUnverifiable,
				"checking the idempotency key needs more than %d reads by key in project %s, for recent keys Jira's search does not show: issues not indexed yet, deleted or hidden from this account; nothing was created: retry once the search shows the new ones",
				maxKeyReads, s.settings.Project))
		}
		reads++
		issue, err := s.read(s.settings.Project + "-" + strconv.Itoa(number))
		switch {
		case err != nil && err.Kind == jira.Answered && err.Status == http.StatusNotFound:
			return nil, false, nil
		case err != nil:
			return nil, false, unverifiable(err, "look for idempotency key in issue %s-%d", s.settings.Project, number)
		}
		return issue, true, nil
	}
	for number := floor + 1; number <= top; number++ {
		if listed[number] {
			continue
		}
		issue, given, failure := readKey(number)
		if failure != nil {
			return nil, failure
		}
		if given && carries(issue, marker, account) {
			return issue, nil
		}
	}
	for number, misses := top+1, 0; misses < notGivenRun; number++ {
		issue, given, failure := readKey(number)
		switch {
		case failure != nil:
			return nil, failure
		case !given:
			misses++
		default:
			misses = 0
			if carries(issue, marker, account) {
				return issue, nil
			}
		}
	}
	return nil, nil
}

func carrying(issues []jiraIssue, marker idempotency, account string) *jiraIssue {
	for i := range issues {
		if carries(&issues[i], marker, account) {
			return &issues[i]
		}
	}
	return nil
}

// carries reports whether the account created an issue that carries a
// marker's label. An issue someone else created never answers for a key,
// even when it copies the label.
func carries(issue *jiraIssue, marker idempotency, account string) bool {
	if issue.Fields.Creator == nil || issue.Fields.Creator.AccountID != account {
		return false
	}
	return slices.ContainsFunc(issue.Fields.Labels, func(label string) bool { return strings.EqualFold(label, marker.label) })
}

type createdIssue struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// create opens an issue in the bound project. Before opening one it looks for
// the key (findKey); after a write whose outcome is unknown it looks again,
// and answers unresolved rather than opening a second issue.
func (s *session) create(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskCreateInput](input)
	if failure != nil {
		return nil, failure
	}
	state := in.State
	if state == "" {
		state = collab.TaskStateOpen
	}
	if categoryOf(state) == "" {
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped, "Jira has no status category for %s", state)
	}
	if failure := refuseReserved(in.Labels); failure != nil {
		return nil, failure
	}
	marker := newIdempotency(s.settings.Project, in.IdempotencyKey, createRequest{in.Title, in.Body, in.Labels, state})

	existing, failure := s.findKey(marker)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		if recordOf(existing) != marker.digest {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
				"idempotency key %q already opened %s for a different request", in.IdempotencyKey, existing.Key)
		}
		return &collab.TaskCreateResult{Task: s.task(existing), Created: false}, nil
	}

	fields := map[string]any{
		"project":   map[string]string{"key": s.settings.Project},
		"issuetype": map[string]string{"name": s.settings.IssueType},
		"summary":   in.Title,
		"labels":    append(slices.Clone(in.Labels), marker.label),
	}
	if in.Body != "" {
		fields["description"] = in.Body
	}
	request := map[string]any{
		"fields":     fields,
		"properties": []map[string]any{{"key": idempotencyProperty, "value": recorded{Digest: marker.digest}}},
	}
	var answered createdIssue
	err := s.client.Write(s.ctx, http.MethodPost, "/issue", request, &answered)
	if err == nil && answered.ID == "" {
		err = &jira.Error{Kind: jira.Uncertain, Status: http.StatusCreated, Message: "Jira acknowledged the issue without its id"}
	}
	var created *jiraIssue
	if err == nil {
		created, err = s.read(answered.ID)
		if err != nil {
			return nil, unresolved("repeat tasks.create with the same idempotencyKey: it returns the issue",
				"Jira opened %s in project %s, and reading it back failed: %s", answered.Key, s.settings.Project, err.Message)
		}
	} else {
		if err.Kind != jira.Uncertain {
			return nil, fail(err, "open an issue in project %s", s.settings.Project)
		}
		settled := s.reconcile(func() (bool, *collab.Failure) {
			issue, failure := s.findKey(marker)
			created = issue
			return issue != nil, failure
		})
		if !settled {
			return nil, unresolved("repeat tasks.create with the same idempotencyKey: the key is recorded on the issue, so the repeat returns the issue if it was opened",
				"opening the issue in project %s may have happened (%s), and reading Jira back did not find it", s.settings.Project, err.Message)
		}
	}
	if created.state() != state {
		moved, failure := s.move(created, state)
		if failure != nil {
			reconcile := "read " + created.Key + " with tasks.get, then transition it to " + string(state) +
				"; a repeat of tasks.create with the same idempotencyKey returns the issue as it is"
			if failure.Outcome == collab.OutcomeUnresolved {
				failure.Error.Reconcile = reconcile
				return nil, failure
			}
			incomplete := unresolved(reconcile, "opened %s in project %s, and moving it to %s failed, so it is %s: %s",
				created.Key, s.settings.Project, state, created.state(), failure.Error.Message)
			incomplete.Error.Reason = reasonIncomplete
			return nil, incomplete
		}
		created = moved
	}
	return &collab.TaskCreateResult{Task: s.task(created), Created: true}, nil
}
