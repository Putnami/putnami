package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func capabilityReleasePlan() *ReleaseSetRun {
	return &ReleaseSetRun{plan: &releaseset.Plan{ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary", "v1.2.3"}, Heads: map[string]*distribution.ChannelHead{"canary": nil, "v1.2.3": nil}, Members: []releaseset.PlannedMember{{Ecosystem: "npm", Coordinate: "@putnami/api", Version: "1.2.3", ProjectID: "/private/repository/api", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{}, Selected: true}}}, immutableChannel: "v1.2.3", sourceRevision: testRevision}
}

// capabilityNoImpactPlan is a plan that selects nothing: its one member
// inherits the resolved canary head's record verbatim.
func capabilityNoImpactPlan(t *testing.T) *ReleaseSetRun {
	t.Helper()
	inherited := distribution.ReleaseSetMember{Ecosystem: "npm", Coordinate: "@putnami/api", Version: "1.2.3", ArtifactDigest: digestFor('f'), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision, SelectionFingerprint: digestFor('a')}
	set := &distribution.ReleaseSet{ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Members: []distribution.ReleaseSetMember{inherited}}
	ref, diagnostics := distribution.DeriveReleaseSetRef(set)
	if len(diagnostics) != 0 {
		t.Fatalf("derive head ref: %+v", diagnostics)
	}
	head := &distribution.ChannelHead{Ref: ref, Generation: 1, ReleaseSet: set}
	return &ReleaseSetRun{plan: &releaseset.Plan{ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}, Heads: map[string]*distribution.ChannelHead{"canary": head}, Members: []releaseset.PlannedMember{{Ecosystem: "npm", Coordinate: "@putnami/api", Version: "1.2.3", ArtifactDigest: digestFor('f'), ProjectID: "/private/repository/api", SourceRevision: testRevision, SelectionFingerprint: digestFor('a'), Dependencies: []distribution.ReleaseSetDependency{}}}}, sourceRevision: testRevision}
}
func capabilityPublishJob(t *testing.T) *ScheduledJob {
	return capabilityTestJob(t.TempDir(), &workspace.Project{ID: "/api", Name: "api"}, "publish", "publish", "/unused", extensionproto.SideEffectsRegistry)
}
func callbackContext(t *testing.T, endpoint string) context.Context {
	t.Helper()
	t.Setenv(InternalReleasePlanCallbackEnv, endpoint)
	t.Setenv(extensionproto.CloudTokenEnv, "opaque-local-client-token-keep-private")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "test,lint")
	ctx := CaptureProcessCapabilities(t.Context())
	if _, exists := os.LookupEnv(InternalReleasePlanCallbackEnv); exists {
		t.Fatal("callback left in ambient environment")
	}
	return ctx
}

func TestReleaseCapabilityHandoffCanonicalSelection(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "canonical-selected-plan-without-credentials")
	run := capabilityReleasePlan()
	plan, err := run.capabilityPlan()
	if err != nil {
		t.Fatal(err)
	}
	digest := plan.PlanDigest
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	expected := `{"protocolVersion":1,"namespace":"putnami","sourceRevision":"` + testRevision + `","channels":["canary","v1.2.3"],"immutableChannel":"v1.2.3","members":[{"ecosystem":"npm","coordinate":"@putnami/api","version":"1.2.3","sourceRevision":"` + testRevision + `","selectionFingerprint":"` + digestFor('a') + `"}]}`
	sum := sha256.Sum256([]byte(expected))
	if string(encoded) != expected || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("canonical handoff differs: %s %s", encoded, digest)
	}
	plan.Channels[0] = "changed"
	plan.Members[0].Coordinate = "other"
	if run.plan.Channels[0] != "canary" || run.plan.Members[0].Coordinate != "@putnami/api" {
		t.Fatal("handoff aliases authoritative plan")
	}
	run.plan.Members = append(run.plan.Members, releaseset.PlannedMember{Ecosystem: "npm", Coordinate: "@putnami/other", Version: "1.2.3", ProjectID: "/other", SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: digestFor('b'), Dependencies: []distribution.ReleaseSetDependency{}, Selected: true})
	if _, err := run.capabilityPlan(); err == nil {
		t.Fatal("mixed source revisions accepted")
	}
}

