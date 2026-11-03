package provider

import (
	"net/http"
	"slices"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// write sends one write about an issue, then reads the issue back. When the
// write's outcome is unknown, the read-back accepts the issue once applied
// reports that the write is in place.
func (s *session) write(issue *jiraIssue, method, path string, body any, operation string, applied func(*jiraIssue) bool) (*jiraIssue, *collab.Failure) {
	err := s.client.Write(s.ctx, method, path, body, nil)
	if err != nil && err.Kind != jira.Uncertain {
		return nil, fail(err, "%s %s", operation, issue.Key)
	}
	var current *jiraIssue
	if s.reconcile(func() (bool, *collab.Failure) {
		read, readErr := s.read(issue.ID)
		if readErr != nil {
			return false, fail(readErr, "read %s back", issue.Key)
		}
		current = read
		return err == nil || applied(read), nil
	}) {
		return current, nil
	}
	if err == nil {
		return nil, unresolved("read the task with tasks.get",
			"Jira accepted the %s of %s, and reading it back failed", operation, issue.Key)
	}
	return nil, unresolved("read the task with tasks.get and compare it with the request before any retry",
		"the %s of %s may have happened (%s), and reading it back did not show it", operation, issue.Key, err.Message)
}

// sameSet reports whether two label or id lists hold the same values.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// update changes summary, description or labels. Labels replace the task's
// labels while the idempotency label stays. expectedRevision is compared with
// the issue read just before the write: Jira has no conditional issue write,
// so a writer between the two is not detected.
func (s *session) update(input any) (any, *collab.Failure) {
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
	fields := map[string]any{}
	summary, description, labels := issue.Fields.Summary, deref(issue.Fields.Description), issue.Fields.Labels
	if in.Title != nil && *in.Title != summary {
		summary = *in.Title
		fields["summary"] = summary
	}
	if in.Body != nil && *in.Body != description {
		description = *in.Body
		fields["description"] = description
	}
	if in.Labels != nil {
		if failure := refuseReserved(*in.Labels); failure != nil {
			return nil, failure
		}
		wanted := slices.Clone(*in.Labels)
		for _, label := range issue.Fields.Labels {
			if reserved(label) {
				wanted = append(wanted, label)
			}
		}
		if wanted == nil {
			wanted = []string{}
		}
		if !sameSet(wanted, issue.Fields.Labels) {
			labels = wanted
			fields["labels"] = labels
		}
	}
	if len(fields) == 0 {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	updated, failure := s.write(issue, http.MethodPut, jira.Path("issue", issue.ID), map[string]any{"fields": fields}, "update", func(current *jiraIssue) bool {
		return current.Fields.Summary == summary && deref(current.Fields.Description) == description && sameSet(current.Fields.Labels, labels)
	})
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(updated)}, nil
}

// transition moves an issue to a semantic state. A task already in the state
// is left as it is. expectedRevision is compared with the issue read just
// before the write.
func (s *session) transition(input any) (any, *collab.Failure) {
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
	if categoryOf(in.State) == "" {
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped, "Jira has no status category for %s", in.State)
	}
	if issue.state() == in.State {
		return &collab.TaskResult{Task: s.task(issue)}, nil
	}
	moved, failure := s.move(issue, in.State)
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(moved)}, nil
}

// jiraTransition is one transition Jira offers an issue.
type jiraTransition struct {
	ID string `json:"id"`
	To struct {
		Name           string `json:"name"`
		StatusCategory *struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	} `json:"to"`
	Fields map[string]struct {
		Required      bool    `json:"required"`
		AllowedValues []named `json:"allowedValues"`
	} `json:"fields"`
}

// move takes the first transition Jira offers into the state's category and
// reads the issue back. canceled needs a transition whose screen sets the
// resolution Won't Do; done sets the resolution Done when the screen asks
// for one.
func (s *session) move(issue *jiraIssue, state collab.TaskState) (*jiraIssue, *collab.Failure) {
	var offered struct {
		Transitions []jiraTransition `json:"transitions"`
	}
	if err := s.client.Get(s.ctx, jira.Path("issue", issue.ID, "transitions"), map[string][]string{"expand": {"transitions.fields"}}, &offered); err != nil {
		return nil, fail(err, "read the transitions of %s", issue.Key)
	}
	target := categoryOf(state)
	var chosen *jiraTransition
	var resolution string
	for i := range offered.Transitions {
		candidate := &offered.Transitions[i]
		if candidate.To.StatusCategory == nil || candidate.To.StatusCategory.Key != target {
			continue
		}
		field, asks := candidate.Fields["resolution"]
		switch {
		case state == collab.TaskStateCanceled:
			resolution = allowed(field.AllowedValues, cancelResolution)
			if !asks || resolution == "" {
				continue
			}
		case state == collab.TaskStateDone && asks:
			resolution = allowed(field.AllowedValues, "Done")
			if resolution == "" {
				for _, value := range field.AllowedValues {
					if !isCancelResolution(value.Name) {
						resolution = value.Name
						break
					}
				}
			}
			if resolution == "" && field.Required {
				continue
			}
		default:
			resolution = ""
		}
		chosen = candidate
		break
	}
	if chosen == nil {
		why := "no transition of " + issue.Key + " leads to a status of category " + target
		if state == collab.TaskStateCanceled {
			why += " whose screen sets the resolution " + cancelResolution
		}
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonNoTransition, "%s; Jira offers %s", why, offeredNames(offered.Transitions))
	}
	body := map[string]any{"transition": map[string]string{"id": chosen.ID}}
	if resolution != "" {
		body["fields"] = map[string]any{"resolution": map[string]string{"name": resolution}}
	}
	moved, failure := s.write(issue, http.MethodPost, jira.Path("issue", issue.ID, "transitions"), body, "transition to "+string(state), func(current *jiraIssue) bool {
		return current.state() == state
	})
	if failure != nil {
		return nil, failure
	}
	if moved.state() != state {
		return nil, unresolved("read the task with tasks.get before any retry",
			"Jira accepted the transition of %s to %s and reports it as %s (%s)", issue.Key, state, moved.state(), moved.statusName())
	}
	return moved, nil
}

// allowed returns the allowed value named name, or the empty string.
func allowed(values []named, name string) string {
	for _, value := range values {
		if strings.EqualFold(normalizeApostrophe(value.Name), name) {
			return value.Name
		}
	}
	return ""
}

func offeredNames(transitions []jiraTransition) string {
	if len(transitions) == 0 {
		return "no transition"
	}
	names := make([]string, 0, len(transitions))
	for _, transition := range transitions {
		category := "?"
		if transition.To.StatusCategory != nil {
			category = transition.To.StatusCategory.Key
		}
		names = append(names, transition.To.Name+" ("+category+")")
	}
	return strings.Join(names, ", ")
}
