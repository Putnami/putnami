package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is an in-memory stand-in for the part of the GitHub REST and
// GraphQL API the provider uses. It keeps GitHub's rules the provider relies
// on — issues, pull requests and discussions share one number sequence, one
// open pull request per base and head, the author cannot approve their own
// pull request, numbered pages with a Link header, a search that serves at
// most 1,000 results, issue and pull request lists and a search that show a
// new item only some time after its create was answered while a read by
// number shows it at once, a deleted issue that answers 410, a moved issue
// whose read redirects to its new repository, a discussion that the issues
// endpoint answers 404 and a GraphQL discussion field finds, and a Date
// header on every answer from the fake's clock — and injects faults: an answer lost after the write was applied, a
// request lost before it was read, and a status answered before or after
// applying.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	owner, name   string
	login         string
	token         string
	collaborators []string
	clock         time.Time
	next          int
	nextID        int64
	issues        map[int]*fakeIssue
	pulls         map[int]*fakePull
	reviews       map[int][]*fakeReview
	branches      map[string]string
	forks         map[string]string
	parents       map[int]int
	checkRuns     map[string][]map[string]any
	statuses      map[string][]map[string]any
	faults        []*fault
	calls         []string
	// listLag is how long, in real time, a new issue or pull request stays
	// out of the issue lists and the search, the way GitHub's index trails a
	// create. Zero lists it at once.
	listLag time.Duration
	// pullListLag is how long, in real time, a new pull request stays out of
	// the pull request list. Zero lists it at once.
	pullListLag time.Duration
	// discussions are the numbers discussions hold: the issues endpoint
	// answers 404 for them, and a GraphQL discussion field finds them.
	discussions map[int]bool
	// discussionsOff is a repository that takes no discussions:
	// hasDiscussionsEnabled answers false.
	discussionsOff bool
	// discussionsForbidden is a credential that cannot read discussions:
	// every discussion field answers FORBIDDEN.
	discussionsForbidden bool
}

type fakeIssue struct {
	id          int64
	number      int
	title, body string
	state       string
	stateReason string
	labels      []string
	assignees   []string
	user        string
	created     time.Time
	updated     time.Time
	pull        bool
	// listed is when the issue enters the issue lists and the search.
	listed time.Time
	// deleted is an issue that answers 410 and is in no list.
	deleted bool
	// movedTo is the owner/name of the repository an issue moved to: a read
	// by number redirects there, and no list of this repository shows it.
	movedTo string
}

// inLists reports whether the issue lists and the search show an issue now.
func (issue *fakeIssue) inLists() bool {
	return !issue.deleted && issue.movedTo == "" && !time.Now().Before(issue.listed)
}

type fakePull struct {
	nodeID    string
	base      string
	head      string
	headOwner string
	sha       string
	shas      []string
	draft     bool
	merged    *time.Time
	// listed is when the pull request enters the pull request list.
	listed time.Time
}

type fakeReview struct {
	id        int64
	user      string
	body      string
	state     string
	commit    string
	submitted time.Time
}

// fault changes the answer to the next matching requests.
type fault struct {
	method string
	path   *regexp.Regexp
	times  int
	// after lets that many matching requests through before the fault
	// applies.
	after int
	// before answers without applying the request; otherwise the request is
	// applied first.
	before bool
	// drop closes the connection without an answer.
	drop   bool
	status int
	body   string
	header map[string]string
}

