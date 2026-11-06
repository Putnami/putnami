package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitLab is an in-memory stand-in for the part of the GitLab REST API v4
// the provider uses. It keeps the GitLab rules the provider relies on — a
// project addressed by its URL-encoded full path, per-project iids and
// global ids that are never reused, labels sent as a comma-separated string
// and created on first use, opened and closed states changed through
// state_event, assignee_ids where 0 means none, lists ordered by creation
// time then id and paged with X-Next-Page, filters on state, labels,
// author, creation time and text that read the store at once, creation times to the
// microsecond shown to the millisecond, ties in creation time listed by id
// descending, quick actions run from description lines and removed from
// them — and injects faults: an
// answer lost after the write was applied, and a status answered before or
// after applying.
type fakeGitLab struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	path      string
	projectID int64
	token     string
	jobToken  string
	me        int64
	users     map[int64]string
	clock     time.Time
	nextID    int64
	nextIID   int
	issues    map[int]*fakeIssue
	faults    []*fault
	calls     []string
	// singleAssignee keeps only the first assignee, as a tier without
	// multiple assignees does.
	singleAssignee bool
	// onPut, when set, answers a PUT with the fault it returns, before
	// applying it.
	onPut func() *fault
	// queries records the query string of every list request.
	queries []string
	// onList, when set, runs before a list request is answered.
	onList func()
}

type fakeIssue struct {
	id          int64
	iid         int
	title       string
	description string
	state       string
	labels      []string
	assignees   []int64
	author      int64
	created     time.Time
	updated     time.Time
	duplicateOf int
	deleted     bool
}

type fault struct {
	method string
	path   *regexp.Regexp
	times  int
	// before answers without applying the request; otherwise the request is
	// applied first.
	before bool
	// drop closes the connection without an answer.
	drop   bool
	status int
	body   string
	header map[string]string
}

const (
	fakeProject  = "acme/platform/app"
	fakeToken    = "glpat-FAKEtokenFAKEtoken0001"
	fakeJobToken = "glcbt-FAKEjobTOKENfakeJOBtoken02"
	fakeHost     = "gitlab.example.com"
	botID        = 7
	otherID      = 8
)

