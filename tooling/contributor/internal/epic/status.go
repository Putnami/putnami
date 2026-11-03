package epic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"

	"go.putnami.dev/tooling/contributor/internal/estimate"
)

// Provider calls one operation of a collaboration contract through the host
// channel and returns its envelope. An error means the call did not reach a
// provider.
type Provider func(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error)

// StatusCommand is one run of `putnami epic status`.
type StatusCommand struct {
	Root        string
	ContextFile string
	// Repository is the lowercase owner/name of the workspace's origin, the
	// repository a child's mission belongs to. Empty means unknown: the
	// command then reads no mission.
	Repository string
	Call       Provider
	Stdout     io.Writer
	Stderr     io.Writer
}

// Issue states, from the first a planned issue holds to the last.
const (
	StatePlanned    = "planned"
	StateLaunched   = "launched"
	StateInProgress = "in_progress"
	StateReady      = "ready"
	StateMerged     = "merged"
	StateClosed     = "closed"
)

// StatusResult is what epic status reports.
type StatusResult struct {
	ProtocolVersion int               `json:"protocolVersion"`
	Plan            string            `json:"plan"`
	Epic            string            `json:"epic"`
	Title           string            `json:"title"`
	Envelope        estimate.Envelope `json:"envelope"`
	// Tokens is the sum of the tokens every child session reported.
	Tokens        int64 `json:"tokens"`
	EnvelopeSpent bool  `json:"envelopeSpent"`
	// Running counts the issues whose session runs: one with a working,
	// waiting or handed-back session, or launched with no session yet. When
	// no session was read, every launched, in-progress or ready issue counts.
	Running     int           `json:"running"`
	Issues      []IssueStatus `json:"issues"`
	Launchable  []Launchable  `json:"launchable"`
	Unavailable []Unavailable `json:"unavailable"`
}

// IssueStatus is the state of one issue of the epic.
type IssueStatus struct {
	Key       string            `json:"key"`
	Title     string            `json:"title"`
	Task      *estimate.TaskRef `json:"task,omitempty"`
	DependsOn []string          `json:"dependsOn"`
	Wave      int               `json:"wave"`
	State     string            `json:"state"`
	TaskState collab.TaskState  `json:"taskState,omitempty"`
	Branch    string            `json:"branch,omitempty"`
	Base      string            `json:"base,omitempty"`
	Proposal  *ProposalView     `json:"proposal,omitempty"`
	Sessions  int               `json:"sessions"`
	// Live counts the sessions that work, wait or were handed back; an
	// ended or failed session does not run.
	Live  int              `json:"live"`
	Stage collab.LoopStage `json:"stage,omitempty"`
	// Tokens is the sum of the tokens the issue's sessions reported;
	// UsageReported is false when none reported usage or no mission was read.
	Tokens        int64 `json:"tokens"`
	UsageReported bool  `json:"usageReported"`
	// Share is the high bound of the issue's token estimate. OverShare is
	// true past 150 % of it, where the child stops for the owner.
	Share     int  `json:"share"`
	OverShare bool `json:"overShare"`
	// Rebase is the command a stacked issue runs once its parent merged.
	Rebase string `json:"rebase,omitempty"`
	// SessionsRead is true when the issue's mission was read.
	SessionsRead bool `json:"sessionsRead"`
}

// ProposalView is the part of a proposal the status shows.
type ProposalView struct {
	Ref        collab.Ref           `json:"ref"`
	URL        string               `json:"url,omitempty"`
	State      collab.ProposalState `json:"state"`
	HeadCommit string               `json:"headCommit,omitempty"`
}

// Launchable is an issue that may launch now, and the branch it starts
// from. StacksOn names the unmerged parent it stacks on, if any.
type Launchable struct {
	Issue    string `json:"issue"`
	Base     string `json:"base"`
	StacksOn string `json:"stacksOn,omitempty"`
}

