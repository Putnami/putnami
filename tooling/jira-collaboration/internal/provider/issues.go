package provider

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// jiraIssue is the part of a Jira issue the provider reads.
type jiraIssue struct {
	ID         string                     `json:"id"`
	Key        string                     `json:"key"`
	Fields     issueFields                `json:"fields"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
}

type issueFields struct {
	Summary     string      `json:"summary"`
	Description *string     `json:"description"`
	Status      *jiraStatus `json:"status"`
	Resolution  *named      `json:"resolution"`
	Labels      []string    `json:"labels"`
	Assignee    *jiraUser   `json:"assignee"`
	Creator     *jiraUser   `json:"creator"`
	Created     string      `json:"created"`
	Updated     string      `json:"updated"`
}

type jiraStatus struct {
	Name           string `json:"name"`
	StatusCategory *struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

type named struct {
	Name string `json:"name"`
}

type jiraUser struct {
	AccountID string `json:"accountId"`
}

// issueFieldNames are the fields every issue read asks for.
var issueFieldNames = []string{"summary", "description", "status", "resolution", "labels", "assignee", "creator", "created", "updated"}

// idempotencyProperty is the issue property that records the digest of the
// create request that opened the issue.
const idempotencyProperty = "putnami.idempotency"

// Status category keys.
const (
	categoryToDo       = "new"
	categoryInProgress = "indeterminate"
	categoryDone       = "done"
	categoryNone       = "undefined"
)

// cancelResolutions are the resolutions that make a done issue canceled.
// cancelResolution is the one a transition to canceled sets.
var cancelResolutions = []string{"Won't Do", "Duplicate"}

const cancelResolution = "Won't Do"

func (i *jiraIssue) category() string {
	if i.Fields.Status == nil || i.Fields.Status.StatusCategory == nil {
		return ""
	}
	return i.Fields.Status.StatusCategory.Key
}

func (i *jiraIssue) statusName() string {
	if i.Fields.Status == nil {
		return ""
	}
	return i.Fields.Status.Name
}

func (i *jiraIssue) resolution() string {
	if i.Fields.Resolution == nil {
		return ""
	}
	return i.Fields.Resolution.Name
}

// state reads an issue's semantic state from its status category and, for a
// done issue, its resolution. A status without a category reads as open.
func (i *jiraIssue) state() collab.TaskState {
	switch i.category() {
	case categoryInProgress:
		return collab.TaskStateInProgress
	case categoryDone:
		if isCancelResolution(i.resolution()) {
			return collab.TaskStateCanceled
		}
		return collab.TaskStateDone
	}
	return collab.TaskStateOpen
}

func isCancelResolution(name string) bool {
	return slices.ContainsFunc(cancelResolutions, func(cancel string) bool {
		return strings.EqualFold(normalizeApostrophe(name), cancel)
	})
}

// normalizeApostrophe reads a typographic apostrophe as a plain one, so
// "Won’t Do" is "Won't Do".
func normalizeApostrophe(value string) string {
	return strings.ReplaceAll(value, "’", "'")
}

// categoryOf is the status category a semantic state lives in; empty for
// blocked, which no category holds.
func categoryOf(state collab.TaskState) string {
	switch state {
	case collab.TaskStateOpen:
		return categoryToDo
	case collab.TaskStateInProgress:
		return categoryInProgress
	case collab.TaskStateDone, collab.TaskStateCanceled:
		return categoryDone
	}
	return ""
}

func (i *jiraIssue) assigneeIDs() []string {
	if i.Fields.Assignee == nil || i.Fields.Assignee.AccountID == "" {
		return []string{}
	}
	return []string{i.Fields.Assignee.AccountID}
}

// revision covers everything a task shows and a caller can change: summary,
// description, status with its category and resolution, labels and
// assignees.
func (i *jiraIssue) revision() string {
	labels := slices.Clone(i.Fields.Labels)
	if labels == nil {
		labels = []string{}
	}
	sort.Strings(labels)
	assignees := i.assigneeIDs()
	sort.Strings(assignees)
	return "v" + digest([]any{i.Fields.Summary, deref(i.Fields.Description), i.statusName(), i.category(), i.resolution(), labels, assignees})[:32]
}

// keyNumber is the number of an issue key, PROJ-123 → 123; 0 when the key
// is not one.
func keyNumber(key string) int {
	at := strings.LastIndexByte(key, '-')
	if at <= 0 {
		return 0
	}
	number, err := strconv.Atoi(key[at+1:])
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

// reservedLabelPrefix starts every idempotency label. Such a label is the
// provider's record, never a task label.
const reservedLabelPrefix = "putnami-idempotency-"

func reserved(label string) bool {
	return strings.HasPrefix(strings.ToLower(label), reservedLabelPrefix)
}

func refuseReserved(labels []string) *collab.Failure {
	for _, label := range labels {
		if reserved(label) {
			return collab.Fail(collab.OutcomeInvalid, reasonReservedLabel,
				"label %q is reserved: labels starting with %s record an idempotency key", label, reservedLabelPrefix)
		}
	}
	return nil
}

// task renders an issue as a task.
func (s *session) task(issue *jiraIssue) collab.Task {
	state := issue.state()
	visible := []string{}
	for _, label := range issue.Fields.Labels {
		if !reserved(label) {
			visible = append(visible, label)
		}
	}
	assignees := []string{}
	for _, id := range issue.assigneeIDs() {
		if id = token(id); id != "" {
			assignees = append(assignees, id)
		}
	}
	task := collab.Task{
		Ref:           collab.Ref{Source: s.source, ID: issue.ID},
		Key:           token(issue.Key),
		Revision:      issue.revision(),
		Title:         oneLine(issue.Fields.Summary, collab.MaxTitleLength, issue.Key),
		Body:          boundedBody(deref(issue.Fields.Description)),
		State:         state,
		ProviderState: oneLine(issue.statusName(), collab.MaxLabelLength, string(state)),
		Labels:        boundedLabels(visible),
		Assignees:     assignees,
		CreatedAt:     timestamp(issue.Fields.Created),
		UpdatedAt:     timestamp(issue.Fields.Updated),
	}
	if task.Key != "" {
		task.URL = "https://" + s.settings.Site + "/browse/" + task.Key
		if len(task.URL) > collab.MaxURLLength {
			task.URL = ""
		}
	}
	return task
}

// read reads one issue by id or key, with the idempotency property.
func (s *session) read(idOrKey string) (*jiraIssue, *jira.Error) {
	var issue jiraIssue
	query := map[string][]string{"fields": {strings.Join(issueFieldNames, ",")}, "properties": {idempotencyProperty}}
	if err := s.client.Get(s.ctx, jira.Path("issue", idOrKey), query, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// issue reads the issue a reference names. A reference another source issued
// is not found.
func (s *session) issue(ref collab.Ref) (*jiraIssue, *collab.Failure) {
	id, ours := s.id(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, "task.missing", "no task %s in source %s", ref.ID, ref.Source)
	}
	issue, err := s.read(id)
	if err != nil {
		return nil, fail(err, "read issue %s of %s", id, s.settings.Site)
	}
	if issue.ID != id {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "Jira answered issue %q for issue %s", issue.ID, id)
	}
	return issue, nil
}

func (s *session) checkRevision(issue *jiraIssue, expected string) *collab.Failure {
	if current := issue.revision(); expected != "" && current != expected {
		failure := collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, "task %s is at revision %s, not %s", issue.Key, current, expected)
		failure.Error.Current = current
		return failure
	}
	return nil
}
