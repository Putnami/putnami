package provider

import (
	"encoding/base64"
	"slices"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/linear-collaboration/internal/linear"
)

// read reads the issue a reference names. A reference another source issued,
// an id that is not a UUID, an issue Linear does not know and a deleted issue
// are all not found.
func (s *session) read(ref collab.Ref) (*issue, *collab.Failure) {
	id, ours := s.issueID(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, reasonTaskMissing, "no task %s in source %s", ref.ID, ref.Source)
	}
	found, failure := s.lookup(id)
	if failure != nil {
		return nil, failure
	}
	if found == nil || found.trashed() {
		return nil, collab.Fail(collab.OutcomeNotFound, reasonTaskMissing, "no task %s in source %s", ref.ID, ref.Source)
	}
	return found, nil
}

// lookup reads an issue by id; nil when Linear does not know it.
func (s *session) lookup(id string) (*issue, *collab.Failure) {
	var data struct {
		Issue *issue `json:"issue"`
	}
	if err := s.client.Query(s.ctx, "PutnamiIssue", issueQuery, map[string]any{"id": id}, &data); err != nil {
		if err.Kind == linear.Answered && err.NotFound {
			return nil, nil
		}
		return nil, fail(err, "read issue %s", id)
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return nil, nil
	}
	return data.Issue, nil
}

func (s *session) get(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskRefInput](input)
	if failure != nil {
		return nil, failure
	}
	found, failure := s.read(in.Ref)
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(found)}, nil
}

// cursorPrefix starts every cursor this provider issues: the rest is Linear's
// end cursor, base64url encoded.
const cursorPrefix = "l."

func parseCursor(value string) (string, *collab.Failure) {
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, cursorPrefix))
	if !strings.HasPrefix(value, cursorPrefix) || err != nil || len(decoded) == 0 {
		return "", collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", value)
	}
	return string(decoded), nil
}

func formatCursor(linearCursor string) string {
	encoded := cursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(linearCursor))
	if len(encoded) > collab.MaxCursorLength {
		return ""
	}
	return encoded
}