// Unavailable is an answer the status could not use. Issue is empty when the
// answer stopped every call to the contract.
type Unavailable struct {
	Contract  string `json:"contract"`
	Operation string `json:"operation"`
	Issue     string `json:"issue,omitempty"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message,omitempty"`
}

// callTimeout bounds one provider call.
const callTimeout = 30 * time.Second

// maxSessionPages bounds the pages of sessions read for one mission.
const maxSessionPages = 10

// Run reports the epic the args name and returns the exit code: 2 for a
// usage error, 1 for a plan, a launch record or a policy it refuses or a
// provider it cannot reach, 0 for a status.
func (c StatusCommand) Run(ctx context.Context, args []string) int {
	loaded, code := load("epic status", c.Root, c.ContextFile, args, c.Stderr)
	if code != 0 {
		return code
	}
	launches, err := readLaunches(filepath.Join(filepath.Dir(loaded.file), LaunchesFile),
		filepath.Join(filepath.Dir(loaded.result.Plan), LaunchesFile), loaded.plan)
	if err != nil {
		fmt.Fprintf(c.Stderr, "epic status: %v\n", err)
		return 1
	}
	reader := &statusReader{call: c.Call, repository: c.Repository, disabled: map[string]bool{}}
	result, err := reader.status(ctx, loaded, launches)
	if err != nil {
		fmt.Fprintf(c.Stderr, "epic status: %v\n", err)
		return 1
	}
	if loaded.structured {
		return writeJSON("epic status", c.Stdout, c.Stderr, result)
	}
	writeStatusText(c.Stdout, result)
	return 0
}

// statusReader reads the children through the providers. A contract whose
// provider answered unsupported or denied is not called again.
type statusReader struct {
	call        Provider
	repository  string
	disabled    map[string]bool
	unavailable []Unavailable
}

// errSkipped reports a call the reader did not make, or whose answer it
// recorded as unavailable.
var errSkipped = errors.New("skipped")

func (r *statusReader) do(ctx context.Context, issue, contract, operation string, input, output any) error {
	if r.disabled[contract] {
		return errSkipped
	}
	if r.call == nil {
		return errors.New("no collaboration provider: run it as `putnami epic status`, which opens the host channel")
	}
	request, err := json.Marshal(input)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	envelope, err := r.call(callCtx, contract, operation, request)
	if err != nil {
		return fmt.Errorf("%s.%s: %w", contract, operation, err)
	}
	if envelope.Outcome != collab.OutcomeOK {
		entry := Unavailable{Contract: contract, Operation: operation, Issue: issue, Outcome: string(envelope.Outcome)}
		if envelope.Error != nil {
			entry.Message = envelope.Error.Message
		}
		if envelope.Outcome == collab.OutcomeUnsupported || envelope.Outcome == collab.OutcomeDenied {
			r.disabled[contract] = true
			entry.Issue = ""
		}
		r.unavailable = append(r.unavailable, entry)
		return errSkipped
	}
	if err := json.Unmarshal(envelope.Result, output); err != nil {
		return fmt.Errorf("invalid %s.%s result: %w", contract, operation, err)
	}
	return nil
}

func (r *statusReader) status(ctx context.Context, loaded loaded, launches map[string]Launch) (StatusResult, error) {
	plan, estimated := loaded.plan, loaded.result
	result := StatusResult{
		ProtocolVersion: estimate.ResultProtocolVersion, Plan: estimated.Plan, Epic: plan.Epic, Title: plan.Title,
		Envelope: plan.Envelope, Issues: []IssueStatus{}, Launchable: []Launchable{}, Unavailable: []Unavailable{},
	}
	byKey := map[string]int{}
	for i, issue := range estimated.Issues {
		status, err := r.issue(ctx, plan, issue, launches)
		if err != nil {
			return StatusResult{}, err
		}
		byKey[issue.Key] = i
		result.Issues = append(result.Issues, status)
		result.Tokens += status.Tokens
		if running(status) {
			result.Running++
		}
	}
	for i := range result.Issues {
		if err := r.rebase(ctx, plan, &result.Issues[i], result.Issues, byKey); err != nil {
			return StatusResult{}, err
		}
	}
	result.EnvelopeSpent = result.Tokens >= int64(plan.Envelope.Tokens)
	result.Launchable = launchable(plan, result)
	if r.unavailable != nil {
		result.Unavailable = r.unavailable
	}
	return result, nil
}

