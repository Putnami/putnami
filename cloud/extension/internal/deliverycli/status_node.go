package deliverycli

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The ci and cache statuses. Each entry has one builder that both its
// own `status` command and `putnami cloud status` call, and a pure fold that
// turns the served answers into the node.

// ciUsageStatusWindow is the window the ci status reads its usage over: the
// week the usage endpoint also defaults to.
const ciUsageStatusWindow = "168h"

// CIStatusNode reads the workspace CI status, then its usage over 7 days, and
// folds both. The two reads run one after the other: they share the workspace
// session, which a 401 refreshes in place.
func CIStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("ci", "ci", err, "putnami cloud ci status")
	}
	node, _ := ciStatusNode(ctx, ioctx)
	return node
}

// ciStatusNode is CIStatusNode once the workspace is resolved. It also
// returns the status read error, so the command keeps an auth failure as its
// exit 3 error.
func ciStatusNode(ctx *clicore.WorkspaceContext, ioctx clicore.IO) (clicore.StatusNode, error) {
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	status, err := readCIStatus(reqCtx, ctx)
	if err != nil {
		return clicore.UnknownStatus("ci", "ci", err, "putnami cloud ci status"), err
	}
	usage, usageErr := readCIUsage(reqCtx, ctx, ciUsageStatusWindow)
	return CIStatusNodeFrom(*status, usage, usageErr), nil
}

func readCIStatus(reqCtx context.Context, ctx *clicore.WorkspaceContext) (*CIStatusResponse, error) {
	return ciCall[CIStatusResponse](reqCtx, ctx, "status", ctx.WorkspaceURL("/ci/status"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIStatusResponse, error) {
			return api.GetV1WorkspacesCiStatus(callCtx, deliveryapiclient.GetV1WorkspacesCiStatusInput{
				Path: deliveryapiclient.GetV1WorkspacesCiStatusPath{Workspace: ctx.WorkspaceID},
			})
		})
}

// readCIUsage reads the usage rollup. An empty window takes the server's.
func readCIUsage(reqCtx context.Context, ctx *clicore.WorkspaceContext, window string) (*CIUsageResponse, error) {
	target := ctx.WorkspaceURL("/ci/usage")
	var query deliveryapiclient.GetV1WorkspacesCiUsageQuery
	if window != "" {
		target += "?" + url.Values{"window": {window}}.Encode()
		query.Window = &window
	}
	return ciCall[CIUsageResponse](reqCtx, ctx, "usage", target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIUsageResponse, error) {
			return api.GetV1WorkspacesCiUsage(callCtx, deliveryapiclient.GetV1WorkspacesCiUsageInput{
				Path:  deliveryapiclient.GetV1WorkspacesCiUsagePath{Workspace: ctx.WorkspaceID},
				Query: query,
			})
		})
}

// CIStatusNodeFrom folds a CI status answer and its usage rollup into one
// node. Each readiness dimension is a check; what ran last, what runs now and
// how it is dispatched are ok checks that state facts. usage is nil when it
// could not be read; usageErr then says why, as an unknown check.
//
// The node takes the worst check, with one stronger rule: a stuck run makes
// it failing, because a run that holds a lease and never ends blocks the
// queue whatever the dimensions say.
func CIStatusNodeFrom(status CIStatusResponse, usage *CIUsageResponse, usageErr error) clicore.StatusNode {
	node := clicore.StatusNode{ID: "ci", Title: "ci"}
	node.Metrics = ciStatusMetrics(status, usage)

	states := make([]clicore.StatusState, 0, len(status.Dimensions)+1)
	var notes []string
	for _, dimension := range status.Dimensions {
		child := CIDimensionNode(dimension)
		node.Children = append(node.Children, child)
		states = append(states, child.State)
		if child.State != clicore.StatusOK {
			notes = append(notes, dimension.Name+" "+strings.ReplaceAll(dimension.State, "-", " "))
		}
	}
	node.Children = append(node.Children, ciFactNodes(status)...)
	if usage == nil && usageErr != nil {
		child := clicore.UnknownStatus("ci.usage", "usage", usageErr, "putnami cloud ci usage")
		child.Detail = "runs and spend not read: " + child.Detail
		node.Children = append(node.Children, child)
		states = append(states, child.State)
	}

	node.State = clicore.WorstStatus(states...)
	if len(status.Dimensions) == 0 && !status.Healthy {
		// An answer without dimensions carries only the verdict.
		node.State = clicore.WorstStatus(node.State, clicore.StatusDegraded)
	}
	switch {
	case len(notes) > 0:
		node.Detail = strings.Join(notes, ", ")
	case len(status.Dimensions) > 0:
		node.Detail = fmt.Sprintf("%d of %d checks healthy", len(status.Dimensions), len(status.Dimensions))
	case status.Healthy:
		node.Detail = "healthy"
	default:
		node.Detail = "not healthy"
	}
	if status.StuckRuns > 0 {
		node.State = clicore.StatusFailing
		node.Detail += fmt.Sprintf("; %d stuck runs", status.StuckRuns)
		node.Fix = "putnami cloud ci repair --reason <why>"
	}
	if usage != nil {
		node.Detail += fmt.Sprintf("; %d runs, ~€%.2f in %s", usage.ScannedRuns, usage.Spend.TotalEUR, ciWindowLabel(usage.WindowHours))
	}
	return node
}

