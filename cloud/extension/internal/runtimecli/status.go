package runtimecli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The deployments endpoint's response shape (workspace_id + per-project rows) is
// read from control-api's generated client into deploymentsSummary /
// deploymentSummaryEntry (control_client.go). They carry every member the
// endpoint declares, the commit-link fields (commit_sha/ref/repository_url)
// included, so --output carries the provenance the server returns.

// Status backs `putnami cloud env status`: one screen answering project ×
// environment × revision × health for the workspace, plus each project's last
// run — its status, what triggered it, and why it failed — so a failed
// deploy names its service without opening logs. It resolves the linked
// workspace + a workspace-scoped bearer (the same seam deploy/logs use), issues
// a single authenticated GET against the deployments endpoint, and renders a
// compact human table by default or the stable {workspace_id, deployments:[…]}
// object under --output=json|jsonl / --json.
//
// Above the table it prints one continuous-delivery (CD) header line for the
// --env environment: the channels it follows and the newest move of
// each. status_header.go says how a failed header read degrades; the header
// never fails the command.
//
// --health adds ERRORS (10m), TOP ERROR, and ON MAIN to the rows of the --env
// environment: one logs read for the environment, run concurrently with
// the other reads, and the served commit checked against main in the local
// checkout. status_health.go says how it counts and how it degrades; it never
// fails the command either.
//
// --provenance switches the default human table to the per-workload deploy
// provenance chain (git commit and tree → release → serving revision → config
// version), so prod state is readable in one command. It changes only
// the human view; structured output already carries every field the provenance
// table reads.
//
// Status is workspace-wide: it takes no positional app and never calls
// ResolveApp — a monorepo root has no single workload to name, and the endpoint
// already spans every project.
func Status(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}

	// signal.NotifyContext wires Ctrl-C / SIGTERM to context cancellation so a
	// hangup during the request tears the connection down promptly rather than
	// orphaning the socket — matching the deploy/observability request plumbing.
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return statusRun(ctx, reqCtx, params, workspaceRoot)
}

// statusResult is the structured object `putnami cloud env status --output` emits:
// the deployments summary with its keys and shapes unchanged, plus the CD
// headers under the additive channel_follow key and, with --health only, the
// health columns under the additive health key.
type statusResult struct {
	deploymentsSummary
	ChannelFollow []statusChannelFollow `json:"channel_follow"`
	Health        *statusHealthReport   `json:"health,omitempty"`
}

// statusChannelFollow is one environment's CD header. Channels is the
// server's answer — an empty array for an environment that follows no channel —
// and is null when the header could not be read, in which case Unavailable
// says why.
type statusChannelFollow struct {
	Environment        string            `json:"environment"`
	DefinitionRevision int64             `json:"definition_revision,omitempty"`
	Channels           []followedChannel `json:"channels"`
	Unavailable        string            `json:"unavailable,omitempty"`
}

// statusRun is everything Status does once the workspace, bearer, and
// control plane are resolved: fetch the summary and the CD headers, then render.
// Only a failed summary fails the command. With --health the logs read starts
// first and runs while the summary and the header are read; ON MAIN reads the
// git checkout at workspaceRoot once the summary names the commits.
func statusRun(ctx *clicore.WorkspaceContext, reqCtx context.Context, params map[string]any, workspaceRoot string) error {
	environment := clicore.StringParam(params, "env", "environment")
	withHealth := clicore.BoolParam(params, false, "health")
	if withHealth && environment == "" {
		return clicore.NewError("--health reads one environment: pass --env <environment>", clicore.ExitUsage)
	}
	var errorsRead <-chan statusErrorsRead
	if withHealth {
		healthCtx, cancel := context.WithCancel(reqCtx)
		defer cancel()
		errorsRead = startStatusErrorsRead(statusHeaderContext(ctx), healthCtx, environment, statusNow(ctx.IO))
	}
	resp, headers, err := statusFetchAll(ctx, reqCtx, environment)
	if err != nil {
		return err
	}
	var health *statusHealthReport
	if withHealth {
		onMain := statusOnMain(reqCtx, workspaceRoot, statusEnvironmentRows(resp, environment))
		health = buildStatusHealth(resp, environment, <-errorsRead, onMain)
	}

	// --provenance switches the human table to the per-workload provenance chain
	// (project → commit → release → revision → config version). Structured output
	// is unchanged: the response already carries every field the provenance view
	// reads (commit_sha/last_release_id/revision/config_version), so --output
	// consumers get the same stable object regardless of the flag.
	human := renderStatusTable(resp, health)
	if clicore.BoolParam(params, false, "provenance") {
		human = renderProvenanceTable(resp, health)
	}
	clicore.WriteResult(statusResult{deploymentsSummary: *resp, ChannelFollow: headers, Health: health}, params, ctx.IO,
		renderStatusHeaders(headers)+renderStatusHealthNotes(health)+human)
	return nil
}

