package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLinear is a Linear GraphQL stand-in that answers the operations this
// provider sends, by operation name, for one organization and its teams.
type fakeLinear struct {
	t      *testing.T
	server *httptest.Server
	key    string

	mu      sync.Mutex
	urlKey  string
	teams   []fakeTeam
	states  []fakeState
	labels  []*fakeLabel
	issues  []*fakeIssue
	numbers map[string]int
	clock   time.Time
	// calls counts the requests of each operation.
	calls map[string]int
	// faults are consumed in order by the requests of their operation.
	faults map[string][]fault
	// normalize rewrites a stored description, as Linear's markdown
	// normalization may.
	normalize func(string) string
}

type fakeTeam struct{ id, key string }

type fakeState struct {
	id, name, kind, team string
	position             float64
}

type fakeLabel struct {
	id, name, team string
	group          bool
}

type fakeIssue struct {
	id, identifier, title, description, team, state, assignee string
	labels                                                    []string
	created, updated                                          time.Time
	trashed                                                   bool
}

// fault replaces one request's answer. before drops or answers the request
// without applying it; otherwise the request is applied, then its answer is
// dropped or replaced.
type fault struct {
	before bool
	drop   bool
	status int
	body   string
}

const (
	fakeKey   = "lin_api_fakeKEYfakeKEYfakeKEYfakeKEY0000"
	teamID    = "11111111-1111-4111-8111-111111111111"
	otherTeam = "22222222-2222-4222-8222-222222222222"
)

func newFakeLinear(t *testing.T) *fakeLinear {
	t.Helper()
	f := &fakeLinear{
		t: t, key: fakeKey, urlKey: "acme",
		teams:   []fakeTeam{{teamID, "ENG"}, {otherTeam, "OPS"}},
		numbers: map[string]int{}, calls: map[string]int{}, faults: map[string][]fault{},
		clock: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
	}
	for _, team := range []string{teamID, otherTeam} {
		short := team[:4]
		f.states = append(f.states,
			fakeState{"st-" + short + "-triage", "Triage", "triage", team, 0},
			fakeState{"st-" + short + "-backlog", "Backlog", "backlog", team, 1},
			fakeState{"st-" + short + "-review", "Ready", "unstarted", team, 3},
			fakeState{"st-" + short + "-todo", "Todo", "unstarted", team, 2},
			fakeState{"st-" + short + "-doing", "In Progress", "started", team, 4},
			fakeState{"st-" + short + "-done", "Done", "completed", team, 5},
			fakeState{"st-" + short + "-dup", "Duplicate", "duplicate", team, 7},
			fakeState{"st-" + short + "-canceled", "Canceled", "canceled", team, 6},
		)
	}
	f.labels = append(f.labels,
		&fakeLabel{id: "lb-workspace-bug", name: "Bug"},
		&fakeLabel{id: "lb-group-area", name: "Area", group: true},
	)
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLinear) count(operation string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[operation]
}