// ciStatusMetrics measures the queue now, the cache reuse of the last run,
// and the runs and spend of the usage window.
func ciStatusMetrics(status CIStatusResponse, usage *CIUsageResponse) []clicore.StatusMetric {
	bound := ""
	if status.QueueLowerBound {
		bound = " (lower bound)"
	}
	metrics := []clicore.StatusMetric{
		clicore.CountMetric("queued_runs", "queued runs"+bound, status.QueueDepth, clicore.MetricHealth),
		clicore.CountMetric("stuck_runs", "stuck runs"+bound, status.StuckRuns, clicore.MetricHealth),
	}
	if status.OldestQueuedSeconds > 0 {
		metrics = append(metrics, clicore.StatusMetric{
			ID: "oldest_queued", Title: "oldest queued", Value: float64(status.OldestQueuedSeconds),
			Unit: clicore.UnitSeconds, Kind: clicore.MetricHealth,
		})
	}
	if status.CheckOutboxPending > 0 {
		metrics = append(metrics, clicore.CountMetric("undelivered_checks", "undelivered checks", status.CheckOutboxPending, clicore.MetricHealth))
	}
	if cache := status.LastRunCache; cache != nil && cache.Tasks > 0 {
		metrics = append(metrics, clicore.CountMetric("reused_tasks", "reused tasks", cache.Hits, clicore.MetricHealth).
			Of(float64(cache.Tasks)).Over("last run"))
	}
	if usage != nil {
		window := ciWindowLabel(usage.WindowHours)
		runs := clicore.CountMetric("runs", "runs", usage.ScannedRuns, clicore.MetricUsage).Over(window)
		if usage.ScanLowerBound {
			runs.Title = "runs (lower bound)"
		}
		// The platform prices each run from fixed rates; it reads no bill, so
		// the spend is always an estimate.
		spend := clicore.StatusMetric{
			ID: "spend", Title: "spend", Value: usage.Spend.TotalEUR, Unit: clicore.UnitEUR,
			Window: window, Kind: clicore.MetricUsage, Estimated: true,
		}
		metrics = append(metrics, runs, spend)
	}
	return metrics
}

// ciFactNodes states what the old status text printed as lines: how runs are
// dispatched and what the last run executed, the run executing now, and the
// cache reuse of the last run. They are facts, not verdicts, so they are ok.
// An unrecorded provenance value reads "unknown": a blank would read as
// "nothing to report".
func ciFactNodes(status CIStatusResponse) []clicore.StatusNode {
	var facts []clicore.StatusNode
	if status.DispatchAxis != "" {
		facts = append(facts, clicore.StatusNode{
			ID: "ci.dispatch", Title: "dispatch", State: clicore.StatusOK,
			Detail: fmt.Sprintf("%s; last run channel %s, image %s, CLI %s, source %s",
				status.DispatchAxis, dashUnknown(status.LastRunRunnerChannel), dashUnknown(shortStatusDigest(status.LastRunDigest)),
				dashUnknown(status.LastRunCLIVersion), dashUnknown(shortStatusID(status.LastRunSourceRevision))),
			Facts: nonEmptyFacts(map[string]string{
				"axis":            status.DispatchAxis,
				"runner_channel":  status.LastRunRunnerChannel,
				"runner_selector": status.LastRunRunnerSelector,
				"digest":          status.LastRunDigest,
				"cli_version":     status.LastRunCLIVersion,
				"source_revision": status.LastRunSourceRevision,
			}),
		})
	}
	if status.LiveRun != "" {
		facts = append(facts, clicore.StatusNode{
			ID: "ci.live_run", Title: "live run", State: clicore.StatusOK,
			Detail: fmt.Sprintf("%s, %s heartbeat #%d, %s ago", status.LiveRun, dashUnknown(status.LiveRunPhase),
				status.LiveRunSequence, ciFormatDurationSeconds(status.LiveRunHeartbeatSeconds)),
			Facts: nonEmptyFacts(map[string]string{"run": status.LiveRun, "phase": status.LiveRunPhase}),
		})
	}
	if cache := status.LastRunCache; cache != nil && cache.Tasks > 0 {
		// Served and executed time stay two figures: a task served from cache
		// reports its replay cost, not the execution it replaced.
		facts = append(facts, clicore.StatusNode{
			ID: "ci.last_run", Title: "last run", State: clicore.StatusOK,
			Detail: fmt.Sprintf("%s, %d of %d tasks reused, %d executed, %s served from cache, %s executed",
				dashUnknown(shortStatusID(status.LastRun)), cache.Hits, cache.Tasks, cache.Misses,
				statusDuration(cache.ServedMS), statusDuration(cache.ExecutedMS)),
			Facts: nonEmptyFacts(map[string]string{"run": status.LastRun}),
		})
	}
	return facts
}