// find lists the team's issues in Linear's creation order, one page of
// Linear's cursor pagination at a time, filtered by Linear itself.
func (s *session) find(input any) (any, *collab.Failure) {
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
	after, failure := parseCursor(cursor)
	if failure != nil {
		return nil, failure
	}
	result := &collab.TaskListResult{Items: []collab.Task{}}
	filter := map[string]any{"team": map[string]any{"id": map[string]any{"eq": s.team.ID}}}
	var conditions []any
	if len(in.States) > 0 {
		var types []string
		for _, state := range in.States {
			types = append(types, typesOf(state)...)
		}
		if len(types) == 0 {
			return result, nil
		}
		filter["state"] = map[string]any{"type": map[string]any{"in": types}}
	}
	for _, name := range in.Labels {
		conditions = append(conditions, map[string]any{"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eqIgnoreCase": name}}}})
	}
	if in.Query != "" {
		conditions = append(conditions, map[string]any{"or": []any{
			map[string]any{"title": map[string]any{"containsIgnoreCase": in.Query}},
			map[string]any{"description": map[string]any{"containsIgnoreCase": in.Query}},
		}})
	}
	if len(conditions) > 0 {
		filter["and"] = conditions
	}
	variables := map[string]any{"filter": filter, "first": size}
	if after != "" {
		variables["after"] = after
	}
	var data struct {
		Issues struct {
			Nodes    []issue `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	}
	if err := s.client.Query(s.ctx, "PutnamiIssues", issuesQuery, variables, &data); err != nil {
		return nil, fail(err, "list the issues of team %s", s.team.Key)
	}
	for i := range data.Issues.Nodes {
		if len(result.Items) == size {
			break
		}
		result.Items = append(result.Items, s.task(&data.Issues.Nodes[i]))
	}
	if data.Issues.PageInfo.HasNextPage && data.Issues.PageInfo.EndCursor != "" {
		result.Page.Next = formatCursor(data.Issues.PageInfo.EndCursor)
	}
	return result, nil
}

// createRequest is what a create's digest covers.
type createRequest struct {
	Title  string           `json:"title"`
	Body   string           `json:"body"`
	Labels []string         `json:"labels"`
	State  collab.TaskState `json:"state"`
}

// create opens an issue whose id derives from the idempotency key and whose
// description records the request's digest. It reads that id first, so a
// repeat returns the issue; after a mutation whose outcome is unknown it reads
// the id back, and answers unresolved rather than sending it again.
func (s *session) create(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskCreateInput](input)
	if failure != nil {
		return nil, failure
	}
	state := in.State
	if state == "" {
		state = collab.TaskStateOpen
	}
	if targetType(state) == "" {
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped, "Linear has no workflow state type for %s", state)
	}
	id := keyedID(s.team.ID, in.IdempotencyKey)
	requestDigest := collabhelper.Digest(createRequest{in.Title, in.Body, labelKeys(in.Labels), state})

	existing, failure := s.lookup(id)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		return s.replay(existing, requestDigest, in.IdempotencyKey)
	}

	request := map[string]any{"id": id, "teamId": s.team.ID, "title": in.Title, "description": withMarker(in.Body, requestDigest)}
	if len(in.Labels) > 0 {
		ids, failure := s.labelIDs(in.Labels)
		if failure != nil {
			return nil, failure
		}
		request["labelIds"] = ids
	}
	if state != collab.TaskStateOpen {
		target, failure := s.firstState(s.team.ID, state)
		if failure != nil {
			return nil, failure
		}
		request["stateId"] = target.ID
	}
	var data struct {
		IssueCreate struct {
			Issue *issue `json:"issue"`
		} `json:"issueCreate"`
	}
	err := s.client.Mutate(s.ctx, "PutnamiCreateIssue", createIssueMutation, map[string]any{"input": request}, &data)
	if err == nil && (data.IssueCreate.Issue == nil || data.IssueCreate.Issue.ID != id) {
		err = &linear.Error{Kind: linear.Uncertain, Message: "Linear acknowledged the issue without naming it"}
	}
	if err == nil {
		return &collab.TaskCreateResult{Task: s.task(data.IssueCreate.Issue), Created: true}, nil
	}
	if err.Kind != linear.Uncertain {
		// A concurrent create with the same key holds the id: answer from it.
		if found, failure := s.lookup(id); failure == nil && found != nil {
			return s.replay(found, requestDigest, in.IdempotencyKey)
		}
		return nil, fail(err, "open an issue in team %s", s.team.Key)
	}
	var created *issue
	if !s.reconcile(func() (bool, *collab.Failure) {
		found, failure := s.lookup(id)
		created = found
		return found != nil, failure
	}) {
		return nil, unresolved("repeat tasks.create with the same idempotencyKey: the key names the issue's id, so the repeat returns the issue if it was opened",
			"opening the issue in team %s may have happened (%s), and reading issue %s back did not find it", s.team.Key, err.Message, id)
	}
	if _, recorded := splitMarker(created.description()); recorded != "" && recorded != requestDigest {
		return s.replay(created, requestDigest, in.IdempotencyKey)
	}
	return &collab.TaskCreateResult{Task: s.task(created), Created: true}, nil
}

// replay answers a create whose key names an issue that exists. The id
// proves the key; the marker, when the description still carries it, tells
// whether the request is the same.
func (s *session) replay(existing *issue, requestDigest, key string) (any, *collab.Failure) {
	if existing.trashed() {
		return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
			"idempotency key %q opened %s, which was deleted; use another key", key, existing.Identifier)
	}
	if _, recorded := splitMarker(existing.description()); recorded != "" && recorded != requestDigest {
		return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
			"idempotency key %q already opened %s for a different request", key, existing.Identifier)
	}
	return &collab.TaskCreateResult{Task: s.task(existing), Created: false}, nil
}

func (s *session) checkRevision(found *issue, expected string) *collab.Failure {
	if current := found.revision(); expected != "" && current != expected {
		failure := collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, "task %s is at revision %s, not %s", found.Identifier, current, expected)
		failure.Error.Current = current
		return failure
	}
	return nil
}

// write applies an update to an issue and returns the issue as Linear answers
// it. When the outcome is unknown, it reads the issue back and accepts it when
// applied reports that the update is in place.
func (s *session) write(found *issue, input map[string]any, operation string, applied func(*issue) bool) (*issue, *collab.Failure) {
	var data struct {
		IssueUpdate struct {
			Issue *issue `json:"issue"`
		} `json:"issueUpdate"`
	}
	err := s.client.Mutate(s.ctx, "PutnamiUpdateIssue", updateIssueMutation, map[string]any{"id": found.ID, "input": input}, &data)
	if err == nil && (data.IssueUpdate.Issue == nil || data.IssueUpdate.Issue.ID != found.ID) {
		err = &linear.Error{Kind: linear.Uncertain, Message: "Linear acknowledged the update without the issue"}
	}
	if err == nil {
		return data.IssueUpdate.Issue, nil
	}
	if err.Kind != linear.Uncertain {
		return nil, fail(err, "%s %s", operation, found.Identifier)
	}
	var current *issue
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.lookup(found.ID)
		current = read
		return read != nil && applied(read), failure
	}) {
		return current, nil
	}
	return nil, unresolved("read the task with tasks.get and compare it with the request before any retry",
		"the %s of %s may have happened (%s), and reading it back did not show it", operation, found.Identifier, err.Message)
}

// update changes title, body or labels. The body keeps its digest marker, and
// labels replace the issue's whole label set.
func (s *session) update(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskUpdateInput](input)
	if failure != nil {
		return nil, failure
	}
	found, failure := s.read(in.Ref)
	if failure != nil {
		return nil, failure
	}
	if failure := s.checkRevision(found, in.ExpectedRevision); failure != nil {
		return nil, failure
	}
	patch := map[string]any{}
	title, text := found.Title, ""
	if in.Title != nil && *in.Title != found.Title {
		title = *in.Title
		patch["title"] = title
	}
	if in.Body != nil {
		text, _ = splitMarker(*in.Body)
		if joined := rejoin(found.description(), *in.Body); joined != found.description() {
			patch["description"] = joined
		}
	}
	var wanted []string
	if in.Labels != nil {
		ids, failure := s.labelIDs(*in.Labels)
		if failure != nil {
			return nil, failure
		}
		if !collabhelper.SameSet(ids, found.labelIDs()) {
			wanted = ids
			patch["labelIds"] = ids
		}
	}
	if len(patch) == 0 {
		return &collab.TaskResult{Task: s.task(found)}, nil
	}
	updated, failure := s.write(found, patch, "update", func(current *issue) bool {
		body, _ := splitMarker(current.description())
		return current.Title == title &&
			(in.Body == nil || strings.TrimSpace(body) == strings.TrimSpace(text)) &&
			(wanted == nil || collabhelper.SameSet(current.labelIDs(), wanted))
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// transition moves an issue to the first workflow state of its team that has
// the target type. A task already in the state is left as it is.
func (s *session) transition(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskTransitionInput](input)
	if failure != nil {
		return nil, failure
	}
	found, failure := s.read(in.Ref)
	if failure != nil {
		return nil, failure
	}
	if failure := s.checkRevision(found, in.ExpectedRevision); failure != nil {
		return nil, failure
	}
	if targetType(in.State) == "" {
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped, "Linear has no workflow state type for %s", in.State)
	}
	if stateOf(found.State.Type) == in.State {
		return &collab.TaskResult{Task: s.task(found)}, nil
	}
	target, failure := s.firstState(found.Team.ID, in.State)
	if failure != nil {
		return nil, failure
	}
	updated, failure := s.write(found, map[string]any{"stateId": target.ID}, "transition to "+string(in.State), func(current *issue) bool {
		return stateOf(current.State.Type) == in.State
	})
	if failure != nil {
		return nil, failure
	}
	if stateOf(updated.State.Type) != in.State {
		return nil, unresolved("read the task with tasks.get before any retry",
			"Linear accepted the transition of %s to %s and reports it in state %s", found.Identifier, in.State, updated.State.Name)
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// firstState is the first workflow state, by position, of a team that has
// the type a transition to state moves to.
func (s *session) firstState(teamID string, state collab.TaskState) (workflowState, *collab.Failure) {
	states, cached := s.states[teamID]
	if !cached {
		var data struct {
			WorkflowStates struct {
				Nodes []workflowState `json:"nodes"`
			} `json:"workflowStates"`
		}
		if err := s.client.Query(s.ctx, "PutnamiStates", statesQuery, map[string]any{"team": teamID}, &data); err != nil {
			return workflowState{}, fail(err, "read the workflow states of team %s", teamID)
		}
		states = data.WorkflowStates.Nodes
		sort.SliceStable(states, func(a, b int) bool { return states[a].Position < states[b].Position })
		s.states[teamID] = states
	}
	wanted := targetType(state)
	for _, candidate := range states {
		if candidate.Type == wanted {
			return candidate, nil
		}
	}
	return workflowState{}, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped,
		"the team has no workflow state of type %s, which %s moves to", wanted, state)
}

// labelIDs resolves label names, ignoring case, among the labels the team can
// apply: its own and the workspace's, never a label group or a retired label.
// A team label matches before a workspace label. A name no label carries
// creates that label in the team.
func (s *session) labelIDs(names []string) ([]string, *collab.Failure) {
	if len(names) == 0 {
		return []string{}, nil
	}
	nameConditions := make([]any, 0, len(names))
	for _, name := range names {
		nameConditions = append(nameConditions, map[string]any{"name": map[string]any{"eqIgnoreCase": name}})
	}
	filter := map[string]any{
		"isGroup": map[string]any{"eq": false},
		"and": []any{
			map[string]any{"or": []any{
				map[string]any{"team": map[string]any{"id": map[string]any{"eq": s.team.ID}}},
				map[string]any{"team": map[string]any{"null": true}},
			}},
			map[string]any{"or": nameConditions},
		},
	}
	var data struct {
		IssueLabels struct {
			Nodes []label `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := s.client.Query(s.ctx, "PutnamiLabels", labelsQuery, map[string]any{"filter": filter}, &data); err != nil {
		return nil, fail(err, "read the labels of team %s", s.team.Key)
	}
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id := ""
		for _, candidate := range data.IssueLabels.Nodes {
			if candidate.IsGroup || candidate.RetiredAt != nil || !strings.EqualFold(candidate.Name, name) {
				continue
			}
			if id == "" || (candidate.Team != nil && candidate.Team.ID == s.team.ID) {
				id = candidate.ID
			}
		}
		if id == "" {
			created, failure := s.createLabel(name)
			if failure != nil {
				return nil, failure
			}
			id = created.ID
			data.IssueLabels.Nodes = append(data.IssueLabels.Nodes, created)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// createLabel creates a label in the team. A creation whose outcome is
// unknown answers unresolved with reason linear.incomplete: no task changed,
// and a repeat resolves the label by its name if it was created.
func (s *session) createLabel(name string) (label, *collab.Failure) {
	var data struct {
		IssueLabelCreate struct {
			IssueLabel *label `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	err := s.client.Mutate(s.ctx, "PutnamiCreateLabel", createLabelMutation,
		map[string]any{"input": map[string]any{"name": name, "teamId": s.team.ID}}, &data)
	if err == nil && (data.IssueLabelCreate.IssueLabel == nil || data.IssueLabelCreate.IssueLabel.ID == "") {
		err = &linear.Error{Kind: linear.Uncertain, Message: "Linear acknowledged the label without naming it"}
	}
	if err == nil {
		return *data.IssueLabelCreate.IssueLabel, nil
	}
	if err.Kind != linear.Uncertain {
		return label{}, fail(err, "create label %q in team %s", name, s.team.Key)
	}
	failure := unresolved("repeat the request: a label is resolved by its name, so the repeat uses the label if it was created",
		"creating label %q in team %s may have happened (%s); no task was changed", name, s.team.Key, err.Message)
	failure.Error.Reason = reasonIncomplete
	return label{}, failure
}

// labelKeys is the set of label names as label resolution sees it: case
// folded, sorted, without repeats, and empty rather than absent.
func labelKeys(names []string) []string {
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, strings.ToLower(name))
	}
	sort.Strings(keys)
	return slices.Compact(keys)
}
