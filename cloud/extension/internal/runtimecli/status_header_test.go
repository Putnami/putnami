package runtimecli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	summaryPath = "/v1/workspaces/ws-acme/deployments"
	prodCDPath  = "/v1/workspaces/ws-acme/environments/prod/channel-follow"
	stageCDPath = "/v1/workspaces/ws-acme/environments/staging/channel-follow"
)

// prodChannelFollow is the prod environment's CD read: canary converged at
// generation 42, and an override channel no move reached yet.
const prodChannelFollow = `{"workspace_id":"ws-acme","environment":"prod","definition_revision":4,"channels":[
	{"channel":"canary","receipt":{"namespace":"acme","channel":"canary","generation":42,"release_set_id":"rs_ab12cd34ef567890","moved_at":"2026-09-18T10:02:30Z","received_at":"2026-09-18T10:02:31Z","disposition":"converged","release_ids":[],"settled":true}},
	{"channel":"stable"}
]}`

// statusRoutes serves each path from its own handler and 404s the rest, the
// way a control plane that predates a route answers.
func statusRoutes(routes map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := routes[r.URL.Path]; ok {
			handler(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestStatusPrintsTheCDHeaderOfTheEnvEnvironment is the human contract: one
// line for the --env environment, above a blank line and the unchanged table.
// No other environment's header is requested, whatever the rows name.
func TestStatusPrintsTheCDHeaderOfTheEnvEnvironment(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		stageCDPath: func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the staging header was requested although --env is prod")
			w.WriteHeader(http.StatusInternalServerError)
		},
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{"env": "prod"}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	lines := strings.Split(strings.Join(cap.stdout, "\n"), "\n")
	want := []string{
		"CD prod ← canary: gen 42 converged 2026-09-18T10:02Z (rs_ab12cd34…); ← stable: no move received yet",
		"",
	}
	if len(lines) < len(want)+1 {
		t.Fatalf("output too short:\n%s", strings.Join(lines, "\n"))
	}
	for i, line := range want {
		if lines[i] != line {
			t.Fatalf("line %d = %q, want %q\n%s", i, lines[i], line, strings.Join(lines, "\n"))
		}
	}
	if !strings.HasPrefix(lines[len(want)], "PROJECT") {
		t.Fatalf("the table does not follow the header:\n%s", strings.Join(lines, "\n"))
	}
}

// TestStatusWithoutEnvPrintsNoHeader pins the direct-caller path (the manifest
// defaults --env to prod): no environment means no header request, no header
// line, and channel_follow encoded as an empty array, never null.
func TestStatusWithoutEnvPrintsNoHeader(t *testing.T) {
	var headerRequests atomic.Int32
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath: func(w http.ResponseWriter, r *http.Request) {
			headerRequests.Add(1)
			jsonReply(http.StatusOK, prodChannelFollow)(w, r)
		},
	}))
	defer srv.Close()

	human := &captureIO{}
	if err := runStatus(t, srv, human, map[string]any{}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if out := strings.Join(human.stdout, "\n"); !strings.HasPrefix(out, "PROJECT") {
		t.Fatalf("output = %q, want the table with no header line", out)
	}

	structured := &captureIO{}
	if err := runStatus(t, srv, structured, map[string]any{"output": "json"}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := decodeResultData([]byte(strings.Join(structured.stdout, "\n")), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["channel_follow"]); got != "[]" {
		t.Fatalf("channel_follow = %s, want []", got)
	}
	if n := headerRequests.Load(); n != 0 {
		t.Fatalf("header requested %d times, want none without --env", n)
	}
}

// TestStatusCDHeaderNeverFailsTheCommand pins the degradation contract: an
// older control plane (plain-text 404), a refusal, an unreadable ledger, and a
// body that does not decode each print `CD prod: unavailable (<reason>)` while
// the command succeeds and prints its table.
func TestStatusCDHeaderNeverFailsTheCommand(t *testing.T) {
	cases := []struct {
		name  string
		reply http.HandlerFunc
		want  string
	}{
		{"route absent on an older control plane", nil, "CD prod: unavailable (404 Not Found)"},
		{"forbidden", jsonReply(http.StatusForbidden, `{"error":"Forbidden","message":"Insufficient permissions"}`), "CD prod: unavailable (403 Forbidden)"},
		{"ledger unavailable", jsonReply(http.StatusServiceUnavailable, `{"error":"the channel-move ledger could not be read"}`), "CD prod: unavailable (503 Service Unavailable)"},
		{"undecodable body", jsonReply(http.StatusOK, `{"channels":"nope"`), "CD prod: unavailable (invalid response)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{summaryPath: jsonReply(http.StatusOK, statusSample)}
			if tc.reply != nil {
				routes[prodCDPath] = tc.reply
			}
			srv := httptest.NewServer(statusRoutes(routes))
			defer srv.Close()

			cap := &captureIO{}
			if err := runStatus(t, srv, cap, map[string]any{"env": "prod"}); err != nil {
				t.Fatalf("Status failed on a degraded header: %v", err)
			}
			out := strings.Join(cap.stdout, "\n")
			if first := strings.SplitN(out, "\n", 2)[0]; first != tc.want {
				t.Fatalf("header = %q, want %q\n%s", first, tc.want, out)
			}
			if !strings.Contains(out, "accounts/workloads/auth-server") {
				t.Fatalf("the table is missing after a degraded header:\n%s", out)
			}
		})
	}

	// A transport failure is the same degradation, with a short reason and no URL.
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{summaryPath: jsonReply(http.StatusOK, statusSample)}))
	defer srv.Close()
	ctx := newStatusCtxFor(srv, &captureIO{})
	ctx.ControlPlane = "http://127.0.0.1:1"
	header := fetchChannelFollow(ctx, context.Background(), "prod")
	if !strings.HasPrefix(header.Unavailable, "request failed: ") || strings.Contains(header.Unavailable, "http://") || header.Channels != nil {
		t.Fatalf("transport failure = %+v, want a short request-failed reason and null channels", header)
	}
}

