package deliverycli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.putnami.dev/client"
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	"go.putnami.dev/cloud/extension/internal/clicore"
)

// `putnami cloud ci queue list|pause|resume` read and drive the line of a
// branch's merge queue.
//
// list prints the entries in line order, the requests that wait outside the
// line and what each waits for, the entries that landed or dropped in the last
// 24 hours, and the commit a pull request of the branch rebases on. pause
// stops a queue that is on: nothing is built and nothing lands, and every
// entry keeps its place. resume moves the queue again from its first entry.

// queuePauseReasonMax mirrors delivery-api's limit on a pause reason.
const queuePauseReasonMax = 500

// CIQueueEntries is the CLI's twin of delivery-api's QueueEntries.
type CIQueueEntries struct {
	Repository string              `json:"repository"`
	Branch     string              `json:"branch"`
	Open       []CIQueueEntry      `json:"open"`
	Recent     []CIQueueEndedEntry `json:"recent"`
	RebaseOn   CIQueueRebaseOn     `json:"rebaseOn"`
}

// CIQueueEntry is one open entry: in the line when Position is set, otherwise
// a request that waits for WaitingFor.
type CIQueueEntry struct {
	Position      int              `json:"position,omitempty"`
	PullRequest   int              `json:"pullRequest"`
	Title         string           `json:"title"`
	Author        string           `json:"author"`
	State         string           `json:"state"`
	WaitingFor    string           `json:"waitingFor,omitempty"`
	WaitingDetail string           `json:"waitingDetail,omitempty"`
	Conflict      *CIQueueConflict `json:"conflict,omitempty"`
	HeadSHA       string           `json:"headSha"`
	BuiltOn       string           `json:"builtOn,omitempty"`
	QueueCommit   string           `json:"queueCommit,omitempty"`
	RunID         string           `json:"runId,omitempty"`
	Attempt       int              `json:"attempt"`
	LostRuns      int              `json:"lostRuns,omitempty"`
	RequestedAt   *time.Time       `json:"requestedAt,omitempty"`
	EnteredAt     *time.Time       `json:"enteredAt,omitempty"`
	UpdatedAt     *time.Time       `json:"updatedAt,omitempty"`
}

// CIQueueConflict says what a waiting request conflicts with.
type CIQueueConflict struct {
	With  int        `json:"with,omitempty"`
	On    string     `json:"on"`
	Files []string   `json:"files"`
	More  int        `json:"more,omitempty"`
	At    *time.Time `json:"at,omitempty"`
}