// TestReleaseCapabilityHandoffArmsAPlanThatSelectsNothing pins the no-impact
// publication: the finalizer still releases the head set unchanged (ADR 0017),
// so the broker must be armed for that one release. The handoff carries an
// empty member list under the run's own source revision, and a member the run
// did not select never appears in it.
func TestReleaseCapabilityHandoffArmsAPlanThatSelectsNothing(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "member-less-plan-arms-the-head-confirming-release")
	run := capabilityNoImpactPlan(t)
	if !run.NoImpact() {
		t.Fatal("fixture must select nothing")
	}
	plan, err := run.capabilityPlan()
	if err != nil {
		t.Fatal(err)
	}
	digest := plan.PlanDigest
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	expected := `{"protocolVersion":1,"namespace":"putnami","sourceRevision":"` + testRevision + `","channels":["canary"],"members":[]}`
	sum := sha256.Sum256([]byte(expected))
	if string(encoded) != expected || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("member-less handoff differs: %s %s", encoded, digest)
	}
	run.sourceRevision = ""
	if _, err := run.capabilityPlan(); err == nil {
		t.Fatal("a run without an immutable source revision handed off a plan")
	}

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		var request releasePlanHandoff
		if err := json.Unmarshal(body, &request); err != nil || len(request.Members) != 0 || request.SourceRevision != testRevision {
			t.Errorf("member-less callback body = %s (%v)", body, err)
		}
		_ = json.NewEncoder(w).Encode(releasePlanAcknowledgment{ProtocolVersion: releasePlanProtocolVersion, PlanDigest: request.PlanDigest})
	}))
	defer server.Close()
	ctx := callbackContext(t, server.URL+releasePlanCallbackPath)
	run = capabilityNoImpactPlan(t)
	// No publish job is planned for a no-impact run; the release alone arms.
	if err := HandoffReleaseSetPlan(ctx, run, nil, true); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatalf("no-impact handoff callbacks = %d, want 1", received.Load())
	}
	dry := capabilityNoImpactPlan(t)
	dry.dryRun = true
	if err := HandoffReleaseSetPlan(ctx, dry, nil, true); err != nil || received.Load() != 1 {
		t.Fatalf("dry run armed publication: err=%v callbacks=%d", err, received.Load())
	}
}

func TestReleaseCapabilityHandoffPrivateIdempotentACK(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "private-callback-and-exact-ack")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != releasePlanCallbackPath || r.Header.Get("Authorization") != "Bearer opaque-local-client-token-keep-private" {
			t.Error("incorrect private callback")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "/private/") || strings.Contains(string(body), "opaque-local") || strings.Contains(string(body), "artifactDigest") {
			t.Error("handoff exposed execution details")
		}
		var request releasePlanHandoff
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(releasePlanAcknowledgment{ProtocolVersion: releasePlanProtocolVersion, PlanDigest: request.PlanDigest})
	}))
	defer server.Close()
	ctx := callbackContext(t, server.URL+releasePlanCallbackPath)
	run := capabilityReleasePlan()
	jobs := []*ScheduledJob{capabilityPublishJob(t)}
	if err := HandoffReleaseSetPlan(ctx, run, jobs, true); err != nil {
		t.Fatal(err)
	}
	if err := HandoffReleaseSetPlan(ctx, run, jobs, true); err != nil || calls != 1 {
		t.Fatalf("nonidempotent handoff %v calls%d", err, calls)
	}
	run.plan.Members[0].Version = "1.2.4"
	if err := HandoffReleaseSetPlan(ctx, run, jobs, true); err == nil || calls != 1 {
		t.Fatal("different plan obtained same capability")
	}
	env := scopeProcessCapabilities(ctx, []string{InternalReleasePlanCallbackEnv + "=hostile"}, jobs[0])
	for _, entry := range env {
		if strings.HasPrefix(entry, InternalReleasePlanCallbackEnv+"=") {
			t.Fatal("job inherited callback")
		}
	}
}

