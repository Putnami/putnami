package deliverycli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestCIDimensionNodeMapsEveryState(t *testing.T) {
	for state, want := range map[string]clicore.StatusState{
		"healthy":              clicore.StatusOK,
		"paused":               clicore.StatusDegraded,
		"enforcement-disabled": clicore.StatusDegraded,
		"runner-unavailable":   clicore.StatusUnknown,
		"provenance-unknown":   clicore.StatusUnknown,
		"stale-runner":         clicore.StatusFailing,
		"check-backlog":        clicore.StatusFailing,
		"something-new":        clicore.StatusDegraded,
	} {
		node := CIDimensionNode(CIStatusDimension{Name: "runner", State: state})
		if node.State != want || node.Detail != state || node.ID != "ci.runner" {
			t.Fatalf("dimension %s = %+v, want %s", state, node, want)
		}
	}
	node := CIDimensionNode(CIStatusDimension{Name: "queue", State: "paused", Explanation: " paused by an operator. ", Recovery: "putnami cloud ci resume."})
	if node.Detail != "paused by an operator" || node.Fix != "putnami cloud ci resume" || node.Facts != nil {
		t.Fatalf("node = %+v, want the explanation and the recovery without a trailing period", node)
	}
	// A long explanation keeps its first sentence on the line; a recovery
	// that is a sentence gives the command it quotes, or no fix.
	node = CIDimensionNode(CIStatusDimension{Name: "runner", State: "provenance-unknown",
		Explanation: "runner freshness NOT evaluated: no digest. This is unknown, not healthy.",
		Recovery:    "start one run (`putnami cloud ci start`) and re-read this status"})
	if node.Detail != "runner freshness NOT evaluated: no digest" || node.Fix != "putnami cloud ci start" ||
		node.Facts["explanation"] != "runner freshness NOT evaluated: no digest. This is unknown, not healthy" ||
		node.Facts["recovery"] != "start one run (`putnami cloud ci start`) and re-read this status" {
		t.Fatalf("node = %+v", node)
	}
	node = CIDimensionNode(CIStatusDimension{Name: "config", State: "degraded", Recovery: "configure delivery.ci; see the CI guide"})
	if node.Fix != "" || node.Facts["recovery"] != "configure delivery.ci; see the CI guide" {
		t.Fatalf("node = %+v, want no fix and the recovery in the facts", node)
	}
}

func TestCIStatusNodeFromTakesTheWorstDimension(t *testing.T) {
	node := CIStatusNodeFrom(CIStatusResponse{Dimensions: []CIStatusDimension{
		{Name: "runner", State: "healthy"},
		{Name: "pipeline", State: "paused"},
		{Name: "check-publish-outbox", State: "provenance-unknown"},
	}}, nil, nil)
	if node.State != clicore.StatusUnknown || node.Detail != "pipeline paused, check-publish-outbox provenance unknown" || len(node.Children) != 3 {
		t.Fatalf("node = %+v, want unknown naming the two checks that are not ok", node)
	}
	healthy := CIStatusNodeFrom(CIStatusResponse{Healthy: true, QueueDepth: 3, Dimensions: []CIStatusDimension{{Name: "runner", State: "healthy"}}}, nil, nil)
	if healthy.State != clicore.StatusOK || healthy.Detail != "1 of 1 checks healthy" {
		t.Fatalf("healthy = %+v", healthy)
	}
	if queued := metricByID(healthy, "queued_runs"); queued == nil || queued.Value != 3 || queued.Kind != clicore.MetricHealth {
		t.Fatalf("queued runs metric = %+v", queued)
	}
	unhealthy := CIStatusNodeFrom(CIStatusResponse{}, nil, nil)
	if unhealthy.State != clicore.StatusDegraded || unhealthy.Detail != "not healthy" {
		t.Fatalf("unhealthy without dimensions = %+v, want degraded", unhealthy)
	}
	stuck := CIStatusNodeFrom(CIStatusResponse{Healthy: true, StuckRuns: 2, QueueLowerBound: true}, nil, nil)
	if stuck.State != clicore.StatusFailing || !strings.Contains(stuck.Detail, "2 stuck runs") || stuck.Fix != "putnami cloud ci repair --reason <why>" {
		t.Fatalf("stuck = %+v, want failing with the repair command", stuck)
	}
	if metric := metricByID(stuck, "stuck_runs"); metric == nil || metric.Title != "stuck runs (lower bound)" {
		t.Fatalf("stuck runs metric = %+v, want the lower bound named", metric)
	}
}

