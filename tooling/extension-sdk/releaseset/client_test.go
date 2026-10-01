package releaseset

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
)

func testResolveRequest() *distribution.ResolveRequest {
	return &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
	}
}

func TestClientResolveUsesExactBoundedPrivateRequestFile(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	request := testResolveRequest()
	response := testResolveResponse(t)
	client.Env = fakeEnv(t, response, observation)

	got, err := client.Resolve(context.Background(), request)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Heads["canary"].Ref != response.Heads["canary"].Ref {
		t.Fatalf("ref = %+v, want %+v", got.Heads["canary"].Ref, response.Heads["canary"].Ref)
	}
	lines := strings.Split(strings.TrimSuffix(mustRead(t, observation), "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("observation = %q", lines)
	}
	if lines[0] != "cloud|release-set|resolve|--request-file" {
		t.Fatalf("argv = %q", lines[0])
	}
	if !filepath.IsAbs(lines[1]) {
		t.Fatalf("request path is not absolute: %q", lines[1])
	}
	if lines[2] != privateRequestAccess {
		t.Fatalf("request access = %q, want %s", lines[2], privateRequestAccess)
	}
	parsed, diagnostics := distribution.ParseAndValidateResolveRequest([]byte(lines[3]))
	if parsed == nil || hasDiagnosticErrors(diagnostics) ||
		parsed.ProtocolVersion != request.ProtocolVersion || parsed.Namespace != request.Namespace ||
		len(parsed.Channels) != 1 || parsed.Channels[0] != "canary" || parsed.ReleaseID != request.ReleaseID {
		t.Fatalf("observed request = %+v diagnostics=%v", parsed, diagnostics)
	}
	if _, err := os.Stat(lines[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary request survived call: %v", err)
	}
}

// A release-id selector resolves the immutable set instead of a channel, and
// the answer's generation is 0 because no channel names it.
func TestClientResolveReadsAnImmutableRelease(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	head := testResolveResponse(t).Heads["canary"]
	release := *head
	release.Generation = 0
	client.Env = fakeEnv(t, distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Release:         &release,
	}, observation)

	got, err := client.Resolve(context.Background(), &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		ReleaseID:       head.Ref.ID,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Release == nil || got.Release.Ref != head.Ref || got.Release.Generation != 0 {
		t.Fatalf("release = %+v", got.Release)
	}
}

func TestClientRejectsMalformedOrContaminatedResponseAndCleansUp(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	response := testResolveResponse(t)
	badHead := *response.Heads["canary"]
	badHead.Ref.ID = "rs_" + strings.Repeat("f", 64)
	badRef := distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Heads:           map[string]*distribution.ChannelHead{"canary": &badHead},
	}

	cases := []struct {
		name          string
		response      any
		contamination string
	}{
		{"malformed ref", badRef, ""},
		{"stdout prefix", response, "noise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client.Env = fakeEnv(t, tc.response, observation)
			if tc.contamination != "" {
				client.Env = append(client.Env, "FAKE_CONTAMINATION="+tc.contamination)
			}
			_, err := client.Resolve(context.Background(), testResolveRequest())
			if err == nil {
				t.Fatal("Resolve accepted malformed provider stdout")
			}
			lines := strings.Split(strings.TrimSuffix(mustRead(t, observation), "\n"), "\n")
			if _, statErr := os.Stat(lines[1]); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("temporary request survived failure: %v", statErr)
			}
		})
	}
}

