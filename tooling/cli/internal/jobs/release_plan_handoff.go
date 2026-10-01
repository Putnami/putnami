package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/releaseset"
)

// InternalReleasePlanCallbackEnv is a runner-owned, process-private callback.
// It is captured before repository execution and never inherited by jobs.
const InternalReleasePlanCallbackEnv = "PUTNAMI_INTERNAL_RELEASE_PLAN_CALLBACK"

const releasePlanCallbackPath = "/v1/release-plan"
const maxReleasePlanBytes = 1 << 20

// releasePlanCallbackDialTimeout bounds the loopback connect. The broker is a
// local process, so a slow dial means a missing broker, not a slow network.
const releasePlanCallbackDialTimeout = 3 * time.Second

// releasePlanCallbackTimeout bounds one callback round trip. The broker
// answers only after it mints the publish capability upstream, and that call
// is bounded at 15 s on its side; a cold DNS+TCP+TLS handshake from a fresh
// runner has taken close to 5 s, so the client budget must cover the broker's
// whole bound with headroom. Tests shorten it.
var releasePlanCallbackTimeout = 20 * time.Second

// releasePlanCallbackAttempts is the number of POSTs the handoff makes before
// it gives up. The broker dedupes by plan digest and joins an in-flight mint,
// so a repeated POST for the same plan is idempotent.
const releasePlanCallbackAttempts = 2

func (run *ReleaseSetRun) capabilityPlan() (releasePlanHandoff, error) {
	var zero releasePlanHandoff
	if run == nil || run.plan == nil {
		return zero, fmt.Errorf("release capability requires an immutable release plan")
	}
	if err := releaseset.ValidatePlan(run.plan); err != nil {
		return zero, fmt.Errorf("release capability requires a valid immutable plan")
	}
	// A plan that selects nothing is still a publication: the finalizer
	// releases the head set unchanged so the provider confirms every channel
	// (ADR 0017) and carries the declared mirror intent (ADR 0021 §6). The broker
	// must therefore arm for it, with an empty member list: the capability it
	// obtains grants the release-set authority alone, no registry write.
	if !sourceRevisionPattern.MatchString(run.sourceRevision) {
		return zero, fmt.Errorf("release capability requires an immutable source revision")
	}
	selected := run.plan.SelectedMembers()
	result := releasePlanHandoff{ProtocolVersion: releasePlanProtocolVersion, Namespace: run.plan.Namespace, SourceRevision: run.sourceRevision, Channels: append([]string(nil), run.plan.Channels...), ImmutableChannel: run.immutableChannel, Members: make([]releasePlanMember, 0, len(selected))}
	for _, member := range selected {
		if member.SourceRevision != result.SourceRevision {
			return zero, fmt.Errorf("selected release members do not share the run's immutable source revision")
		}
		result.Members = append(result.Members, releasePlanMember{Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version, SourceRevision: member.SourceRevision, SelectionFingerprint: member.SelectionFingerprint})
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxReleasePlanBytes {
		return zero, fmt.Errorf("release capability plan exceeds its bound")
	}
	sum := sha256.Sum256(encoded)
	result.PlanDigest = "sha256:" + hex.EncodeToString(sum[:])
	return result, nil
}

// HandoffReleaseSetPlan gives the image-owned broker the immutable selected
// publication tuple. The ACK grants no job authority: the normal scheduler
// still requires every configured functional gate to succeed. The broker owns
// Distribution credentials and renewal; no bearer is returned to Framework.
func HandoffReleaseSetPlan(ctx context.Context, run *ReleaseSetRun, planned []*ScheduledJob, executes bool) error {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.planCallback == "" || !executes || run == nil || run.dryRun {
		return nil
	}
	// The finalizer's release is itself a cloud side effect, and a plan that
	// selects nothing plans no publish job at all: it still needs the broker
	// armed, or the head-confirming release is refused as an unarmed write.
	hasPublish := run.NoImpact()
	for _, job := range planned {
		if hasCloudCapabilitySideEffects(job) {
			hasPublish = true
			break
		}
	}
	if !hasPublish {
		return nil
	}
	if capabilities.after == "" || capabilities.cloudToken == "" || len(capabilities.cloudToken) > 64<<10 || strings.ContainsAny(capabilities.cloudToken, " \t\r\n\x00") {
		return fmt.Errorf("release plan callback requires a captured runner capability")
	}
	endpoint, err := privateReleasePlanEndpoint(capabilities.planCallback)
	if err != nil {
		return err
	}
	plan, err := run.capabilityPlan()
	if err != nil {
		return err
	}
	capabilities.planHandoffMu.Lock()
	defer capabilities.planHandoffMu.Unlock()
	if capabilities.planHandoffDigest != "" {
		if capabilities.planHandoffDigest != plan.PlanDigest {
			return fmt.Errorf("runner capability is already bound to another release plan")
		}
		return nil
	}
	body, err := json.Marshal(plan)
	if err != nil || len(body) > maxReleasePlanBytes {
		return fmt.Errorf("release capability plan exceeds its bound")
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: releasePlanCallbackDialTimeout}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: releasePlanCallbackTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var response *http.Response
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("release plan callback request is invalid")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+capabilities.cloudToken)
		response, err = client.Do(req) //nolint:gosec // G704: numeric HTTP loopback with a fixed path, no proxy and no redirects; validated above.
		if err == nil {
			break
		}
		// Only transport-level failures are retried: the broker never answered,
		// so a retry cannot double-apply anything. An HTTP refusal is final.
		if attempt < releasePlanCallbackAttempts && ctx.Err() == nil {
			continue
		}
		return releasePlanCallbackTransportError(err, attempt)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("release plan callback refused the selected plan (HTTP %d)", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(encoded) > 4096 {
		return fmt.Errorf("release plan callback returned an invalid acknowledgment")
	}
	var ack releasePlanAcknowledgment
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ack) != nil || decoder.Decode(&struct{}{}) != io.EOF || ack.ProtocolVersion != releasePlanProtocolVersion || ack.PlanDigest != plan.PlanDigest {
		return fmt.Errorf("release plan callback returned a different acknowledgment")
	}
	capabilities.planHandoffDigest = plan.PlanDigest
	return nil
}

// releasePlanCallbackTransportError names the failure class so a run log can
// tell a broker that never answered from one that answered too slowly. The
// wrapped error carries only the loopback endpoint and the network operation,
// never the bearer.
func releasePlanCallbackTransportError(err error, attempts int) error {
	if os.IsTimeout(err) {
		return fmt.Errorf("release plan callback timed out after %s (%d attempts): %w", releasePlanCallbackTimeout, attempts, err)
	}
	return fmt.Errorf("release plan callback is unavailable (%d attempts): %w", attempts, err)
}

func privateReleasePlanEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != releasePlanCallbackPath || endpoint.Port() == "" {
		return nil, fmt.Errorf("release plan callback requires the private loopback endpoint")
	}
	address := net.ParseIP(endpoint.Hostname())
	if address == nil || !address.IsLoopback() {
		return nil, fmt.Errorf("release plan callback requires the private loopback endpoint")
	}
	return endpoint, nil
}