// TestCIStatusNodeFromKeepsEveryFact pins that the facts the old text printed
// as lines (provenance, liveness, cache economics, the check outbox and the
// oldest queued run) survive as checks or metrics, and that an unrecorded
// provenance value reads unknown rather than blank.
func TestCIStatusNodeFromKeepsEveryFact(t *testing.T) {
	node := CIStatusNodeFrom(CIStatusResponse{
		Healthy: true, QueueDepth: 1, OldestQueuedSeconds: 75,
		DispatchAxis: "service", LastRunDigest: "sha256:abc", LastRunCLIVersion: "0.1.0-2c62ff1a",
		LastRunRunnerChannel: "canary", LastRunRunnerSelector: "oci.example/runner:canary", LastRunSourceRevision: strings.Repeat("a", 40),
		LiveRun: "run-42", LiveRunPhase: "gate", LiveRunSequence: 7, LiveRunHeartbeatSeconds: 8,
		LastRun: "run-41", LastRunCache: &CIRunCache{Tasks: 10, Hits: 9, Misses: 1, HitPercent: 90, ServedMS: 90, ExecutedMS: 120000},
		CheckOutboxPending: 4, CheckOutboxOldestPendingSeconds: 900,
	}, nil, nil)
	want := map[string]string{
		"ci.dispatch": "service; last run channel canary, image sha256:abc, CLI 0.1.0-2c62ff1a, source " + strings.Repeat("a", 12),
		"ci.live_run": "run-42, gate heartbeat #7, 8s ago",
		"ci.last_run": "run-41, 9 of 10 tasks reused, 1 executed, 90ms served from cache, 2m00s executed",
	}
	for _, child := range node.Children {
		if detail, ok := want[child.ID]; ok {
			if child.State != clicore.StatusOK || child.Detail != detail {
				t.Fatalf("%s = %+v, want ok %q", child.ID, child, detail)
			}
			delete(want, child.ID)
		}
	}
	if len(want) > 0 {
		t.Fatalf("facts missing from the node: %v", want)
	}
	reused := metricByID(node, "reused_tasks")
	if reused == nil || reused.FormatValue() != "9 of 10 (90%)" || reused.Window != "last run" {
		t.Fatalf("reused tasks metric = %+v", reused)
	}
	if metric := metricByID(node, "oldest_queued"); metric == nil || metric.FormatValue() != "1m15s" {
		t.Fatalf("oldest queued metric = %+v", metric)
	}
	if metric := metricByID(node, "undelivered_checks"); metric == nil || metric.Value != 4 {
		t.Fatalf("undelivered checks metric = %+v", metric)
	}

	unrecorded := CIStatusNodeFrom(CIStatusResponse{Healthy: true, DispatchAxis: "service"}, nil, nil)
	if got := unrecorded.Children[0].Detail; got != "service; last run channel unknown, image unknown, CLI unknown, source unknown" {
		t.Fatalf("unrecorded provenance = %q, want every value stated unknown", got)
	}
	if idle := CIStatusNodeFrom(CIStatusResponse{Healthy: true}, nil, nil); len(idle.Children) != 0 || metricByID(idle, "reused_tasks") != nil {
		t.Fatalf("an idle workspace printed facts nobody measured: %+v", idle)
	}
}