const fakeToken = "ghp_FAKEtokenFAKEtokenFAKEtokenFAKE0000"

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		t: t, owner: "acme", name: "app", login: "putnami-bot", token: fakeToken,
		collaborators: []string{"putnami-bot", "octocat", "hubot"},
		clock:         time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC),
		next:          1, nextID: 1000,
		issues: map[int]*fakeIssue{}, pulls: map[int]*fakePull{}, reviews: map[int][]*fakeReview{},
		branches: map[string]string{
			"main":    strings.Repeat("a", 40),
			"topic-a": strings.Repeat("b", 40),
			"topic-b": strings.Repeat("c", 40),
		},
		forks:       map[string]string{},
		parents:     map[int]int{},
		discussions: map[int]bool{},
		checkRuns:   map[string][]map[string]any{},
		statuses:    map[string][]map[string]any{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

// fail installs a fault.
func (f *fakeGitHub) fail(method, path string, fault fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fault.method = method
	fault.path = regexp.MustCompile(path)
	if fault.times == 0 {
		fault.times = 1
	}
	f.faults = append(f.faults, &fault)
}

// count reports how many requests matched method and a path pattern.
func (f *fakeGitHub) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	pattern := regexp.MustCompile(path)
	n := 0
	for _, call := range f.calls {
		m, p, _ := strings.Cut(call, " ")
		if m == method && pattern.MatchString(p) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Date", f.clock.UTC().Format(http.TimeFormat))
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	var active *fault
	for _, candidate := range f.faults {
		if candidate.times > 0 && candidate.method == r.Method && candidate.path.MatchString(r.URL.Path) {
			if candidate.after > 0 {
				candidate.after--
				continue
			}
			candidate.times--
			active = candidate
			break
		}
	}
	if active != nil && active.before {
		f.answerFault(w, active)
		return
	}
	recorder := httptest.NewRecorder()
	f.route(recorder, r)
	if active != nil {
		f.answerFault(w, active)
		return
	}
	for name, values := range recorder.Header() {
		w.Header()[name] = values
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func (f *fakeGitHub) answerFault(w http.ResponseWriter, active *fault) {
	if active.drop {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			f.t.Errorf("hijack: %v", err)
			return
		}
		_ = connection.Close()
		return
	}
	for name, value := range active.header {
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(active.status)
	_, _ = w.Write([]byte(active.body))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func validation(message string) map[string]any {
	return map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Issue", "code": "custom", "message": message}}}
}

var routes = []struct {
	method  string
	pattern *regexp.Regexp
	handle  func(f *fakeGitHub, w http.ResponseWriter, r *http.Request, match []string)
}{
	{"GET", regexp.MustCompile(`^/user$`), (*fakeGitHub).user},
	{"GET", regexp.MustCompile(`^/search/issues$`), (*fakeGitHub).search},
	{"POST", regexp.MustCompile(`^/graphql$`), (*fakeGitHub).graphql},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues$`), (*fakeGitHub).listIssues},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues$`), (*fakeGitHub).createIssue},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)$`), (*fakeGitHub).getIssue},
	{"PATCH", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)$`), (*fakeGitHub).patchIssue},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)/labels$`), (*fakeGitHub).addLabels},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)/assignees$`), (*fakeGitHub).addAssignees},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)/parent$`), (*fakeGitHub).getParent},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)/sub_issues$`), (*fakeGitHub).addSubIssue},
	{"DELETE", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/([0-9]+)/sub_issue$`), (*fakeGitHub).removeSubIssue},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`), (*fakeGitHub).listPulls},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`), (*fakeGitHub).createPull},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)$`), (*fakeGitHub).getPull},
	{"PATCH", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)$`), (*fakeGitHub).patchPull},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)/reviews$`), (*fakeGitHub).listReviews},
	{"POST", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)/reviews$`), (*fakeGitHub).createReview},
	{"PUT", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)/merge$`), (*fakeGitHub).mergePull},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/ref/heads/(.+)$`), (*fakeGitHub).getRef},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([0-9a-f]+)/check-runs$`), (*fakeGitHub).listCheckRuns},
	{"GET", regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([0-9a-f]+)/status$`), (*fakeGitHub).combinedStatus},
}

func (f *fakeGitHub) route(w http.ResponseWriter, r *http.Request) {
	for _, route := range routes {
		match := route.pattern.FindStringSubmatch(r.URL.Path)
		if match == nil || route.method != r.Method {
			continue
		}
		if strings.HasPrefix(r.URL.Path, "/repos/") && !f.ownRepository(match[1], match[2]) {
			// Outside the repository, only a fork's refs and the issues that
			// moved out of it are readable.
			if moved := f.movedIssue(match, r); moved != nil {
				writeJSON(w, http.StatusOK, f.issueJSON(moved))
				return
			}
			if !(strings.Contains(r.URL.Path, "/git/ref/heads/") && strings.EqualFold(match[2], f.name)) {
				writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
				return
			}
		}
		route.handle(f, w, r, match)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func (f *fakeGitHub) ownRepository(owner, name string) bool {
	return strings.EqualFold(owner, f.owner) && strings.EqualFold(name, f.name)
}

// movedIssue is the issue a read by number in another repository finds there
// because it moved out of this one.
func (f *fakeGitHub) movedIssue(match []string, r *http.Request) *fakeIssue {
	if r.Method != http.MethodGet || len(match) != 4 || !strings.HasSuffix(r.URL.Path, "/issues/"+match[3]) {
		return nil
	}
	issue := f.issues[f.number(match[3])]
	if issue == nil || !strings.EqualFold(issue.movedTo, match[1]+"/"+match[2]) {
		return nil
	}
	return issue
}

func decodeBody(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

func (f *fakeGitHub) number(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}

func (f *fakeGitHub) issueJSON(issue *fakeIssue) map[string]any {
	labels := make([]any, 0, len(issue.labels))
	for _, label := range issue.labels {
		labels = append(labels, map[string]any{"name": label})
	}
	assignees := make([]any, 0, len(issue.assignees))
	for _, login := range issue.assignees {
		assignees = append(assignees, map[string]any{"login": login})
	}
	kind := "issues"
	if issue.pull {
		kind = "pull"
	}
	document := map[string]any{
		"id": issue.id, "number": issue.number, "title": issue.title, "body": nil, "state": issue.state, "state_reason": nil,
		"labels": labels, "assignees": assignees, "user": map[string]any{"login": issue.user},
		"html_url":   fmt.Sprintf("https://github.com/%s/%s/%s/%d", f.owner, f.name, kind, issue.number),
		"created_at": issue.created.Format(time.RFC3339), "updated_at": issue.updated.Format(time.RFC3339),
		"repository_url": f.server.URL + "/repos/" + f.owner + "/" + f.name,
	}
	if issue.movedTo != "" {
		document["repository_url"] = f.server.URL + "/repos/" + issue.movedTo
	}
	if issue.body != "" {
		document["body"] = issue.body
	}
	if issue.stateReason != "" {
		document["state_reason"] = issue.stateReason
	}
	if issue.pull {
		document["pull_request"] = map[string]any{"url": fmt.Sprintf("%s/repos/%s/%s/pulls/%d", f.server.URL, f.owner, f.name, issue.number)}
	}
	return document
}

func (f *fakeGitHub) pullJSON(number int) map[string]any {
	issue, pull := f.issues[number], f.pulls[number]
	document := f.issueJSON(issue)
	delete(document, "pull_request")
	delete(document, "state_reason")
	document["node_id"] = pull.nodeID
	document["draft"] = pull.draft
	document["merged_at"] = nil
	if pull.merged != nil {
		document["merged_at"] = pull.merged.Format(time.RFC3339)
	}
	document["base"] = map[string]any{"ref": pull.base, "label": f.owner + ":" + pull.base, "sha": f.branches[pull.base],
		"repo": map[string]any{"full_name": f.owner + "/" + f.name}}
	document["head"] = map[string]any{"ref": pull.head, "label": pull.headOwner + ":" + pull.head, "sha": pull.sha,
		"repo": map[string]any{"full_name": pull.headOwner + "/" + f.name}}
	return document
}

// paginate answers one numbered page and a Link header naming the next.
func paginate[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	if end < len(items) {
		next := *r.URL
		query := next.Query()
		query.Set("page", strconv.Itoa(page+1))
		next.RawQuery = query.Encode()
		w.Header().Set("Link", `<http://`+r.Host+next.String()+`>; rel="next"`)
	}
	return items[start:end]
}

