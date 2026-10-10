package distributioncli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	diag "go.putnami.dev/protocol/diagnostic"
	protocol "go.putnami.dev/protocol/distribution"
)

// The provider node speaks one line or none: the parent CLI parses stdout as a
// single compact protocol response. Every release outcome emits exactly that
// one line, and a provider failure emits none and exits non-zero. Nothing rides
// after the exchange any more — the CI run reports only its own lifecycle and
// the release set emits its own channel fact.
func TestReleaseSetEmitsExactlyOneProtocolLinePerOutcome(t *testing.T) {
	for _, outcome := range []protocol.ReleaseOutcome{
		protocol.ReleaseOutcomeReleased,
		protocol.ReleaseOutcomeAlreadyCurrent,
		protocol.ReleaseOutcomeConflict,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			release, response := releaseExchange(t, outcome)
			requestPath := writeReleaseSetRequest(t, release, 0o600)
			providerBody := mustJSON(t, response)
			root, env, ioctx, bearer := releaseSetDirectBearerFixture(t)
			// A hosted run is the case the retired handoff hooked: it must change
			// nothing about what the provider writes.
			env["CI_RUN_ID"] = "run-1"
			requests := 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				requests++
				assertReleaseSetProviderRequest(t, request, protocol.ReleaseCommand, bearer)
				return releaseSetRawResponse(http.StatusOK, providerBody), nil
			})}
			var lines []string
			ioctx.Stdout = func(line string) { lines = append(lines, line) }

			if err := ReleaseSet(nil, []string{protocol.ReleaseCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx); err != nil {
				t.Fatalf("release-set %s = %v", outcome, err)
			}
			if len(lines) != 1 || lines[0] != string(providerBody) {
				t.Fatalf("stdout = %q, want exactly the one compact protocol response", lines)
			}
			if requests != 1 {
				t.Fatalf("provider requests = %d, want exactly the release exchange", requests)
			}
		})
	}
}

func TestReleaseSetEmitsNothingWhenTheProviderFails(t *testing.T) {
	release, _ := releaseExchange(t, protocol.ReleaseOutcomeReleased)
	requestPath := writeReleaseSetRequest(t, release, 0o600)
	root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
	env["CI_RUN_ID"] = "run-1"
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		return releaseSetRawResponse(http.StatusInternalServerError, []byte(`{"error":"provider down"}`)), nil
	})}
	lines := 0
	ioctx.Stdout = func(string) { lines++ }

	err := ReleaseSet(nil, []string{protocol.ReleaseCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx)
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("provider failure = %v (exit %d), want an API failure", err, clicore.ExitCode(err))
	}
	if lines != 0 {
		t.Fatalf("stdout lines = %d, want none on failure", lines)
	}
}

// TestReleaseSetNamesAMissingMemberAndExitsNonZero keeps a member_missing
// refusal readable: the error names the code and the provider's message, the
// exit is non-zero, and stdout carries no protocol line. A 409 that names any
// other code keeps the bare status line.
func TestReleaseSetNamesAMissingMemberAndExitsNonZero(t *testing.T) {
	const message = "release member put putnami/config@1.0.0 names artifact sha256:cc, which the Put registry does not store; publish the artifact before releasing it"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "member missing",
			body: `{"error":"` + message + `","code":"member_missing","message":"` + message + `"}`,
			want: "release-set release request failed: HTTP 409 Conflict: member_missing: " + message,
		},
		{
			name: "another conflict",
			body: `{"error":"immutable","code":"registry.release_set.channel_immutable","message":"immutable"}`,
			want: "release-set release request failed: HTTP 409 Conflict",
		},
		{
			name: "an older provider",
			body: `{"error":"conflict"}`,
			want: "release-set release request failed: HTTP 409 Conflict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, _ := releaseExchange(t, protocol.ReleaseOutcomeReleased)
			requestPath := writeReleaseSetRequest(t, release, 0o600)
			root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				return releaseSetRawResponse(http.StatusConflict, []byte(tc.body)), nil
			})}
			lines := 0
			ioctx.Stdout = func(string) { lines++ }

			err := ReleaseSet(nil, []string{protocol.ReleaseCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx)
			if err == nil || clicore.ExitCode(err) != clicore.ExitAPI {
				t.Fatalf("refusal = %v (exit %d), want an API failure", err, clicore.ExitCode(err))
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
			if lines != 0 {
				t.Fatalf("stdout lines = %d, want none on a refusal", lines)
			}
		})
	}
}