func TestClientChannelSetAndChannelStatusUseExactPublicOperations(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	head := testResolveResponse(t).Heads["canary"]

	client.Env = fakeEnv(t, distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeReleased,
		Current:         map[string]*distribution.ChannelHead{"stable": {Ref: head.Ref, Generation: 3}},
	}, observation)
	moved, err := client.ChannelSet(context.Background(), &distribution.ChannelSetRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channel:         "stable",
		Expected:        nil,
		From:            distribution.ChannelSource{Channel: "canary"},
	})
	if err != nil {
		t.Fatalf("ChannelSet: %v", err)
	}
	if moved.Current["stable"].Generation != 3 {
		t.Fatalf("channel-set answer = %+v", moved.Current)
	}
	if first := strings.Split(mustRead(t, observation), "\n")[0]; first != "cloud|release-set|channel-set|--request-file" {
		t.Fatalf("channel-set argv = %q", first)
	}

	client.Env = fakeEnv(t, distribution.ChannelStatusResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Desired:         &distribution.ChannelHead{Ref: head.Ref, Generation: 3},
		Observed:        map[string]uint64{"npm": 3, "oci": 2},
	}, observation)
	status, err := client.ChannelStatus(context.Background(), &distribution.ChannelStatusRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channel:         "stable",
	})
	if err != nil {
		t.Fatalf("ChannelStatus: %v", err)
	}
	if status.Observed["oci"] != 2 || status.Desired.Generation != 3 {
		t.Fatalf("channel-status answer = %+v", status)
	}
	if first := strings.Split(mustRead(t, observation), "\n")[0]; first != "cloud|release-set|channel-status|--request-file" {
		t.Fatalf("channel-status argv = %q", first)
	}
}

// A channel-set answer that reports another channel would move a head the
// caller never named, so the client refuses it rather than reporting success.
func TestClientChannelSetRefusesAnAnswerAboutAnotherChannel(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	head := testResolveResponse(t).Heads["canary"]
	client.Env = fakeEnv(t, distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeReleased,
		Current:         map[string]*distribution.ChannelHead{"canary": {Ref: head.Ref, Generation: 3}},
	}, observation)
	_, err := client.ChannelSet(context.Background(), &distribution.ChannelSetRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channel:         "stable",
		From:            distribution.ChannelSource{ReleaseID: head.Ref.ID},
	})
	if err == nil || !strings.Contains(err.Error(), "requested target channel") {
		t.Fatalf("error = %v, want a refusal naming the requested channel", err)
	}
}

// A provider cannot acknowledge a metadata move from an immutable release id
// while returning a different set: that would turn a successful CLI command
// into a silent move to the wrong artifact cohort.
func TestClientChannelSetBindsSuccessToTheSourceReleaseID(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	head := testResolveResponse(t).Heads["canary"]
	other := head.Ref
	other.ID = "rs_" + strings.Repeat("f", 64)
	other.Digest = "sha256:" + strings.Repeat("f", 64)
	client.Env = fakeEnv(t, distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeReleased,
		Current:         map[string]*distribution.ChannelHead{"stable": {Ref: other, Generation: 3}},
	}, observation)
	_, err := client.ChannelSet(context.Background(), &distribution.ChannelSetRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channel:         "stable",
		From:            distribution.ChannelSource{ReleaseID: head.Ref.ID},
	})
	if err == nil || !strings.Contains(err.Error(), distribution.ErrorCodeRefMismatch) {
		t.Fatalf("error = %v, want a release-id mismatch", err)
	}
}

func TestClientReleaseTransportsMirrorIntentThroughStrictProviderFile(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	request := testReleaseRequest(t)
	request.Mirrors = map[string]distribution.MirrorTarget{"npm": {To: "https://registry.npmjs.org"}}
	ref, diagnostics := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	client.Env = fakeEnv(t, distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeReleased,
		Current:         map[string]*distribution.ChannelHead{"canary": {Ref: ref, Generation: 8}},
	}, observation)
	if _, err := client.Release(context.Background(), request); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(mustRead(t, observation), "\n"), "\n")
	if len(lines) < 4 || lines[0] != "cloud|release-set|release|--request-file" || lines[2] != privateRequestAccess {
		t.Fatalf("unexpected release transport: %q", lines)
	}
	parsed, diagnostics := distribution.ParseAndValidateReleaseRequest([]byte(lines[3]))
	if parsed == nil || hasDiagnosticErrors(diagnostics) || !maps.Equal(parsed.Mirrors, request.Mirrors) {
		t.Fatalf("strict provider did not receive mirror intent: %+v, %v", parsed, diagnostics)
	}
	if _, err := os.Stat(lines[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary release request survived call: %v", err)
	}
}