// CIQueueEndedEntry is an entry that landed or dropped.
type CIQueueEndedEntry struct {
	PullRequest int        `json:"pullRequest"`
	Title       string     `json:"title"`
	Author      string     `json:"author"`
	State       string     `json:"state"`
	HeadSHA     string     `json:"headSha"`
	QueueCommit string     `json:"queueCommit,omitempty"`
	RunID       string     `json:"runId,omitempty"`
	Attempt     int        `json:"attempt"`
	LostRuns    int        `json:"lostRuns,omitempty"`
	LandedSHA   string     `json:"landedSha,omitempty"`
	LandingNote string     `json:"landingNote,omitempty"`
	DropReason  string     `json:"dropReason,omitempty"`
	DropDetail  string     `json:"dropDetail,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
}

// CIQueueRebaseOn is the commit a pull request of the branch rebases on.
type CIQueueRebaseOn struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// CIQueueLine is the structured output of `queue list`.
type CIQueueLine struct {
	Setting CIQueueSetting `json:"setting"`
	Entries CIQueueEntries `json:"entries"`
}

func readQueueEntries(reqCtx context.Context, ctx *clicore.WorkspaceContext, branch string) (*CIQueueEntries, error) {
	target := ctx.WorkspaceURL("/ci/queue/entries") + "?branch=" + url.QueryEscape(branch)
	return queueCall[deliveryapiclient.QueueEntries, CIQueueEntries](reqCtx, ctx, "list", target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.QueueEntries, error) {
			return api.GetV1WorkspacesCiQueueEntries(callCtx, deliveryapiclient.GetV1WorkspacesCiQueueEntriesInput{
				Path:  deliveryapiclient.GetV1WorkspacesCiQueueEntriesPath{Workspace: ctx.WorkspaceID},
				Query: deliveryapiclient.GetV1WorkspacesCiQueueEntriesQuery{Branch: branch},
			})
		})
}

func ciQueueList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	setting, err := readQueueSetting(reqCtx, ctx, "list", branch)
	if err != nil {
		return err
	}
	entries, err := readQueueEntries(reqCtx, ctx, branch)
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(CIQueueLine{Setting: *setting, Entries: *entries}, params, ioctx, "")
		return nil
	}
	for _, line := range queueLineLines(setting, entries, branch) {
		ioctx.Stdout(line)
	}
	return nil
}

func queueLineLines(setting *CIQueueSetting, entries *CIQueueEntries, branch string) []string {
	lines := []string{queueSettingLine(setting, branch)}
	var inLine, waiting []CIQueueEntry
	for _, entry := range entries.Open {
		if entry.Position > 0 {
			inLine = append(inLine, entry)
		} else {
			waiting = append(waiting, entry)
		}
	}
	if len(inLine) == 0 {
		lines = append(lines, "In line: none.")
	} else {
		lines = append(lines, fmt.Sprintf("In line: %d.", len(inLine)))
		for _, entry := range inLine {
			lines = append(lines, queueEntryLine(entry))
		}
	}
	if len(waiting) > 0 {
		lines = append(lines, fmt.Sprintf("Waiting to enter: %d.", len(waiting)))
		for _, entry := range waiting {
			lines = append(lines, queueWaitingLine(entry))
		}
	}
	if len(entries.Recent) > 0 {
		lines = append(lines, "Landed or dropped in the last 24 hours:")
		for _, entry := range entries.Recent {
			lines = append(lines, queueEndedLine(entry))
		}
	}
	if entries.RebaseOn.Ref != "" {
		lines = append(lines,
			fmt.Sprintf("Rebase on %s (%s):", entries.RebaseOn.Ref, shortSHA(entries.RebaseOn.SHA)),
			fmt.Sprintf("  git fetch origin %s && git rebase FETCH_HEAD", entries.RebaseOn.Ref))
	}
	return lines
}

func queueEntryLine(entry CIQueueEntry) string {
	line := fmt.Sprintf("  %d. #%d %s %s", entry.Position, entry.PullRequest, entry.State, queueTitle(entry.Title, entry.Author))
	var facts []string
	if entry.BuiltOn != "" {
		facts = append(facts, "built on "+shortSHA(entry.BuiltOn))
	}
	if entry.QueueCommit != "" {
		facts = append(facts, "queue commit "+shortSHA(entry.QueueCommit))
	}
	if entry.RunID != "" {
		facts = append(facts, "run "+ciShortRunID(entry.RunID))
	}
	if entry.Attempt > 1 {
		facts = append(facts, fmt.Sprintf("attempt %d", entry.Attempt))
	}
	if entry.LostRuns > 0 {
		facts = append(facts, fmt.Sprintf("%d lost run(s)", entry.LostRuns))
	}
	if len(facts) > 0 {
		line += ": " + strings.Join(facts, ", ")
	}
	return line
}

func queueWaitingLine(entry CIQueueEntry) string {
	line := fmt.Sprintf("  #%d %s", entry.PullRequest, queueTitle(entry.Title, entry.Author))
	if entry.WaitingFor == "" {
		return line + ": enters at the next pass"
	}
	line += ": waits for " + entry.WaitingFor
	if entry.WaitingDetail != "" {
		line += " (" + entry.WaitingDetail + ")"
	}
	return line
}

func queueEndedLine(entry CIQueueEndedEntry) string {
	line := fmt.Sprintf("  #%d %s %s", entry.PullRequest, entry.State, queueTitle(entry.Title, entry.Author))
	switch {
	case entry.LandedSHA != "":
		line += ": " + shortSHA(entry.LandedSHA)
	case entry.DropReason != "":
		line += ": " + entry.DropReason
		if entry.DropDetail != "" {
			line += " (" + entry.DropDetail + ")"
		}
	}
	if entry.LandingNote != "" {
		line += "; " + entry.LandingNote
	}
	if entry.EndedAt != nil {
		line += ", " + entry.EndedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return line
}

func queueTitle(title, author string) string {
	out := fmt.Sprintf("%q", title)
	if author != "" {
		out += " by " + author
	}
	return out
}

func queuePausedBy(setting *CIQueueSetting) string {
	pause := setting.Pause
	if pause == nil {
		return ""
	}
	out := ", paused by " + clicore.FirstString(pause.ByLogin, pause.By, "someone")
	if pause.At != nil {
		out += " on " + pause.At.UTC().Format("2006-01-02 15:04 UTC")
	}
	if pause.Reason != "" {
		out += fmt.Sprintf(": %q", pause.Reason)
	}
	return out
}

func ciQueuePause(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	if len(reason) > queuePauseReasonMax {
		return clicore.NewError(fmt.Sprintf("--reason holds at most %d bytes", queuePauseReasonMax), clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	setting, err := readQueueSetting(reqCtx, ctx, "pause", branch)
	if err != nil {
		return err
	}
	if !setting.Enabled {
		return clicore.NewError("cloud ci queue pause: "+queueSettingLine(setting, branch)+" Only a queue that is on pauses.", clicore.ExitFailure)
	}
	if setting.Paused {
		clicore.WriteResult(CIQueueResult{Setting: *setting}, params, ioctx, queueSettingLine(setting, branch))
		return nil
	}
	if err := ciConfirm(params, ioctx, fmt.Sprintf("Pause the merge queue of %s of %s? Nothing is built and nothing lands "+
		"until you resume it; every entry keeps its place.", branch, setting.Repository)); err != nil {
		return err
	}
	updated, err := putQueueSetting(reqCtx, ctx, "pause", branch, deliveryapiclient.QueueSettingRequest{Paused: client.Some(true), Reason: &reason})
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(CIQueueResult{Setting: *updated}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout(queueSettingLine(updated, branch))
	ioctx.Stdout("A queue run already running finishes; new requests still enter in order. Resume with `putnami cloud ci queue resume" + queueBranchFlag(branch) + "`.")
	return nil
}

func ciQueueResume(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	setting, err := readQueueSetting(reqCtx, ctx, "resume", branch)
	if err != nil {
		return err
	}
	if !setting.Paused {
		clicore.WriteResult(CIQueueResult{Setting: *setting}, params, ioctx, queueSettingLine(setting, branch))
		return nil
	}
	updated, err := putQueueSetting(reqCtx, ctx, "resume", branch, deliveryapiclient.QueueSettingRequest{Paused: client.Some(false)})
	if err != nil {
		return err
	}
	clicore.WriteResult(CIQueueResult{Setting: *updated}, params, ioctx,
		queueSettingLine(updated, branch)+" The queue moves again from its first entry.")
	return nil
}
