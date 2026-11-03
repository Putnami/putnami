package provider

import (
	"encoding/base64"
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

// fakeJira is an in-memory stand-in for the part of the Jira Cloud REST API v2
// the provider uses. It keeps the Jira rules the provider relies on — issue
// ids and per-project key numbers that are never reused, a workflow whose
// statuses carry a category and whose Done transition takes a resolution, the
// JQL the provider writes, token pages ordered by key, a search that shows a
// new issue only some time after its create was answered while a read by id
// or key shows it at once, issue properties set at creation — and injects
// faults: an answer lost after the write was applied, and a status answered
// before or after applying.
type fakeJira struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	project string
	account string
	email   string
	token   string
	clock   time.Time
	nextID  int
	nextKey int
	issues  map[string]*fakeIssue
	faults  []*fault
	calls   []string
	// searchLag is how long, in real time, a new issue stays out of the
	// search, the way Jira's index trails a create. Zero shows it at once.
	searchLag time.Duration
	// cancelOff is a workflow whose Done transition offers no Won't Do.
	cancelOff bool
	// resolutionRequired makes the Done transition require a resolution.
	resolutionRequired bool
}

type fakeIssue struct {
	id          int
	project     string
	number      int
	summary     string
	description *string
	status      string
	resolution  string
	labels      []string
	assignee    string
	creator     string
	properties  map[string]json.RawMessage
	created     time.Time
	updated     time.Time
	indexed     time.Time
	deleted     bool
}

func (i *fakeIssue) key() string { return i.project + "-" + strconv.Itoa(i.number) }

// fakeStatuses is the fake's workflow: every status reaches every other.
var fakeStatuses = []struct{ id, name, category string }{
	{"11", "To Do", "new"}, {"21", "In Progress", "indeterminate"}, {"31", "Done", "done"},
}

func categoryKeyOf(status string) string {
	for _, candidate := range fakeStatuses {
		if candidate.name == status {
			return candidate.category
		}
	}
	return "undefined"
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
	fakeEmail   = "bot@example.com"
	fakeToken   = "ATATT3xFfGF0FAKEtokenFAKEtokenFAKEtoken0000=ABCD"
	fakeAccount = "557058:bot"
)

func newFakeJira(t *testing.T) *fakeJira {
	f := &fakeJira{
		t: t, project: "ENG", account: fakeAccount, email: fakeEmail, token: fakeToken,
		clock:  time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC),
		nextID: 10000, nextKey: 1, issues: map[string]*fakeIssue{},
	}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJira) addFault(method, path string, fault fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fault.method = method
	fault.path = regexp.MustCompile(path)
	if fault.times == 0 {
		fault.times = 1
	}
	f.faults = append(f.faults, &fault)
}