func TestClientReleaseCarriesOnlyFrameworkPublishedImagesInPrivateFile(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "published.json")
	pathObservation := filepath.Join(tempDir, "published.path")
	hostile := filepath.Join(tempDir, "hostile.json")
	if err := os.WriteFile(hostile, []byte(`{"protocolVersion":1,"published":[{"project":"hostile","digest":"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := testReleaseRequest(t)
	derived, diagnostics := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	releaseResponse := distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeReleased,
		Current:         map[string]*distribution.ChannelHead{"canary": {Ref: derived, Generation: 8}},
	}
	encodedResponse, err := json.Marshal(releaseResponse)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(fakeProviderExecutable(t))
	client.TempDir = tempDir
	client.Env = append(os.Environ(),
		fakeProviderEnv+"="+fakePublished,
		"FAKE_RESPONSE="+string(encodedResponse),
		"FAKE_OBSERVATION="+observation,
		"FAKE_PATH_OBSERVATION="+pathObservation,
		PublishedImagesFileEnv+"="+hostile,
	)
	want := []PublishedImage{
		{Project: "svc/alpha", Digest: testDigest('a')},
		{Project: "svc/beta", Digest: testDigest('b')},
	}
	ctx := WithPublishedImages(context.Background(), want)
	if _, err := client.Release(ctx, request); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := ParsePublishedImages([]byte(mustRead(t, observation)))
	if err != nil || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published images = %+v, err=%v, want %+v", got, err, want)
	}
	privatePath := strings.TrimSpace(mustRead(t, pathObservation))
	if privatePath == hostile || !filepath.IsAbs(privatePath) {
		t.Fatalf("published path = %q, want framework-owned absolute file", privatePath)
	}
	if _, err := os.Stat(privatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published evidence file survived provider call: %v", err)
	}
}

func TestClientNeverInheritsPublishedImagesFileFromAmbientEnvironment(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "ambient-observation")
	hostile := filepath.Join(tempDir, "hostile.json")
	t.Setenv(PublishedImagesFileEnv, hostile)
	response := testResolveResponse(t)
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(fakeProviderExecutable(t))
	client.TempDir = tempDir
	client.Env = nil
	t.Setenv(fakeProviderEnv, fakeAmbient)
	t.Setenv("FAKE_OBSERVATION", observation)
	t.Setenv("FAKE_RESPONSE", string(encodedResponse))
	if _, err := client.Resolve(context.Background(), testResolveRequest()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := strings.TrimSpace(mustRead(t, observation)); got != "" {
		t.Fatalf("ambient published images path reached provider: %q", got)
	}
}

func TestClientBoundsTimeoutAndNeverLeaksProviderStderr(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	client.Timeout = 20 * time.Millisecond
	client.Env = append(fakeEnv(t, testResolveResponse(t), observation), "FAKE_SLEEP=1")
	started := time.Now()
	_, err := client.Resolve(context.Background(), testResolveRequest())
	if !errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("provider timeout returned after %s, want a bounded subprocess wait", elapsed)
	}

	// Restore the ordinary fixture bound after the intentionally tiny timeout.
	// A full workspace gate can delay process startup beyond one second.
	client.Timeout = 10 * time.Second
	client.Env = append(fakeEnv(t, testResolveResponse(t), observation), "FAKE_FAILURE=1", "FAKE_SECRET=do-not-leak")
	_, err = client.Resolve(context.Background(), testResolveRequest())
	if err == nil || strings.Contains(err.Error(), "do-not-leak") || !strings.Contains(err.Error(), distribution.ProtocolName) {
		t.Fatalf("provider failure error was unsafe or omitted protocol identity: %v", err)
	}
}

// TestClientCarriesTheProviderFailureReasonRedacted pins the diagnosability
// a hosted run needs: a failed provider names its last stderr line, bounded,
// with every secret-shaped environment value replaced.
func TestClientCarriesTheProviderFailureReasonRedacted(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	client.Env = append(fakeEnv(t, testResolveResponse(t), observation),
		"FAKE_FAILURE=1", "FAKE_STDERR=CI publish handoff: HTTP 403 Forbidden for bearer do-notleak and do-not\033leak", "FAKE_SECRET=do-notleak")
	_, err := client.Resolve(context.Background(), testResolveRequest())
	if err == nil {
		t.Fatal("expected a provider failure")
	}
	message := err.Error()
	if !strings.Contains(message, "exit status 17") || !strings.Contains(message, "CI publish handoff: HTTP 403 Forbidden for bearer [redacted] and [redacted]") {
		t.Fatalf("provider failure = %q, want the exit status and the redacted reason line", message)
	}
	if strings.Contains(message, "do-notleak") {
		t.Fatalf("provider failure leaked a secret-shaped environment value: %q", message)
	}

	client.Env = append(fakeEnv(t, testResolveResponse(t), observation), "FAKE_FAILURE=1", "FAKE_STDERR="+strings.Repeat("x", 2000))
	_, err = client.Resolve(context.Background(), testResolveRequest())
	if err == nil || len(err.Error()) > 700 || !strings.HasSuffix(err.Error(), "…") {
		t.Fatalf("provider failure reason is not bounded: %d bytes", len(err.Error()))
	}
}

// A credential can travel inside a value whose KEY says nothing about it: a
// module proxy or registry URL carries its bearer in userinfo, and GOPROXY is
// named for what it addresses. The key-name rule cannot see that one, so the
// reason is redacted by URL shape as well.
func TestClientFailureReasonRedactsACredentialCarriedInsideAURL(t *testing.T) {
	const proxySecret = "np-9f86d081884c7d659a2feaa0c55ad015"
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	proxy := "https://putnami:" + proxySecret + "@go.example.com"
	client.Env = append(fakeEnv(t, testResolveResponse(t), observation),
		"FAKE_FAILURE=1",
		"GOPROXY="+proxy,
		"FAKE_STDERR=putnami: release-set members from "+proxy+" refused with 403")
	_, err := client.Resolve(context.Background(), testResolveRequest())
	if err == nil {
		t.Fatal("expected a provider failure")
	}
	message := err.Error()
	if strings.Contains(message, proxySecret) {
		t.Fatalf("provider failure leaked a URL-embedded credential: %q", message)
	}
	if !strings.Contains(message, "refused with 403") || !strings.Contains(message, redactedSecret) {
		t.Fatalf("provider failure = %q, want the redacted reason line", message)
	}
}

// The environment rules cover what THIS process handed the provider. A provider
// mints its own registry bearers, so a token it prints was never in that
// environment and only the bearer shape can remove it.
func TestClientFailureReasonRedactsABearerTheProviderMintedItself(t *testing.T) {
	const minted = "eyJhbGciOiJFUzI1NiJ9.minted-inside-the-provider"
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	client.Env = append(fakeEnv(t, testResolveResponse(t), observation),
		"FAKE_FAILURE=1",
		"FAKE_STDERR=putnami: PUT /v2/release-sets refused with 400 unknown channel canary@2 Authorization: Bearer "+minted)
	_, err := client.Resolve(context.Background(), testResolveRequest())
	if err == nil {
		t.Fatal("expected a provider failure")
	}
	message := err.Error()
	if strings.Contains(message, minted) {
		t.Fatalf("provider failure leaked a bearer the provider minted: %q", message)
	}
	if !strings.Contains(message, "400 unknown channel canary@2") || !strings.Contains(message, "Bearer "+redactedSecret) {
		t.Fatalf("provider failure = %q, want the redacted reason line", message)
	}
}

func TestClientPreservesCallerCancellation(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Resolve(ctx, testResolveRequest())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("cancellation error = %v, want context.Canceled without ErrProviderTimeout", err)
	}
}

func TestClientBoundsProviderStdoutAndStderr(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "observed")
	client := fakeProviderClient(t, tempDir, observation)
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			client.Env = append(fakeEnv(t, testResolveResponse(t), observation), "FAKE_OVERFLOW="+stream)
			if _, err := client.Resolve(context.Background(), testResolveRequest()); err == nil || !strings.Contains(err.Error(), stream+" exceeds") {
				t.Fatalf("overflow error = %v", err)
			}
		})
	}
}

func TestClientDistinguishesProviderAbsence(t *testing.T) {
	_, err := (&Client{}).Resolve(context.Background(), testResolveRequest())
	if !errors.Is(err, ErrProviderAbsent) {
		t.Fatalf("error = %v, want ErrProviderAbsent", err)
	}
}

func fakeProviderClient(t *testing.T, tempDir, observation string) *Client {
	t.Helper()
	// A full workspace gate can saturate process startup for more than one
	// second. Keep ordinary protocol fixtures comfortably above that contention;
	// the dedicated timeout test overrides this value with a 20 ms bound.
	return &Client{Executable: fakeProviderExecutable(t), TempDir: tempDir, Timeout: 10 * time.Second, Env: fakeEnv(t, testResolveResponse(t), observation)}
}

func fakeEnv(t *testing.T, response any, observation string) []string {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), fakeProviderEnv+"="+fakeProtocol, "FAKE_RESPONSE="+string(data), "FAKE_OBSERVATION="+observation)
}

func testResolveResponse(t *testing.T) distribution.ResolveResponse {
	t.Helper()
	set := *testPlan(t).Baseline().ReleaseSet
	ref, diagnostics := distribution.DeriveReleaseSetRef(&set)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	return distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 7, ReleaseSet: &set},
		},
	}
}

// testReleaseRequest releases the resolved set again on its own channel, with
// the neutral visibility chain a T4 publication sends.
func testReleaseRequest(t *testing.T) *distribution.ReleaseRequest {
	t.Helper()
	head := testResolveResponse(t).Heads["canary"]
	released := *head.ReleaseSet
	released.Members = append([]distribution.ReleaseSetMember(nil), released.Members...)
	released.Members[0].ArtifactDigest = testDigest('d')
	return &distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		ReleaseSet:      released,
		Channels: []distribution.ChannelRequest{
			{Name: "canary", Expected: &head.Ref, Visibility: distribution.VisibilityInternal},
		},
		Visibility: distribution.VisibilityChain{Repo: distribution.VisibilityInternal},
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClientMemberEvidenceIsPrivateOwnedAndRemoved(t *testing.T) {
	tempDir := t.TempDir()
	observation := filepath.Join(tempDir, "members.json")
	pathObservation := filepath.Join(tempDir, "members.path")
	request := testReleaseRequest(t)
	ref, diagnostics := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if hasDiagnosticErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	response, _ := json.Marshal(distribution.ReleaseResponse{ProtocolVersion: distribution.ProtocolVersion, Outcome: distribution.ReleaseOutcomeReleased, Current: map[string]*distribution.ChannelHead{"canary": {Ref: ref, Generation: 1}}})
	client := NewClient(fakeProviderExecutable(t))
	client.TempDir = tempDir
	client.Env = append(os.Environ(), fakeProviderEnv+"="+fakeMembers, "FAKE_RESPONSE="+string(response), "FAKE_OBSERVATION="+observation, "FAKE_PATH_OBSERVATION="+pathObservation, MemberEvidenceFileEnv+"=/hostile")
	members := []MemberEvidence{{Project: "apps/service", Ecosystem: "put", Coordinate: "workspace/config", Version: "v1", Digest: testDigest('a'), Publisher: "@putnami/cloud", Command: "publish", Step: "cloud-publish-config"}}
	ctx := WithMemberEvidence(t.Context(), members)
	members[0].Project = "mutated"
	if _, err := client.Release(ctx, request); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version int              `json:"version"`
		Members []MemberEvidence `json:"members"`
	}
	if err := json.Unmarshal([]byte(mustRead(t, observation)), &got); err != nil || got.Version != 1 || len(got.Members) != 1 || got.Members[0].Project != "apps/service" {
		t.Fatalf("evidence=%+v err=%v", got, err)
	}
	privatePath := mustRead(t, pathObservation)
	if !filepath.IsAbs(privatePath) || privatePath == "/hostile" {
		t.Fatalf("private path=%q", privatePath)
	}
	if _, err := os.Stat(privatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private file survived: %v", err)
	}
}
