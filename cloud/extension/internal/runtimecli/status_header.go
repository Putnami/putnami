package runtimecli

// status_header.go renders the continuous-delivery (CD) header of
// `putnami cloud env status`: one line for the --env environment naming the
// channels it follows and what the newest move of each did, read from
// GET /v1/workspaces/{workspace}/environments/{environment}/channel-follow.
//
//	CD prod ← canary: gen 42 converged 2026-09-18T10:02Z (rs_ab12cd34…)
//
// WHICH ENVIRONMENT. The one --env names; the manifest defaults it to prod. A
// caller that passes no environment gets no header (channel_follow is []).
//
// THE HEADER NEVER FAILS THE COMMAND. A control plane that predates the route
// (404), a caller without access (403), an unreadable ledger (503), a timeout,
// or a body that does not decode all degrade to
// `CD <env>: unavailable (<short reason>)`, and the deployment table prints as
// before.
//
// THE HEADER NEVER RE-MINTS THE SESSION. The header request runs concurrently
// with the summary (and the --health logs read), and the one-shot 401 re-mint
// writes the context's bearer and may rotate the stored refresh token;
// concurrent re-mints would race on both. The header request therefore uses its
// own copy of the context with re-minting disabled, so a 401 degrades like any
// other refusal while the summary request keeps its re-mint.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// statusHeaderTimeout bounds one header request or one --health logs
	// page, so a slow ledger or logs read cannot hold the deployment table back.
	statusHeaderTimeout = 10 * time.Second
	// statusReasonWidth bounds a free-text reason on the one-line header.
	statusReasonWidth = 120
)

// statusHeaderContext copies the workspace context for header requests, with
// the 401 re-mint disabled. See the file comment.
func statusHeaderContext(ctx *clicore.WorkspaceContext) *clicore.WorkspaceContext {
	header := *ctx
	header.RefreshAuth = nil
	return &header
}

// fetchChannelFollow reads one environment's header through control-api's
// generated client. It never returns an error: every
// failure is recorded as the header's Unavailable reason via
// clicore.ServiceFailureReason, which is why this read never re-mints either —
// see the file comment — a provider-declared error carries only its HTTP
// status line here, not its free-text message: the generated client withholds
// it, a disclosed trade-off against the hand-written
// read this replaces.
func fetchChannelFollow(ctx *clicore.WorkspaceContext, reqCtx context.Context, environment string) statusChannelFollow {
	out := statusChannelFollow{Environment: environment}
	readCtx, cancel := context.WithTimeout(reqCtx, statusHeaderTimeout)
	defer cancel()
	control, err := controlClient(ctx)
	if err != nil {
		out.Unavailable = clicore.ServiceFailureReason(err)
		return out
	}
	resp, err := control.GetV1WorkspacesEnvironmentsChannelFollow(ctx.CallContext(readCtx), controlapiclient.GetV1WorkspacesEnvironmentsChannelFollowInput{
		Path: controlapiclient.GetV1WorkspacesEnvironmentsChannelFollowPath{Workspace: ctx.WorkspaceID, Environment: environment},
	})
	if err != nil {
		out.Unavailable = clicore.ServiceFailureReason(err)
		return out
	}
	out.DefinitionRevision = clicore.Deref(resp.DefinitionRevision)
	channels := clicore.Deref(resp.Channels)
	out.Channels = make([]followedChannel, 0, len(channels))
	for _, channel := range channels {
		out.Channels = append(out.Channels, convertFollowedChannel(channel))
	}
	return out
}

// statusLogsRead issues one --health logs page read through
// observability-api's generated client; its failure must never fail the
// command. It answers the page, or the short reason the read is unavailable
// (clicore.ServiceFailureReason: the status line of a refusal, "invalid
// response", or "request failed: <cause>" for a transport failure or a
// timeout bounded by statusHeaderTimeout). Like fetchChannelFollow it calls
// with ctx's bearer and never re-mints: the caller passes a context copy with
// re-minting disabled (see statusHeaderContext). A refusal names its status
// line only: the generated client withholds the provider's free-text message.
func statusLogsRead(ctx *clicore.WorkspaceContext, reqCtx context.Context, query observabilityapiclient.GetV1WorkspacesLogsQuery) (*observabilityapiclient.LogsPage, string) {
	readCtx, cancel := context.WithTimeout(reqCtx, statusHeaderTimeout)
	defer cancel()
	api, err := clicore.NewServiceClient[observabilityapiclient.ObservabilityClient](observabilityapiclient.RegisterObservabilityClient, ctx.ServiceBinding())
	if err != nil {
		return nil, statusShortReason(clicore.ServiceFailureReason(err))
	}
	page, err := api.GetV1WorkspacesLogs(ctx.CallContext(readCtx), observabilityapiclient.GetV1WorkspacesLogsInput{
		Path:  observabilityapiclient.GetV1WorkspacesLogsPath{Workspace: ctx.WorkspaceID},
		Query: query,
	})
	if err != nil {
		return nil, statusShortReason(clicore.ServiceFailureReason(err))
	}
	if page == nil {
		page = &observabilityapiclient.LogsPage{}
	}
	return page, ""
}

