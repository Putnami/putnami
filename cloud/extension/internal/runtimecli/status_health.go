package runtimecli

// status_health.go adds `putnami cloud env status --health`. For each row
// of the --env environment it adds three columns:
//
//	ERRORS (10m)  entries at severity ERROR or above in the last 10 minutes
//	TOP ERROR     the most frequent of those messages
//	ON MAIN       whether the served commit is on main in the local checkout
//
// ONE LOGS READ PER ENVIRONMENT. The errors come from one
// GET /v1/workspaces/{workspace}/logs?environment=<env>&level=error&from=&to=
// read, not one read per service: every page is one Cloud Logging entries.list
// call, and that API allows 60 calls per minute per project, which a
// 25-service fan-out would exhaust in three runs. Entries are grouped by their
// `service` label on the client. The deploy stamps that label with the folded
// app name (statusServiceLabel), and a collector-written entry would carry the
// raw app path, so a row matches either form.
//
// THE COUNT IS HONEST. The read stops after statusHealthMaxPages pages. When
// entries remain past that point, or a later page fails, every count is a
// lower bound: the cell reads "N+" and the JSON says capped. An entry with no
// service label, or with a label that matches no row (or several rows), is
// counted separately and never attributed to a row.
//
// THE LOGS READ NEVER FAILS THE COMMAND. It runs concurrently with the summary
// and the CD header, with the same per-request timeout and the same
// no-re-mint context copy as the header. A failed first page (403 for a
// machine token, 404 on an older control plane, 5xx, a timeout) prints
// `Errors <env>: unavailable (<reason>)` above the table and "-" in the
// columns.
//
// ON MAIN READS THE LOCAL CHECKOUT ONLY. It never fetches. It is "yes" when the
// served commit is an ancestor of origin/main (or main when origin/main does
// not resolve), "no" when the commit is present locally and is not, and
// "unknown" otherwise. A squash-merged pull request head reads "no": that exact
// commit is not on main. Git runs at most once to find the main ref and once
// per distinct commit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// statusHealthWindow is the lookback of the ERRORS (10m) column, and
	// statusHealthWindowLabel names it in the column and the JSON.
	statusHealthWindow      = 10 * time.Minute
	statusHealthWindowLabel = "10m"
	// statusHealthLevel is the minimum severity the logs read selects, in the
	// logs endpoint's level vocabulary: ERROR and above.
	statusHealthLevel = "error"
	// statusHealthPageLimit is the logs endpoint's page ceiling.
	statusHealthPageLimit = 1000
	// statusHealthMaxPages bounds the logs read: at most this many
	// entries.list calls per run, and 3000 entries.
	statusHealthMaxPages = 3
	// statusTopErrorWidth bounds the TOP ERROR cell.
	statusTopErrorWidth = 60
	// statusGitTimeout bounds every git process ON MAIN runs.
	statusGitTimeout = 5 * time.Second
	// statusServiceAttribute is the log entry label naming the service. It
	// mirrors the provisioner's otelAttrService and the reader's AttrService.
	statusServiceAttribute = "service"
)

// The ON MAIN answers.
const (
	statusOnMainYes     = "yes"
	statusOnMainNo      = "no"
	statusOnMainUnknown = "unknown"
)

// statusHealthReport is the additive `health` key of the structured output.
// Logs is null when the logs read failed, in which case Unavailable says why
// and every service's errors are null. ON MAIN does not depend on the logs
// read, so services always carry it.
type statusHealthReport struct {
	Environment string `json:"environment"`
	// Window is the lookback; From and To are the exact RFC 3339 bounds sent.
	Window string `json:"window"`
	From   string `json:"from"`
	To     string `json:"to"`
	// Level is the minimum severity read.
	Level       string             `json:"level"`
	Logs        *statusLogsSummary `json:"logs"`
	Unavailable string             `json:"unavailable,omitempty"`
	// MainRef is the ref ON MAIN compared against: origin/main or main. When
	// no commit was checked it is empty; when the ref could not be resolved,
	// MainRefUnavailable says why.
	MainRef            string                `json:"main_ref,omitempty"`
	MainRefUnavailable string                `json:"main_ref_unavailable,omitempty"`
	Services           []statusServiceHealth `json:"services"`
}