// TestStatusCDHeaderNeverReMintsTheSession pins that a 401 on a header request
// degrades instead of re-minting: header requests run concurrently, and a
// re-mint writes the shared bearer and may rotate the stored refresh token. The
// summary keeps its own one-shot re-mint.
func TestStatusCDHeaderNeverReMintsTheSession(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusUnauthorized, `{"error":"Authentication required"}`),
	}))
	defer srv.Close()

	cap := &captureIO{}
	ctx := newStatusCtxFor(srv, cap)
	var remints atomic.Int32
	ctx.RefreshAuth = func() (clicore.Bearer, error) {
		remints.Add(1)
		return clicore.NewBearer("fresh"), nil
	}
	if err := statusRun(ctx, context.Background(), map[string]any{"env": "prod"}, t.TempDir()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := remints.Load(); got != 0 {
		t.Fatalf("re-mints = %d, want 0: a header request must never re-mint the session", got)
	}
	if first := strings.SplitN(strings.Join(cap.stdout, "\n"), "\n", 2)[0]; first != "CD prod: unavailable (401 Unauthorized)" {
		t.Fatalf("header = %q, want the 401 degradation", first)
	}
}

// TestStatusEnvFetchesItsHeaderInParallelWithTheSummary proves the two
// requests overlap: the summary handler does not answer until the header
// request has arrived. A serial implementation would never send the header and
// the summary would time out waiting for it. Only the --env environment is read.
func TestStatusEnvFetchesItsHeaderInParallelWithTheSummary(t *testing.T) {
	headerArrived := make(chan struct{})
	var once sync.Once
	var requested sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, true)
		switch r.URL.Path {
		case summaryPath:
			select {
			case <-headerArrived:
			case <-time.After(5 * time.Second):
				t.Error("the summary was answered before any header request arrived: the requests are serial")
			}
			jsonReply(http.StatusOK, statusSample)(w, r)
		case prodCDPath:
			once.Do(func() { close(headerArrived) })
			jsonReply(http.StatusOK, prodChannelFollow)(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{"env": "prod"}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, asked := requested.Load(stageCDPath); asked {
		t.Fatal("the staging header was read although --env names prod")
	}
	out := strings.Join(cap.stdout, "\n")
	if strings.Count(out, "CD ") != 1 || !strings.HasPrefix(out, "CD prod ← canary: gen 42") {
		t.Fatalf("output = %q, want exactly the prod header", out)
	}
}

// TestStatusStructuredOutputCarriesTheCDHeaders pins the additive JSON field:
// channel_follow carries the --env header — the server's channels, or null
// channels with the unavailable reason — while workspace_id and deployments
// keep their keys and shapes.
func TestStatusStructuredOutputCarriesTheCDHeaders(t *testing.T) {
	srv := httptest.NewServer(statusRoutes(map[string]http.HandlerFunc{
		summaryPath: jsonReply(http.StatusOK, statusSample),
		prodCDPath:  jsonReply(http.StatusOK, prodChannelFollow),
		stageCDPath: jsonReply(http.StatusServiceUnavailable, `{"error":"the channel-move ledger is not available yet"}`),
	}))
	defer srv.Close()

	type header struct {
		Environment        string            `json:"environment"`
		DefinitionRevision int64             `json:"definition_revision"`
		Channels           []followedChannel `json:"channels"`
		Unavailable        string            `json:"unavailable"`
	}
	read := func(env string) (map[string]json.RawMessage, header, string) {
		t.Helper()
		cap := &captureIO{}
		if err := runStatus(t, srv, cap, map[string]any{"output": "json", "env": env}); err != nil {
			t.Fatalf("Status: %v", err)
		}
		joined := strings.Join(cap.stdout, "\n")
		var raw map[string]json.RawMessage
		if err := decodeResultData([]byte(joined), &raw); err != nil {
			t.Fatalf("decode: %v\n%s", err, joined)
		}
		var headers []header
		if err := json.Unmarshal(raw["channel_follow"], &headers); err != nil || len(headers) != 1 {
			t.Fatalf("channel_follow = %s (%v), want one header", raw["channel_follow"], err)
		}
		var rawHeaders []map[string]json.RawMessage
		if err := json.Unmarshal(raw["channel_follow"], &rawHeaders); err != nil || len(rawHeaders) != 1 {
			t.Fatalf("channel_follow = %s (%v), want one header object", raw["channel_follow"], err)
		}
		return raw, headers[0], string(rawHeaders[0]["channels"])
	}

	raw, prod, _ := read("prod")
	for _, key := range []string{"workspace_id", "deployments", "channel_follow"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("structured object misses %q", key)
		}
	}
	var deployments []json.RawMessage
	if err := json.Unmarshal(raw["deployments"], &deployments); err != nil || len(deployments) != 3 {
		t.Fatalf("deployments = %s (%v), want the summary's 3 rows", raw["deployments"], err)
	}
	if prod.Environment != "prod" || prod.DefinitionRevision != 4 || len(prod.Channels) != 2 || prod.Unavailable != "" {
		t.Fatalf("prod header = %+v, want the server's answer", prod)
	}
	if receipt := prod.Channels[0].Receipt; receipt == nil || receipt.Generation != 42 || receipt.Disposition != "converged" || !receipt.Settled {
		t.Fatalf("prod canary receipt = %+v, want generation 42 converged", receipt)
	}

	_, staging, stagingChannels := read("staging")
	if staging.Environment != "staging" || staging.Channels != nil || staging.Unavailable != "503 Service Unavailable" {
		t.Fatalf("staging header = %+v, want null channels and the unavailable reason", staging)
	}
	if stagingChannels != "null" {
		t.Fatalf("unavailable header channels = %s, want null, not an empty list", stagingChannels)
	}
}