func TestCIStatusNodeFromReadsUsage(t *testing.T) {
	status := CIStatusResponse{Healthy: true, Dimensions: []CIStatusDimension{{Name: "runner", State: "healthy"}}}
	node := CIStatusNodeFrom(status, &CIUsageResponse{
		WindowHours: 168, ScannedRuns: 41, Spend: CIUsageSpend{Currency: "EUR", Estimated: true, TotalEUR: 12.3},
	}, nil)
	if node.Detail != "1 of 1 checks healthy; 41 runs, ~€12.30 in 7 days" {
		t.Fatalf("detail = %q", node.Detail)
	}
	runs, spend := metricByID(node, "runs"), metricByID(node, "spend")
	if runs == nil || runs.Value != 41 || runs.Kind != clicore.MetricUsage || runs.Window != "7 days" {
		t.Fatalf("runs metric = %+v", runs)
	}
	if spend == nil || spend.FormatValue() != "~€12.30" || spend.Unit != clicore.UnitEUR || !spend.Estimated || spend.Kind != clicore.MetricUsage {
		t.Fatalf("spend metric = %+v", spend)
	}

	unread := CIStatusNodeFrom(status, nil, errors.New("cloud ci usage: forbidden"))
	last := unread.Children[len(unread.Children)-1]
	if unread.State != clicore.StatusUnknown || last.ID != "ci.usage" || last.State != clicore.StatusUnknown ||
		last.Detail != "runs and spend not read: cloud ci usage: forbidden" || last.Fix != "putnami cloud ci usage" {
		t.Fatalf("usage read failure = %+v, want an unknown usage check", unread)
	}
	if metricByID(unread, "runs") != nil {
		t.Fatal("an unread usage rollup produced a runs metric")
	}
}

func TestCIWindowLabel(t *testing.T) {
	for hours, want := range map[int]string{168: "7 days", 24: "24 hours", 48: "2 days", 1: "1 hour", 36: "36 hours"} {
		if got := ciWindowLabel(hours); got != want {
			t.Fatalf("ciWindowLabel(%d) = %q, want %q", hours, got, want)
		}
	}
}

func TestCIStatusNodeIsUnknownWithoutAWorkspace(t *testing.T) {
	node := CIStatusNode(map[string]any{}, t.TempDir(), map[string]string{"HOME": t.TempDir()}, clicore.IO{})
	if node.State != clicore.StatusUnknown || node.Fix != "putnami cloud ci status" {
		t.Fatalf("node = %+v, want unknown", node)
	}
}

// TestCIStatusPrintsTheStatusReport drives `cloud ci status` end to end: the
// status and usage reads, the shared report layout, and the exit rule.
func TestCIStatusPrintsTheStatusReport(t *testing.T) {
	fake := ciStatusFake(CIStatusResponse{
		Workspace: "ws-acme", Healthy: false, QueueDepth: 0,
		Dimensions: []CIStatusDimension{
			{Name: "pipeline", State: "healthy", Explanation: "pipeline is open."},
			{Name: "runner", State: "provenance-unknown", Explanation: "runner freshness NOT evaluated: a newer run is pending.", Recovery: "Wait for the newest run, then run `putnami cloud ci status`."},
		},
		LastRun: "run-41", LastRunCache: &CIRunCache{Tasks: 645, Hits: 311, Misses: 334, HitPercent: 48, ServedMS: 4000, ExecutedMS: 3075000},
	})
	lines, err := captureCI(t, fake, []string{"status"})
	if err != nil {
		t.Fatalf("an unknown ci status must exit 0 without --strict: %v", err)
	}
	want := strings.Join([]string{
		"ci  unknown  runner provenance unknown; 41 runs, ~€12.30 in 7 days",
		"",
		"  METRIC        VALUE             WINDOW",
		"  queued runs   0                 now",
		"  stuck runs    0                 now",
		"  reused tasks  311 of 645 (48%)  last run",
		"  runs          41                7 days",
		"  spend         ~€12.30           7 days",
		"",
		"  STATE    CHECK     DETAIL",
		"  ok       pipeline  pipeline is open",
		"  unknown  runner    runner freshness NOT evaluated: a newer run is pending",
		"                     fix: putnami cloud ci status",
		"  ok       last run  run-41, 311 of 645 tasks reused, 334 executed, 4s served from cache, 51m15s executed",
	}, "\n")
	if got := strings.Join(lines, "\n"); got != want {
		t.Fatalf("ci status = %q, want %q", got, want)
	}
	var usage *ciCapturedRequest
	for index := range fake.requests {
		if fake.requests[index].Path == "/v1/workspaces/ws-acme/ci/usage" {
			usage = &fake.requests[index]
		}
	}
	if usage == nil || usage.Query != "window=168h" {
		t.Fatalf("usage request = %+v, want the 7-day window", usage)
	}

	if _, err := captureCI(t, fake, []string{"status", "--strict"}); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("--strict on an unknown status = %v, want exit %d", err, clicore.ExitFailure)
	}
	if _, err := captureCI(t, fake, []string{"status", "extra"}); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("a name = %v, want a usage error", err)
	}
}

