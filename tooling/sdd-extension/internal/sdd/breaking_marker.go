package sdd

import (
	"errors"
	"fmt"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/sdd/extension/internal/gitread"
)

// The breaking-marker gate of `validate-workspace`.
//
// A pull request merges by squash: one commit, written from the proposal's
// title and, when the repository squashes with it, its body, is the only
// commit of the change that lands on the base branch, and `putnami version`
// and the API checks read the breaking marker from it.
//
// This gate compares the proposed squash message, which the runner sets in
// PUTNAMI_PROPOSAL_MESSAGE, with the commits from the run's impacted baseline
// to HEAD, and fails when a commit declares a breaking change the message does
// not. Every message is read with protocolcli.ParseConventionalCommit, the
// parser the version bump calls.
//
// The verdict reads an environment variable and Git history, and the candidate
// cut holds neither, so the task is uncached. A shallow clone is refused: its
// range can stop at the clone depth.

// ProposalMessageEnv names the variable that carries the proposed squash
// message: its subject, a blank line, and its body when the squash keeps one.
const ProposalMessageEnv = "PUTNAMI_PROPOSAL_MESSAGE"

// ErrorCodeBreakingMarkerDropped is the finding for one commit whose breaking
// change the squash message does not declare.
const ErrorCodeBreakingMarkerDropped = "sdd.breaking_marker_dropped"

// BreakingMarkerReport is the typed data of one breaking-marker judgment.
type BreakingMarkerReport struct {
	// Proposal reports whether a proposed squash message was set.
	Proposal bool `json:"proposal"`
	// Note explains a pass that compared nothing.
	Note string `json:"note,omitempty"`
	// Baseline is the ref the commits were read from, itself excluded.
	Baseline string `json:"baseline,omitempty"`
	// Subject is the squash commit's subject, split from the message as Git
	// splits a commit message.
	Subject string `json:"subject,omitempty"`
	// Conventional reports whether that subject is a conventional commit. A
	// subject that is not declares nothing, whatever its body says.
	Conventional bool `json:"conventional"`
	// Declares reports whether the squash message declares a breaking change.
	Declares bool `json:"declares"`
	// Commits counts the commits read, merges included.
	Commits int `json:"commits"`
	// Breaking lists the commits that declare a breaking change, newest first.
	Breaking []BreakingCommit `json:"breaking,omitempty"`
	// Diagnostics carries one finding per breaking commit the squash message
	// drops.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// BreakingCommit is one commit that declares a breaking change.
type BreakingCommit struct {
	// Commit is the first 12 characters of the commit ID.
	Commit string `json:"commit"`
	// Subject is the commit's subject.
	Subject string `json:"subject"`
}

// BuildBreakingMarkerResult compares the proposed squash message with the
// commits HEAD adds to baseline in the repository at root. message is the value
// of ProposalMessageEnv and baseline the run's impacted baseline; either may be
// empty.
func BuildBreakingMarkerResult(root, message, baseline string) (BreakingMarkerReport, error) {
	var report BreakingMarkerReport
	if strings.TrimSpace(message) == "" {
		report.Note = "no proposal message to compare: " + ProposalMessageEnv + " is unset or blank"
		return report, nil
	}
	report.Proposal = true
	subject, body := splitCommitMessage(message)
	squash, conventional := protocolcli.ParseConventionalCommit(subject, body)
	report.Subject = subject
	report.Conventional = conventional
	report.Declares = conventional && squash.Breaking

	baseline = strings.TrimSpace(baseline)
	if baseline == "" {
		return report, WithResultData(protocolcli.Classify(fmt.Errorf(
			"breaking marker: %s is set, but the run has no impacted baseline to read the commits of the change from; "+
				"this step needs an --impacted run", ProposalMessageEnv), protocolcli.ErrInvalidConfig), report)
	}
	report.Baseline = baseline
	shallow, err := gitread.IsShallow(root)
	if err != nil {
		return report, WithResultData(fmt.Errorf("breaking marker: %w", err), report)
	}
	if shallow {
		return report, WithResultData(protocolcli.Classify(fmt.Errorf(
			"breaking marker: the repository is a shallow clone, so the commits since %s may stop at the clone depth "+
				"and hide a breaking change; fetch the whole history", baseline), protocolcli.ErrInvalidConfig), report)
	}
	commits, err := gitread.CommitsSince(root, baseline)
	if err != nil {
		return report, WithResultData(fmt.Errorf("breaking marker: %w", err), report)
	}
	report.Commits = len(commits)
	for _, commit := range commits {
		if declared, ok := protocolcli.ParseConventionalCommit(commit.Subject, commit.Body); ok && declared.Breaking {
			report.Breaking = append(report.Breaking, BreakingCommit{Commit: shortCommit(commit.SHA), Subject: commit.Subject})
		}
	}
	if len(report.Breaking) == 0 || report.Declares {
		return report, nil
	}

	var failure strings.Builder
	fmt.Fprintf(&failure, "breaking marker: the squash commit %q drops the breaking change that %s since %s:\n",
		subject, declaringCommits(len(report.Breaking)), baseline)
	for _, commit := range report.Breaking {
		fmt.Fprintf(&failure, "  %s %s\n", commit.Commit, commit.Subject)
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeBreakingMarkerDropped, "",
			"commit %s %q declares a breaking change that the squash commit %q drops", commit.Commit, commit.Subject, subject))
	}
	if conventional {
		failure.WriteString(`Add "!" before the colon of the title, or a "BREAKING CHANGE:" footer to the body when the squash commit keeps the body.`)
	} else {
		failure.WriteString(`The title is not a conventional commit, so nothing in the body counts: write it as "type(scope)!: summary".`)
	}
	failure.WriteString(" If the change breaks nothing, reword the commit.")
	return report, WithResultData(protocolcli.Classify(errors.New(failure.String()), protocolcli.ErrInvalidConfig), report)
}

// splitCommitMessage splits a message the way Git reads a commit message: the
// leading blank lines are skipped, the subject is the first paragraph with its
// lines trimmed and joined by one space, and the body is everything after the
// blank line that ends it. A line that holds only whitespace is blank, and a
// CRLF line ending reads as LF. A footer with no blank line above it is
// therefore part of the subject, where it declares nothing.
func splitCommitMessage(message string) (subject, body string) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	lines = lines[start:]
	end := 0
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		lines[end] = strings.TrimSpace(lines[end])
		end++
	}
	return strings.Join(lines[:end], " "), strings.Join(lines[min(end+1, len(lines)):], "\n")
}

func declaringCommits(n int) string {
	if n == 1 {
		return "1 commit declares"
	}
	return fmt.Sprintf("%d commits declare", n)
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