func TestParseReleaseSetInvocationRequiresExactV2Tokens(t *testing.T) {
	path := "/tmp/request.json"
	for _, operation := range []string{protocol.ResolveCommand, protocol.ReleaseCommand, protocol.ChannelSetCommand, protocol.ChannelStatusCommand} {
		t.Run(operation, func(t *testing.T) {
			gotOperation, gotPath, err := parseReleaseSetInvocation([]string{operation, protocol.RequestFileFlag, path, "--putnamiContext", "ctx.json"})
			if err != nil || gotOperation != operation || gotPath != path {
				t.Fatalf("parse = %q %q %v", gotOperation, gotPath, err)
			}
		})
	}
	for _, args := range [][]string{
		{"advance", protocol.RequestFileFlag, path},
		{"put", protocol.RequestFileFlag, path},
		{protocol.ReleaseCommand, protocol.RequestFileFlag + "=" + path},
		{protocol.ReleaseCommand, protocol.RequestFileFlag, path, "extra"},
		{protocol.ReleaseCommand, protocol.RequestFileFlag, "--other"},
		{protocol.ReleaseCommand, protocol.RequestFileFlag, path, "--putnamiContext"},
	} {
		if _, _, err := parseReleaseSetInvocation(args); err == nil {
			t.Fatalf("args %q accepted", args)
		}
	}
}

func TestReleaseSetOperationsPostOneStrictV2Exchange(t *testing.T) {
	baseSet, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	targetSet, targetRef := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	tests := []struct {
		operation string
		request   any
		response  any
		assert    func(*testing.T, []byte)
	}{
		{
			operation: protocol.ResolveCommand,
			request:   &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary", "stable"}},
			response: &protocol.ResolveResponse{ProtocolVersion: protocol.ProtocolVersion, Heads: map[string]*protocol.ChannelHead{
				"canary": {Ref: baseRef, Generation: 1, ReleaseSet: &baseSet}, "stable": nil,
			}},
			assert: func(t *testing.T, raw []byte) {
				request, diagnostics := protocol.ParseAndValidateResolveRequest(raw)
				if request == nil || diag.HasErrors(diagnostics) || len(request.Channels) != 2 {
					t.Fatalf("resolve request = %+v diagnostics=%+v", request, diagnostics)
				}
			},
		},
		{
			operation: protocol.ReleaseCommand,
			request:   releaseRequest(targetSet, &baseRef),
			response: &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeReleased, Current: map[string]*protocol.ChannelHead{
				"canary": {Ref: targetRef, Generation: 2},
			}},
			assert: func(t *testing.T, raw []byte) {
				request, diagnostics := protocol.ParseAndValidateReleaseRequest(raw)
				if request == nil || diag.HasErrors(diagnostics) || request.Mirrors["npm"].To != "https://registry.npmjs.org/putnami" {
					t.Fatalf("release request = %+v diagnostics=%+v", request, diagnostics)
				}
			},
		},
		{
			operation: protocol.ChannelSetCommand,
			request:   &protocol.ChannelSetRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channel: "stable", Expected: nil, From: protocol.ChannelSource{ReleaseID: targetRef.ID}},
			response: &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeReleased, Current: map[string]*protocol.ChannelHead{
				"stable": {Ref: targetRef, Generation: 1},
			}},
			assert: func(t *testing.T, raw []byte) {
				request, diagnostics := protocol.ParseAndValidateChannelSetRequest(raw)
				if request == nil || diag.HasErrors(diagnostics) || request.From.ReleaseID != targetRef.ID {
					t.Fatalf("channel-set request = %+v diagnostics=%+v", request, diagnostics)
				}
			},
		},
		{
			operation: protocol.ChannelStatusCommand,
			request:   &protocol.ChannelStatusRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channel: "canary"},
			response: &protocol.ChannelStatusResponse{ProtocolVersion: protocol.ProtocolVersion, Desired: &protocol.ChannelHead{Ref: targetRef, Generation: 2}, Observed: map[string]uint64{
				"npm": 2, "go": 1,
			}},
			assert: func(t *testing.T, raw []byte) {
				request, diagnostics := protocol.ParseAndValidateChannelStatusRequest(raw)
				if request == nil || diag.HasErrors(diagnostics) || request.Channel != "canary" {
					t.Fatalf("channel-status request = %+v diagnostics=%+v", request, diagnostics)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			root, env, ioctx, bearer := releaseSetDirectBearerFixture(t)
			requestPath := writeReleaseSetRequest(t, tc.request, 0o600)
			prepared, err := prepareReleaseSetRequest(tc.operation, mustJSON(t, tc.request))
			if err != nil {
				t.Fatal(err)
			}
			wantResponse := mustJSON(t, tc.response)
			calls := 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				assertReleaseSetProviderRequest(t, request, tc.operation, bearer)
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				// The publication broker compares inherited members byte for
				// byte, so the wire carries the protocol encoding, not the
				// generated client's alphabetical one.
				if !bytes.Equal(raw, prepared.body) {
					t.Fatalf("posted body = %s, want the validated protocol bytes %s", raw, prepared.body)
				}
				tc.assert(t, raw)
				return releaseSetRawResponse(http.StatusOK, wantResponse), nil
			})}
			var stdout []string
			ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
			if err := ReleaseSet(nil, []string{tc.operation, protocol.RequestFileFlag, requestPath}, root, env, ioctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(stdout) != 1 || stdout[0] != string(wantResponse) {
				t.Fatalf("calls/stdout = %d/%q, want one provider call and compact response %s", calls, stdout, wantResponse)
			}
		})
	}
}