func (f *fakeJira) count(prefix string) int {
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

func (f *fakeJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(f.email+":"+f.token)) {
		writeJSON(w, http.StatusUnauthorized, jiraError("Client must be authenticated to access this resource."))
		return
	}
	var active *fault
	for _, candidate := range f.faults {
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

func (f *fakeJira) answerFault(w http.ResponseWriter, active *fault) {
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

func jiraError(messages ...string) map[string]any {
	return map[string]any{"errorMessages": messages, "errors": map[string]string{}}
}

var routes = []struct {
	method  string
	pattern *regexp.Regexp
	handle  func(f *fakeJira, w http.ResponseWriter, r *http.Request, match []string)
}{
	{"GET", regexp.MustCompile(`^/rest/api/2/myself$`), (*fakeJira).myself},
	{"POST", regexp.MustCompile(`^/rest/api/2/search/jql$`), (*fakeJira).search},
	{"POST", regexp.MustCompile(`^/rest/api/2/issue$`), (*fakeJira).create},
	{"GET", regexp.MustCompile(`^/rest/api/2/issue/([^/]+)$`), (*fakeJira).get},
	{"PUT", regexp.MustCompile(`^/rest/api/2/issue/([^/]+)$`), (*fakeJira).edit},
	{"GET", regexp.MustCompile(`^/rest/api/2/issue/([^/]+)/transitions$`), (*fakeJira).transitions},
	{"POST", regexp.MustCompile(`^/rest/api/2/issue/([^/]+)/transitions$`), (*fakeJira).transition},
}

func (f *fakeJira) route(w http.ResponseWriter, r *http.Request) {
	for _, route := range routes {
		if route.method != r.Method {
			continue
		}
		if match := route.pattern.FindStringSubmatch(r.URL.Path); match != nil {
			route.handle(f, w, r, match)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, jiraError("no route "+r.Method+" "+r.URL.Path))
}

func (f *fakeJira) myself(w http.ResponseWriter, _ *http.Request, _ []string) {
	writeJSON(w, http.StatusOK, map[string]any{"accountId": f.account, "emailAddress": f.email})
}

// lookup finds an issue by id or key; a moved issue answers under its new key.
func (f *fakeJira) lookup(idOrKey string) *fakeIssue {
	if issue, ok := f.issues[idOrKey]; ok && !issue.deleted {
		return issue
	}
	for _, issue := range f.issues {
		if !issue.deleted && issue.key() == idOrKey {
			return issue
		}
	}
	return nil
}

func formatTime(t time.Time) string { return t.UTC().Format(jiraTime) }

func (f *fakeJira) render(issue *fakeIssue, properties []string) map[string]any {
	var resolution any
	if issue.resolution != "" {
		resolution = map[string]any{"name": issue.resolution}
	}
	var assignee any
	if issue.assignee != "" {
		assignee = map[string]any{"accountId": issue.assignee}
	}
	labels := issue.labels
	if labels == nil {
		labels = []string{}
	}
	rendered := map[string]any{
		"id": strconv.Itoa(issue.id), "key": issue.key(), "self": f.server.URL + "/rest/api/2/issue/" + strconv.Itoa(issue.id),
		"fields": map[string]any{
			"summary": issue.summary, "description": issue.description,
			"status":     map[string]any{"name": issue.status, "statusCategory": map[string]any{"key": categoryKeyOf(issue.status)}},
			"resolution": resolution, "labels": labels, "assignee": assignee,
			"creator": map[string]any{"accountId": issue.creator},
			"created": formatTime(issue.created), "updated": formatTime(issue.updated),
		},
	}
	if len(properties) > 0 {
		selected := map[string]json.RawMessage{}
		for _, name := range properties {
			if value, ok := issue.properties[name]; ok {
				selected[name] = value
			}
		}
		rendered["properties"] = selected
	}
	return rendered
}

func (f *fakeJira) get(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.lookup(match[1])
	if issue == nil {
		writeJSON(w, http.StatusNotFound, jiraError("Issue does not exist or you do not have permission to see it."))
		return
	}
	writeJSON(w, http.StatusOK, f.render(issue, strings.Split(r.URL.Query().Get("properties"), ",")))
}

// newIssue adds an issue as another client would.
func (f *fakeJira) newIssue(summary, description string, labels []string, creator string) *fakeIssue {
	issue := &fakeIssue{
		id: f.nextID, project: f.project, number: f.nextKey, summary: summary, status: "To Do",
		labels: slices.Clone(labels), creator: creator, properties: map[string]json.RawMessage{},
		created: f.clock, updated: f.clock, indexed: time.Now().Add(f.searchLag),
	}
	if description != "" {
		issue.description = &description
	}
	f.nextID++
	f.nextKey++
	f.issues[strconv.Itoa(issue.id)] = issue
	return issue
}

func (f *fakeJira) create(w http.ResponseWriter, r *http.Request, _ []string) {
	var request struct {
		Fields struct {
			Project     struct{ Key string }  `json:"project"`
			IssueType   struct{ Name string } `json:"issuetype"`
			Summary     string                `json:"summary"`
			Description *string               `json:"description"`
			Labels      []string              `json:"labels"`
		} `json:"fields"`
		Properties []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, jiraError(err.Error()))
		return
	}
	switch {
	case request.Fields.Project.Key != f.project:
		writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"project": "valid project is required"}})
		return
	case request.Fields.IssueType.Name != "Task":
		writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"issuetype": "Specify a valid issue type"}})
		return
	case strings.TrimSpace(request.Fields.Summary) == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"summary": "You must specify a summary of the issue."}})
		return
	}
	for _, label := range request.Fields.Labels {
		if strings.ContainsAny(label, " \t") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"labels": "The label '" + label + "' contains spaces which is invalid."}})
			return
		}
	}
	issue := f.newIssue(request.Fields.Summary, deref(request.Fields.Description), request.Fields.Labels, f.account)
	for _, property := range request.Properties {
		issue.properties[property.Key] = property.Value
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": strconv.Itoa(issue.id), "key": issue.key()})
}