func (f *fakeGitHub) sortedIssues(descending bool) []*fakeIssue {
	issues := make([]*fakeIssue, 0, len(f.issues))
	for _, issue := range f.issues {
		issues = append(issues, issue)
	}
	sort.Slice(issues, func(i, j int) bool {
		if descending {
			return issues[i].number > issues[j].number
		}
		return issues[i].number < issues[j].number
	})
	return issues
}

func hasAll(have []string, want []string) bool {
	for _, label := range want {
		if !slices.ContainsFunc(have, func(name string) bool { return strings.EqualFold(name, label) }) {
			return false
		}
	}
	return true
}

func (f *fakeGitHub) user(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, http.StatusOK, map[string]any{"login": f.login})
}

func (f *fakeGitHub) listIssues(w http.ResponseWriter, r *http.Request, _ []string) {
	query := r.URL.Query()
	var labels []string
	if value := query.Get("labels"); value != "" {
		labels = strings.Split(value, ",")
	}
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	var matches []map[string]any
	for _, issue := range f.sortedIssues(query.Get("direction") == "desc") {
		if !issue.inLists() || (state != "all" && issue.state != state) || !hasAll(issue.labels, labels) {
			continue
		}
		if creator := query.Get("creator"); creator != "" && creator != issue.user {
			continue
		}
		matches = append(matches, f.issueJSON(issue))
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, matches)))
}

func orEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

var searchToken = regexp.MustCompile(`[a-z]+:"[^"]*"|\S+`)

func (f *fakeGitHub) search(w http.ResponseWriter, r *http.Request, _ []string) {
	query := r.URL.Query()
	var words, labels []string
	repo, kind, state, author := "", "", "", ""
	for _, term := range searchToken.FindAllString(query.Get("q"), -1) {
		qualifier, value, qualified := strings.Cut(term, ":")
		value = strings.Trim(value, `"`)
		switch {
		case qualified && qualifier == "repo":
			repo = value
		case qualified && qualifier == "is" && (value == "issue" || value == "pr"):
			kind = value
		case qualified && qualifier == "is":
			state = value
		case qualified && qualifier == "label":
			labels = append(labels, value)
		case qualified && qualifier == "author":
			author = value
		case qualified && qualifier == "in":
		default:
			words = append(words, strings.ToLower(term))
		}
	}
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	page, _ := strconv.Atoi(query.Get("page"))
	if perPage > 0 && page > 0 && page*perPage > 1000 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Only the first 1000 search results are available"})
		return
	}
	var matches []map[string]any
	for _, issue := range f.sortedIssues(query.Get("order") == "desc") {
		if !issue.inLists() || !strings.EqualFold(repo, f.owner+"/"+f.name) || (kind == "issue" && issue.pull) || (kind == "pr" && !issue.pull) ||
			(state != "" && issue.state != state) || !hasAll(issue.labels, labels) || (author != "" && author != issue.user) {
			continue
		}
		text := strings.ToLower(issue.title + " " + issue.body)
		if slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(text, word) }) {
			continue
		}
		matches = append(matches, f.issueJSON(issue))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(matches), "items": orEmpty(paginate(w, r, matches))})
}

func (f *fakeGitHub) newIssue(title, body string, labels []string) *fakeIssue {
	now := f.tick()
	issue := &fakeIssue{id: f.nextID, number: f.next, title: title, body: body, state: "open", labels: labels, user: f.login,
		created: now, updated: now, listed: time.Now().Add(f.listLag)}
	f.issues[issue.number] = issue
	f.next++
	f.nextID++
	return issue
}

func (f *fakeGitHub) createIssue(w http.ResponseWriter, r *http.Request, _ []string) {
	var request struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		Assignees []string `json:"assignees"`
	}
	if err := decodeBody(r, &request); err != nil || request.Title == "" {
		writeJSON(w, http.StatusUnprocessableEntity, validation("title is required"))
		return
	}
	issue := f.newIssue(request.Title, request.Body, request.Labels)
	writeJSON(w, http.StatusCreated, f.issueJSON(issue))
}

func (f *fakeGitHub) getIssue(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.issues[f.number(match[3])]
	switch {
	case issue == nil:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	case issue.deleted:
		writeJSON(w, http.StatusGone, map[string]any{"message": "This issue was deleted"})
	case issue.movedTo != "":
		w.Header().Set("Location", "http://"+r.Host+"/repos/"+issue.movedTo+"/issues/"+match[3])
		writeJSON(w, http.StatusMovedPermanently, map[string]any{"message": "Moved Permanently"})
	default:
		writeJSON(w, http.StatusOK, f.issueJSON(issue))
	}
}

func (f *fakeGitHub) assignable(logins []string) []string {
	var kept []string
	for _, login := range logins {
		if slices.Contains(f.collaborators, login) && !slices.Contains(kept, login) {
			kept = append(kept, login)
		}
	}
	return kept
}

