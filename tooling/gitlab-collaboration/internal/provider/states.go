package provider

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// TaskSettings are the settings of a tasks binding.
type TaskSettings struct {
	// Project is the full path of the project served: namespace/name.
	Project string `json:"project"`
	// Host is the GitLab instance, gitlab.com unless set.
	Host string `json:"host,omitempty"`
	// States maps each semantic task state to the issue labels that mean it.
	// The first label of a state is the one a transition applies; the others
	// are recognized when reading.
	States StateLabels `json:"states,omitempty"`
	// StateLabelPrefix makes every label with this prefix a state label too:
	// removed by a transition and kept out of a task's labels, even when no
	// state lists it.
	StateLabelPrefix string `json:"stateLabelPrefix,omitempty"`
	// KeyScanPages is how many pages of 100 of the account's most recent
	// issues a create searches for its idempotency key before it asks
	// GitLab's search. Default 3, at most 10.
	KeyScanPages int `json:"keyScanPages,omitempty"`
}

// StateLabels lists the labels of each semantic state. done and canceled are
// closed issues; a canceled label is what tells a canceled issue from a done
// one, and a done label is applied on a transition and never read.
type StateLabels struct {
	Open       []string `json:"open,omitempty"`
	InProgress []string `json:"in_progress,omitempty"`
	Blocked    []string `json:"blocked,omitempty"`
	Done       []string `json:"done,omitempty"`
	Canceled   []string `json:"canceled,omitempty"`
}

const taskSettingNames = "project, host, states, stateLabelPrefix and keyScanPages"

// maxLabelRunes bounds a GitLab label name.
const maxLabelRunes = 255

// stateMap is a validated StateLabels.
type stateMap struct {
	labels map[collab.TaskState][]string
	prefix string
}

// readPrecedence is the order in which an opened issue's labels decide its
// state when it carries the labels of more than one.
var readPrecedence = []collab.TaskState{collab.TaskStateBlocked, collab.TaskStateInProgress, collab.TaskStateOpen}

func parseStates(settings TaskSettings) (stateMap, *collab.Failure) {
	m := stateMap{prefix: settings.StateLabelPrefix, labels: map[collab.TaskState][]string{
		collab.TaskStateOpen:       settings.States.Open,
		collab.TaskStateInProgress: settings.States.InProgress,
		collab.TaskStateBlocked:    settings.States.Blocked,
		collab.TaskStateDone:       settings.States.Done,
		collab.TaskStateCanceled:   settings.States.Canceled,
	}}
	if m.prefix != "" && strings.TrimSpace(m.prefix) != m.prefix {
		return stateMap{}, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.stateLabelPrefix carries surrounding whitespace")
	}
	owner := map[string]collab.TaskState{}
	for _, state := range collab.ValidTaskStates {
		for _, label := range m.labels[state] {
			if label == "" || strings.TrimSpace(label) != label || len([]rune(label)) > maxLabelRunes || strings.Contains(label, ",") {
				return stateMap{}, collab.Fail(collab.OutcomeInvalid, reasonSettings,
					"settings.states.%s: %q is not a GitLab label name (1 to %d characters, no comma, no surrounding whitespace)", state, label, maxLabelRunes)
			}
			if other, taken := owner[strings.ToLower(label)]; taken {
				return stateMap{}, collab.Fail(collab.OutcomeInvalid, reasonSettings,
					"settings.states: label %q means both %s and %s", label, other, state)
			}
			owner[strings.ToLower(label)] = state
		}
	}
	return m, nil
}

// isStateLabel reports whether a label belongs to the state mapping.
func (m stateMap) isStateLabel(label string) bool {
	if m.prefix != "" && strings.HasPrefix(strings.ToLower(label), strings.ToLower(m.prefix)) {
		return true
	}
	for _, labels := range m.labels {
		if slices.ContainsFunc(labels, func(candidate string) bool { return strings.EqualFold(candidate, label) }) {
			return true
		}
	}
	return false
}

// primary is the label a transition to state applies, if any.
func (m stateMap) primary(state collab.TaskState) string {
	if labels := m.labels[state]; len(labels) > 0 {
		return labels[0]
	}
	return ""
}

// carried returns the first label of state an issue carries, if any.
func (m stateMap) carried(names []string, state collab.TaskState) (string, bool) {
	for _, label := range m.labels[state] {
		if index := slices.IndexFunc(names, func(name string) bool { return strings.EqualFold(name, label) }); index >= 0 {
			return names[index], true
		}
	}
	return "", false
}