func (f *fakeJira) edit(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.lookup(match[1])
	if issue == nil {
		writeJSON(w, http.StatusNotFound, jiraError("Issue does not exist or you do not have permission to see it."))
		return
	}
	var request struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, jiraError(err.Error()))
		return
	}
	for name, raw := range request.Fields {
		var err error
		switch name {
		case "summary":
			err = json.Unmarshal(raw, &issue.summary)
		case "description":
			var description string
			err = json.Unmarshal(raw, &description)
			issue.description = nil
			if description != "" {
				issue.description = &description
			}
		case "labels":
			var labels []string
			err = json.Unmarshal(raw, &labels)
			issue.labels = labels
		default:
			err = fmt.Errorf("field %s cannot be set", name)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{name: err.Error()}})
			return
		}
	}
	issue.updated = f.clock
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeJira) offered(issue *fakeIssue) []map[string]any {
	var transitions []map[string]any
	for _, status := range fakeStatuses {
		if status.name == issue.status {
			continue
		}
		transition := map[string]any{
			"id": status.id, "name": "To " + status.name,
			"to":     map[string]any{"name": status.name, "statusCategory": map[string]any{"key": status.category}},
			"fields": map[string]any{},
		}
		if status.category == "done" {
			allowedValues := []map[string]any{{"name": "Done"}, {"name": "Duplicate"}}
			if !f.cancelOff {
				allowedValues = append(allowedValues, map[string]any{"name": "Won't Do"})
			}
			transition["fields"] = map[string]any{"resolution": map[string]any{"required": f.resolutionRequired, "allowedValues": allowedValues}}
		}
		transitions = append(transitions, transition)
	}
	return transitions
}

func (f *fakeJira) transitions(w http.ResponseWriter, _ *http.Request, match []string) {
	issue := f.lookup(match[1])
	if issue == nil {
		writeJSON(w, http.StatusNotFound, jiraError("Issue does not exist or you do not have permission to see it."))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transitions": f.offered(issue)})
}

func (f *fakeJira) transition(w http.ResponseWriter, r *http.Request, match []string) {
	issue := f.lookup(match[1])
	if issue == nil {
		writeJSON(w, http.StatusNotFound, jiraError("Issue does not exist or you do not have permission to see it."))
		return
	}
	var request struct {
		Transition struct{ ID string } `json:"transition"`
		Fields     struct {
			Resolution *struct{ Name string } `json:"resolution"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, jiraError(err.Error()))
		return
	}
	for _, status := range fakeStatuses {
		if status.id != request.Transition.ID || status.name == issue.status {
			continue
		}
		issue.status = status.name
		issue.resolution = ""
		if status.category == "done" {
			issue.resolution = "Done"
			if request.Fields.Resolution != nil {
				issue.resolution = request.Fields.Resolution.Name
			} else if f.resolutionRequired {
				writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"resolution": "Resolution is required."}})
				return
			}
		}
		issue.updated = f.clock
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusBadRequest, jiraError("Transition id '"+request.Transition.ID+"' is not valid for this issue."))
}

func (f *fakeJira) search(w http.ResponseWriter, r *http.Request, _ []string) {
	var request struct {
		JQL           string   `json:"jql"`
		MaxResults    int      `json:"maxResults"`
		Fields        []string `json:"fields"`
		Properties    []string `json:"properties"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, jiraError(err.Error()))
		return
	}
	query, err := parseJQL(request.JQL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jiraError("Error in the JQL Query: "+err.Error()))
		return
	}
	// A token names the last key number of the page it follows, so it
	// positions a traversal whatever was created or deleted since.
	after := 0
	if request.NextPageToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(request.NextPageToken)
		if err == nil {
			after, err = strconv.Atoi(strings.TrimPrefix(string(decoded), "after:"))
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, jiraError("The provided next page token is invalid or expired."))
			return
		}
	}
	now := time.Now()
	var matched []*fakeIssue
	for _, issue := range f.issues {
		if !issue.deleted && !now.Before(issue.indexed) && query.matches(issue, f.account) {
			matched = append(matched, issue)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if query.descending {
			return matched[i].number > matched[j].number
		}
		return matched[i].number < matched[j].number
	})
	start := 0
	for start < len(matched) && after != 0 && (query.descending && matched[start].number >= after || !query.descending && matched[start].number <= after) {
		start++
	}
	size := request.MaxResults
	if size <= 0 {
		size = 50
	}
	end := min(start+size, len(matched))
	issues := make([]map[string]any, 0, end-start)
	for _, issue := range matched[start:end] {
		issues = append(issues, f.render(issue, request.Properties))
	}
	answer := map[string]any{"issues": issues, "isLast": end == len(matched)}
	if end < len(matched) {
		answer["nextPageToken"] = base64.StdEncoding.EncodeToString([]byte("after:" + strconv.Itoa(matched[end-1].number)))
	}
	writeJSON(w, http.StatusOK, answer)
}

