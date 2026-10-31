package sdd

import (
	"encoding/json"
	"fmt"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

// The feature-coverage ratchet of `validate-workspace` (ADR 0003).
//
// The debt list only shrinks. feature-coverage-validate judges the list as it
// stands and reads no history, so this step compares it with the list the
// change started from: options.sdd.featureCoverage.debt in
// putnami.workspace.json at the merge base of the run's impacted baseline and
// HEAD. Each current entry the merge base does not list fails, named. The merge
// base, not the baseline commit, is the comparison point: a branch behind its
// base still holds an entry the base removed since, and that entry is not new.
//
// The verdict reads Git history, which the candidate cut does not hold, so the
// task is uncached. A shallow clone is refused: its history can stop before the
// merge base.

// ErrorCodeGrownCoverageDebt reports a debt entry the list at the merge base
// does not hold.
const ErrorCodeGrownCoverageDebt = "sdd.grown_coverage_debt"

// ErrorCodeCoverageDebtBaselineUnavailable reports a debt list at the merge
// base that this step cannot read.
const ErrorCodeCoverageDebtBaselineUnavailable = "sdd.coverage_debt_baseline_unavailable"

// coverageBaselineMaxBytes bounds the putnami.workspace.json read from history.
const coverageBaselineMaxBytes = 8 << 20

// FeatureCoverageRatchetReport is the typed data of one feature-coverage
// ratchet judgment.
type FeatureCoverageRatchetReport struct {
	// Adopted reports that the workspace declares options.sdd.featureCoverage.
	Adopted bool `json:"adopted"`
	// Baseline is the run's impacted baseline.
	Baseline string `json:"baseline,omitempty"`
	// MergeBase is the commit the debt list was read at.
	MergeBase string `json:"mergeBase,omitempty"`
	// Debt lists the current debt entries, in file order.
	Debt []string `json:"debt"`
	// BaselineDebt lists the debt entries at the merge base, in file order.
	BaselineDebt []string `json:"baselineDebt"`
	// Grown lists the current entries the merge base does not list, in file
	// order.
	Grown []string `json:"grown"`
	// Note explains a run that compared nothing.
	Note string `json:"note,omitempty"`
	// Diagnostics carries every finding.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// BuildFeatureCoverageRatchetResult compares the committed debt list with the
// list at the merge base of baseline and HEAD in the repository that holds
// root, the workspace root. workspaceOptions are the workspace option blocks the
// job context carries, and baseline is the run's impacted baseline, or empty.
func BuildFeatureCoverageRatchetResult(root string, workspaceOptions map[string]map[string]any, baseline string) (FeatureCoverageRatchetReport, error) {
	report := FeatureCoverageRatchetReport{Debt: []string{}, BaselineDebt: []string{}, Grown: []string{}}
	debt, adopted, findings := featureCoverageDebt(workspaceOptions["sdd"])
	if len(findings) > 0 {
		report.Diagnostics = findings
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage ratchet: options.sdd.%s is invalid", FeatureCoverageOption),
			protocolcli.ErrInvalidConfig), report)
	}
	if !adopted {
		report.Note = fmt.Sprintf("not adopted (no options.sdd.%s)", FeatureCoverageOption)
		return report, nil
	}
	report.Adopted = true
	report.Debt = append(report.Debt, debt...)
	baseline = strings.TrimSpace(baseline)
	if baseline == "" {
		report.Note = "no impacted baseline to read the debt list from: run validate with --impacted to refuse a new entry"
		return report, nil
	}
	report.Baseline = baseline

	shallow, err := gitread.IsShallow(root)
	if err != nil {
		return report, WithResultData(fmt.Errorf("feature coverage ratchet: %w", err), report)
	}
	if shallow {
		return report, WithResultData(protocolcli.Classify(fmt.Errorf(
			"feature coverage ratchet: the repository is a shallow clone, so its history may stop before the merge base with %s; fetch the whole history",
			baseline), protocolcli.ErrInvalidConfig), report)
	}
	mergeBase, err := gitread.MergeBase(root, baseline)
	if err != nil {
		return report, WithResultData(fmt.Errorf("feature coverage ratchet: %w", err), report)
	}
	report.MergeBase = mergeBase

	block, found, err := workspaceSDDOptionsAt(root, mergeBase)
	if err != nil {
		report.Diagnostics = []diag.Diagnostic{diag.Errorf(ErrorCodeCoverageDebtBaselineUnavailable,
			wsproto.WorkspaceConfigFilename, "read the debt list at the merge base %s: %v", shortCommit(mergeBase), err)}
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage ratchet: %w", err), protocolcli.ErrInvalidConfig), report)
	}
	baselineDebt, baselineAdopted, baselineFindings := featureCoverageDebt(block)
	if len(baselineFindings) > 0 {
		report.Diagnostics = []diag.Diagnostic{diag.Errorf(ErrorCodeCoverageDebtBaselineUnavailable,
			wsproto.WorkspaceConfigFilename, "the debt list at the merge base %s is invalid: %s",
			shortCommit(mergeBase), baselineFindings[0].Message)}
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage ratchet: the debt list at the merge base %s is invalid", shortCommit(mergeBase)),
			protocolcli.ErrInvalidConfig), report)
	}
	if !found || !baselineAdopted {
		report.Note = fmt.Sprintf("the merge base %s declares no options.sdd.%s: this change adopts the gate",
			shortCommit(mergeBase), FeatureCoverageOption)
		return report, nil
	}
	report.BaselineDebt = append(report.BaselineDebt, baselineDebt...)

	listed := make(map[string]bool, len(baselineDebt))
	for _, id := range baselineDebt {
		listed[id] = true
	}
	for i, id := range debt {
		if listed[id] {
			continue
		}
		report.Grown = append(report.Grown, id)
		field := fmt.Sprintf("%s#options.sdd.%s.%s[%d]", wsproto.WorkspaceConfigFilename,
			FeatureCoverageOption, featureCoverageDebtMember, i)
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeGrownCoverageDebt, field,
			"%s is a new debt entry: the list at the merge base %s with %s does not name it, and the list only shrinks; give the project a feature, a spec or a featureAuthority instead",
			id, shortCommit(mergeBase), baseline))
	}
	if len(report.Diagnostics) > 0 {
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage ratchet: %d new debt entr%s", len(report.Grown), pluralY(len(report.Grown))),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// workspaceSDDOptionsAt reads options.sdd of the workspace config at commit.
// found is false when the commit holds no workspace config, or one without an
// options.sdd block.
func workspaceSDDOptionsAt(root, commit string) (map[string]any, bool, error) {
	top, prefix, err := gitread.RepositoryLocation(root)
	if err != nil {
		return nil, false, err
	}
	entry, exists, err := gitread.TreeEntryAt(top, commit, prefix+wsproto.WorkspaceConfigFilename)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
		return nil, false, fmt.Errorf("%s is not a regular file", wsproto.WorkspaceConfigFilename)
	}
	data, err := gitread.ReadTreeBlob(top, entry.ObjectID, coverageBaselineMaxBytes)
	if err != nil {
		return nil, false, err
	}
	var document struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	raw, found := document.Options["sdd"]
	if !found {
		return nil, false, nil
	}
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, false, fmt.Errorf("decode %s#options.sdd: %w", wsproto.WorkspaceConfigFilename, err)
	}
	return block, true, nil
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