// issue reads one issue's task, sessions and proposal, and derives its state.
func (r *statusReader) issue(ctx context.Context, plan estimate.EpicPlan, issue estimate.IssueEstimate, launches map[string]Launch) (IssueStatus, error) {
	status := IssueStatus{
		Key: issue.Key, Title: issue.Title, Task: issue.Task, DependsOn: issue.DependsOn, Wave: issue.Wave,
		State: StatePlanned, Share: issue.Tokens.High,
	}
	launch, launched := launches[issue.Key]
	if launched {
		status.Branch, status.Base = launch.Branch, launch.Base
	}
	if issue.Task != nil {
		var found collab.TaskResult
		err := r.do(ctx, issue.Key, collab.ContractTasks, collab.OperationGet,
			collab.TaskRefInput{Ref: collab.Ref{Source: issue.Task.Source, ID: issue.Task.ID}}, &found)
		if err != nil && !errors.Is(err, errSkipped) {
			return IssueStatus{}, err
		}
		status.TaskState = found.Task.State
		if err := r.sessions(ctx, &status); err != nil {
			return IssueStatus{}, err
		}
	}
	if status.Branch != "" {
		if err := r.proposal(ctx, plan, &status); err != nil {
			return IssueStatus{}, err
		}
	}
	status.State = issueState(status, launched)
	status.OverShare = status.Tokens*2 > int64(status.Share)*3
	return status, nil
}

// sessions reads the sessions of the issue's mission, issue-<id>, and sums
// their tokens. A task id that is not a number names no mission.
func (r *statusReader) sessions(ctx context.Context, status *IssueStatus) error {
	if !numeric(status.Task.ID) {
		return nil
	}
	if r.repository == "" {
		if !r.disabled[collab.ContractMissions] {
			r.disabled[collab.ContractMissions] = true
			r.unavailable = append(r.unavailable, Unavailable{Contract: collab.ContractMissions, Operation: collab.OperationStatus,
				Outcome: string(collab.OutcomeUnsupported), Message: "the origin remote names no repository"})
		}
		return nil
	}
	input := collab.MissionsStatusInput{
		Mission: collab.MissionName{Repository: r.repository, Key: "issue-" + status.Task.ID},
		Page:    &collab.PageRequest{Size: collab.MaxPageSize},
	}
	for page := 0; page < maxSessionPages; page++ {
		var answer collab.MissionsStatusResult
		err := r.do(ctx, status.Key, collab.ContractMissions, collab.OperationStatus, input, &answer)
		if errors.Is(err, errSkipped) {
			return nil
		}
		if err != nil {
			return err
		}
		status.SessionsRead = true
		for _, session := range answer.Sessions {
			switch session.State {
			case collab.SessionStateWorking, collab.SessionStateWaiting, collab.SessionStateHandedBack:
				status.Live++
			}
			if status.Sessions == 0 {
				status.Stage = session.Stage
				if status.Branch == "" {
					status.Branch = session.Branch
				}
			}
			status.Sessions++
			if session.Usage != nil {
				status.UsageReported = true
				status.Tokens += session.Usage.InputTokens + session.Usage.OutputTokens +
					session.Usage.CacheWriteTokens + session.Usage.CacheReadTokens
			}
		}
		if answer.Page.Next == "" {
			return nil
		}
		input.Page = &collab.PageRequest{Size: collab.MaxPageSize, Cursor: answer.Page.Next}
	}
	r.unavailable = append(r.unavailable, Unavailable{Contract: collab.ContractMissions, Operation: collab.OperationStatus,
		Issue: status.Key, Outcome: string(collab.OutcomeOK), Message: fmt.Sprintf("only the first %d sessions were read", maxSessionPages*collab.MaxPageSize)})
	return nil
}