// jqlQuery is the JQL the provider writes, parsed: clauses joined by AND,
// one of them possibly an OR of text clauses, then ORDER BY key.
type jqlQuery struct {
	project    string
	categories []int
	labels     []string
	creator    bool
	text       []string
	descending bool
}

var (
	jqlString   = `"((?:[^"\\]|\\.)*)"`
	jqlClause   = regexp.MustCompile(`^(?:project = ` + jqlString + `|labels = ` + jqlString + `|statusCategory IN \(([0-9, ]+)\)|creator = currentUser\(\)|\(summary ~ ` + jqlString + ` OR description ~ ` + jqlString + `\))`)
	jqlOrder    = regexp.MustCompile(`^ ORDER BY key (ASC|DESC)$`)
	jqlUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`)
)

func parseJQL(text string) (*jqlQuery, error) {
	query := &jqlQuery{}
	rest := text
	for {
		match := jqlClause.FindStringSubmatch(rest)
		if match == nil {
			return nil, fmt.Errorf("the fake cannot read %q", rest)
		}
		switch {
		case strings.HasPrefix(match[0], "project"):
			query.project = jqlUnescape.Replace(match[1])
		case strings.HasPrefix(match[0], "labels"):
			query.labels = append(query.labels, jqlUnescape.Replace(match[2]))
		case strings.HasPrefix(match[0], "statusCategory"):
			for _, id := range strings.Split(match[3], ",") {
				n, err := strconv.Atoi(strings.TrimSpace(id))
				if err != nil {
					return nil, err
				}
				query.categories = append(query.categories, n)
			}
		case strings.HasPrefix(match[0], "creator"):
			query.creator = true
		default:
			if match[4] != match[5] {
				return nil, fmt.Errorf("summary and description texts differ")
			}
			query.text = strings.Fields(jqlUnescape.Replace(match[4]))
		}
		rest = rest[len(match[0]):]
		if strings.HasPrefix(rest, " AND ") {
			rest = rest[len(" AND "):]
			continue
		}
		order := jqlOrder.FindStringSubmatch(rest)
		if order == nil {
			return nil, fmt.Errorf("the fake cannot read %q", rest)
		}
		query.descending = order[1] == "DESC"
		if query.project == "" && len(query.labels) == 0 && !query.creator {
			return nil, fmt.Errorf("unbounded JQL queries are not allowed here")
		}
		return query, nil
	}
}

var categoryNumbers = map[string]int{"undefined": 1, "new": 2, "done": 3, "indeterminate": 4}

func (q *jqlQuery) matches(issue *fakeIssue, account string) bool {
	if q.project != "" && issue.project != q.project {
		return false
	}
	if q.categories != nil && !slices.Contains(q.categories, categoryNumbers[categoryKeyOf(issue.status)]) {
		return false
	}
	for _, label := range q.labels {
		if !slices.ContainsFunc(issue.labels, func(name string) bool { return strings.EqualFold(name, label) }) {
			return false
		}
	}
	if q.creator && issue.creator != account {
		return false
	}
	if len(q.text) > 0 {
		haystack := strings.ToLower(issue.summary + " " + deref(issue.description))
		for _, word := range q.text {
			if !strings.Contains(haystack, word) {
				return false
			}
		}
	}
	return true
}