// shortStatusID is the 12-character prefix `ci list` prints for a run id or
// a commit; the full value stays in the facts.
func shortStatusID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// shortStatusDigest is a digest with 12 hex characters: sha256:52a4ff1eddc4.
func shortStatusDigest(digest string) string {
	algorithm, hex, found := strings.Cut(strings.TrimSpace(digest), ":")
	if !found {
		return shortStatusID(digest)
	}
	return algorithm + ":" + shortStatusID(hex)
}

// statusDuration renders milliseconds under a second as they are, and longer
// spans to the second: 90ms, 17m24s.
func statusDuration(ms int64) string {
	if ms < 1000 {
		return ciFormatDurationMS(ms)
	}
	return ciFormatDurationSeconds(int((ms + 500) / 1000))
}

// nonEmptyFacts drops the facts that were not recorded, so a script reads a
// missing key rather than an empty value.
func nonEmptyFacts(facts map[string]string) map[string]string {
	for key, value := range facts {
		if strings.TrimSpace(value) == "" {
			delete(facts, key)
		}
	}
	if len(facts) == 0 {
		return nil
	}
	return facts
}

// ciWindowLabel names a usage window in hours the way status metrics do.
func ciWindowLabel(hours int) string {
	switch {
	case hours > 0 && hours%24 == 0 && hours != 24:
		return fmt.Sprintf("%d days", hours/24)
	case hours == 1:
		return "1 hour"
	default:
		return fmt.Sprintf("%d hours", hours)
	}
}

// CIDimensionNode maps one CI readiness dimension onto the shared scale.
//
//	healthy                                              ok
//	degraded, paused, draining, enforcement-disabled     degraded
//	*-unavailable, provenance-unknown                    unknown
//	stale-runner, check-backlog                          failing
//	any other state                                      degraded
func CIDimensionNode(dimension CIStatusDimension) clicore.StatusNode {
	explanation := statusSentence(dimension.Explanation)
	recovery := statusSentence(dimension.Recovery)
	node := clicore.StatusNode{
		ID:     "ci." + dimension.Name,
		Title:  dimension.Name,
		Detail: firstStatusSentence(explanation),
		Fix:    recoveryCommand(recovery),
	}
	// The served sentences stay whole in the facts when the line shortens
	// them, so a script or a reader of the JSON loses nothing.
	node.Facts = nonEmptyFacts(map[string]string{
		"explanation": map[bool]string{true: explanation}[explanation != node.Detail],
		"recovery":    map[bool]string{true: recovery}[recovery != node.Fix],
	})
	switch state := strings.TrimSpace(dimension.State); {
	case state == "healthy":
		node.State = clicore.StatusOK
	case strings.HasSuffix(state, "-unavailable"), state == "provenance-unknown":
		node.State = clicore.StatusUnknown
	case state == "stale-runner", state == "check-backlog":
		node.State = clicore.StatusFailing
	default:
		node.State = clicore.StatusDegraded
	}
	if node.Detail == "" {
		node.Detail = dimension.State
	}
	return node
}

// firstStatusSentence is the first sentence of a served explanation, so a
// check stays one line.
func firstStatusSentence(text string) string {
	if first, _, found := strings.Cut(text, ". "); found {
		return first
	}
	return text
}

// recoveryCommand is the command a served recovery names: the recovery
// itself when it is a command, else the first command it quotes in
// backticks. A recovery that names no command leaves no fix.
func recoveryCommand(recovery string) string {
	if strings.HasPrefix(recovery, "putnami ") {
		return recovery
	}
	for rest := recovery; ; {
		_, after, found := strings.Cut(rest, "`")
		if !found {
			return ""
		}
		quoted, tail, closed := strings.Cut(after, "`")
		if !closed {
			return ""
		}
		if strings.HasPrefix(quoted, "putnami ") {
			return quoted
		}
		rest = tail
	}
}