// proposal finds the issue's proposal from its branch: first against the
// base it launched from, then against the epic's base, which a stacked
// proposal targets once its parent merged.
func (r *statusReader) proposal(ctx context.Context, plan estimate.EpicPlan, status *IssueStatus) error {
	bases := []string{plan.Base}
	if status.Base != "" && status.Base != plan.Base {
		bases = []string{status.Base, plan.Base}
	}
	for _, base := range bases {
		var found collab.ProposalListResult
		err := r.do(ctx, status.Key, collab.ContractProposals, collab.OperationFind,
			collab.ProposalFindInput{Change: collab.Change{Base: base, Head: status.Branch}}, &found)
		if errors.Is(err, errSkipped) {
			return nil
		}
		if err != nil {
			return err
		}
		if p, ok := preferred(found.Items); ok {
			status.Proposal = &ProposalView{Ref: p.Ref, URL: p.URL, State: p.State, HeadCommit: p.Change.HeadCommit}
			return nil
		}
	}
	return nil
}

// preferred picks the proposal that stands for the branch: a merged one,
// else an open or draft one, else a closed one. A closed early proposal
// never hides the one that replaced it.
func preferred(items []collab.Proposal) (collab.Proposal, bool) {
	rank := func(state collab.ProposalState) int {
		switch state {
		case collab.ProposalStateMerged:
			return 0
		case collab.ProposalStateClosed:
			return 2
		}
		return 1
	}
	best := -1
	for i, p := range items {
		if best < 0 || rank(p.State) < rank(items[best].State) {
			best = i
		}
	}
	if best < 0 {
		return collab.Proposal{}, false
	}
	return items[best], true
}

// rebase sets the command a stacked issue runs once the parent it launched
// on merged: git rebase --onto <base> <parent's last head>, the head commit
// proposals.status reports for the merged parent.
func (r *statusReader) rebase(ctx context.Context, plan estimate.EpicPlan, status *IssueStatus, issues []IssueStatus, byKey map[string]int) error {
	if status.Base == "" || status.Base == plan.Base || status.State == StateMerged || status.State == StateClosed {
		return nil
	}
	for _, key := range status.DependsOn {
		parent := issues[byKey[key]]
		if parent.Branch != status.Base || parent.State != StateMerged || parent.Proposal == nil {
			continue
		}
		var answer collab.ProposalStatusResult
		err := r.do(ctx, status.Key, collab.ContractProposals, collab.OperationStatus,
			collab.ProposalRefInput{Ref: parent.Proposal.Ref}, &answer)
		if errors.Is(err, errSkipped) {
			return nil
		}
		if err != nil {
			return err
		}
		if head := answer.Proposal.Change.HeadCommit; head != "" {
			status.Rebase = "git rebase --onto " + plan.Base + " " + head
		}
		return nil
	}
	return nil
}

// running reports whether the issue's session takes one of the envelope's
// sessions at once.
func running(status IssueStatus) bool {
	switch status.State {
	case StateMerged, StateClosed, StatePlanned:
		return false
	case StateLaunched:
		return true
	}
	if status.SessionsRead {
		return status.Live > 0
	}
	return true
}