func TestPrepareReleaseSetRequestRejectsMismatchedV2Exchanges(t *testing.T) {
	_, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	targetSet, targetRef := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	_, otherRef := releaseSetSnapshot(t, "other", "0.3.0-other", 'c')
	for _, tc := range []struct {
		name      string
		operation string
		request   any
		response  any
	}{
		{
			name: "resolve extra head", operation: protocol.ResolveCommand,
			request:  &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}},
			response: &protocol.ResolveResponse{ProtocolVersion: protocol.ProtocolVersion, Heads: map[string]*protocol.ChannelHead{"stable": nil}},
		},
		{
			name: "release answers different ref", operation: protocol.ReleaseCommand,
			request: releaseRequest(targetSet, &baseRef),
			response: &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeReleased, Current: map[string]*protocol.ChannelHead{
				"canary": {Ref: otherRef, Generation: 2},
			}},
		},
		{
			name: "channel set answers different channel", operation: protocol.ChannelSetCommand,
			request: &protocol.ChannelSetRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channel: "stable", Expected: nil, From: protocol.ChannelSource{ReleaseID: targetRef.ID}},
			response: &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeReleased, Current: map[string]*protocol.ChannelHead{
				"canary": {Ref: targetRef, Generation: 2},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := prepareReleaseSetRequest(tc.operation, mustJSON(t, tc.request))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepared.consume(mustJSON(t, tc.response)); err == nil || !strings.Contains(err.Error(), "exchange") {
				t.Fatalf("mismatched exchange error = %v", err)
			}
		})
	}
}

func TestReleaseSetRejectsHostileRequestFilesBeforeNetwork(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	validRequest := &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}}
	validPath := writeReleaseSetRequest(t, validRequest, 0o600)
	insecurePath := writeReleaseSetRequest(t, validRequest, 0o644)
	unknownFieldPath := writeReleaseSetRaw(t, []byte(`{"protocolVersion":2,"namespace":"putnami","channels":["canary"],"unknown":true}`), 0o600)
	oversizedPath := writeReleaseSetRaw(t, bytes.Repeat([]byte{' '}, protocol.MaxJSONBytes+1), 0o600)
	symlinkPath := filepath.Join(t.TempDir(), "request-link.json")
	if err := os.Symlink(validPath, symlinkPath); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "relative", args: []string{protocol.ResolveCommand, protocol.RequestFileFlag, filepath.Base(validPath)}},
		{name: "mode 0644", args: []string{protocol.ResolveCommand, protocol.RequestFileFlag, insecurePath}},
		{name: "symlink", args: []string{protocol.ResolveCommand, protocol.RequestFileFlag, symlinkPath}},
		{name: "oversized", args: []string{protocol.ResolveCommand, protocol.RequestFileFlag, oversizedPath}},
		{name: "unknown field", args: []string{protocol.ResolveCommand, protocol.RequestFileFlag, unknownFieldPath}},
		{name: "advance is retired", args: []string{"advance", protocol.RequestFileFlag, validPath}},
		{name: "put is retired", args: []string{"put", protocol.RequestFileFlag, validPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				t.Fatalf("unexpected HTTP request: %s", request.URL)
				return nil, nil
			})}
			var stdout []string
			ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
			if err := ReleaseSet(nil, tc.args, root, env, ioctx); err == nil {
				t.Fatal("ReleaseSet succeeded, want fail-closed error")
			}
			if calls != 0 || len(stdout) != 0 {
				t.Fatalf("calls/stdout = %d/%q, want no side effects", calls, stdout)
			}
		})
	}
}