func (f *fakeGitHub) patchIssue(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.issues[f.number(match[3])]
	if issue == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	var request map[string]json.RawMessage
	if err := decodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	for member, raw := range request {
		switch member {
		case "title":
			_ = json.Unmarshal(raw, &issue.title)
		case "body":
			_ = json.Unmarshal(raw, &issue.body)
		case "labels":
			issue.labels = nil
			_ = json.Unmarshal(raw, &issue.labels)
		case "assignees":
			var logins []string
			_ = json.Unmarshal(raw, &logins)
			issue.assignees = f.assignable(logins)
		case "state":
			var state string
			_ = json.Unmarshal(raw, &state)
			if state == "open" && issue.state == "closed" {
				issue.stateReason = "reopened"
			}
			issue.state = state
		case "state_reason":
			_ = json.Unmarshal(raw, &issue.stateReason)
		}
	}
	if issue.state == "open" && issue.stateReason != "reopened" {
		issue.stateReason = ""
	}
	issue.updated = f.tick()
	writeJSON(w, http.StatusOK, f.issueJSON(issue))
}

func (f *fakeGitHub) addLabels(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.issues[f.number(match[3])]
	var request struct {
		Labels []string `json:"labels"`
	}
	if issue == nil || decodeBody(r, &request) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	for _, label := range request.Labels {
		if !slices.Contains(issue.labels, label) {
			issue.labels = append(issue.labels, label)
		}
	}
	issue.updated = f.tick()
	labels := make([]any, 0, len(issue.labels))
	for _, label := range issue.labels {
		labels = append(labels, map[string]any{"name": label})
	}
	writeJSON(w, http.StatusOK, labels)
}

func (f *fakeGitHub) addAssignees(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.issues[f.number(match[3])]
	var request struct {
		Assignees []string `json:"assignees"`
	}
	if issue == nil || decodeBody(r, &request) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	issue.assignees = f.assignable(append(issue.assignees, request.Assignees...))
	issue.updated = f.tick()
	writeJSON(w, http.StatusCreated, f.issueJSON(issue))
}

func (f *fakeGitHub) getParent(w http.ResponseWriter, _ *http.Request, match []string) {
	parent, found := f.parents[f.number(match[3])]
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, f.issueJSON(f.issues[parent]))
}

func (f *fakeGitHub) byID(id int64) *fakeIssue {
	for _, issue := range f.issues {
		if issue.id == id {
			return issue
		}
	}
	return nil
}

func (f *fakeGitHub) addSubIssue(w http.ResponseWriter, r *http.Request, match []string) {
	parent := f.issues[f.number(match[3])]
	var request struct {
		SubIssueID    int64 `json:"sub_issue_id"`
		ReplaceParent bool  `json:"replace_parent"`
	}
	if parent == nil || decodeBody(r, &request) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	child := f.byID(request.SubIssueID)
	if child == nil || child.number == parent.number {
		writeJSON(w, http.StatusUnprocessableEntity, validation("invalid sub-issue"))
		return
	}
	if _, has := f.parents[child.number]; has && !request.ReplaceParent {
		writeJSON(w, http.StatusUnprocessableEntity, validation("the issue already has a parent"))
		return
	}
	f.parents[child.number] = parent.number
	writeJSON(w, http.StatusCreated, f.issueJSON(parent))
}

func (f *fakeGitHub) removeSubIssue(w http.ResponseWriter, r *http.Request, match []string) {
	parent := f.number(match[3])
	var request struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}
	child := (*fakeIssue)(nil)
	if decodeBody(r, &request) == nil {
		child = f.byID(request.SubIssueID)
	}
	if child == nil || f.parents[child.number] != parent {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	delete(f.parents, child.number)
	writeJSON(w, http.StatusOK, f.issueJSON(f.issues[parent]))
}

func (f *fakeGitHub) listPulls(w http.ResponseWriter, r *http.Request, _ []string) {
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	var matches []map[string]any
	for _, issue := range f.sortedIssues(query.Get("direction") == "desc") {
		pull := f.pulls[issue.number]
		if pull == nil || (state != "all" && issue.state != state) || time.Now().Before(pull.listed) {
			continue
		}
		if head := query.Get("head"); head != "" && !strings.EqualFold(head, pull.headOwner+":"+pull.head) {
			continue
		}
		if base := query.Get("base"); base != "" && base != pull.base {
			continue
		}
		matches = append(matches, f.pullJSON(issue.number))
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, matches)))
}

