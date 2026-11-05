package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
)

// The GraphQL documents this provider sends. Each names its operation, which
// Linear and a stand-in both read from the request's operationName.
const (
	issueFields = `id identifier title description url createdAt updatedAt trashed
state { id name type } team { id key } labels(first: 100) { nodes { id name } } assignee { id displayName }`

	servedQuery = `query PutnamiServed($key: String!) {
  organization { urlKey }
  teams(filter: { key: { eqIgnoreCase: $key } }, first: 5) { nodes { id key } }
}`

	issueQuery = `query PutnamiIssue($id: String!) { issue(id: $id) { ` + issueFields + ` } }`

	issuesQuery = `query PutnamiIssues($filter: IssueFilter!, $first: Int!, $after: String) {
  issues(filter: $filter, first: $first, after: $after, orderBy: createdAt) {
    nodes { ` + issueFields + ` }
    pageInfo { hasNextPage endCursor }
  }
}`

	statesQuery = `query PutnamiStates($team: ID!) {
  workflowStates(filter: { team: { id: { eq: $team } } }, first: 250) { nodes { id name type position } }
}`

	labelsQuery = `query PutnamiLabels($filter: IssueLabelFilter!) {
  issueLabels(filter: $filter, first: 250) { nodes { id name isGroup retiredAt team { id } } }
}`

	createIssueMutation = `mutation PutnamiCreateIssue($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { ` + issueFields + ` } }
}`

	updateIssueMutation = `mutation PutnamiUpdateIssue($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success issue { ` + issueFields + ` } }
}`

	createLabelMutation = `mutation PutnamiCreateLabel($input: IssueLabelCreateInput!) {
  issueLabelCreate(input: $input) { success issueLabel { id name isGroup retiredAt team { id } } }
}`
)

// team is the team a binding serves.
type team struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// workflowState is one state of a team's workflow.
type workflowState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// label is one issue label.
type label struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	IsGroup   bool    `json:"isGroup"`
	RetiredAt *string `json:"retiredAt"`
	Team      *team   `json:"team"`
}