// renderStatusHeaders renders every header line followed by one blank line, or
// nothing when there is no header to print.
func renderStatusHeaders(headers []statusChannelFollow) string {
	if len(headers) == 0 {
		return ""
	}
	var b strings.Builder
	for _, header := range headers {
		b.WriteString(renderStatusHeader(header))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// renderStatusHeader renders ONE environment's line: every followed channel,
// joined by "; ".
func renderStatusHeader(header statusChannelFollow) string {
	prefix := "CD " + statusCell(header.Environment)
	if header.Unavailable != "" {
		return prefix + ": unavailable (" + header.Unavailable + ")"
	}
	if len(header.Channels) == 0 {
		return prefix + ": follows no channel"
	}
	parts := make([]string, 0, len(header.Channels))
	for _, followed := range header.Channels {
		parts = append(parts, "← "+followed.Channel+": "+statusMove(followed.Receipt))
	}
	return prefix + " " + strings.Join(parts, "; ")
}

// statusMove renders the newest move of one channel:
// "gen <n> <outcome> <time> (<release set>), release <id>", then ": <reason>" when the move
// settled on anything other than converged or opened. A move still being
// delivered reads "in flight" instead of its provisional disposition.
func statusMove(receipt *channelMoveReceipt) string {
	if receipt == nil {
		return "no move received yet"
	}
	outcome := receipt.Disposition
	if !receipt.Settled {
		outcome = "in flight"
	}
	parts := []string{fmt.Sprintf("gen %d", receipt.Generation), statusCell(outcome)}
	at := receipt.MovedAt
	if at.IsZero() {
		at = receipt.ReceivedAt
	}
	if !at.IsZero() {
		parts = append(parts, at.UTC().Format("2006-01-02T15:04Z"))
	}
	if receipt.ReleaseSetID != "" {
		parts = append(parts, "("+shortReleaseSetID(receipt.ReleaseSetID)+")")
	}
	line := strings.Join(parts, " ")
	if releases := moveReleases(receipt); releases != "" {
		line += ", " + releases
	}
	if receipt.Settled && receipt.Reason != "" && receipt.Disposition != "converged" && receipt.Disposition != "opened" {
		line += ": " + statusShortReason(receipt.Reason)
	}
	return line
}

// moveReleases names the releases a move opened, in full, so the reader can
// pass one to `putnami cloud deploy status`: "release cm_…" or
// "releases cm_…, cm_…". A move that opened nothing names none.
func moveReleases(receipt *channelMoveReceipt) string {
	if receipt == nil || len(receipt.ReleaseIDs) == 0 {
		return ""
	}
	if len(receipt.ReleaseIDs) == 1 {
		return "release " + receipt.ReleaseIDs[0]
	}
	return "releases " + strings.Join(receipt.ReleaseIDs, ", ")
}

// shortReleaseSetID keeps the "rs_" prefix and eight hex digits of a release
// set id, the way a short commit sha is read.
func shortReleaseSetID(id string) string {
	const keep = len("rs_") + 8
	if len(id) <= keep {
		return id
	}
	return id[:keep] + "…"
}

// statusShortReason collapses a reason to one line of at most statusReasonWidth
// characters.
func statusShortReason(reason string) string {
	return statusTruncate(statusOneLine(reason), statusReasonWidth)
}

// statusTruncate cuts a line to at most width characters, ending a cut line
// with an ellipsis.
func statusTruncate(line string, width int) string {
	if utf8.RuneCountInString(line) <= width {
		return line
	}
	runes := []rune(line)
	return string(runes[:width-1]) + "…"
}