func TestReleaseCapabilityHandoffRefusesInvalidACKAndRedirects(t *testing.T) {
	for _, mode := range []string{"wrong digest", "unknown field", "oversize", "trailing", "redirect", "denied"} {
		t.Run(mode, func(t *testing.T) {
			destinationCalls := 0
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls++ }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
				case "denied":
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte("private-server-token"))
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
				default:
					var request releasePlanHandoff
					_ = json.NewDecoder(r.Body).Decode(&request)
					body := releasePlanAcknowledgment{ProtocolVersion: releasePlanProtocolVersion, PlanDigest: request.PlanDigest}
					if mode == "wrong digest" {
						body.PlanDigest = "other"
					}
					if mode == "unknown field" {
						_, _ = w.Write([]byte(`{"protocolVersion":1,"planDigest":"` + request.PlanDigest + `","access_token":"private-server-token"}`))
						return
					}
					_ = json.NewEncoder(w).Encode(body)
					if mode == "trailing" {
						_, _ = w.Write([]byte(`{}`))
					}
				}
			}))
			defer server.Close()
			err := HandoffReleaseSetPlan(callbackContext(t, server.URL+releasePlanCallbackPath), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true)
			if mode == "denied" && (err == nil || !strings.Contains(err.Error(), "HTTP 403")) {
				t.Fatalf("missing refusal status: %v", err)
			}
			if err == nil || strings.Contains(err.Error(), "private-server-token") || destinationCalls != 0 {
				t.Fatalf("unsafe callback result %v destination%d", err, destinationCalls)
			}
		})
	}
	for _, endpoint := range []string{"https://127.0.0.1:123/v1/release-plan", "http://example.com:123/v1/release-plan", "http://localhost:123/v1/release-plan", "http://user@127.0.0.1:123/v1/release-plan", "http://127.0.0.1:123/v1/release-plan?other=1", "http://127.0.0.1:123/other"} {
		if err := HandoffReleaseSetPlan(callbackContext(t, endpoint), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true); err == nil {
			t.Fatalf("unsafe endpoint accepted %s", endpoint)
		}
	}
}

func TestReleaseCapabilityHandoffSkipsNonexecutingPlans(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "no-handoff-for-preview-or-dry-run")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	ctx := callbackContext(t, server.URL+releasePlanCallbackPath)
	run := capabilityReleasePlan()
	jobs := []*ScheduledJob{capabilityPublishJob(t)}
	if err := HandoffReleaseSetPlan(ctx, run, jobs, false); err != nil {
		t.Fatal(err)
	}
	run.dryRun = true
	if err := HandoffReleaseSetPlan(ctx, run, jobs, true); err != nil {
		t.Fatal(err)
	}
	run.dryRun = false
	if err := HandoffReleaseSetPlan(ctx, run, nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("preview or dry-run publication armed a broker")
	}
}

// This vector is shared with Cloud Records and its image-owned Node broker.
func TestReleaseCapabilityWireMatchesCloudDigest(t *testing.T) {
	source := strings.Repeat("a", 40)
	plan := releasePlanHandoff{ProtocolVersion: releasePlanProtocolVersion, Namespace: "putnami", SourceRevision: source, Channels: []string{"canary", "v1.2.3"}, ImmutableChannel: "v1.2.3", Members: []releasePlanMember{
		{Ecosystem: "npm", Coordinate: "@scope/pkg", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor('1')},
		{Ecosystem: "go", Coordinate: "go.putnami.dev/protocol/cli", Version: "v1.2.3", SourceRevision: source, SelectionFingerprint: digestFor('2')},
		{Ecosystem: "oci", Coordinate: "putnami/cloud", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor('3')},
		{Ecosystem: "put", Coordinate: "putnami/app", Version: "1.2.3", SourceRevision: source, SelectionFingerprint: digestFor('4')},
	}}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if got := "sha256:" + hex.EncodeToString(digest[:]); got != "sha256:e75cd832da7476399dd2aaccf24c97583fa4ae75d3c29399774525f70b6549ce" {
		t.Fatalf("cross-language digest = %s", got)
	}
}

func TestReleasePlanCallbackWithoutAfterIsRefusedAtCapture(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "private-selected-publication-handoff", "callback-without-after-refused")
	for _, after := range []string{"", " "} {
		t.Run("after="+after, func(t *testing.T) {
			t.Setenv(InternalReleasePlanCallbackEnv, "http://127.0.0.1:123/v1/release-plan")
			t.Setenv(extensionproto.CloudTokenEnv, "private-token-never-log")
			t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)
			output, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = previous; _ = output.Close() }()
			ctx := CaptureProcessCapabilities(t.Context())
			capabilities := processCapabilitiesFromContext(ctx)
			if capabilities == nil || capabilities.planCallback != "" || capabilities.cloudToken != "" {
				t.Fatal("callback retained without AFTER")
			}
			if _, present := os.LookupEnv(extensionproto.CloudTokenEnv); present {
				t.Fatal("capability remained ambient")
			}
			if _, present := os.LookupEnv(InternalReleasePlanCallbackEnv); present {
				t.Fatal("callback remained ambient")
			}
			if _, err := output.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			diagnostic, err := io.ReadAll(output)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(diagnostic), "PUTNAMI_CLOUD_CAPABILITY_AFTER is required") || strings.Contains(string(diagnostic), "private-token-never-log") {
				t.Fatalf("unsafe or missing capture diagnostic: %s", diagnostic)
			}
		})
	}
}