func TestReleaseSetRejectsInvalidProviderResponseWithoutStdout(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "malformed", body: []byte(`{"protocolVersion":2`)},
		{name: "only an undeclared field", body: []byte(`{"future":"private-release-canary"}`)},
		{name: "oversized", body: bytes.Repeat([]byte{' '}, protocol.MaxJSONBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
			requestPath := writeReleaseSetRequest(t, &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}}, 0o600)
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
				return releaseSetRawResponse(http.StatusOK, tc.body), nil
			})}
			var stdout []string
			ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
			err := ReleaseSet(nil, []string{protocol.ResolveCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx)
			if err == nil {
				t.Fatal("ReleaseSet succeeded with an invalid provider response")
			}
			if len(stdout) != 0 || strings.Contains(err.Error(), "private-release-canary") {
				t.Fatalf("stdout = %q, err = %v", stdout, err)
			}
		})
	}
}

// TestReleaseSetDropsUndeclaredProviderFieldsAndNeverSurfacesThem proves a
// provider answer that is complete without its undeclared member resolves,
// and the member reaches neither stdout nor an error.
func TestReleaseSetDropsUndeclaredProviderFieldsAndNeverSurfacesThem(t *testing.T) {
	validSet, validRef := releaseSetSnapshot(t, "putnami", "0.1.0-valid", 'a')
	validResponse := mustJSON(t, &protocol.ResolveResponse{ProtocolVersion: protocol.ProtocolVersion, Heads: map[string]*protocol.ChannelHead{
		"canary": {Ref: validRef, Generation: 1, ReleaseSet: &validSet},
	}})
	body := append([]byte(nil), validResponse[:len(validResponse)-1]...)
	body = append(body, []byte(`,"future":"private-release-canary"}`)...)
	root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
	requestPath := writeReleaseSetRequest(t, &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}}, 0o600)
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(*http.Request) (*http.Response, error) {
		return releaseSetRawResponse(http.StatusOK, body), nil
	})}
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := ReleaseSet(nil, []string{protocol.ResolveCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx); err != nil {
		t.Fatalf("complete provider answer with an undeclared member refused: %v", err)
	}
	if len(stdout) == 0 {
		t.Fatal("ReleaseSet printed nothing")
	}
	for _, line := range stdout {
		if strings.Contains(line, "future") || strings.Contains(line, "private-release-canary") {
			t.Fatalf("undeclared member surfaced on stdout: %q", line)
		}
	}
}

func TestReleaseSetFailsClosedOnProviderNetworkStatusAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider func(*http.Request) (*http.Response, error)
	}{
		{name: "network", provider: func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") }},
		{name: "status", provider: func(*http.Request) (*http.Response, error) {
			return releaseSetRawResponse(http.StatusServiceUnavailable, []byte(`{"error":"unavailable"}`)), nil
		}},
		{name: "cross-authority redirect", provider: func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Status: http.StatusText(http.StatusTemporaryRedirect), Header: http.Header{"Location": []string{"https://attacker.example/steal"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
			requestPath := writeReleaseSetRequest(t, &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}}, 0o600)
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(tc.provider)}
			var stdout []string
			ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
			if err := ReleaseSet(nil, []string{protocol.ResolveCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx); err == nil {
				t.Fatal("ReleaseSet succeeded, want provider failure")
			}
			if len(stdout) != 0 {
				t.Fatalf("stdout = %q, want none", stdout)
			}
		})
	}
}