func TestCIStatusWritesOneEnvelopeWithTheNode(t *testing.T) {
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{{Name: "runner", State: "healthy"}}})
	lines, err := captureCI(t, fake, []string{"status", "--output", "json"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var envelope struct {
		Data clicore.StatusNode `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &envelope); err != nil {
		t.Fatalf("output is not one JSON envelope: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if envelope.Data.ID != "ci" || envelope.Data.State != clicore.StatusOK || metricByID(envelope.Data, "spend") == nil {
		t.Fatalf("envelope data = %+v, want the ci node", envelope.Data)
	}
}

func TestCIStatusUnreadUsageIsAnUnknownCheck(t *testing.T) {
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{}})
	fake.responses["GET /v1/workspaces/ws-acme/ci/usage"] = ciStubResponse{status: http.StatusForbidden, body: map[string]any{"error": "forbidden"}}
	lines, err := captureCI(t, fake, []string{"status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if text := strings.Join(lines, "\n"); !strings.Contains(text, "unknown  usage  runs and spend not read: cloud ci usage: forbidden") ||
		!strings.Contains(text, "fix: putnami cloud ci usage") {
		t.Fatalf("an unread usage rollup is not an unknown check: %q", text)
	}
}

func TestCIStatusKeepsTheAuthError(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/ci/status": {status: http.StatusUnauthorized, body: map[string]any{"error": "session expired"}},
	}}
	if _, err := captureCI(t, fake, []string{"status"}); clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("a refused session = %v, want exit %d", err, clicore.ExitAuth)
	}
	fake.responses["GET /v1/workspaces/ws-acme/ci/status"] = ciStubResponse{status: http.StatusBadGateway, body: map[string]any{"error": "upstream down"}}
	lines, err := captureCI(t, fake, []string{"status"})
	if err != nil || !strings.HasPrefix(strings.Join(lines, "\n"), "ci  unknown  cloud ci status: upstream down") {
		t.Fatalf("a failed read = %v\n%s, want an unknown status", err, strings.Join(lines, "\n"))
	}
}

func ciStatusFake(status CIStatusResponse) *ciFakeServer {
	return &ciFakeServer{responses: map[string]ciStubResponse{
		"GET /v1/workspaces/ws-acme/ci/status": {status: http.StatusOK, body: status},
		"GET /v1/workspaces/ws-acme/ci/usage": {status: http.StatusOK, body: CIUsageResponse{
			Workspace: "ws-acme", WindowHours: 168, ScannedRuns: 41,
			Spend: CIUsageSpend{Currency: "EUR", Estimated: true, TotalEUR: 12.3},
		}},
	}}
}

func metricByID(node clicore.StatusNode, id string) *clicore.StatusMetric {
	for index := range node.Metrics {
		if node.Metrics[index].ID == id {
			return &node.Metrics[index]
		}
	}
	return nil
}

func TestCacheStatusNodeFromReadsTheConfigAndIdentity(t *testing.T) {
	identity := map[string]any{"email": "dev@example.com"}
	unconfigured := CacheStatusNodeFrom(nil, nil, identity, nil, nil)
	if unconfigured.State != clicore.StatusDegraded || unconfigured.Detail != "not configured" ||
		unconfigured.Children[0].Fix != "putnami cloud setup" {
		t.Fatalf("unconfigured = %+v, want degraded with the setup command", unconfigured)
	}

	enabled := &CacheConfig{Enabled: true, URL: "https://cache.test", Mode: "full", Token: &clicore.TokenSource{Command: cacheTokenCommand()}}
	node := CacheStatusNodeFrom(enabled, nil, identity, &CIStatusResponse{
		LastRunCache: &CIRunCache{Tasks: 645, Hits: 311, Misses: 334, ServedMS: 4000, ExecutedMS: 3075000},
	}, nil)
	want := strings.Join([]string{
		"cache  ok  enabled; last run reused 311 of 645 tasks",
		"",
		"  METRIC             VALUE             WINDOW",
		"  reused tasks       311 of 645 (48%)  last run",
		"  served from cache  4s                last run",
		"  executed           51m15s            last run",
		"",
		"  STATE  CHECK     DETAIL",
		"  ok     config    enabled in .putnami/cache.json, https://cache.test (mode full)",
		"  ok     identity  dev@example.com, token from `putnami cloud token --for cache`",
	}, "\n")
	if got := strings.Join(clicore.RenderStatusReport(node), "\n"); got != want {
		t.Fatalf("cache status = %q, want %q", got, want)
	}

	unread := CacheStatusNodeFrom(enabled, nil, identity, nil, errors.New("no control-plane URL"))
	if unread.State != clicore.StatusUnknown || unread.Children[2].ID != "cache.last_run" || unread.Detail != "enabled" {
		t.Fatalf("unread last run = %+v, want an unknown last run check", unread)
	}

	disabled := CacheStatusNodeFrom(&CacheConfig{URL: "https://cache.test", Mode: "full"}, nil, identity, nil, nil)
	if disabled.State != clicore.StatusOK || disabled.Detail != "disabled" {
		t.Fatalf("disabled = %+v, want ok", disabled)
	}
	signedOut := CacheStatusNodeFrom(enabled, nil, nil, nil, nil)
	if signedOut.State != clicore.StatusDegraded || signedOut.Children[1].Fix != "putnami cloud login" {
		t.Fatalf("signed out = %+v, want degraded with the login command", signedOut)
	}
	noSource := CacheStatusNodeFrom(&CacheConfig{Enabled: true, URL: "https://cache.test"}, nil, identity, nil, nil)
	if noSource.State != clicore.StatusDegraded || noSource.Children[1].Fix != "putnami cloud setup" {
		t.Fatalf("no token source = %+v, want degraded with the setup command", noSource)
	}
	broken := CacheStatusNodeFrom(nil, errors.New("parse .putnami/cache.json: unexpected end of JSON input"), identity, nil, nil)
	if broken.State != clicore.StatusUnknown || broken.Children[0].State != clicore.StatusUnknown {
		t.Fatalf("unreadable = %+v, want unknown", broken)
	}
}

func TestCacheStatusNodeReadsTheLocalConfig(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{"HOME": t.TempDir()}
	if node := CacheStatusNode(map[string]any{}, root, env, clicore.IO{}); node.State != clicore.StatusDegraded || node.Detail != "not configured" {
		t.Fatalf("unconfigured = %+v, want degraded", node)
	}
	file := filepath.Join(root, CacheFileRelative)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if node := CacheStatusNode(map[string]any{}, root, env, clicore.IO{}); node.Children[0].State != clicore.StatusOK || node.Detail != "disabled" {
		t.Fatalf("disabled = %+v, want an ok config check", node)
	}
	if err := os.WriteFile(file, []byte(`{"enabled":true,"url":"https://cache.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Enabled but not linked: the last run read fails, and says so.
	node := CacheStatusNode(map[string]any{}, root, env, clicore.IO{})
	if node.State != clicore.StatusUnknown || node.Children[len(node.Children)-1].ID != "cache.last_run" {
		t.Fatalf("enabled without a link = %+v, want an unknown last run check", node)
	}
}

// TestCacheStatusPrintsTheStatusReport drives `cloud cache status` end to end
// against the CI status route the reuse figures come from.
func TestCacheStatusPrintsTheStatusReport(t *testing.T) {
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{},
		LastRunCache: &CIRunCache{Tasks: 4, Hits: 3, Misses: 1, ServedMS: 21, ExecutedMS: 41000}})
	ioctx, root := newCITestIO(t, fake)
	if err := WriteCacheConfig(root, &CacheConfig{Enabled: true, URL: "https://cache.test", Mode: "full",
		Token: &clicore.TokenSource{Command: cacheTokenCommand()}}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	args := []string{"status"}
	if err := Cache(clicore.MergeParams(clicore.ParseFlags(args)), args, root, ioctx.Env, ioctx); err != nil {
		t.Fatalf("cache status: %v", err)
	}
	if text := strings.Join(lines, "\n"); !strings.HasPrefix(text, "cache  ok  enabled; last run reused 3 of 4 tasks") ||
		!strings.Contains(text, "ok     config    enabled in .putnami/cache.json, https://cache.test (mode full)") {
		t.Fatalf("cache status = %q", text)
	}
}