// statusFetchAll issues the summary GET and the --env environment's CD header
// GET at once: the header does not depend on the summary. With no environment
// there is no header, and the headers slice is empty (never nil), so
// channel_follow always encodes as an array.
func statusFetchAll(ctx *clicore.WorkspaceContext, reqCtx context.Context, environment string) (*deploymentsSummary, []statusChannelFollow, error) {
	if environment == "" {
		resp, err := statusFetch(ctx, reqCtx)
		if err != nil {
			return nil, nil, err
		}
		return resp, []statusChannelFollow{}, nil
	}
	header := make(chan statusChannelFollow, 1)
	headerCtx := statusHeaderContext(ctx)
	go func() { header <- fetchChannelFollow(headerCtx, reqCtx, environment) }()
	resp, err := statusFetch(ctx, reqCtx)
	if err != nil {
		return nil, nil, err
	}
	return resp, []statusChannelFollow{<-header}, nil
}

// statusFetch reads the workspace's deployments summary through control-api's
// generated client, in place of a hand-written GET. It keeps ctx's full
// re-mint-capable WorkspaceContext: a forwarded-user credential does not renew
// itself inside the framework, so the call goes through
// clicore.CallWithSession, the CLI's own single-401 re-mint wrapper, the unary
// twin of the one status_header.go's header read intentionally does not use
// (see statusHeaderContext). A provider refusal renders as
// "status: <status line>" and exits ExitAuth on 401, ExitAPI otherwise: the
// generated client withholds the provider's free-text message, a disclosed
// trade-off against a hand-written read.
func statusFetch(ctx *clicore.WorkspaceContext, reqCtx context.Context) (*deploymentsSummary, error) {
	control, err := controlClient(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := clicore.CallWithSession(reqCtx, ctx, func(callCtx context.Context) (*controlapiclient.DeploymentsSummaryResponse, error) {
		return control.GetV1WorkspacesDeployments(callCtx, controlapiclient.GetV1WorkspacesDeploymentsInput{
			Path: controlapiclient.GetV1WorkspacesDeploymentsPath{Workspace: ctx.WorkspaceID},
		})
	})
	if err != nil {
		return nil, clicore.ServiceCallError(reqCtx, err, clicore.ServiceCallOptions{
			Target:        ctx.WorkspaceURL("/deployments"),
			Prefix:        "status: ",
			CancelMessage: "status query canceled by user",
		})
	}
	return convertDeploymentsSummary(resp), nil
}

// renderStatusTable renders the compact one-screen human table
// (PROJECT  ENV  REVISION  HEALTH  LAST RUN  TRIGGER  URL  REASON). HEALTH is the
// state of the serving (last successful) release; LAST RUN, TRIGGER, and REASON
// describe the project's most recent run, which may have failed since.
// REASON is last because it is free text of any length: it never widens the
// other columns. An empty state renders as "unknown" so a blank health cell
// never reads as "no value". Zero deployments prints a clear "no deployments"
// line (the command still exits 0). A non-nil health (--health) inserts
// ERRORS (10m), TOP ERROR, and ON MAIN before REASON.
func renderStatusTable(resp *deploymentsSummary, health *statusHealthReport) string {
	if resp == nil || len(resp.Deployments) == 0 {
		return "No deployments found for workspace " + workspaceLabel(resp) + "."
	}
	header := []string{"PROJECT", "ENV", "REVISION", "HEALTH", "LAST RUN", "TRIGGER", "URL"}
	if health != nil {
		header = append(header, statusHealthHeader...)
	}
	header = append(header, "REASON")
	rows := make([][]string, 0, len(resp.Deployments))
	for _, d := range resp.Deployments {
		row := []string{
			statusCell(d.Project),
			statusCell(d.Environment),
			statusCell(d.Revision),
			statusHealth(d.State),
			statusCell(d.LastRunStatus),
			statusTrigger(d, resp.TriggersUnavailable),
			statusCell(d.URL),
		}
		if health != nil {
			row = append(row, statusHealthCells(health, d)...)
		}
		rows = append(rows, append(row, statusCell(statusOneLine(d.LastRunReason))))
	}
	return renderStatusRows(header, rows)
}

// statusTrigger renders what opened the project's last run: the channel move
// as "<kind> <namespace>/<channel>#<generation>" when the server named one, and
// "cli" for a run no move opened — the same derivation the deploy history and
// status reads use for their provenance marker. With no last run on record it
// renders "-". When the server could not read the channel-move ledger
// (triggersUnavailable), an absent trigger is not evidence of a human deploy,
// so it renders "unknown" instead of "cli". An unknown kind still renders, by
// name.
func statusTrigger(d deploymentSummaryEntry, triggersUnavailable bool) string {
	if d.Trigger == nil || strings.TrimSpace(d.Trigger.Kind) == "" {
		if strings.TrimSpace(d.LastRunStatus) == "" {
			return "-"
		}
		if triggersUnavailable {
			return "unknown"
		}
		return "cli"
	}
	channel := d.Trigger.Channel
	if d.Trigger.Namespace != "" && channel != "" {
		channel = d.Trigger.Namespace + "/" + channel
	}
	if channel == "" {
		return d.Trigger.Kind
	}
	if d.Trigger.Generation > 0 {
		channel += fmt.Sprintf("#%d", d.Trigger.Generation)
	}
	return d.Trigger.Kind + " " + channel
}

// statusOneLine collapses a multi-line reason (a wrapped provider error) to one
// line so it cannot break the table. Structured output keeps the exact text.
func statusOneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// renderProvenanceTable renders the `--provenance` per-workload table: the deploy
// provenance chain PROJECT | COMMIT | TREE | RELEASE | REVISION | CONFIG VERSION
// that answers "what git sha → release → serving revision → config version is
// running" for every workload in one screen. The commit is the short sha;
// TREE is the short git tree the commit's content was built from, see
// provenanceTree. Every absent value (a non-git deploy's commit, a move that
// carried no tree, an unpinned revision's config version) renders as "-" via
// statusCell. An empty state reuses the default table's zero-deployments line. A
// non-nil health (--health) appends ERRORS (10m), TOP ERROR, and ON MAIN.
func renderProvenanceTable(resp *deploymentsSummary, health *statusHealthReport) string {
	if resp == nil || len(resp.Deployments) == 0 {
		return "No deployments found for workspace " + workspaceLabel(resp) + "."
	}
	header := []string{"PROJECT", "COMMIT", "TREE", "RELEASE", "REVISION", "CONFIG VERSION"}
	if health != nil {
		header = append(header, statusHealthHeader...)
	}
	rows := make([][]string, 0, len(resp.Deployments))
	for _, d := range resp.Deployments {
		row := []string{
			statusCell(d.Project),
			statusCell(shortCommitSHA(d.CommitSHA)),
			statusCell(provenanceTree(d)),
			statusCell(d.LastReleaseID),
			statusCell(d.Revision),
			statusCell(d.ConfigVersion),
		}
		if health != nil {
			row = append(row, statusHealthCells(health, d)...)
		}
		rows = append(rows, row)
	}
	return renderStatusRows(header, rows)
}

// provenanceTree is the short git tree beside a row's COMMIT: the tree the
// channel move that opened the project's last run recorded. It renders only
// when that run's commit IS the row's commit, so the tree always describes the
// commit printed next to it. A last run on another commit (it failed, and an
// older revision still serves), a run no move opened, and a move that carried
// no tree all render "-".
func provenanceTree(d deploymentSummaryEntry) string {
	if d.Trigger == nil {
		return ""
	}
	commit := strings.TrimSpace(d.CommitSHA)
	if commit == "" || strings.TrimSpace(d.Trigger.SourceRevision) != commit {
		return ""
	}
	return shortCommitSHA(d.Trigger.SourceTree)
}

// renderStatusRows left-aligns a header + rows into the compact two-space table
// both the default and --provenance status views share, sizing each column to
// its widest cell.
func renderStatusRows(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	b.WriteString(formatStatusRow(header, widths))
	for _, row := range rows {
		b.WriteString("\n")
		b.WriteString(formatStatusRow(row, widths))
	}
	return b.String()
}

// shortCommitSHA truncates a commit or tree sha to its 7-char short form (the
// git default) for the provenance table, leaving a shorter or empty value
// untouched so statusCell can render "-" for a non-git deploy.
func shortCommitSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// formatStatusRow left-pads every column to its width, joined by two spaces, and
// trims the trailing padding of the last column.
func formatStatusRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		if i == len(cells)-1 {
			parts[i] = cell
			continue
		}
		parts[i] = fmt.Sprintf("%-*s", widths[i], cell)
	}
	return strings.TrimRight(strings.Join(parts, "  "), " ")
}

// statusCell renders a value or "-" for an empty one, so a blank column reads as
// "not reported" rather than as confusing whitespace.
func statusCell(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// statusHealth renders the deployment state, mapping an empty state to "unknown"
// (not blank) per the one-screen contract.
func statusHealth(state string) string {
	if strings.TrimSpace(state) == "" {
		return "unknown"
	}
	return state
}

// workspaceLabel returns the workspace id for the empty-result message, or a
// placeholder when the link carried none.
func workspaceLabel(resp *deploymentsSummary) string {
	if resp == nil || strings.TrimSpace(resp.WorkspaceID) == "" {
		return "(unknown)"
	}
	return resp.WorkspaceID
}