func TestReleaseSetCredentialUsesExactConfiguredMachineBrokerWithoutHumanSession(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, map[string]string{"PUTNAMI_HOME": home, "PUTNAMI_REGISTRY_PUT_URL": "https://broker.test/put"})
	bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": "put", "sub": "ci-publisher"})
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryPut, Host: "broker.test", URL: "https://broker.test/put"}},
		PutAuth: map[string]string{"broker.test": bearer, "unrelated.test": "must-not-be-used"},
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := resolveReleaseSetProviderCredential(nil, t.TempDir(), env, clicore.IO{
		Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
			t.Fatalf("direct broker bearer unexpectedly made a request: %s %s", request.Method, request.URL)
			return nil, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != bearer || credential.Endpoint.Host != "broker.test" || credential.Endpoint.URL != "https://broker.test/put" {
		t.Fatalf("machine credential = %+v, want exact configured host and bearer", credential)
	}
}

func TestReleaseSetCredentialMachineExchangeFailureNeverFallsThroughToHuman(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, map[string]string{
		"PUTNAMI_HOME":             home,
		"PUTNAMI_REGISTRY_PUT_URL": "https://broker.test/put",
		"PUTNAMI_AUTH_URL":         "https://broker.test",
	})
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryPut, Host: "broker.test", URL: "https://broker.test/put"}},
		PutAuth: map[string]string{"broker.test": "pkt_sealed_broker"},
	}); err != nil {
		t.Fatal(err)
	}
	tokenCalls := 0
	_, err := resolveReleaseSetProviderCredential(nil, t.TempDir(), env, clicore.IO{
		Client: &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
			switch {
			case request.Method == http.MethodGet && request.URL.Path == "/.well-known/openid-configuration":
				return workflowJSONResponse(http.StatusOK, map[string]any{"issuer": "https://broker.test", "token_endpoint": "https://broker.test/token"}), nil
			case request.Method == http.MethodPost && request.URL.Path == "/token":
				tokenCalls++
				return workflowJSONResponse(http.StatusUnauthorized, map[string]any{"error": "invalid_grant", "error_description": "sealed broker rejected exchange"}), nil
			case request.URL.Path == "/apikeys":
				t.Fatal("machine credential failure fell through to human key mint")
			}
			t.Fatalf("unexpected machine credential request: %s %s", request.Method, request.URL)
			return nil, nil
		})},
	})
	if err == nil || !strings.Contains(err.Error(), "sealed broker rejected exchange") {
		t.Fatalf("machine exchange error = %v", err)
	}
	if tokenCalls != 1 {
		t.Fatalf("machine token exchanges = %d, want one and no human fallback", tokenCalls)
	}
}

func TestReleaseSetCredentialHumanMintsNativePutMarkerBearer(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	env["PUTNAMI_REGISTRY_PUT_URL"] = "https://put.putnami.dev"
	env["PUTNAMI_AUTH_URL"] = "https://auth.test"
	bearer := workflowJWT(map[string]any{
		"aud": "distribution", "scope": "put", "sub": "user-admin",
		"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
	})
	keyCalls, tokenCalls, revokeCalls := 0, 0, 0
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if request.Body != nil {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/.well-known/openid-configuration":
			return workflowJSONResponse(http.StatusOK, map[string]any{"issuer": "https://auth.test", "token_endpoint": "https://auth.test/token"}), nil
		case request.Method == http.MethodPost && request.URL.Path == "/apikeys":
			keyCalls++
			if body["allowed_scopes"] != "put" || body["workspace_id"] != "ws-consumer" {
				t.Fatalf("human key request = %+v, want bare Put marker for linked workspace", body)
			}
			return workflowJSONResponse(http.StatusCreated, workflowAPIKeyCreated("pkt_ephemeral")), nil
		case request.Method == http.MethodPost && request.URL.Path == "/token":
			tokenCalls++
			if body["scope"] != "put" || body["client_id"] != "distribution" {
				t.Fatalf("human token request = %+v, want native Put marker", body)
			}
			return workflowJSONResponse(http.StatusOK, map[string]any{"access_token": bearer, "token_type": "Bearer", "expires_in": 300}), nil
		case request.Method == http.MethodDelete && request.URL.Path == "/apikeys/key-1":
			revokeCalls++
			return workflowJSONResponse(http.StatusOK, workflowAPIKeyRevoked), nil
		}
		t.Fatalf("unexpected human credential request: %s %s", request.Method, request.URL)
		return nil, nil
	})}
	credential, err := resolveReleaseSetProviderCredential(nil, root, env, ioctx)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != bearer || credential.Endpoint.Host != "put.putnami.dev" {
		t.Fatalf("human credential = %+v", credential)
	}
	if keyCalls != 1 || tokenCalls != 1 || revokeCalls != 1 {
		t.Fatalf("human mint calls key/token/revoke = %d/%d/%d", keyCalls, tokenCalls, revokeCalls)
	}
}