// statusLogsSummary describes the logs read. When Capped is true, entries
// remained unread, so every count is a lower bound; Stopped names the failure
// when a page after the first failed.
type statusLogsSummary struct {
	Entries   int                      `json:"entries"`
	Pages     int                      `json:"pages"`
	Capped    bool                     `json:"capped"`
	Stopped   string                   `json:"stopped,omitempty"`
	Unlabeled int                      `json:"unlabeled"`
	Unmatched []statusUnmatchedService `json:"unmatched"`
}

// statusUnmatchedService counts the entries whose service label matches no
// row of the environment, or more than one.
type statusUnmatchedService struct {
	Service string `json:"service"`
	Errors  int    `json:"errors"`
}

// statusServiceHealth is one row of the --env environment. Errors is null when
// the logs read failed.
type statusServiceHealth struct {
	Project      string          `json:"project"`
	CommitSHA    string          `json:"commit_sha,omitempty"`
	OnMain       string          `json:"on_main"`
	OnMainReason string          `json:"on_main_reason,omitempty"`
	Errors       *int            `json:"errors"`
	TopError     *statusTopError `json:"top_error,omitempty"`
}

// statusTopError is the most frequent normalized message of a service and the
// number of entries that carried it.
type statusTopError struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// statusErrorsRead is the raw result of the logs read, before it is matched to
// the summary's rows.
type statusErrorsRead struct {
	From, To    time.Time
	Entries     []statusErrorEntry
	Pages       int
	Capped      bool
	Stopped     string
	Unavailable string
}

// statusErrorEntry keeps the two fields --health reads from a log entry.
type statusErrorEntry struct {
	Service string
	Message string
}

// startStatusErrorsRead starts the logs read in its own goroutine and answers
// on a buffered channel, so an abandoned read never blocks. ctx must be a
// statusHeaderContext copy.
func startStatusErrorsRead(ctx *clicore.WorkspaceContext, reqCtx context.Context, environment string, now time.Time) <-chan statusErrorsRead {
	out := make(chan statusErrorsRead, 1)
	go func() { out <- readStatusErrors(ctx, reqCtx, environment, now) }()
	return out
}

// readStatusErrors reads the environment's ERROR entries of the last
// statusHealthWindow, following the cursor for at most statusHealthMaxPages
// pages. Every page carries the same from/to, so the cursor continues one
// query. It never returns an error: a failed first page makes the read
// Unavailable, and a failed later page caps it. All pages share ONE
// statusHeaderTimeout budget, so a slow logs backend holds the command no
// longer than a single header read would.
func readStatusErrors(ctx *clicore.WorkspaceContext, reqCtx context.Context, environment string, now time.Time) statusErrorsRead {
	reqCtx, cancel := context.WithTimeout(reqCtx, statusHeaderTimeout)
	defer cancel()
	to := now.UTC().Truncate(time.Second)
	read := statusErrorsRead{From: to.Add(-statusHealthWindow), To: to}
	level, from, until, limit := statusHealthLevel, read.From.Format(time.RFC3339), read.To.Format(time.RFC3339), int64(statusHealthPageLimit)
	query := observabilityapiclient.GetV1WorkspacesLogsQuery{
		Environment: &environment, Level: &level, From: &from, To: &until, Limit: &limit,
	}
	for read.Pages < statusHealthMaxPages {
		page, unavailable := statusLogsRead(ctx, reqCtx, query)
		if unavailable != "" {
			if read.Pages == 0 {
				read.Unavailable = unavailable
			} else {
				read.Capped, read.Stopped = true, unavailable
			}
			return read
		}
		read.Pages++
		for _, entry := range clicore.Deref(page.Entries) {
			read.Entries = append(read.Entries, statusErrorEntry{
				Service: strings.TrimSpace(clicore.Deref(entry.Attributes)[statusServiceAttribute]),
				Message: statusErrorMessage(clicore.Deref(entry.Body)),
			})
		}
		cursor := clicore.Deref(page.NextCursor)
		if cursor == "" {
			return read
		}
		query.Cursor = &cursor
	}
	read.Capped = true
	return read
}