func newFakeGitLab(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{
		t: t, path: fakeProject, projectID: 42, token: fakeToken, jobToken: fakeJobToken, me: botID,
		users:  map[int64]string{botID: "bot", otherID: "someone", 9: "alice", 10: "bob"},
		clock:  time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC),
		nextID: 5000, nextIID: 1, issues: map[int]*fakeIssue{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitLab) addFault(method, path string, fault fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fault.method = method
	fault.path = regexp.MustCompile(path)
	if fault.times == 0 {
		fault.times = 1
	}
	f.faults = append(f.faults, &fault)
}

// count reports how many requests started with prefix ("PUT /api/v4/...").
func (f *fakeGitLab) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitLab) writes() int {
	return f.count("POST ") + f.count("PUT ")
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token && (f.jobToken == "" || r.Header.Get("JOB-TOKEN") != f.jobToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
		return
	}
	if r.Method == http.MethodGet {
		f.queries = append(f.queries, r.URL.RawQuery)
	}
	var active *fault
	if r.Method == http.MethodPut && f.onPut != nil {
		active = f.onPut()
	}
	for _, candidate := range f.faults {
		if active != nil {
			break
		}
		if candidate.times > 0 && candidate.method == r.Method && candidate.path.MatchString(r.URL.Path) {
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
	f.clock = f.clock.Add(time.Second)
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

func (f *fakeGitLab) answerFault(w http.ResponseWriter, active *fault) {
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

var (
	issuesRoute = regexp.MustCompile(`^/api/v4/projects/([^/]+)/issues$`)
	issueRoute  = regexp.MustCompile(`^/api/v4/projects/([^/]+)/issues/([0-9]+)$`)
)

func (f *fakeGitLab) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	switch {
	case r.Method == http.MethodGet && path == "/api/v4/user":
		writeJSON(w, http.StatusOK, f.user(f.me))
		return
	case r.Method == http.MethodGet && path == "/api/v4/users":
		var found []any
		for id, name := range f.users {
			if strings.EqualFold(name, r.URL.Query().Get("username")) {
				found = append(found, f.user(id))
			}
		}
		writeJSON(w, http.StatusOK, append([]any{}, found...))
		return
	}
	if match := issuesRoute.FindStringSubmatch(path); match != nil {
		if !f.project(w, match[1]) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.list(w, r.URL.Query())
		case http.MethodPost:
			f.create(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
		}
		return
	}
	if match := issueRoute.FindStringSubmatch(path); match != nil {
		if !f.project(w, match[1]) {
			return
		}
		iid, _ := strconv.Atoi(match[2])
		issue := f.issues[iid]
		if issue == nil || issue.deleted {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, f.render(issue))
		case http.MethodPut:
			f.edit(w, r, issue)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
		}
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 no route " + r.Method + " " + path})
}

// project answers 404 for any project but the fake's, addressed by its
// URL-encoded full path.
func (f *fakeGitLab) project(w http.ResponseWriter, escaped string) bool {
	if escaped != url.PathEscape(f.path) {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Project Not Found"})
		return false
	}
	return true
}

func (f *fakeGitLab) user(id int64) map[string]any {
	return map[string]any{"id": id, "username": f.users[id], "name": f.users[id]}
}

func (f *fakeGitLab) render(issue *fakeIssue) map[string]any {
	assignees := []any{}
	for _, id := range issue.assignees {
		assignees = append(assignees, f.user(id))
	}
	links := map[string]any{"self": fmt.Sprintf("%s/api/v4/projects/%d/issues/%d", f.server.URL, f.projectID, issue.iid)}
	if issue.duplicateOf > 0 {
		links["closed_as_duplicate_of"] = fmt.Sprintf("%s/api/v4/projects/%d/issues/%d", f.server.URL, f.projectID, issue.duplicateOf)
	}
	description := any(issue.description)
	if issue.description == "" {
		description = nil
	}
	return map[string]any{
		"id": issue.id, "iid": issue.iid, "project_id": f.projectID, "title": issue.title, "description": description,
		"state": issue.state, "labels": append([]string{}, issue.labels...), "assignees": assignees, "author": f.user(issue.author),
		"web_url":    fmt.Sprintf("https://%s/%s/-/issues/%d", fakeHost, f.path, issue.iid),
		"created_at": issue.created.UTC().Format("2006-01-02T15:04:05.000Z07:00"), "updated_at": issue.updated.Format("2006-01-02T15:04:05.000Z07:00"),
		"_links": links, "user_notes_count": 0,
	}
}

// newIssue adds an issue as author, the way any client would.
func (f *fakeGitLab) newIssue(title, description string, labels []string, author int64) *fakeIssue {
	issue := &fakeIssue{
		id: f.nextID, iid: f.nextIID, title: title, description: description, state: "opened",
		labels: splitLabels(strings.Join(labels, ",")), author: author, created: f.clock, updated: f.clock,
	}
	f.nextID++
	f.nextIID++
	f.issues[issue.iid] = issue
	return issue
}

func splitLabels(value string) []string {
	labels := []string{}
	for _, label := range strings.Split(value, ",") {
		if label = strings.TrimSpace(label); label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

func (f *fakeGitLab) list(w http.ResponseWriter, query url.Values) {
	if f.onList != nil {
		f.onList()
	}
	var matched []*fakeIssue
	for _, issue := range f.issues {
		if issue.deleted {
			continue
		}
		if state := query.Get("state"); state != "" && state != "all" && state != issue.state {
			continue
		}
		if author := query.Get("author_id"); author != "" && author != strconv.FormatInt(issue.author, 10) {
			continue
		}
		if after := query.Get("created_after"); after != "" {
			at, err := time.Parse(time.RFC3339Nano, after)
			if err != nil {
				f.t.Errorf("created_after %q: %v", after, err)
			}
			if issue.created.Before(at) {
				continue
			}
		}
		if labels := query.Get("labels"); labels != "" {
			all := true
			for _, label := range strings.Split(labels, ",") {
				if !slices.ContainsFunc(issue.labels, func(have string) bool { return strings.EqualFold(have, label) }) {
					all = false
				}
			}
			if !all {
				continue
			}
		}
		if search := strings.ToLower(query.Get("search")); search != "" {
			in := query.Get("in")
			if in == "" {
				in = "title,description"
			}
			found := (strings.Contains(in, "title") && strings.Contains(strings.ToLower(issue.title), search)) ||
				(strings.Contains(in, "description") && strings.Contains(strings.ToLower(issue.description), search))
			if !found {
				continue
			}
		}
		matched = append(matched, issue)
	}
	// GitLab orders by creation time, then by id descending whichever way
	// the time is sorted.
	descending := query.Get("sort") == "desc"
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created) != descending
		}
		return a.id > b.id
	})
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	if perPage <= 0 {
		perPage = 20
	}
	page, _ := strconv.Atoi(query.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(matched))
	end := min(start+perPage, len(matched))
	out := []any{}
	for _, issue := range matched[start:end] {
		out = append(out, f.render(issue))
	}
	if end < len(matched) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	} else {
		w.Header().Set("X-Next-Page", "")
	}
	writeJSON(w, http.StatusOK, out)
}