func (f *fakeGitHub) createPull(w http.ResponseWriter, r *http.Request, _ []string) {
	var request struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if err := decodeBody(r, &request); err != nil || request.Title == "" {
		writeJSON(w, http.StatusUnprocessableEntity, validation("title is required"))
		return
	}
	owner, branch, fork := strings.Cut(request.Head, ":")
	if !fork {
		owner, branch = f.owner, request.Head
	}
	sha := f.branches[branch]
	if !strings.EqualFold(owner, f.owner) {
		sha = f.forks[owner+":"+branch]
	}
	if sha == "" || f.branches[request.Base] == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
			"errors": []any{map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"}}})
		return
	}
	for number, pull := range f.pulls {
		if f.issues[number].state == "open" && pull.base == request.Base && pull.head == branch && strings.EqualFold(pull.headOwner, owner) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
				"errors": []any{map[string]any{"resource": "PullRequest", "code": "custom",
					"message": "A pull request already exists for " + owner + ":" + branch + "."}}})
			return
		}
	}
	issue := f.newIssue(request.Title, request.Body, nil)
	issue.pull = true
	f.pulls[issue.number] = &fakePull{nodeID: "PR_" + strconv.Itoa(issue.number), base: request.Base, head: branch, headOwner: owner,
		sha: sha, shas: []string{sha}, draft: request.Draft, listed: time.Now().Add(f.pullListLag)}
	writeJSON(w, http.StatusCreated, f.pullJSON(issue.number))
}

func (f *fakeGitHub) getPull(w http.ResponseWriter, _ *http.Request, match []string) {
	number := f.number(match[3])
	if f.pulls[number] == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, f.pullJSON(number))
}