// TestStatusMoveRendersEveryOutcome pins the one-line move description: the
// in-flight state, the receive time standing in for a missing move time, the
// reason shown only for an outcome other than converged or opened, and long or
// multi-line reasons collapsed to one bounded line.
func TestStatusMoveRendersEveryOutcome(t *testing.T) {
	moved := time.Date(2026, 9, 18, 10, 2, 59, 0, time.UTC)
	long := strings.Repeat("apps/api: no oci image member; ", 10)
	cases := []struct {
		name    string
		receipt *channelMoveReceipt
		want    string
	}{
		{"no move", nil, "no move received yet"},
		{"opened", &channelMoveReceipt{Generation: 7, Disposition: "opened", Settled: true, MovedAt: moved, ReleaseSetID: "rs_0123456789abcdef", ReleaseIDs: []string{"cm_1"}, Reason: "opened 1 run"},
			"gen 7 opened 2026-09-18T10:02Z (rs_01234567…), release cm_1"},
		{"in flight", &channelMoveReceipt{Generation: 8, Disposition: "opened", Settled: false, MovedAt: moved, ReleaseSetID: "rs_0123456789abcdef"},
			"gen 8 in flight 2026-09-18T10:02Z (rs_01234567…)"},
		{"not deployable", &channelMoveReceipt{Generation: 9, Disposition: "not_deployable", Settled: true, ReceivedAt: moved, ReleaseSetID: "rs_0123456789abcdef", Reason: "prod: apps/api:\n  no oci image member"},
			"gen 9 not_deployable 2026-09-18T10:02Z (rs_01234567…): prod: apps/api: no oci image member"},
		{"converged keeps its note to itself", &channelMoveReceipt{Generation: 10, Disposition: "converged", Settled: true, MovedAt: moved, Reason: "prod: already serving"},
			"gen 10 converged 2026-09-18T10:02Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusMove(tc.receipt); got != tc.want {
				t.Fatalf("statusMove = %q, want %q", got, tc.want)
			}
		})
	}

	partial := statusMove(&channelMoveReceipt{Generation: 11, Disposition: "partially_opened", Settled: true, Reason: long})
	reason := strings.TrimPrefix(partial, "gen 11 partially_opened: ")
	if reason == partial || len([]rune(reason)) != statusReasonWidth || !strings.HasSuffix(reason, "…") {
		t.Fatalf("long reason = %q, want it cut to %d characters ending in an ellipsis", partial, statusReasonWidth)
	}
}