// statusSentence trims a served sentence to a status detail: no surrounding
// space and no trailing period.
func statusSentence(text string) string {
	return strings.TrimSuffix(strings.TrimSpace(text), ".")
}

// CacheStatusNode is the build cache status. Two checks read this machine:
// the cache config in .putnami/cache.json and the identity its token source
// signs in with. When the cache is enabled, the reuse of the last CI run is
// read from the ci status; that read is best effort, and an unknown check
// says when it failed. No endpoint serves the cache size yet.
func CacheStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	cfg, cfgErr := ReadCacheConfig(workspaceRoot)
	var lastRun *CIStatusResponse
	var lastRunErr error
	if cfgErr == nil && cfg != nil && cfg.Enabled {
		lastRun, lastRunErr = readLastRunCache(params, workspaceRoot, env, ioctx)
	}
	return CacheStatusNodeFrom(cfg, cfgErr, localIdentity(env), lastRun, lastRunErr)
}

func readLastRunCache(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*CIStatusResponse, error) {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	return readCIStatus(reqCtx, ctx)
}

// CacheStatusNodeFrom folds the cache config, the signed-in identity and the
// ci status answer into the cache node. The node takes the worst check.
func CacheStatusNodeFrom(cfg *CacheConfig, cfgErr error, identity map[string]any, lastRun *CIStatusResponse, lastRunErr error) clicore.StatusNode {
	node := clicore.StatusNode{ID: "cache", Title: "cache"}
	config := clicore.StatusNode{ID: "cache.config", Title: "config"}
	switch {
	case cfgErr != nil:
		config = clicore.UnknownStatus(config.ID, config.Title, cfgErr, "putnami cloud setup")
		node.Detail = "config not read"
	case cfg == nil:
		config.State = clicore.StatusDegraded
		config.Detail = "not configured: no " + CacheFileRelative
		config.Fix = "putnami cloud setup"
		node.Detail = "not configured"
	case !cfg.Enabled:
		config.State = clicore.StatusOK
		config.Detail = fmt.Sprintf("disabled in %s, %s (mode %s)", CacheFileRelative, cfg.URL, cfg.Mode)
		node.Detail = "disabled"
	default:
		config.State = clicore.StatusOK
		config.Detail = fmt.Sprintf("enabled in %s, %s (mode %s)", CacheFileRelative, cfg.URL, cfg.Mode)
		node.Detail = "enabled"
	}
	node.Children = append(node.Children, config, cacheIdentityNode(cfg, identity))

	if lastRunErr != nil {
		child := clicore.UnknownStatus("cache.last_run", "last run", lastRunErr, "putnami cloud ci status")
		child.Detail = "last run reuse not read: " + child.Detail
		node.Children = append(node.Children, child)
	} else if lastRun != nil {
		if cache := lastRun.LastRunCache; cache != nil && cache.Tasks > 0 {
			node.Metrics = []clicore.StatusMetric{
				clicore.CountMetric("reused_tasks", "reused tasks", cache.Hits, clicore.MetricHealth).
					Of(float64(cache.Tasks)).Over("last run"),
				{ID: "served_time", Title: "served from cache", Value: float64(cache.ServedMS) / 1000,
					Unit: clicore.UnitSeconds, Window: "last run", Kind: clicore.MetricHealth},
				{ID: "executed_time", Title: "executed", Value: float64(cache.ExecutedMS) / 1000,
					Unit: clicore.UnitSeconds, Window: "last run", Kind: clicore.MetricHealth},
			}
			node.Detail += fmt.Sprintf("; last run reused %d of %d tasks", cache.Hits, cache.Tasks)
		}
	}

	node.State = clicore.WorstStatus(node.ChildStates()...)
	return node
}

// cacheIdentityNode says who the cache token source signs in as. An enabled
// cache without a token source, or without a session, cannot read the cache.
func cacheIdentityNode(cfg *CacheConfig, identity map[string]any) clicore.StatusNode {
	node := clicore.StatusNode{ID: "cache.identity", Title: "identity", State: clicore.StatusOK}
	who := identityText(identity)
	source := cfg.tokenSourceText()
	switch {
	case who == "":
		node.State = clicore.StatusDegraded
		node.Detail = "not signed in"
		node.Fix = "putnami cloud login"
	case cfg != nil && cfg.Enabled && source == "":
		node.State = clicore.StatusDegraded
		node.Detail = who + ", no token source in " + CacheFileRelative
		node.Fix = "putnami cloud setup"
	case source != "":
		node.Detail = who + ", token from `" + source + "`"
	default:
		node.Detail = who
	}
	return node
}