// storePutLease records lease as the Put credential for put.putnami.dev, the
// way `cloud token --for put --materialize` leaves it behind.
func storePutLease(t *testing.T, env map[string]string, lease string) {
	t.Helper()
	env["PUTNAMI_REGISTRY_PUT_URL"] = "https://put.putnami.dev"
	env["PUTNAMI_AUTH_URL"] = "https://auth.test"
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryPut, Host: "put.putnami.dev", URL: "https://put.putnami.dev"}},
		PutAuth: map[string]string{"put.putnami.dev": lease},
	}); err != nil {
		t.Fatal(err)
	}
}

// A materialized Put lease that is no longer fresh can only earn a 401 from
// put-server. The provider credential skips it, says so on stderr, and mints a
// fresh bearer through the user path; the stored entry is left in place.
func TestReleaseSetCredentialStaleStoredLeaseMintsAFreshBearer(t *testing.T) {
	for _, test := range []struct {
		name string
		exp  time.Duration
	}{
		{name: "expired an hour ago", exp: -time.Hour},
		{name: "expires inside the freshness margin", exp: 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, env, ioctx := distributionWorkflowFixture(t)
			now := ioctx.Now()
			stale := workflowJWT(map[string]any{
				"aud": "distribution", "scope": "put", "sub": "user-admin", "exp": now.Add(test.exp).Unix(),
			})
			storePutLease(t, env, stale)
			fresh := workflowJWT(map[string]any{
				"aud": "distribution", "scope": "put", "sub": "user-admin", "exp": now.Add(5 * time.Minute).Unix(),
				"scope_ref": map[string]any{"workspace_id": "ws-consumer"},
			})
			var stderr []string
			ioctx.Stderr = func(line string) { stderr = append(stderr, line) }
			keyCalls, tokenCalls, revokeCalls := 0, 0, 0
			ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.Contains(request.Header.Get("Authorization"), stale) {
					t.Fatalf("stale lease was sent: %s %s", request.Method, request.URL)
				}
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/.well-known/openid-configuration":
					return workflowJSONResponse(http.StatusOK, map[string]any{"issuer": "https://auth.test", "token_endpoint": "https://auth.test/token"}), nil
				case request.Method == http.MethodPost && request.URL.Path == "/apikeys":
					keyCalls++
					return workflowJSONResponse(http.StatusCreated, workflowAPIKeyCreated("pkt_ephemeral")), nil
				case request.Method == http.MethodPost && request.URL.Path == "/token":
					tokenCalls++
					return workflowJSONResponse(http.StatusOK, map[string]any{"access_token": fresh, "token_type": "Bearer", "expires_in": 300}), nil
				case request.Method == http.MethodDelete && request.URL.Path == "/apikeys/key-1":
					revokeCalls++
					return workflowJSONResponse(http.StatusOK, workflowAPIKeyRevoked), nil
				}
				t.Fatalf("unexpected credential request: %s %s", request.Method, request.URL)
				return nil, nil
			})}

			credential, err := resolveReleaseSetProviderCredential(nil, root, env, ioctx)
			if err != nil {
				t.Fatal(err)
			}
			if credential.Token != fresh || credential.Endpoint.Host != "put.putnami.dev" {
				t.Fatalf("credential = %+v, want the freshly minted bearer", credential)
			}
			if keyCalls != 1 || tokenCalls != 1 || revokeCalls != 1 {
				t.Fatalf("mint calls key/token/revoke = %d/%d/%d, want 1/1/1", keyCalls, tokenCalls, revokeCalls)
			}
			wantExp := now.Add(test.exp).UTC().Format(time.RFC3339)
			if len(stderr) != 1 || !strings.Contains(stderr[0], "put.putnami.dev") || !strings.Contains(stderr[0], wantExp) || strings.Contains(stderr[0], stale) {
				t.Fatalf("stderr = %q, want one notice naming the host and the expiry %s, without the lease", stderr, wantExp)
			}
			state, err := readRegistriesState(env)
			if err != nil {
				t.Fatal(err)
			}
			if state.PutAuth["put.putnami.dev"] != stale {
				t.Fatalf("stored lease = %q, want it left in place", state.PutAuth["put.putnami.dev"])
			}
		})
	}
}