// issue is a Linear issue as issueFields reads it.
type issue struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	URL         string  `json:"url"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	Trashed     *bool   `json:"trashed"`
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Team   team `json:"team"`
	Labels struct {
		Nodes []label `json:"nodes"`
	} `json:"labels"`
	Assignee *struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
}

func (i *issue) description() string {
	if i.Description == nil {
		return ""
	}
	return *i.Description
}

func (i *issue) trashed() bool { return i.Trashed != nil && *i.Trashed }

func (i *issue) labelNames() []string {
	names := make([]string, 0, len(i.Labels.Nodes))
	for _, node := range i.Labels.Nodes {
		names = append(names, node.Name)
	}
	return names
}

func (i *issue) labelIDs() []string {
	ids := make([]string, 0, len(i.Labels.Nodes))
	for _, node := range i.Labels.Nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

func (i *issue) assigneeIDs() []string {
	if i.Assignee == nil || i.Assignee.ID == "" {
		return []string{}
	}
	return []string{i.Assignee.ID}
}

// revision covers everything a task shows and a caller can change: title, the
// whole description, the workflow state, labels and assignees.
func (i *issue) revision() string {
	labels := i.labelNames()
	sort.Strings(labels)
	assignees := i.assigneeIDs()
	sort.Strings(assignees)
	return "v" + collabhelper.Digest([]any{i.Title, i.description(), i.State.Name, labels, assignees})[:32]
}

// stateOf maps a workflow state type onto the contract's states. A type this
// provider does not know reads as open.
func stateOf(stateType string) collab.TaskState {
	switch stateType {
	case "started":
		return collab.TaskStateInProgress
	case "completed":
		return collab.TaskStateDone
	case "canceled", "duplicate":
		return collab.TaskStateCanceled
	}
	return collab.TaskStateOpen
}

// typesOf lists the workflow state types that read as a state.
func typesOf(state collab.TaskState) []string {
	switch state {
	case collab.TaskStateOpen:
		return []string{"triage", "backlog", "unstarted"}
	case collab.TaskStateInProgress:
		return []string{"started"}
	case collab.TaskStateDone:
		return []string{"completed"}
	case collab.TaskStateCanceled:
		return []string{"canceled", "duplicate"}
	}
	return nil
}

// targetType is the workflow state type a transition to a state moves to;
// empty for a state Linear has no type for.
func targetType(state collab.TaskState) string {
	switch state {
	case collab.TaskStateOpen:
		return "unstarted"
	case collab.TaskStateInProgress:
		return "started"
	case collab.TaskStateDone:
		return "completed"
	case collab.TaskStateCanceled:
		return "canceled"
	}
	return ""
}

// task renders an issue as a task.
func (s *session) task(i *issue) collab.Task {
	body, _ := splitMarker(i.description())
	assignees := []string{}
	if i.Assignee != nil {
		name := token(i.Assignee.DisplayName)
		if name == "" {
			name = token(i.Assignee.ID)
		}
		if name != "" {
			assignees = append(assignees, name)
		}
	}
	state := stateOf(i.State.Type)
	return collab.Task{
		Ref:           s.ref(i.ID),
		Key:           token(i.Identifier),
		Revision:      i.revision(),
		URL:           collabhelper.DisplayURL(i.URL, collab.MaxURLLength),
		Title:         collabhelper.OneLine(i.Title, collab.MaxTitleLength, i.Identifier),
		Body:          collabhelper.BoundedBody(body, collab.MaxBodyBytes),
		State:         state,
		ProviderState: collabhelper.OneLine(i.State.Name, collab.MaxLabelLength, string(state)),
		Labels:        collabhelper.BoundedLabels(i.labelNames(), collab.MaxLabels, collab.MaxLabelLength),
		Assignees:     assignees,
		CreatedAt:     collabhelper.Timestamp(i.CreatedAt),
		UpdatedAt:     collabhelper.Timestamp(i.UpdatedAt),
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// issueID reads the issue UUID of a reference this organization issued; ok is
// false for any other reference.
func (s *session) issueID(ref collab.Ref) (string, bool) {
	if !strings.EqualFold(ref.Source, s.source) || !uuidPattern.MatchString(ref.ID) {
		return "", false
	}
	return ref.ID, true
}

func (s *session) ref(id string) collab.Ref {
	return collab.Ref{Source: s.source, ID: id}
}

// keyedID is the id a create gives the issue its idempotency key names: the
// SHA-256 of the team's id and the key, shaped as a UUID v4.
func keyedID(teamID, key string) string {
	sum := sha256.Sum256([]byte("putnami tasks.create\x00" + teamID + "\x00" + key))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	hexed := hex.EncodeToString(sum[:16])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// A digest marker records, at the end of the description of the issue a
// keyed create produced, a digest of what the request asked for. It is an
// HTML comment whose payload sits in a code span, so Linear's markdown
// normalization leaves the payload as it is. Linear displays the line in the
// issue. The issue's id records the key; the marker tells a repeat from
// another request under the same key.
//
//	<!-- `putnami-idempotency d=<sha256 of the request>` -->

var markerPattern = regexp.MustCompile("(?:\\r?\\n)*\\\\?<!-- ?`?putnami-idempotency d=([0-9a-f]{64})`? ?\\\\?-->\\s*$")

func markerComment(requestDigest string) string {
	return "<!-- `putnami-idempotency d=" + requestDigest + "` -->"
}

// withMarker appends a marker to a body.
func withMarker(body, requestDigest string) string {
	if strings.TrimSpace(body) == "" {
		return markerComment(requestDigest)
	}
	return strings.TrimRight(body, "\r\n") + "\n\n" + markerComment(requestDigest)
}

// splitMarker separates a body from the digest of the marker at its end; the
// digest is empty when the body carries none.
func splitMarker(body string) (string, string) {
	match := markerPattern.FindStringSubmatchIndex(body)
	if match == nil {
		return body, ""
	}
	return body[:match[0]], body[match[2]:match[3]]
}

// rejoin replaces the text of a body and keeps the marker it carried.
func rejoin(raw, text string) string {
	_, recorded := splitMarker(raw)
	text, _ = splitMarker(text)
	if recorded == "" {
		return text
	}
	return withMarker(text, recorded)
}