func (f *fakeGitHub) patchPull(w http.ResponseWriter, r *http.Request, match []string) {
	number := f.number(match[3])
	if f.pulls[number] == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	var request struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		State *string `json:"state"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	issue := f.issues[number]
	if request.Title != nil {
		issue.title = *request.Title
	}
	if request.Body != nil {
		issue.body = *request.Body
	}
	if request.State != nil {
		issue.state = *request.State
	}
	issue.updated = f.tick()
	writeJSON(w, http.StatusOK, f.pullJSON(number))
}

// push moves a pull request's head to a new commit.
func (f *fakeGitHub) push(number int, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull := f.pulls[number]
	pull.sha = sha
	pull.shas = append(pull.shas, sha)
	if strings.EqualFold(pull.headOwner, f.owner) {
		f.branches[pull.head] = sha
	}
	f.issues[number].updated = f.tick()
}

// moveBranch moves a branch of the repository and leaves the head of its pull
// requests behind, the way GitHub serves them for a few seconds after a push.
func (f *fakeGitHub) moveBranch(branch, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[branch] = sha
}

func (f *fakeGitHub) reviewJSON(number int, review *fakeReview) map[string]any {
	return map[string]any{"id": review.id, "user": map[string]any{"login": review.user}, "body": review.body, "state": review.state,
		"commit_id": review.commit, "submitted_at": review.submitted.Format(time.RFC3339),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", f.owner, f.name, number, review.id)}
}

func (f *fakeGitHub) listReviews(w http.ResponseWriter, r *http.Request, match []string) {
	number := f.number(match[3])
	if f.pulls[number] == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	reviews := make([]map[string]any, 0, len(f.reviews[number]))
	for _, review := range f.reviews[number] {
		reviews = append(reviews, f.reviewJSON(number, review))
	}
	writeJSON(w, http.StatusOK, orEmpty(paginate(w, r, reviews)))
}

func (f *fakeGitHub) createReview(w http.ResponseWriter, r *http.Request, match []string) {
	number := f.number(match[3])
	pull := f.pulls[number]
	if pull == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	var request struct {
		Event    string `json:"event"`
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	states := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}
	state, known := states[request.Event]
	switch {
	case !known:
		writeJSON(w, http.StatusUnprocessableEntity, validation("event is invalid"))
		return
	case state != "COMMENTED" && f.issues[number].user == f.login:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Unprocessable Entity",
			"errors": []any{"Can not approve your own pull request"}})
		return
	case request.Event != "APPROVE" && request.Body == "":
		writeJSON(w, http.StatusUnprocessableEntity, validation("body is required"))
		return
	case request.CommitID != "" && !slices.Contains(pull.shas, request.CommitID):
		writeJSON(w, http.StatusUnprocessableEntity, validation("commit_id is not part of the pull request"))
		return
	}
	commit := request.CommitID
	if commit == "" {
		commit = pull.sha
	}
	review := &fakeReview{id: f.nextID, user: f.login, body: request.Body, state: state, commit: commit, submitted: f.tick()}
	f.nextID++
	f.reviews[number] = append(f.reviews[number], review)
	writeJSON(w, http.StatusOK, f.reviewJSON(number, review))
}

func (f *fakeGitHub) mergePull(w http.ResponseWriter, r *http.Request, match []string) {
	number := f.number(match[3])
	pull := f.pulls[number]
	var request struct {
		SHA string `json:"sha"`
	}
	if pull == nil || decodeBody(r, &request) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	switch {
	case f.issues[number].state != "open":
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "Pull Request is not mergeable"})
		return
	case request.SHA != "" && request.SHA != pull.sha:
		writeJSON(w, http.StatusConflict, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
		return
	}
	now := f.tick()
	pull.merged = &now
	f.issues[number].state = "closed"
	f.issues[number].updated = now
	writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": strings.Repeat("d", 40), "message": "Pull Request successfully merged"})
}

func (f *fakeGitHub) getRef(w http.ResponseWriter, _ *http.Request, match []string) {
	sha := f.branches[match[3]]
	if !strings.EqualFold(match[1], f.owner) {
		sha = f.forks[match[1]+":"+match[3]]
	}
	if sha == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/heads/" + match[3], "object": map[string]any{"sha": sha, "type": "commit"}})
}

func (f *fakeGitHub) listCheckRuns(w http.ResponseWriter, r *http.Request, match []string) {
	runs := f.checkRuns[match[3]]
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": orEmpty(paginate(w, r, runs))})
}

func (f *fakeGitHub) combinedStatus(w http.ResponseWriter, r *http.Request, match []string) {
	statuses := f.statuses[match[3]]
	writeJSON(w, http.StatusOK, map[string]any{"state": "pending", "total_count": len(statuses), "statuses": orEmpty(paginate(w, r, statuses))})
}

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request, _ []string) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
		return
	}
	if strings.HasPrefix(request.Query, "query") {
		f.discussionQuery(w, request.Query, request.Variables)
		return
	}
	id, _ := request.Variables["id"].(string)
	for number, pull := range f.pulls {
		if pull.nodeID != id {
			continue
		}
		pull.draft = strings.Contains(request.Query, "convertPullRequestToDraft")
		f.issues[number].updated = f.tick()
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"pullRequest": map[string]any{"isDraft": pull.draft}}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + id + "'"}}})
}

// discussionField is one aliased discussion field of a repository query.
var discussionField = regexp.MustCompile(`(\w+): discussion\(number: ([0-9]+)\) \{ number \}`)

// discussionQuery answers a query of the repository's hasDiscussionsEnabled
// and of aliased discussion fields the way GitHub does: a number no
// discussion holds is null with a NOT_FOUND error at its path, and a
// credential that cannot read discussions gets null with FORBIDDEN.
func (f *fakeGitHub) discussionQuery(w http.ResponseWriter, query string, variables map[string]any) {
	owner, _ := variables["owner"].(string)
	name, _ := variables["name"].(string)
	if !f.ownRepository(owner, name) {
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": nil},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository"}, "message": "Could not resolve to a Repository"}}})
		return
	}
	repository := map[string]any{}
	if strings.Contains(query, "hasDiscussionsEnabled") {
		repository["hasDiscussionsEnabled"] = !f.discussionsOff
	}
	var errs []any
	for _, match := range discussionField.FindAllStringSubmatch(query, -1) {
		alias, number := match[1], f.number(match[2])
		switch {
		case f.discussionsForbidden:
			repository[alias] = nil
			errs = append(errs, map[string]any{"type": "FORBIDDEN", "path": []any{"repository", alias}, "message": "Resource not accessible by personal access token"})
		case f.discussions[number]:
			repository[alias] = map[string]any{"number": number}
		default:
			repository[alias] = nil
			errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{"repository", alias},
				"message": "Could not resolve to a Discussion with the number of " + match[2] + "."})
		}
	}
	answer := map[string]any{"data": map[string]any{"repository": repository}}
	if len(errs) > 0 {
		answer["errors"] = errs
	}
	writeJSON(w, http.StatusOK, answer)
}