// stateOf reads an issue's semantic state and the provider state that decided
// it. A closed issue is canceled when it carries a canceled label or GitLab
// closed it as a duplicate, and done otherwise; an opened issue is in the
// first state of readPrecedence whose label it carries, and open otherwise.
func (m stateMap) stateOf(issue *glIssue) (collab.TaskState, string) {
	names := issue.labelNames()
	if issue.closed() {
		if label, ok := m.carried(names, collab.TaskStateCanceled); ok {
			return collab.TaskStateCanceled, label
		}
		if issue.duplicate() {
			return collab.TaskStateCanceled, "closed:duplicate"
		}
		return collab.TaskStateDone, "closed"
	}
	for _, state := range readPrecedence {
		if label, ok := m.carried(names, state); ok {
			return state, label
		}
	}
	return collab.TaskStateOpen, "opened"
}

// settled reports whether an issue already is what a transition to state
// leaves: a closed issue by its semantic state alone, an opened issue also
// through the state's first label when the state has one. An opened issue
// that reads as state through another of its labels is not settled, so a
// transition to the state it is in still applies the first label.
func (m stateMap) settled(issue *glIssue, state collab.TaskState) bool {
	current, providerState := m.stateOf(issue)
	if current != state {
		return false
	}
	primary := m.primary(state)
	return issue.closed() || primary == "" || strings.EqualFold(providerState, primary)
}

// visibleLabels are an issue's labels outside the state mapping, which is
// what a task's labels are and what tasks.update replaces.
func (m stateMap) visibleLabels(names []string) []string {
	visible := make([]string, 0, len(names))
	for _, name := range names {
		if !m.isStateLabel(name) {
			visible = append(visible, name)
		}
	}
	return visible
}

// stateLabels are the labels of names that belong to the state mapping.
func (m stateMap) stateLabels(names []string) []string {
	var labels []string
	for _, name := range names {
		if m.isStateLabel(name) {
			labels = append(labels, name)
		}
	}
	return labels
}

// refuseLabels refuses a request that sets a state label directly, since a
// state changes only through tasks.transition, or a label GitLab's
// comma-separated label list cannot carry.
func (m stateMap) refuseLabels(labels []string) *collab.Failure {
	for _, label := range labels {
		if strings.Contains(label, ",") {
			return collab.Fail(collab.OutcomeInvalid, reasonLabelComma, "label %q carries a comma, which GitLab reads as a separator", label)
		}
		if m.isStateLabel(label) {
			return collab.Fail(collab.OutcomeInvalid, reasonStateLabel,
				"label %q is a state label of this binding; change the state with tasks.transition", label)
		}
	}
	return nil
}

// target is what an issue must look like to be in a state.
type target struct {
	closed bool
	labels []string
}

// targetOf computes the GitLab state and labels a transition writes: every
// state label removed, the state's primary label added. in_progress, blocked
// and canceled need a label, or GitLab cannot represent them.
func (m stateMap) targetOf(issue *glIssue, state collab.TaskState) (target, *collab.Failure) {
	switch state {
	case collab.TaskStateInProgress, collab.TaskStateBlocked, collab.TaskStateCanceled:
		if m.primary(state) == "" {
			return target{}, unmapped(state)
		}
	}
	labels := m.visibleLabels(issue.labelNames())
	if primary := m.primary(state); primary != "" {
		labels = append(labels, primary)
	}
	return target{closed: closedState(state), labels: labels}, nil
}

func unmapped(state collab.TaskState) *collab.Failure {
	return collab.Fail(collab.OutcomeUnsupported, reasonStateUnmapped,
		"no label means %s in this binding: add settings.states.%s", state, state)
}

func closedState(state collab.TaskState) bool {
	return state == collab.TaskStateDone || state == collab.TaskStateCanceled
}

// sameLabels compares two label sets, ignoring order and case.
func sameLabels(a, b []string) bool {
	fold := func(labels []string) []string {
		out := make([]string, len(labels))
		for i, label := range labels {
			out[i] = strings.ToLower(label)
		}
		sort.Strings(out)
		return slices.Compact(out)
	}
	return slices.Equal(fold(a), fold(b))
}

func describeState(state collab.TaskState, providerState string) string {
	return fmt.Sprintf("%s (%s)", state, providerState)
}