// statusErrorMessage normalizes a log body into the key TOP ERROR groups on:
// the `message` (else `msg`) string of a JSON object body, else the body
// itself; then its first line, trimmed.
func statusErrorMessage(body string) string {
	text := strings.TrimSpace(body)
	if strings.HasPrefix(text, "{") {
		var payload map[string]any
		if json.Unmarshal([]byte(text), &payload) == nil {
			for _, key := range []string{"message", "msg"} {
				if message, ok := payload[key].(string); ok && strings.TrimSpace(message) != "" {
					text = strings.TrimSpace(message)
					break
				}
			}
		}
	}
	if end := strings.IndexAny(text, "\r\n"); end >= 0 {
		text = text[:end]
	}
	return strings.TrimSpace(text)
}

// statusServiceLabel folds an app name into the `service` label value the
// deploy stamps on a Cloud Run service. MIRROR, NOT FORK: it must stay
// byte-identical to cloudRunServiceLabel in the runtime provisioner (the
// stamp) and in the observability log reader (the read filter). It is copied
// rather than imported so the CLI does not depend on the provisioner.
func statusServiceLabel(app string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(strings.TrimSpace(app)) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// buildStatusHealth matches the logs read and the ON MAIN answers to the
// summary rows of the environment, in row order.
func buildStatusHealth(resp *deploymentsSummary, environment string, read statusErrorsRead, onMain statusMainAnswers) *statusHealthReport {
	report := &statusHealthReport{
		Environment:        environment,
		Window:             statusHealthWindowLabel,
		From:               read.From.Format(time.RFC3339),
		To:                 read.To.Format(time.RFC3339),
		Level:              statusHealthLevel,
		Unavailable:        read.Unavailable,
		MainRef:            onMain.Ref,
		MainRefUnavailable: onMain.RefUnavailable,
		Services:           []statusServiceHealth{},
	}
	for _, row := range statusEnvironmentRows(resp, environment) {
		answer := onMain.answer(row.CommitSHA)
		report.Services = append(report.Services, statusServiceHealth{
			Project:      row.Project,
			CommitSHA:    row.CommitSHA,
			OnMain:       answer.State,
			OnMainReason: answer.Reason,
		})
	}
	if read.Unavailable != "" {
		return report
	}

	// A label names a row by the raw project name or by its folded form. A
	// label that names several rows is ambiguous and stays unattributed.
	rowsByLabel := map[string][]int{}
	for index, service := range report.Services {
		for _, label := range []string{strings.TrimSpace(service.Project), statusServiceLabel(service.Project)} {
			if label != "" && !slices.Contains(rowsByLabel[label], index) {
				rowsByLabel[label] = append(rowsByLabel[label], index)
			}
		}
	}
	logs := &statusLogsSummary{Entries: len(read.Entries), Pages: read.Pages, Capped: read.Capped, Stopped: read.Stopped}
	counts := make([]int, len(report.Services))
	messages := make([]map[string]int, len(report.Services))
	unmatched := map[string]int{}
	for _, entry := range read.Entries {
		rows := rowsByLabel[entry.Service]
		switch {
		case entry.Service == "":
			logs.Unlabeled++
		case len(rows) == 1:
			counts[rows[0]]++
			if messages[rows[0]] == nil {
				messages[rows[0]] = map[string]int{}
			}
			messages[rows[0]][entry.Message]++
		default:
			unmatched[entry.Service]++
		}
	}
	for index := range report.Services {
		count := counts[index]
		report.Services[index].Errors = &count
		report.Services[index].TopError = statusTopMessage(messages[index])
	}
	logs.Unmatched = statusSortedUnmatched(unmatched)
	report.Logs = logs
	return report
}

// statusEnvironmentRows lists the summary rows of one environment, in order.
func statusEnvironmentRows(resp *deploymentsSummary, environment string) []deploymentSummaryEntry {
	if resp == nil {
		return nil
	}
	var rows []deploymentSummaryEntry
	for _, row := range resp.Deployments {
		if strings.TrimSpace(row.Environment) == environment {
			rows = append(rows, row)
		}
	}
	return rows
}

// statusTopMessage picks the most frequent non-empty message; a tie goes to
// the message that sorts first, so the answer never depends on map order.
func statusTopMessage(counts map[string]int) *statusTopError {
	var top *statusTopError
	for message, count := range counts {
		if message == "" {
			continue
		}
		if top == nil || count > top.Count || (count == top.Count && message < top.Message) {
			top = &statusTopError{Message: message, Count: count}
		}
	}
	return top
}

// statusSortedUnmatched orders the unmatched labels by count, then by name.
func statusSortedUnmatched(counts map[string]int) []statusUnmatchedService {
	out := make([]statusUnmatchedService, 0, len(counts))
	for service, count := range counts {
		out = append(out, statusUnmatchedService{Service: service, Errors: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Errors != out[j].Errors {
			return out[i].Errors > out[j].Errors
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// statusOnMainAnswer is the ON MAIN state of one commit, and why it is unknown.
type statusOnMainAnswer struct {
	State  string
	Reason string
}

// statusMainAnswers holds the ON MAIN answer of every distinct commit.
type statusMainAnswers struct {
	Ref            string
	RefUnavailable string
	byCommit       map[string]statusOnMainAnswer
}

func (m statusMainAnswers) answer(sha string) statusOnMainAnswer {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return statusOnMainAnswer{State: statusOnMainUnknown, Reason: "no commit recorded"}
	}
	if answer, ok := m.byCommit[sha]; ok {
		return answer
	}
	return statusOnMainAnswer{State: statusOnMainUnknown, Reason: "not checked"}
}

// statusCommitPattern accepts a full or abbreviated hex commit id. Anything
// else never reaches git, so a recorded value can never be read as an option
// or a revision expression.
var statusCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// statusGitRun runs one read-only git command in dir and answers its trimmed
// stdout. Lazy fetches and credential prompts are disabled, so a partial
// clone never reaches the network. A package var so tests can count the
// processes.
var statusGitRun = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed argv; the only variable argument is a hex-checked commit id
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// statusOnMain answers ON MAIN for the commits of the environment's rows from
// the local checkout at dir. Git runs once to find the main ref, then once per
// distinct commit, and not at all when no row has a commit.
func statusOnMain(reqCtx context.Context, dir string, rows []deploymentSummaryEntry) statusMainAnswers {
	answers := statusMainAnswers{byCommit: map[string]statusOnMainAnswer{}}
	var commits []string
	for _, row := range rows {
		sha := strings.TrimSpace(row.CommitSHA)
		if sha == "" {
			continue
		}
		if _, seen := answers.byCommit[sha]; seen {
			continue
		}
		if !statusCommitPattern.MatchString(sha) {
			answers.byCommit[sha] = statusOnMainAnswer{State: statusOnMainUnknown, Reason: "not a commit id"}
			continue
		}
		answers.byCommit[sha] = statusOnMainAnswer{}
		commits = append(commits, sha)
	}
	if len(commits) == 0 {
		return answers
	}

	ctx, cancel := context.WithTimeout(reqCtx, statusGitTimeout)
	defer cancel()
	ref, name, reason := statusMainRef(ctx, dir)
	answers.Ref, answers.RefUnavailable = name, reason
	for _, sha := range commits {
		if ref == "" {
			answers.byCommit[sha] = statusOnMainAnswer{State: statusOnMainUnknown, Reason: reason}
			continue
		}
		answers.byCommit[sha] = statusIsAncestor(ctx, dir, sha, ref)
	}
	return answers
}

// statusMainRef finds the main ref in one git process: refs/remotes/origin/main
// when it exists, else refs/heads/main. It answers the full ref, its short
// name, and why neither resolved.
func statusMainRef(ctx context.Context, dir string) (string, string, string) {
	const remote, local = "refs/remotes/origin/main", "refs/heads/main"
	out, err := statusGitRun(ctx, dir, "for-each-ref", "--format=%(refname)", remote, local)
	if err != nil {
		return "", "", statusGitFailure(ctx, err, "not a git checkout")
	}
	refs := strings.Fields(out)
	switch {
	case slices.Contains(refs, remote):
		return remote, "origin/main", ""
	case slices.Contains(refs, local):
		return local, "main", ""
	default:
		return "", "", "no origin/main or main ref"
	}
}

// statusIsAncestor answers ON MAIN for one commit: git merge-base
// --is-ancestor exits 0 for an ancestor, 1 for a commit that is not one, and
// fails otherwise (a commit the checkout does not have).
func statusIsAncestor(ctx context.Context, dir, sha, ref string) statusOnMainAnswer {
	_, err := statusGitRun(ctx, dir, "merge-base", "--is-ancestor", sha, ref)
	if err == nil {
		return statusOnMainAnswer{State: statusOnMainYes}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
		return statusOnMainAnswer{State: statusOnMainNo}
	}
	return statusOnMainAnswer{State: statusOnMainUnknown, Reason: statusGitFailure(ctx, err, "commit not in the local checkout")}
}

// statusGitFailure names why a git process failed: the timeout, a missing git
// binary, or the fallback for a git error exit.
func statusGitFailure(ctx context.Context, err error, exitReason string) string {
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return "git timed out"
	case errors.As(err, &exit):
		return exitReason
	case errors.Is(err, exec.ErrNotFound):
		return "git not found"
	default:
		return "git failed"
	}
}

// renderStatusHealthNotes renders the note lines --health prints above the
// table, followed by one blank line, or nothing when there is nothing to say:
// an unavailable logs read, a capped read, entries no row could claim, and a
// main ref that did not resolve.
func renderStatusHealthNotes(health *statusHealthReport) string {
	if health == nil {
		return ""
	}
	var lines []string
	prefix := "Errors " + health.Environment + ": "
	switch {
	case health.Unavailable != "":
		lines = append(lines, prefix+"unavailable ("+health.Unavailable+")")
	case health.Logs != nil:
		var parts []string
		if health.Logs.Capped {
			stop := "the read stopped after " + statusEntries(health.Logs.Entries)
			if health.Logs.Stopped != "" {
				stop += " (" + health.Logs.Stopped + ")"
			}
			parts = append(parts, stop+", so counts are lower bounds")
		}
		if health.Logs.Unlabeled > 0 {
			parts = append(parts, fmt.Sprintf("%s without a service label", statusEntries(health.Logs.Unlabeled)))
		}
		if len(health.Logs.Unmatched) > 0 {
			total := 0
			names := make([]string, 0, len(health.Logs.Unmatched))
			for _, service := range health.Logs.Unmatched {
				total += service.Errors
				if len(names) < 3 {
					names = append(names, fmt.Sprintf("%s (%d)", service.Service, service.Errors))
				}
			}
			if len(health.Logs.Unmatched) > len(names) {
				names = append(names, "…")
			}
			parts = append(parts, fmt.Sprintf("%s from services matching no row: %s", statusEntries(total), strings.Join(names, ", ")))
		}
		if len(parts) > 0 {
			lines = append(lines, prefix+strings.Join(parts, "; "))
		}
	}
	if health.MainRefUnavailable != "" {
		lines = append(lines, "On main: unknown ("+health.MainRefUnavailable+")")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func statusEntries(count int) string {
	if count == 1 {
		return "1 entry"
	}
	return strconv.Itoa(count) + " entries"
}

// statusHealthHeader names the --health columns, in order.
var statusHealthHeader = []string{"ERRORS (" + statusHealthWindowLabel + ")", "TOP ERROR", "ON MAIN"}

// statusHealthCells renders the --health cells of one row. A row of another
// environment reads "-" in all three; so do the error cells when the logs read
// failed.
func statusHealthCells(health *statusHealthReport, row deploymentSummaryEntry) []string {
	if strings.TrimSpace(row.Environment) != health.Environment {
		return []string{"-", "-", "-"}
	}
	index := slices.IndexFunc(health.Services, func(service statusServiceHealth) bool { return service.Project == row.Project })
	if index < 0 {
		return []string{"-", "-", "-"}
	}
	service := health.Services[index]
	errorsCell, topCell := "-", "-"
	if service.Errors != nil {
		errorsCell = strconv.Itoa(*service.Errors)
		if health.Logs != nil && health.Logs.Capped {
			errorsCell += "+"
		}
	}
	if service.TopError != nil {
		topCell = statusCell(statusTruncate(statusOneLine(service.TopError.Message), statusTopErrorWidth))
	}
	return []string{errorsCell, topCell, service.OnMain}
}

// statusNow reads the command clock, falling back to the wall clock.
func statusNow(ioctx clicore.IO) time.Time {
	if ioctx.Now != nil {
		return ioctx.Now()
	}
	return time.Now()
}