// quickActions runs the quick actions /close, /reopen and /label ~name of a
// description, as GitLab does, and returns the description without them.
func (f *fakeGitLab) quickActions(issue *fakeIssue, description string) string {
	var kept []string
	for _, line := range strings.Split(description, "\n") {
		command, argument, _ := strings.Cut(line, " ")
		switch command {
		case "/close":
			issue.state = "closed"
		case "/reopen":
			issue.state = "opened"
		case "/label":
			issue.labels = splitLabels(strings.Join(issue.labels, ",") + "," + strings.TrimPrefix(argument, "~"))
		default:
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

type fakeEdit struct {
	Title       *string  `json:"title"`
	Description *string  `json:"description"`
	Labels      *string  `json:"labels"`
	StateEvent  string   `json:"state_event"`
	AssigneeIDs *[]int64 `json:"assignee_ids"`
}

func (f *fakeGitLab) decode(w http.ResponseWriter, r *http.Request) (fakeEdit, bool) {
	var edit fakeEdit
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&edit); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body is invalid: " + err.Error()})
		return edit, false
	}
	return edit, true
}

func (f *fakeGitLab) create(w http.ResponseWriter, r *http.Request) {
	edit, ok := f.decode(w, r)
	if !ok {
		return
	}
	if edit.Title == nil || strings.TrimSpace(*edit.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": map[string][]string{"title": {"can't be blank"}}})
		return
	}
	labels := ""
	if edit.Labels != nil {
		labels = *edit.Labels
	}
	issue := f.newIssue(*edit.Title, "", strings.Split(labels, ","), f.me)
	issue.description = f.quickActions(issue, deref(edit.Description))
	writeJSON(w, http.StatusCreated, f.render(issue))
}

func (f *fakeGitLab) edit(w http.ResponseWriter, r *http.Request, issue *fakeIssue) {
	edit, ok := f.decode(w, r)
	if !ok {
		return
	}
	if edit.Title != nil {
		if strings.TrimSpace(*edit.Title) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": map[string][]string{"title": {"can't be blank"}}})
			return
		}
		issue.title = *edit.Title
	}
	if edit.Description != nil {
		issue.description = f.quickActions(issue, *edit.Description)
	}
	if edit.Labels != nil {
		issue.labels = splitLabels(*edit.Labels)
	}
	switch edit.StateEvent {
	case "close":
		issue.state = "closed"
	case "reopen":
		issue.state = "opened"
		issue.duplicateOf = 0
	case "":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state_event does not have a valid value"})
		return
	}
	if edit.AssigneeIDs != nil {
		issue.assignees = nil
		for _, id := range *edit.AssigneeIDs {
			if _, known := f.users[id]; known && !slices.Contains(issue.assignees, id) {
				issue.assignees = append(issue.assignees, id)
			}
		}
		if f.singleAssignee && len(issue.assignees) > 1 {
			issue.assignees = issue.assignees[:1]
		}
	}
	issue.updated = f.clock
	writeJSON(w, http.StatusOK, f.render(issue))
}