// issueState derives an issue's state from its proposal, its task, its
// sessions and its launch record, in that order.
func issueState(status IssueStatus, launched bool) string {
	if status.Proposal != nil {
		switch status.Proposal.State {
		case collab.ProposalStateMerged:
			return StateMerged
		case collab.ProposalStateClosed:
			return StateClosed
		case collab.ProposalStateOpen:
			return StateReady
		}
	}
	if status.TaskState == collab.TaskStateDone || status.TaskState == collab.TaskStateCanceled {
		return StateClosed
	}
	if status.Proposal != nil || status.Sessions > 0 {
		return StateInProgress
	}
	if launched {
		return StateLaunched
	}
	return StatePlanned
}

// launchable names the planned issues that may launch now, in wave order:
// each has a task, and every dependency merged, or exactly one not merged
// whose proposal is ready and which starts from the epic's base, on whose
// branch it stacks: a stack is never two levels deep. None launches once the
// envelope is spent, nor past the envelope's sessions at once.
func launchable(plan estimate.EpicPlan, result StatusResult) []Launchable {
	out := []Launchable{}
	if result.EnvelopeSpent {
		return out
	}
	slots := plan.Envelope.Sessions - result.Running
	byKey := map[string]IssueStatus{}
	for _, issue := range result.Issues {
		byKey[issue.Key] = issue
	}
	waves := 0
	for _, issue := range result.Issues {
		waves = max(waves, issue.Wave)
	}
	for wave := 1; wave <= waves; wave++ {
		for _, issue := range result.Issues {
			if slots <= 0 {
				return out
			}
			if issue.Wave != wave || issue.State != StatePlanned || issue.Task == nil {
				continue
			}
			var open []IssueStatus
			for _, key := range issue.DependsOn {
				if parent := byKey[key]; parent.State != StateMerged {
					open = append(open, parent)
				}
			}
			switch {
			case len(open) == 0:
				out = append(out, Launchable{Issue: issue.Key, Base: plan.Base})
			case len(open) == 1 && open[0].State == StateReady && open[0].Branch != "" &&
				(open[0].Base == "" || open[0].Base == plan.Base):
				out = append(out, Launchable{Issue: issue.Key, Base: open[0].Branch, StacksOn: open[0].Key})
			default:
				continue
			}
			slots--
		}
	}
	return out
}

func numeric(id string) bool {
	if id == "" || len(id) > 9 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// LaunchesFile is the launch record file, beside the epic plan.
const LaunchesFile = "launches.json"

// LaunchTargetLocal is the only launch target: a local workspace the owner
// opens.
const LaunchTargetLocal = "local"

// Launches is the launch record file: one entry per launch, oldest first.
type Launches struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Epic            string   `json:"epic"`
	Launches        []Launch `json:"launches"`
}

// Launch records that the epic session handed one issue to a session: the
// target, the branch the session works on and the branch it starts from.
type Launch struct {
	Issue      string `json:"issue"`
	Target     string `json:"target"`
	Branch     string `json:"branch"`
	Base       string `json:"base"`
	RecordedAt string `json:"recordedAt"`
}