// ackHandler answers the private callback with an exact acknowledgment.
func ackHandler(w http.ResponseWriter, r *http.Request) {
	var request releasePlanHandoff
	_ = json.NewDecoder(r.Body).Decode(&request)
	_ = json.NewEncoder(w).Encode(releasePlanAcknowledgment{ProtocolVersion: releasePlanProtocolVersion, PlanDigest: request.PlanDigest})
}

func TestReleaseCapabilityHandoffBudgetCoversTheBrokerMint(t *testing.T) {
	// The broker answers /v1/release-plan only after minting the publish
	// capability upstream, bounded at 15 s on its side. The client budget must
	// exceed that bound, and the loopback dial must stay short.
	if releasePlanCallbackTimeout < 15*time.Second+time.Second {
		t.Fatalf("callback timeout %s does not cover the broker's 15 s mint bound", releasePlanCallbackTimeout)
	}
	if releasePlanCallbackDialTimeout > 3*time.Second {
		t.Fatalf("loopback dial timeout %s is not short", releasePlanCallbackDialTimeout)
	}
	if releasePlanCallbackAttempts != 2 {
		t.Fatalf("attempts = %d, want one bounded retry", releasePlanCallbackAttempts)
	}
}

func TestReleaseCapabilityHandoffSlowBrokerRetryAndTimeoutClass(t *testing.T) {
	previous := releasePlanCallbackTimeout
	t.Cleanup(func() { releasePlanCallbackTimeout = previous })

	t.Run("slow broker inside the budget succeeds once", func(t *testing.T) {
		releasePlanCallbackTimeout = 3 * time.Second
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			time.Sleep(500 * time.Millisecond)
			ackHandler(w, r)
		}))
		defer server.Close()
		if err := HandoffReleaseSetPlan(callbackContext(t, server.URL+releasePlanCallbackPath), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
	})

	t.Run("transport failure is retried once and the retry ACK is accepted", func(t *testing.T) {
		releasePlanCallbackTimeout = 3 * time.Second
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				// Drop the connection without a response: a transport-level failure.
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			ackHandler(w, r)
		}))
		defer server.Close()
		if err := HandoffReleaseSetPlan(callbackContext(t, server.URL+releasePlanCallbackPath), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
	})

	t.Run("timeout is named and bounded to two attempts", func(t *testing.T) {
		releasePlanCallbackTimeout = 200 * time.Millisecond
		var calls atomic.Int32
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		defer close(release)
		err := HandoffReleaseSetPlan(callbackContext(t, server.URL+releasePlanCallbackPath), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true)
		if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") || strings.Contains(err.Error(), "opaque-local-client-token") {
			t.Fatalf("timeout not classified safely: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
	})

	t.Run("missing broker is named as unavailable with its cause", func(t *testing.T) {
		releasePlanCallbackTimeout = 3 * time.Second
		server := httptest.NewServer(http.HandlerFunc(ackHandler))
		endpoint := server.URL + releasePlanCallbackPath
		server.Close()
		err := HandoffReleaseSetPlan(callbackContext(t, endpoint), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true)
		if err == nil || !strings.Contains(err.Error(), "is unavailable (2 attempts): ") || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("unavailable broker not classified: %v", err)
		}
	})

	t.Run("HTTP refusal is final and not retried", func(t *testing.T) {
		releasePlanCallbackTimeout = 3 * time.Second
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusConflict)
		}))
		defer server.Close()
		err := HandoffReleaseSetPlan(callbackContext(t, server.URL+releasePlanCallbackPath), capabilityReleasePlan(), []*ScheduledJob{capabilityPublishJob(t)}, true)
		if err == nil || !strings.Contains(err.Error(), "HTTP 409") || calls.Load() != 1 {
			t.Fatalf("refusal retried or unnamed: %v calls=%d", err, calls.Load())
		}
	})
}