func (f *fakeLinear) inject(operation string, faults ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[operation] = append(f.faults[operation], faults...)
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != f.key {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Authentication required, not authenticated","extensions":{"code":"AUTHENTICATION_ERROR","type":"authentication error"}}]}`)
		return
	}
	var request struct {
		OperationName string          `json:"operationName"`
		Variables     json.RawMessage `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, `{"errors":[{"message":"bad request"}]}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls[request.OperationName]++
	var injected *fault
	if queue := f.faults[request.OperationName]; len(queue) > 0 {
		injected = &queue[0]
		f.faults[request.OperationName] = queue[1:]
	}
	f.mu.Unlock()
	if injected != nil && injected.before {
		f.answerFault(w, injected)
		return
	}
	f.mu.Lock()
	status, data := f.answer(request.OperationName, request.Variables)
	f.mu.Unlock()
	if injected != nil {
		f.answerFault(w, injected)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (f *fakeLinear) answerFault(w http.ResponseWriter, injected *fault) {
	if injected.drop {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	w.WriteHeader(injected.status)
	_, _ = io.WriteString(w, injected.body)
}

func graphQLErrors(message, code string) []byte {
	encoded, _ := json.Marshal(map[string]any{"errors": []any{map[string]any{"message": message, "extensions": map[string]any{"code": code}}}})
	return encoded
}

func dataOf(value any) []byte {
	encoded, _ := json.Marshal(map[string]any{"data": value})
	return encoded
}

// answer applies one operation; the caller holds f.mu.
func (f *fakeLinear) answer(operation string, raw json.RawMessage) (int, []byte) {
	var variables map[string]any
	_ = json.Unmarshal(raw, &variables)
	switch operation {
	case "PutnamiServed":
		var teams []any
		for _, team := range f.teams {
			if strings.EqualFold(team.key, str(variables["key"])) {
				teams = append(teams, map[string]any{"id": team.id, "key": team.key})
			}
		}
		return 200, dataOf(map[string]any{"organization": map[string]any{"urlKey": f.urlKey}, "teams": map[string]any{"nodes": orEmpty(teams)}})
	case "PutnamiIssue":
		issue := f.issue(str(variables["id"]))
		if issue == nil {
			return 400, graphQLErrors("Entity not found: Issue - Could not find referenced Issue.", "INPUT_ERROR")
		}
		return 200, dataOf(map[string]any{"issue": f.render(issue)})
	case "PutnamiIssues":
		return f.list(variables)
	case "PutnamiStates":
		var nodes []any
		for _, state := range f.states {
			if state.team == str(variables["team"]) {
				nodes = append(nodes, map[string]any{"id": state.id, "name": state.name, "type": state.kind, "position": state.position})
			}
		}
		return 200, dataOf(map[string]any{"workflowStates": map[string]any{"nodes": orEmpty(nodes)}})
	case "PutnamiLabels":
		return f.listLabels(variables)
	case "PutnamiCreateIssue":
		return f.createIssue(variables["input"].(map[string]any))
	case "PutnamiUpdateIssue":
		return f.updateIssue(str(variables["id"]), variables["input"].(map[string]any))
	case "PutnamiCreateLabel":
		input := variables["input"].(map[string]any)
		created := &fakeLabel{id: fmt.Sprintf("lb-%d", len(f.labels)+1), name: str(input["name"]), team: str(input["teamId"])}
		f.labels = append(f.labels, created)
		return 200, dataOf(map[string]any{"issueLabelCreate": map[string]any{"success": true, "issueLabel": f.renderLabel(created)}})
	}
	return 400, graphQLErrors("Unknown operation "+operation, "GRAPHQL_VALIDATION_FAILED")
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

func orEmpty(values []any) []any {
	if values == nil {
		return []any{}
	}
	return values
}

func (f *fakeLinear) issue(id string) *fakeIssue {
	for _, issue := range f.issues {
		if issue.id == id {
			return issue
		}
	}
	return nil
}

func (f *fakeLinear) state(id string) fakeState {
	for _, state := range f.states {
		if state.id == id {
			return state
		}
	}
	return fakeState{}
}

func (f *fakeLinear) teamKey(id string) string {
	for _, team := range f.teams {
		if team.id == id {
			return team.key
		}
	}
	return ""
}

func (f *fakeLinear) label(id string) *fakeLabel {
	for _, label := range f.labels {
		if label.id == id {
			return label
		}
	}
	return nil
}

func (f *fakeLinear) renderLabel(label *fakeLabel) map[string]any {
	rendered := map[string]any{"id": label.id, "name": label.name, "isGroup": label.group, "retiredAt": nil, "team": nil}
	if label.team != "" {
		rendered["team"] = map[string]any{"id": label.team}
	}
	return rendered
}

func (f *fakeLinear) render(issue *fakeIssue) map[string]any {
	state := f.state(issue.state)
	labels := []any{}
	for _, id := range issue.labels {
		if label := f.label(id); label != nil {
			labels = append(labels, map[string]any{"id": label.id, "name": label.name})
		}
	}
	var assignee any
	if issue.assignee != "" {
		assignee = map[string]any{"id": issue.assignee, "displayName": "user-" + issue.assignee[:4]}
	}
	return map[string]any{
		"id": issue.id, "identifier": issue.identifier, "title": issue.title, "description": issue.description,
		"url":       "https://linear.app/" + f.urlKey + "/issue/" + strings.ToLower(issue.identifier),
		"createdAt": issue.created.Format(time.RFC3339Nano), "updatedAt": issue.updated.Format(time.RFC3339Nano),
		"trashed":  issue.trashed,
		"state":    map[string]any{"id": state.id, "name": state.name, "type": state.kind},
		"team":     map[string]any{"id": issue.team, "key": f.teamKey(issue.team)},
		"labels":   map[string]any{"nodes": labels},
		"assignee": assignee,
	}
}

func (f *fakeLinear) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

var fakeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// newIssue adds an issue; the caller holds f.mu.
func (f *fakeLinear) newIssue(id, team, title, description, state string, labels []string) *fakeIssue {
	f.numbers[team]++
	now := f.tick()
	issue := &fakeIssue{
		id: id, identifier: fmt.Sprintf("%s-%d", f.teamKey(team), f.numbers[team]), title: title, description: description,
		team: team, state: state, labels: labels, created: now, updated: now,
	}
	f.issues = append(f.issues, issue)
	return issue
}

func (f *fakeLinear) createIssue(input map[string]any) (int, []byte) {
	id := str(input["id"])
	if !fakeUUID.MatchString(id) {
		return 400, graphQLErrors("Argument Validation Error - id must be a UUID", "INVALID_INPUT")
	}
	if f.issue(id) != nil {
		return 400, graphQLErrors("Entity already exists", "INPUT_ERROR")
	}
	team := str(input["teamId"])
	state := str(input["stateId"])
	if state == "" {
		state = "st-" + team[:4] + "-backlog"
	}
	description := str(input["description"])
	if f.normalize != nil {
		description = f.normalize(description)
	}
	issue := f.newIssue(id, team, str(input["title"]), description, state, stringList(input["labelIds"]))
	return 200, dataOf(map[string]any{"issueCreate": map[string]any{"success": true, "issue": f.render(issue)}})
}

func stringList(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, str(item))
	}
	return out
}

func (f *fakeLinear) updateIssue(id string, input map[string]any) (int, []byte) {
	issue := f.issue(id)
	if issue == nil {
		return 400, graphQLErrors("Entity not found: Issue", "INPUT_ERROR")
	}
	if title, ok := input["title"]; ok {
		issue.title = str(title)
	}
	if description, ok := input["description"]; ok {
		issue.description = str(description)
		if f.normalize != nil {
			issue.description = f.normalize(issue.description)
		}
	}
	if labels, ok := input["labelIds"]; ok {
		issue.labels = stringList(labels)
	}
	if state, ok := input["stateId"]; ok {
		issue.state = str(state)
	}
	issue.updated = f.tick()
	return 200, dataOf(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": f.render(issue)}})
}

// list answers the issues query for the filter subset this provider sends.
func (f *fakeLinear) list(variables map[string]any) (int, []byte) {
	filter, _ := variables["filter"].(map[string]any)
	first := int(variables["first"].(float64))
	after := str(variables["after"])
	var matched []*fakeIssue
	for _, issue := range f.issues {
		if !issue.trashed && f.matches(issue, filter) {
			matched = append(matched, issue)
		}
	}
	start := 0
	if after != "" {
		start = -1
		for i, issue := range matched {
			if issue.id == after {
				start = i + 1
			}
		}
		if start < 0 {
			return 400, graphQLErrors("Invalid cursor", "INVALID_INPUT")
		}
	}
	end := min(start+first, len(matched))
	nodes := []any{}
	for _, issue := range matched[start:end] {
		nodes = append(nodes, f.render(issue))
	}
	var endCursor any
	if end > start {
		endCursor = matched[end-1].id
	}
	return 200, dataOf(map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < len(matched), "endCursor": endCursor}}})
}

func (f *fakeLinear) matches(issue *fakeIssue, filter map[string]any) bool {
	for name, raw := range filter {
		condition, _ := raw.(map[string]any)
		switch name {
		case "team":
			if issue.team != str(condition["id"].(map[string]any)["eq"]) {
				return false
			}
		case "state":
			types := stringList(condition["type"].(map[string]any)["in"])
			if !slices.Contains(types, f.state(issue.state).kind) {
				return false
			}
		case "and":
			for _, member := range raw.([]any) {
				if !f.matches(issue, member.(map[string]any)) {
					return false
				}
			}
		case "or":
			found := false
			for _, member := range raw.([]any) {
				found = found || f.matches(issue, member.(map[string]any))
			}
			if !found {
				return false
			}
		case "labels":
			wanted := str(condition["some"].(map[string]any)["name"].(map[string]any)["eqIgnoreCase"])
			if !slices.ContainsFunc(issue.labels, func(id string) bool {
				label := f.label(id)
				return label != nil && strings.EqualFold(label.name, wanted)
			}) {
				return false
			}
		case "title", "description":
			text := issue.title
			if name == "description" {
				text = issue.description
			}
			if !strings.Contains(strings.ToLower(text), strings.ToLower(str(condition["containsIgnoreCase"]))) {
				return false
			}
		default:
			f.t.Errorf("the fake does not know issue filter %s", name)
			return false
		}
	}
	return true
}

func (f *fakeLinear) listLabels(variables map[string]any) (int, []byte) {
	filter, _ := variables["filter"].(map[string]any)
	nodes := []any{}
	for _, label := range f.labels {
		if f.labelMatches(label, filter) {
			nodes = append(nodes, f.renderLabel(label))
		}
	}
	return 200, dataOf(map[string]any{"issueLabels": map[string]any{"nodes": nodes}})
}

func (f *fakeLinear) labelMatches(label *fakeLabel, filter map[string]any) bool {
	for name, raw := range filter {
		switch name {
		case "isGroup":
			if label.group != raw.(map[string]any)["eq"].(bool) {
				return false
			}
		case "and":
			for _, member := range raw.([]any) {
				if !f.labelMatches(label, member.(map[string]any)) {
					return false
				}
			}
		case "or":
			found := false
			for _, member := range raw.([]any) {
				found = found || f.labelMatches(label, member.(map[string]any))
			}
			if !found {
				return false
			}
		case "team":
			condition := raw.(map[string]any)
			if null, ok := condition["null"]; ok {
				if (label.team == "") != null.(bool) {
					return false
				}
			} else if label.team != str(condition["id"].(map[string]any)["eq"]) {
				return false
			}
		case "name":
			if !strings.EqualFold(label.name, str(raw.(map[string]any)["eqIgnoreCase"])) {
				return false
			}
		default:
			f.t.Errorf("the fake does not know label filter %s", name)
			return false
		}
	}
	return true
}