// A materialized Put lease that is still fresh is preferred: it is sent as it
// is, with no mint and no request at all.
func TestReleaseSetCredentialPrefersAFreshStoredLease(t *testing.T) {
	root, env, ioctx := distributionWorkflowFixture(t)
	lease := workflowJWT(map[string]any{
		"aud": "distribution", "scope": "put", "sub": "user-admin", "exp": ioctx.Now().Add(time.Hour).Unix(),
	})
	storePutLease(t, env, lease)
	var stderr []string
	ioctx.Stderr = func(line string) { stderr = append(stderr, line) }
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("fresh stored lease unexpectedly made a request: %s %s", request.Method, request.URL)
		return nil, nil
	})}

	credential, err := resolveReleaseSetProviderCredential(nil, root, env, ioctx)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != lease || credential.Endpoint.Host != "put.putnami.dev" {
		t.Fatalf("credential = %+v, want the stored lease", credential)
	}
	if len(stderr) != 0 {
		t.Fatalf("stderr = %q, want no notice", stderr)
	}
}

func TestStalePutLease(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	lease := func(claims map[string]any) string {
		claims["aud"] = "distribution"
		return workflowJWT(claims)
	}
	for _, test := range []struct {
		name      string
		token     string
		wantStale bool
		wantExp   time.Time
	}{
		{name: "empty", token: ""},
		{name: "opaque key", token: "pkt_sealed_broker"},
		{name: "lease without exp", token: lease(map[string]any{"sub": "ci-publisher"})},
		{name: "lease with a non-numeric exp", token: lease(map[string]any{"exp": "soon"})},
		{name: "lease with an exp past int64", token: lease(map[string]any{"exp": 1e30})},
		{name: "fresh lease", token: lease(map[string]any{"exp": now.Add(time.Hour).Unix()})},
		{name: "expired lease", token: lease(map[string]any{"exp": now.Add(-time.Minute).Unix()}), wantStale: true, wantExp: now.Add(-time.Minute)},
		{name: "lease inside the margin", token: lease(map[string]any{"exp": now.Add(10 * time.Second).Unix()}), wantStale: true, wantExp: now.Add(10 * time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			exp, stale := stalePutLease(test.token, now)
			if stale != test.wantStale || (stale && !exp.Equal(test.wantExp)) {
				t.Fatalf("stalePutLease = (%v, %v), want (%v, %v)", exp, stale, test.wantExp, test.wantStale)
			}
		})
	}
}

func releaseSetSnapshot(t *testing.T, namespace, version string, digestByte byte) (protocol.ReleaseSet, protocol.ReleaseSetRef) {
	t.Helper()
	releaseSet := protocol.ReleaseSet{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       namespace,
		Members: []protocol.ReleaseSetMember{{
			Ecosystem:            protocol.Ecosystem("npm"),
			Coordinate:           "@putnami/core",
			Version:              version,
			ArtifactDigest:       "sha256:" + strings.Repeat(string(digestByte), 64),
			Dependencies:         []protocol.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("2", 64),
		}},
	}
	ref, diagnostics := protocol.DeriveReleaseSetRef(&releaseSet)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("derive release-set fixture ref: %+v", diagnostics)
	}
	return releaseSet, ref
}

