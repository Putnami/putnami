package analyze

import (
	"os"
	"path/filepath"
	"slices"
	"sort"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/decisions"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

// Diagnostic codes of the decision records.
const (
	CodeDecisionUnread     = "observe.decision_unread"
	CodeDecisionDeleted    = "observe.decision_deleted"
	CodeDecisionsTruncated = "observe.decisions_truncated"
)

// decisionRecords lists the decision records the change concerns: those
// whose scope matches a changed file and those the change adds, modifies,
// renames or deletes, a record renamed out of the decision directories
// included.
func (r *Result) decisionRecords(in Input) {
	dirs, rejected := decisions.Dirs(in.DecisionDirs)
	for _, dir := range rejected {
		r.warn(CodeDecisionUnread, "decisions", "%q is not a directory of the repository, so it is not read for decision records", dir)
	}
	found, err := decisions.Find(in.Root, dirs)
	if err != nil {
		r.warn(CodeDecisionUnread, "decisions", "the decision records under %v cannot be listed: %v", dirs, err)
	}
	status := map[string]string{}
	for _, file := range in.ChangedFiles {
		if decisions.IsRecord(file.Path, dirs) || (file.Status == gitbase.StatusRenamed && decisions.IsRecord(file.PreviousPath, dirs)) {
			status[file.Path] = file.Status
		}
	}
	paths := slices.Clone(found)
	for file := range status {
		if !slices.Contains(paths, file) {
			paths = append(paths, file)
		}
	}
	sort.Strings(paths)
	left := 0
	for _, file := range paths {
		record, ok := r.readRecord(in, file, status[file] == gitbase.StatusDeleted)
		if !ok {
			continue
		}
		decision, concerned := r.decision(in, record, status[file])
		if !concerned {
			continue
		}
		if decision.Change == gitbase.StatusDeleted && decision.Status == evidence.DecisionAccepted {
			r.warn(CodeDecisionDeleted, decision.Path, "the change deletes the accepted decision %s (%s)", decision.Path, decision.Title)
		}
		if len(r.Decisions) == evidence.MaxDecisions {
			left++
			continue
		}
		r.Decisions = append(r.Decisions, decision)
	}
	if left > 0 {
		r.warn(CodeDecisionsTruncated, "decisions", "the change concerns more than %d decision records; %d are left out", evidence.MaxDecisions, left)
	}
}

// readRecord reads a record at the head, or at the base when the change
// deletes it. A deleted record that cannot be read there is listed by its file
// name, with an unknown status.
func (r *Result) readRecord(in Input, file string, deleted bool) (decisions.Record, bool) {
	side := Head
	if deleted {
		side = Base
	}
	var data []byte
	var err error
	found := true
	if in.Show != nil {
		data, found, err = in.Show(side, file)
	} else if !deleted {
		data, err = os.ReadFile(filepath.Join(in.Root, filepath.FromSlash(file)))
	} else {
		found = false
	}
	switch {
	case err != nil:
		r.warn(CodeDecisionUnread, file, "the decision record %s cannot be read: %v", file, err)
		return decisions.Parse(file, nil), deleted
	case !found:
		return decisions.Parse(file, nil), deleted
	}
	return decisions.Parse(file, data), true
}

// decision is the evidence entry of one record, and whether the change
// concerns it.
func (r *Result) decision(in Input, record decisions.Record, change string) (evidence.Decision, bool) {
	decision := evidence.Decision{
		Path: record.Path, Title: bounded(record.Title), Status: record.Status, Date: record.Date, Change: change,
	}
	scopeCut := false
	for _, pattern := range record.Scope {
		if len(decision.Scope) == evidence.MaxDecisionScope {
			scopeCut = true
			break
		}
		if pattern = bounded(pattern); !slices.Contains(decision.Scope, pattern) {
			decision.Scope = append(decision.Scope, pattern)
		}
	}
	governsCut := false
	for _, file := range in.ChangedFiles {
		if !slices.ContainsFunc(decision.Scope, func(pattern string) bool { return decisions.Matches(pattern, file.Path) }) {
			continue
		}
		if len(decision.Governs) == evidence.MaxGovernedFiles {
			governsCut = true
			break
		}
		decision.Governs = append(decision.Governs, file.Path)
	}
	sort.Strings(decision.Governs)
	concerned := change != "" || len(decision.Governs) > 0
	if concerned && scopeCut {
		r.warn(CodeDecisionsTruncated, record.Path, "the decision record %s names more than %d scope patterns; the others are left out", record.Path, evidence.MaxDecisionScope)
	}
	if concerned && governsCut {
		r.warn(CodeDecisionsTruncated, record.Path, "the decision record %s governs more than %d changed files; the others are left out", record.Path, evidence.MaxGovernedFiles)
	}
	return decision, concerned
}