// readLaunches reads the launch record file of plan, and answers each
// issue's latest launch by key. A missing file records no launch.
func readLaunches(path, shown string, plan estimate.EpicPlan) (map[string]Launch, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Launch{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file Launches
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	if file.ProtocolVersion != 1 {
		return nil, fmt.Errorf("%s: protocolVersion must be 1", shown)
	}
	if file.Epic != plan.Epic {
		return nil, fmt.Errorf("%s: records epic %q, not %q", shown, file.Epic, plan.Epic)
	}
	keys := map[string]bool{}
	for _, issue := range plan.Issues {
		keys[issue.Key] = true
	}
	out := map[string]Launch{}
	for i, launch := range file.Launches {
		at := fmt.Sprintf("%s: launches[%d]", shown, i)
		switch {
		case !keys[launch.Issue]:
			return nil, fmt.Errorf("%s.issue: %q names no issue of the epic", at, launch.Issue)
		case launch.Target != LaunchTargetLocal:
			return nil, fmt.Errorf("%s.target: must be %q", at, LaunchTargetLocal)
		case !refName(launch.Branch):
			return nil, fmt.Errorf("%s.branch: must be a branch name", at)
		case !refName(launch.Base):
			return nil, fmt.Errorf("%s.base: must be a branch name", at)
		}
		if _, err := time.Parse(time.RFC3339, launch.RecordedAt); err != nil {
			return nil, fmt.Errorf("%s.recordedAt: must be an RFC 3339 time", at)
		}
		out[launch.Issue] = launch
	}
	return out, nil
}

// refName reports whether name is 1 to 255 printable ASCII characters with
// no space, as a branch name is.
func refName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, r := range name {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}

// writeStatusText writes the status for a reader: the spend, each wave and
// its issues, then what may launch, what must rebase and what was not read.
func writeStatusText(w io.Writer, result StatusResult) {
	fmt.Fprintf(w, "Epic %s: %s\n", result.Plan, result.Title)
	fmt.Fprintf(w, "  Spent: %s of %s tokens; %d of %d sessions running\n", estimate.Millions(int(result.Tokens)),
		estimate.Millions(result.Envelope.Tokens), result.Running, result.Envelope.Sessions)
	if result.EnvelopeSpent {
		fmt.Fprintln(w, "  The envelope is spent: launch nothing new.")
	}
	waves := 0
	for _, issue := range result.Issues {
		waves = max(waves, issue.Wave)
	}
	for wave := 1; wave <= waves; wave++ {
		fmt.Fprintf(w, "Wave %d\n", wave)
		for _, issue := range result.Issues {
			if issue.Wave == wave {
				fmt.Fprintf(w, "  %s%s: %s\n", issue.Key, taskText(issue.Task), issueText(issue))
			}
		}
	}
	if len(result.Launchable) > 0 {
		fmt.Fprintln(w, "Launch now")
		for _, l := range result.Launchable {
			stacked := ""
			if l.StacksOn != "" {
				stacked = ", stacked on " + l.StacksOn
			}
			fmt.Fprintf(w, "  %s: from %s%s\n", l.Issue, l.Base, stacked)
		}
	}
	var rebases []string
	for _, issue := range result.Issues {
		if issue.Rebase != "" {
			rebases = append(rebases, "  "+issue.Key+": "+issue.Rebase)
		}
	}
	if len(rebases) > 0 {
		fmt.Fprintln(w, "Rebase")
		fmt.Fprintln(w, strings.Join(rebases, "\n"))
	}
	if len(result.Unavailable) > 0 {
		fmt.Fprintln(w, "Not read")
		for _, u := range result.Unavailable {
			scope := ""
			if u.Issue != "" {
				scope = " for " + u.Issue
			}
			message := ""
			if u.Message != "" {
				message = ": " + u.Message
			}
			fmt.Fprintf(w, "  %s.%s%s answered %s%s\n", u.Contract, u.Operation, scope, u.Outcome, message)
		}
	}
}

func issueText(issue IssueStatus) string {
	parts := []string{strings.ReplaceAll(issue.State, "_", " ")}
	if issue.Stage != "" {
		parts[0] += " at " + string(issue.Stage)
	}
	if issue.SessionsRead && issue.Live == 0 && (issue.State == StateInProgress || issue.State == StateReady) {
		parts = append(parts, "no live session")
	}
	if issue.Proposal != nil && issue.Proposal.URL != "" {
		parts = append(parts, issue.Proposal.URL)
	} else if issue.Branch != "" {
		parts = append(parts, "branch "+issue.Branch)
	}
	if issue.UsageReported {
		parts = append(parts, estimate.Millions(int(issue.Tokens))+" tokens of a "+estimate.Millions(issue.Share)+" share")
	} else {
		parts = append(parts, "no usage reported, share "+estimate.Millions(issue.Share))
	}
	if issue.OverShare {
		parts = append(parts, "more than 50 % over its share")
	}
	return strings.Join(parts, ", ")
}