func releaseRequest(releaseSet protocol.ReleaseSet, expected *protocol.ReleaseSetRef) *protocol.ReleaseRequest {
	return &protocol.ReleaseRequest{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       releaseSet.Namespace,
		ReleaseSet:      releaseSet,
		Channels: []protocol.ChannelRequest{{
			Name: "canary", Expected: expected, Visibility: protocol.VisibilityInternal,
		}},
		Visibility: protocol.VisibilityChain{Repo: protocol.VisibilityInternal, Versions: protocol.VersionVisibility{}, Set: nil},
		Mirrors:    map[string]protocol.MirrorTarget{"npm": {To: "https://registry.npmjs.org/putnami"}},
	}
}

func releaseExchange(t *testing.T, outcome protocol.ReleaseOutcome) (*protocol.ReleaseRequest, *protocol.ReleaseResponse) {
	t.Helper()
	_, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	targetSet, targetRef := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	request := releaseRequest(targetSet, &baseRef)
	current := &protocol.ChannelHead{Ref: targetRef, Generation: 2}
	switch outcome {
	case protocol.ReleaseOutcomeAlreadyCurrent:
		request.Channels[0].Expected = &targetRef
	case protocol.ReleaseOutcomeConflict:
		request.Channels[0].Expected = nil
		current = &protocol.ChannelHead{Ref: baseRef, Generation: 1}
	}
	return request, &protocol.ReleaseResponse{
		ProtocolVersion: protocol.ProtocolVersion,
		Outcome:         outcome,
		Current:         map[string]*protocol.ChannelHead{"canary": current},
	}
}

func releaseSetDirectBearerFixture(t *testing.T) (string, map[string]string, clicore.IO, string) {
	t.Helper()
	root, env, ioctx := distributionWorkflowFixture(t)
	endpoint := "https://put.putnami.dev"
	bearer := workflowJWT(map[string]any{"aud": "distribution", "scope": "put", "sub": "user-admin"})
	env["PUTNAMI_REGISTRY_PUT_URL"] = endpoint
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryPut, Host: "put.putnami.dev", URL: endpoint}},
		PutAuth: map[string]string{"put.putnami.dev": bearer},
	}); err != nil {
		t.Fatal(err)
	}
	return root, env, ioctx, bearer
}

func assertReleaseSetProviderRequest(t *testing.T, request *http.Request, operation, bearer string) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "put.putnami.dev" || request.URL.Path != releaseSetProviderPath+operation {
		t.Fatalf("provider request = %s %s", request.Method, request.URL)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+bearer {
		t.Fatalf("provider Authorization = %q", got)
	}
	// put-server's generated client sends the JSON body's Content-Type; the
	// answer's media type is checked against the contract on receipt.
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("provider content headers = %v", request.Header)
	}
}

func writeReleaseSetRequest(t *testing.T, request any, mode os.FileMode) string {
	t.Helper()
	return writeReleaseSetRaw(t, mustJSON(t, request), mode)
}

func writeReleaseSetRaw(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func releaseSetRawResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func TestReleaseSetJoinsAnEndpointThatNamesThePutMountOnce(t *testing.T) {
	root, env, ioctx, bearer := releaseSetDirectBearerFixture(t)
	endpoint := "https://put.putnami.dev/put/"
	env["PUTNAMI_REGISTRY_PUT_URL"] = endpoint
	if err := WriteRegistriesState(env, &RegistriesState{
		Version: 1,
		Keys:    []KeyRef{{Registry: RegistryPut, Host: "put.putnami.dev", URL: endpoint}},
		PutAuth: map[string]string{"put.putnami.dev": bearer},
	}); err != nil {
		t.Fatal(err)
	}
	baseSet, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	request := &protocol.ResolveRequest{ProtocolVersion: protocol.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}}
	response := mustJSON(t, &protocol.ResolveResponse{ProtocolVersion: protocol.ProtocolVersion, Heads: map[string]*protocol.ChannelHead{
		"canary": {Ref: baseRef, Generation: 1, ReleaseSet: &baseSet},
	}})
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		assertReleaseSetProviderRequest(t, request, protocol.ResolveCommand, bearer)
		return releaseSetRawResponse(http.StatusOK, response), nil
	})}
	ioctx.Stdout = func(string) {}
	requestPath := writeReleaseSetRequest(t, request, 0o600)
	if err := ReleaseSet(nil, []string{protocol.ResolveCommand, protocol.RequestFileFlag, requestPath}, root, env, ioctx); err != nil {
		t.Fatal(err)
	}
}
