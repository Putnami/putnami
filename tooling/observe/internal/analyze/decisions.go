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
// renames or deletes.
func (r *Result) decisionRecords(in Input) {
	dirs := decisions.Dirs(in.DecisionDirs)
	found, err := decisions.Find(in.Root, dirs)
	if err != nil {
		r.warn(CodeDecisionUnread, "decisions", "the decision records under %s cannot be listed: %v", dirs, err)
	}
	status := map[string]string{}
	for _, file := range in.ChangedFiles {
		if decisions.IsRecord(file.Path, dirs) {
			status[file.Path] = file.Status
		}
	}
	var records []decisions.Record
	for _, file := range found {
		data, err := os.ReadFile(filepath.Join(in.Root, filepath.FromSlash(file)))
		if err != nil {
			r.warn(CodeDecisionUnread, file, "the decision record %s cannot be read: %v", file, err)
			continue
		}
		records = append(records, decisions.Parse(file, data))
	}
	for file, change := range status {
		if change != gitbase.StatusDeleted || slices.Contains(found, file) {
			continue
		}
		records = append(records, r.deletedRecord(in, file))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	for _, record := range records {
		decision, concerned := r.decision(in, record, status[record.Path])
		if !concerned {
			continue
		}
		if len(r.Decisions) == evidence.MaxDecisions {
			r.warn(CodeDecisionsTruncated, "decisions", "the change concerns more than %d decision records; the others are left out", evidence.MaxDecisions)
			return
		}
		if decision.Change == gitbase.StatusDeleted && decision.Status == evidence.DecisionAccepted {
			r.warn(CodeDecisionDeleted, decision.Path, "the change deletes the accepted decision %s (%s)", decision.Path, decision.Title)
		}
		r.Decisions = append(r.Decisions, decision)
	}
}

// deletedRecord reads a record the change deletes at the base. A record that
// cannot be read there is listed by its file name, with an unknown status.
func (r *Result) deletedRecord(in Input, file string) decisions.Record {
	if in.Show != nil {
		data, found, err := in.Show(Base, file)
		if err == nil && found {
			return decisions.Parse(file, data)
		}
		if err != nil {
			r.warn(CodeDecisionUnread, file, "the deleted decision record %s cannot be read at the base: %v", file, err)
		}
	}
	return decisions.Parse(file, nil)
}

// decision is the evidence entry of one record, and whether the change
// concerns it.
func (r *Result) decision(in Input, record decisions.Record, change string) (evidence.Decision, bool) {
	decision := evidence.Decision{
		Path: record.Path, Title: bounded(record.Title), Status: record.Status, Date: record.Date, Change: change,
	}
	for _, pattern := range record.Scope {
		if len(decision.Scope) == evidence.MaxDecisionScope {
			r.warn(CodeDecisionsTruncated, record.Path, "the decision record %s names more than %d scope patterns; the others are left out", record.Path, evidence.MaxDecisionScope)
			break
		}
		decision.Scope = append(decision.Scope, bounded(pattern))
	}
	for _, file := range in.ChangedFiles {
		if !slices.ContainsFunc(decision.Scope, func(pattern string) bool { return decisions.Matches(pattern, file.Path) }) {
			continue
		}
		if len(decision.Governs) == evidence.MaxGovernedFiles {
			r.warn(CodeDecisionsTruncated, record.Path, "the decision record %s governs more than %d changed files; the others are left out", record.Path, evidence.MaxGovernedFiles)
			break
		}
		decision.Governs = append(decision.Governs, file.Path)
	}
	sort.Strings(decision.Governs)
	return decision, change != "" || len(decision.Governs) > 0
}
