package deliverycli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registry "go.putnami.dev/protocol/registry"
)

const (
	testRunCredential = "run-credential-7f3a9c"
	testInstallBearer = "install-bearer-51d0e2"
)

// lockedBuffer is a stderr sink the server and the test read concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// credentialHarness drives one credential server over pipes, the way the
// engine drives the provider process.
type credentialHarness struct {
	t      *testing.T
	stdin  *io.PipeWriter
	lines  chan []byte
	stderr *lockedBuffer
	done   chan error
	server *credentialServer
	// negotiated is what the engine negotiated; next parses answers under it.
	negotiated []string
}

func startCredentialServer(t *testing.T, ingestURL string, local ReadCredentialSource, deny func() error) *credentialHarness {
	t.Helper()
	return runCredentialServer(t, newTestCredentialServer(ingestURL, local, deny))
}

// newTestCredentialServer builds a server whose Delivery caller retries as in
// production, without the wait between attempts.
func newTestCredentialServer(ingestURL string, local ReadCredentialSource, deny func() error) *credentialServer {
	caller := newCapabilityCaller(ingestURL)
	caller.backoff = make([]time.Duration, len(capabilityRetryBackoff))
	if deny == nil {
		deny = func() error { return nil }
	}
	return newCredentialServer(caller, local, deny)
}

// runCredentialServer serves server over pipes until the test ends.
func runCredentialServer(t *testing.T, server *credentialServer) *credentialHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &credentialHarness{
		t:      t,
		stdin:  inW,
		lines:  make(chan []byte, 16),
		stderr: &lockedBuffer{},
		done:   make(chan error, 1),
		server: server,
	}
	go func() {
		err := server.serve(inR, outW, h.stderr)
		_ = outW.Close()
		h.done <- err
	}()
	go func() {
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxPublicationLineBytes+1)
		for scanner.Scan() {
			h.lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(h.lines)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("the credential server did not exit after stdin closed")
		}
	})
	return h
}

func (h *credentialHarness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		h.t.Fatalf("write request: %v", err)
	}
}

// next returns the next answer, checked by the protocol's strict response
// parser for op: what the engine would accept.
func (h *credentialHarness) next(op registry.CredentialOp) *registry.CredentialResponse {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.t.Fatal("the credential server closed stdout before answering")
		}
		response, err := registry.ParseNegotiatedCredentialResponse(line, op, h.negotiated)
		if err != nil {
			h.t.Fatalf("answer %s does not follow the protocol: %v", line, err)
		}
		return response
	case <-time.After(10 * time.Second):
		h.t.Fatal("no answer within 10s")
	}
	return nil
}

func (h *credentialHarness) noAnswer(wait time.Duration) {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if ok {
			h.t.Fatalf("unexpected answer %s", line)
		}
	case <-time.After(wait):
	}
}

func (h *credentialHarness) exit() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("the credential server did not exit")
	}
	return nil
}

func initializeLine(id int, runCredential string) string {
	payload := map[string]any{"protocolVersion": 1, "capabilities": []string{registry.CapabilityCredentialV1}}
	if runCredential != "" {
		payload["runCredential"] = runCredential
	}
	encoded, _ := json.Marshal(map[string]any{"protocolVersion": 1, "id": id, "op": "initialize", "payload": payload})
	return string(encoded)
}

func credentialLine(id int, purpose string) string {
	return fmt.Sprintf(`{"protocolVersion":1,"id":%d,"op":"credential","payload":{"purpose":%q}}`, id, purpose)
}

func mustCredential(t *testing.T, response *registry.CredentialResponse) *registry.Credential {
	t.Helper()
	if !response.OK {
		t.Fatalf("credential refused: %+v", response.Error)
	}
	result, err := registry.ParseCredentialResult(response.Payload)
	if err != nil {
		t.Fatalf("credential result: %v", err)
	}
	return result.Credential
}

func mustRefusal(t *testing.T, response *registry.CredentialResponse, code string) *registry.CredentialRefusal {
	t.Helper()
	if response.OK || response.Error == nil {
		t.Fatalf("answer = %+v, want refusal %s", response, code)
	}
	if response.Error.Code != code {
		t.Fatalf("refusal code = %q (%s), want %q", response.Error.Code, response.Error.Message, code)
	}
	return response.Error
}

// deliveryInstall fakes Delivery's install capability route. Every request is
// checked against the contract before handler answers it.
func deliveryInstall(t *testing.T, calls *atomic.Int64, handler func(w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/delivery/records/capabilities/install" {
			t.Errorf("request = %s %s, want POST /v1/delivery/records/capabilities/install", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testRunCredential {
			t.Errorf("authorization = %q, want the run credential", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body = %q, want {}", body)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/delivery/records"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestCredentialProviderInitializeWithoutRunCredentialEchoesTheCapability(t *testing.T) {
	h := startCredentialServer(t, "", nil, nil)
	h.send(initializeLine(1, ""))
	response := h.next(registry.CredentialOpInitialize)
	if !response.OK || response.ID != 1 {
		t.Fatalf("initialize = %+v", response)
	}
	result, err := registry.ParseCredentialInitializeResult(response.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != registry.CredentialProtocolVersion || result.ProviderName != providerName ||
		!reflect.DeepEqual(result.Capabilities, []string{registry.CapabilityCredentialV1}) {
		t.Fatalf("initialize result = %+v", result)
	}

	h.send(`{"protocolVersion":1,"id":2,"op":"shutdown"}`)
	if response := h.next(registry.CredentialOpShutdown); !response.OK || response.ID != 2 || len(response.Payload) != 0 {
		t.Fatalf("shutdown = %+v", response)
	}
	if err := h.exit(); err != nil {
		t.Fatalf("serve after shutdown = %v, want nil", err)
	}
}

func TestCredentialProviderDeniesInspectionBeforeReadingTheFirstLine(t *testing.T) {
	var denied atomic.Bool
	firstRead := make(chan bool, 1)
	reader := readFunc(func(p []byte) (int, error) {
		select {
		case firstRead <- denied.Load():
		default:
		}
		return 0, io.EOF
	})
	server := newCredentialServer(newCapabilityCaller(""), nil, func() error {
		denied.Store(true)
		return nil
	})
	if err := server.serve(reader, io.Discard, io.Discard); err != nil {
		t.Fatalf("serve = %v", err)
	}
	if !<-firstRead {
		t.Fatal("the server read stdin before it denied inspection")
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func TestCredentialProviderRefusesAnInvalidRunCredentialWithoutQuotingIt(t *testing.T) {
	h := startCredentialServer(t, "", nil, nil)
	invalid := "has whitespace " + testRunCredential
	h.send(initializeLine(1, invalid))
	refusal := mustRefusal(t, h.next(registry.CredentialOpInitialize), refusalInvalidRunCredential)
	if strings.Contains(refusal.Message, testRunCredential) {
		t.Fatalf("refusal quotes the credential: %q", refusal.Message)
	}
	// The session stays uninitialized.
	h.send(credentialLine(2, registry.PurposeRead))
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalNotInitialized)
	if strings.Contains(h.stderr.String(), testRunCredential) {
		t.Fatalf("stderr quotes the credential: %s", h.stderr.String())
	}
}

func TestCredentialProviderRefusesARunCredentialWhenInspectionCannotBeDenied(t *testing.T) {
	h := startCredentialServer(t, "", nil, func() error { return errors.New("prctl refused") })
	h.send(initializeLine(1, testRunCredential))
	mustRefusal(t, h.next(registry.CredentialOpInitialize), refusalInspectionGuard)
	// Without a credential there is nothing to protect: the session opens.
	h.send(initializeLine(2, ""))
	if response := h.next(registry.CredentialOpInitialize); !response.OK {
		t.Fatalf("initialize without a run credential = %+v", response)
	}
	if !strings.Contains(h.stderr.String(), "cannot deny process inspection") {
		t.Fatalf("stderr = %q, want the guard failure", h.stderr.String())
	}
}

func TestCredentialProviderHostedReadAnswersDeliveryInstallCredential(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"protocolVersion": 1,
			"bearer":          testInstallBearer,
			"expiresAt":       "2026-10-02T05:30:00.250+00:00",
			"hosts":           []string{"put.putnami.dev", "GO.putnami.dev", "npm.putnami.dev", "go.putnami.dev"},
			"addedLater":      "a field this provider does not know",
		})
	})
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	if response := h.next(registry.CredentialOpInitialize); !response.OK {
		t.Fatalf("initialize = %+v", response)
	}
	h.send(credentialLine(2, registry.PurposeRead))
	credential := mustCredential(t, h.next(registry.CredentialOpCredential))
	want := &registry.Credential{
		Bearer:    testInstallBearer,
		ExpiresAt: "2026-10-02T05:30:00Z",
		Hosts:     []string{"go.putnami.dev", "npm.putnami.dev", "put.putnami.dev"},
	}
	if !reflect.DeepEqual(credential, want) {
		t.Fatalf("credential = %+v, want %+v", credential, want)
	}
	if calls.Load() != 1 {
		t.Fatalf("Delivery calls = %d, want 1", calls.Load())
	}
	assertNoSecret(t, h.stderr.String())
}

func TestCredentialProviderHostedReadAnswersAbsenceOn204(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	if credential := mustCredential(t, h.next(registry.CredentialOpCredential)); credential != nil {
		t.Fatalf("credential = %+v, want absence", credential)
	}
}

func TestCredentialProviderHostedReadRefusals(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	for _, test := range []struct {
		name     string
		status   int
		body     any
		wantCode string
		check    func(t *testing.T, message string)
	}{
		{
			name:     "code passes through, message is redacted and loses control characters",
			status:   http.StatusForbidden,
			body:     map[string]any{"error": "forbidden", "code": "install_not_permitted", "message": "run " + testRunCredential + "\nis not allowed"},
			wantCode: "install_not_permitted",
			check: func(t *testing.T, message string) {
				if message != "run <redacted> is not allowed" {
					t.Fatalf("message = %q", message)
				}
			},
		},
		{
			name:     "missing code becomes install_refused and the message is cut to 512 bytes",
			status:   http.StatusUnprocessableEntity,
			body:     map[string]any{"error": "unprocessable", "message": long},
			wantCode: refusalInstallRefused,
			check: func(t *testing.T, message string) {
				if len(message) > registry.MaxRefusalMessageBytes || !strings.HasPrefix(long, message) || len(message) < 500 {
					t.Fatalf("message has %d bytes, want the first whole runes within 512", len(message))
				}
			},
		},
		{
			name:     "invalid code becomes install_refused",
			status:   http.StatusUnauthorized,
			body:     map[string]any{"error": "unauthorized", "code": "Not-A-Code", "message": "the run is over"},
			wantCode: refusalInstallRefused,
			check: func(t *testing.T, message string) {
				if message != "the run is over" {
					t.Fatalf("message = %q", message)
				}
			},
		},
		{
			name:     "a body that is not JSON keeps a status message",
			status:   http.StatusNotFound,
			body:     "404 page not found",
			wantCode: refusalInstallRefused,
			check: func(t *testing.T, message string) {
				if message != "Delivery refused the install capability (HTTP 404)" {
					t.Fatalf("message = %q", message)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				if text, ok := test.body.(string); ok {
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, text)
					return
				}
				writeJSON(w, test.status, test.body)
			})
			h := startCredentialServer(t, ingest, nil, nil)
			h.send(initializeLine(1, testRunCredential))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			refusal := mustRefusal(t, h.next(registry.CredentialOpCredential), test.wantCode)
			test.check(t, refusal.Message)
			if calls.Load() != 1 {
				t.Fatalf("Delivery calls = %d, want 1: a refusal is final", calls.Load())
			}
			assertNoSecret(t, h.stderr.String())
		})
	}
}

// TestCredentialProviderHostedReadPassesEveryCodedRefusalThrough pins that a
// 4xx refusal is final and keeps Delivery's code, including codes this
// provider does not know: the provider holds no list of accepted codes.
func TestCredentialProviderHostedReadPassesEveryCodedRefusalThrough(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, "install_capability_denied"},
		{http.StatusConflict, "install_capability_conflict"},
		{http.StatusUnprocessableEntity, "install_capability_scope_too_large"},
		{http.StatusConflict, "run_terminal"},
		{http.StatusUnauthorized, "run_credential_invalid"},
		{http.StatusBadRequest, "invalid_request"},
		{http.StatusForbidden, "a_code_added_later"},
	} {
		t.Run(fmt.Sprintf("%d %s", test.status, test.code), func(t *testing.T) {
			var calls atomic.Int64
			ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, test.status, map[string]any{"error": "refused", "message": "refused", "code": test.code})
			})
			h := startCredentialServer(t, ingest, nil, nil)
			h.send(initializeLine(1, testRunCredential))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			mustRefusal(t, h.next(registry.CredentialOpCredential), test.code)
			if calls.Load() != 1 {
				t.Fatalf("Delivery calls = %d, want 1: a refusal is never retried", calls.Load())
			}
		})
	}
}

func TestCapabilityExchangeEndsInsideTheEngineOpTimeout(t *testing.T) {
	// The engine waits 30 s for one credential or authenticate answer.
	const engineOpTimeout = 30 * time.Second
	if got := len(capabilityRetryBackoff); got != 1 {
		t.Fatalf("retries = %d, want 1 (two attempts)", got)
	}
	if capabilityAttemptTimeout != 12*time.Second || capabilityRetryBackoff[0] != 500*time.Millisecond {
		t.Fatalf("attempt timeout %s, backoff %v; want 12s and 0.5s", capabilityAttemptTimeout, capabilityRetryBackoff)
	}
	worst := time.Duration(len(capabilityRetryBackoff)+1) * capabilityAttemptTimeout
	for _, wait := range capabilityRetryBackoff {
		worst += wait
	}
	if worst >= engineOpTimeout {
		t.Fatalf("worst case %s does not end inside the engine's %s", worst, engineOpTimeout)
	}
	if caller := newCapabilityCaller("https://delivery.example"); caller.client.Timeout != capabilityAttemptTimeout {
		t.Fatalf("client timeout = %s, want the attempt timeout", caller.client.Timeout)
	}
}

func TestCredentialProviderHostedReadRetriesUnavailableDeliveryThenRefuses(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusRequestTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, status, map[string]any{"error": "busy", "code": "busy", "message": "try later"})
			})
			h := startCredentialServer(t, ingest, nil, nil)
			h.send(initializeLine(1, testRunCredential))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			refusal := mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInstallUnavailable)
			if !strings.Contains(refusal.Message, fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("message = %q, want the last status", refusal.Message)
			}
			if calls.Load() != 2 {
				t.Fatalf("Delivery calls = %d, want 2 (one try and one retry)", calls.Load())
			}
		})
	}
}

func TestCredentialProviderHostedReadRecoversOnRetry(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Load() == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"protocolVersion": 1, "bearer": testInstallBearer, "expiresAt": "2026-10-02T05:30:00Z", "hosts": []string{"go.putnami.dev"},
		})
	})
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	if credential := mustCredential(t, h.next(registry.CredentialOpCredential)); credential == nil || credential.Bearer != testInstallBearer {
		t.Fatalf("credential = %+v", credential)
	}
	if calls.Load() != 2 {
		t.Fatalf("Delivery calls = %d, want 2", calls.Load())
	}
}

func TestCredentialProviderHostedReadRetriesATransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ingest := srv.URL + recordsIngestPath
	srv.Close() // nothing listens: every attempt fails to connect
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	refusal := mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInstallUnavailable)
	if refusal.Message != "Delivery did not answer the install capability" {
		t.Fatalf("message = %q", refusal.Message)
	}
}

func TestCredentialProviderHostedReadRefusesAnInvalidGrant(t *testing.T) {
	for name, body := range map[string]any{
		"other protocol version": map[string]any{"protocolVersion": 2, "bearer": testInstallBearer, "expiresAt": "2026-10-02T05:30:00Z", "hosts": []string{"go.putnami.dev"}},
		"no hosts":               map[string]any{"protocolVersion": 1, "bearer": testInstallBearer, "expiresAt": "2026-10-02T05:30:00Z"},
		"bad expiry":             map[string]any{"protocolVersion": 1, "bearer": testInstallBearer, "expiresAt": "tomorrow", "hosts": []string{"go.putnami.dev"}},
		"bearer with a space":    map[string]any{"protocolVersion": 1, "bearer": "two words", "expiresAt": "2026-10-02T05:30:00Z", "hosts": []string{"go.putnami.dev"}},
		"not an object":          []string{"go.putnami.dev"},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, body)
			})
			h := startCredentialServer(t, ingest, nil, nil)
			h.send(initializeLine(1, testRunCredential))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInstallUnavailable)
			assertNoSecret(t, h.stderr.String())
		})
	}
}

func TestCredentialProviderHostedReadRefusesWithoutAnIngestBase(t *testing.T) {
	for name, ingest := range map[string]string{
		"unset":              "",
		"plain http":         "http://delivery.example/v1/delivery/records",
		"credentials in URL": "https://user:pass@delivery.example/v1/delivery/records",
	} {
		t.Run(name, func(t *testing.T) {
			h := startCredentialServer(t, ingest, nil, nil)
			h.send(initializeLine(1, testRunCredential))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInstallUnconfigured)
		})
	}
}

func TestCredentialProviderPublishIsAbsence(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	local := func(context.Context) (*registry.Credential, error) {
		t.Error("the local source answered a publish request")
		return nil, nil
	}
	for _, runCredential := range []string{testRunCredential, ""} {
		h := startCredentialServer(t, ingest, local, nil)
		h.send(initializeLine(1, runCredential))
		h.next(registry.CredentialOpInitialize)
		h.send(credentialLine(2, registry.PurposePublish))
		response := h.next(registry.CredentialOpCredential)
		if credential := mustCredential(t, response); credential != nil {
			t.Fatalf("publish credential = %+v, want absence", credential)
		}
		if string(response.Payload) != "{}" {
			t.Fatalf("publish payload = %s, want {}", response.Payload)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("Delivery calls = %d, want none for publish", calls.Load())
	}
}

func TestCredentialProviderSharesOneDeliveryCallPerPurpose(t *testing.T) {
	var calls atomic.Int64
	arrived := make(chan struct{}, 4)
	release := make(chan struct{})
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		writeJSON(w, http.StatusCreated, map[string]any{
			"protocolVersion": 1, "bearer": testInstallBearer, "expiresAt": "2026-10-02T05:30:00Z", "hosts": []string{"go.putnami.dev"},
		})
	})
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	<-arrived
	h.send(credentialLine(3, registry.PurposeRead))
	// The second request joins the first exchange instead of starting one.
	h.send(credentialLine(4, registry.PurposePublish))
	if response := h.next(registry.CredentialOpCredential); response.ID != 4 {
		t.Fatalf("first answer id = %d, want the publish absence (4) while read waits", response.ID)
	}
	close(release)
	ids := map[int64]bool{}
	for range 2 {
		response := h.next(registry.CredentialOpCredential)
		if credential := mustCredential(t, response); credential == nil || credential.Bearer != testInstallBearer {
			t.Fatalf("answer %d = %+v", response.ID, credential)
		}
		ids[response.ID] = true
	}
	if !ids[2] || !ids[3] || calls.Load() != 1 {
		t.Fatalf("answered %v with %d Delivery calls, want 2 and 3 from one call", ids, calls.Load())
	}
}

func TestCredentialProviderShutdownAbandonsAnExchangeInProgress(t *testing.T) {
	var calls atomic.Int64
	arrived := make(chan struct{}, 1)
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	})
	h := startCredentialServer(t, ingest, nil, nil)
	h.send(initializeLine(1, testRunCredential))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	<-arrived
	h.send(`{"protocolVersion":1,"id":3,"op":"shutdown"}`)
	if response := h.next(registry.CredentialOpShutdown); response.ID != 3 || !response.OK {
		t.Fatalf("answer after shutdown = %+v, want only the shutdown ack", response)
	}
	if err := h.exit(); err != nil {
		t.Fatalf("serve = %v", err)
	}
	h.noAnswer(50 * time.Millisecond)
}

func TestCredentialProviderExitsOnStdinEOF(t *testing.T) {
	h := startCredentialServer(t, "", nil, nil)
	h.send(initializeLine(1, ""))
	h.next(registry.CredentialOpInitialize)
	_ = h.stdin.Close()
	if err := h.exit(); err != nil {
		t.Fatalf("serve after EOF = %v, want nil", err)
	}
}

func TestCredentialProviderEndsTheSessionOnAnOversizedLine(t *testing.T) {
	server := newCredentialServer(newCapabilityCaller(""), nil, func() error { return nil })
	line := strings.Repeat("x", registry.MaxCredentialLineBytes+2) + "\n"
	err := server.serve(strings.NewReader(line), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("serve = %v, want the line bound error", err)
	}
}

func TestCredentialProviderRefusesRequestsOutsideTheProtocol(t *testing.T) {
	h := startCredentialServer(t, "", nil, nil)

	h.send(credentialLine(1, registry.PurposeRead))
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalNotInitialized)

	h.send(initializeLine(2, ""))
	h.next(registry.CredentialOpInitialize)
	h.send(initializeLine(3, ""))
	mustRefusal(t, h.next(registry.CredentialOpInitialize), refusalAlreadyInitialized)

	h.send(`{"protocolVersion":1,"id":4,"op":"rotate"}`)
	refusal := mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInvalidRequest)
	if !strings.Contains(refusal.Message, "op") {
		t.Fatalf("unknown op message = %q", refusal.Message)
	}

	// Strict decoding: an unknown member, a wrong-case member and a purpose
	// outside the protocol are all refused.
	h.send(`{"protocolVersion":1,"id":5,"op":"credential","payload":{"purpose":"read","extra":true}}`)
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInvalidRequest)
	h.send(`{"protocolVersion":1,"id":6,"op":"credential","payload":{"Purpose":"read"}}`)
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInvalidRequest)
	h.send(credentialLine(7, "admin"))
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInvalidRequest)
	h.send(`{"protocolVersion":2,"id":8,"op":"credential","payload":{"purpose":"read"}}`)
	mustRefusal(t, h.next(registry.CredentialOpCredential), refusalInvalidRequest)

	// A line with no request id cannot be answered; the session goes on.
	h.send(`not json`)
	h.send(`{"protocolVersion":1,"op":"credential","payload":{"purpose":"read"}}`)
	h.send(credentialLine(9, registry.PurposePublish))
	if response := h.next(registry.CredentialOpCredential); response.ID != 9 || !response.OK {
		t.Fatalf("answer after unanswerable lines = %+v, want 9", response)
	}
	if !strings.Contains(h.stderr.String(), "malformed request") {
		t.Fatalf("stderr = %q, want the malformed request diagnostics", h.stderr.String())
	}
}

func TestCredentialProviderLocalRead(t *testing.T) {
	good := &registry.Credential{Bearer: "user-bearer", ExpiresAt: "2026-10-02T05:30:00Z", Hosts: []string{"go.putnami.dev", "npm.putnami.dev"}}
	jwt := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0.c2lnbmF0dXJl"
	for _, test := range []struct {
		name       string
		local      ReadCredentialSource
		want       *registry.Credential
		wantStderr string
	}{
		{name: "credential", local: func(context.Context) (*registry.Credential, error) { return good, nil }, want: good},
		{name: "absence", local: func(context.Context) (*registry.Credential, error) { return nil, nil }},
		{name: "no source"},
		{
			name: "error is absence with a redacted diagnostic",
			local: func(context.Context) (*registry.Credential, error) {
				return nil, errors.New("auth server said " + jwt + " pkt_secretkey is bad")
			},
			wantStderr: "auth server said <redacted> <redacted> is bad; using native credentials",
		},
		{
			name: "invalid credential is absence",
			local: func(context.Context) (*registry.Credential, error) {
				return &registry.Credential{Bearer: "user-bearer", ExpiresAt: "2026-10-02T05:30:00Z", Hosts: []string{"NPM.putnami.dev"}}, nil
			},
			wantStderr: "does not follow the protocol",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startCredentialServer(t, "", test.local, nil)
			h.send(initializeLine(1, ""))
			h.next(registry.CredentialOpInitialize)
			h.send(credentialLine(2, registry.PurposeRead))
			got := mustCredential(t, h.next(registry.CredentialOpCredential))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("credential = %+v, want %+v", got, test.want)
			}
			stderr := h.stderr.String()
			if test.wantStderr != "" && !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr = %q, want %q", stderr, test.wantStderr)
			}
			if strings.Contains(stderr, jwt) || strings.Contains(stderr, "pkt_secretkey") || strings.Contains(stderr, "user-bearer") {
				t.Fatalf("stderr carries a secret: %q", stderr)
			}
		})
	}
}

func TestCredentialProviderLocalReadStopsTheSourceAtTheTimeout(t *testing.T) {
	stopped := make(chan error, 1)
	local := func(ctx context.Context) (*registry.Credential, error) {
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, ctx.Err()
	}
	h := startCredentialServer(t, "", local, nil)
	h.server.localTimeout = 20 * time.Millisecond
	h.send(initializeLine(1, ""))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	if credential := mustCredential(t, h.next(registry.CredentialOpCredential)); credential != nil {
		t.Fatalf("credential = %+v, want absence", credential)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the source stopped with %v, want the read's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the source was not told to stop")
	}
	if !strings.Contains(h.stderr.String(), "did not answer within") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

// TestCredentialProviderLocalReadNeverRunsTwoSourcesAtOnce pins that a read
// that timed out keeps the source to itself until the source returns, so two
// sign-in reads never write registries.json at the same time.
func TestCredentialProviderLocalReadNeverRunsTwoSourcesAtOnce(t *testing.T) {
	var running, calls atomic.Int64
	release := make(chan struct{})
	local := func(context.Context) (*registry.Credential, error) {
		if running.Add(1) > 1 {
			t.Error("two local reads ran at once")
		}
		defer running.Add(-1)
		if calls.Add(1) == 1 {
			<-release // the first read ignores its deadline, like a stuck write
		}
		return nil, nil
	}
	h := startCredentialServer(t, "", local, nil)
	h.server.localTimeout = 20 * time.Millisecond
	h.send(initializeLine(1, ""))
	h.next(registry.CredentialOpInitialize)

	h.send(credentialLine(2, registry.PurposeRead))
	mustCredential(t, h.next(registry.CredentialOpCredential))
	h.send(credentialLine(3, registry.PurposeRead))
	mustCredential(t, h.next(registry.CredentialOpCredential))
	if calls.Load() != 1 {
		t.Fatalf("source calls = %d while the first still runs, want 1", calls.Load())
	}
	if !strings.Contains(h.stderr.String(), "an earlier read of this machine's sign-in did not stop") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}

	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for len(h.server.localSlot) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first read never gave the source back")
		}
		time.Sleep(time.Millisecond)
	}
	h.send(credentialLine(4, registry.PurposeRead))
	mustCredential(t, h.next(registry.CredentialOpCredential))
	if calls.Load() != 2 {
		t.Fatalf("source calls = %d, want 2 once the first read returned", calls.Load())
	}
}

func TestCredentialProviderShutdownWaitsForTheLocalSourceToStop(t *testing.T) {
	started := make(chan struct{})
	var stopped atomic.Bool
	local := func(ctx context.Context) (*registry.Credential, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // a write still finishing
		stopped.Store(true)
		return nil, ctx.Err()
	}
	h := startCredentialServer(t, "", local, nil)
	h.send(initializeLine(1, ""))
	h.next(registry.CredentialOpInitialize)
	h.send(credentialLine(2, registry.PurposeRead))
	<-started
	h.send(`{"protocolVersion":1,"id":3,"op":"shutdown"}`)
	if response := h.next(registry.CredentialOpShutdown); response.ID != 3 || !response.OK {
		t.Fatalf("answer after shutdown = %+v, want only the shutdown ack", response)
	}
	if !stopped.Load() {
		t.Fatal("shutdown answered before the local source stopped")
	}
	if err := h.exit(); err != nil {
		t.Fatalf("serve = %v", err)
	}
}

func TestCredentialIngestBase(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.putnami.cloud/v1/delivery/records/": "https://api.putnami.cloud/v1/delivery/records",
		"http://127.0.0.1:8080/v1/delivery/records":      "http://127.0.0.1:8080/v1/delivery/records",
		"http://localhost:8080/records":                  "http://localhost:8080/records",
		"http://[::1]:8080/records":                      "http://[::1]:8080/records",
		"":                                               "",
		"http://delivery.example/records":                "",
		"https://u:p@delivery.example/records":           "",
		"https://delivery.example/records?x=1":           "",
		"https://delivery.example/records#x":             "",
		"ftp://delivery.example/records":                 "",
		"/v1/delivery/records":                           "",
	} {
		got, err := credentialIngestBase(raw)
		if want == "" {
			if err == nil {
				t.Errorf("credentialIngestBase(%q) = %q, want an error", raw, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("credentialIngestBase(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// TestCapabilityServiceBase: the generated client binds to the ingest base's
// origin, since every generated capability route carries the ingest path. A
// base without that path cannot reach the routes and counts as unconfigured.
func TestCapabilityServiceBase(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.putnami.cloud/v1/delivery/records/":       "https://api.putnami.cloud",
		"https://gateway.example/delivery/v1/delivery/records": "https://gateway.example/delivery",
		"http://127.0.0.1:8080/v1/delivery/records":            "http://127.0.0.1:8080",
		"http://localhost:8080/records":                        "",
		"http://delivery.example/v1/delivery/records":          "",
		"": "",
	} {
		got, err := capabilityServiceBase(raw)
		if want == "" {
			if err == nil || newCapabilityCaller(raw).configured() {
				t.Errorf("capabilityServiceBase(%q) = %q, want an error", raw, got)
			}
			continue
		}
		if err != nil || got != want || !newCapabilityCaller(raw).configured() {
			t.Errorf("capabilityServiceBase(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// TestCapabilityCallerRefusesAnUnknownRouteWithoutSending: only the five
// generated routes are reachable; anything else is an outage, not a request.
func TestCapabilityCallerRefusesAnUnknownRouteWithoutSending(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	caller := newCapabilityCaller(ingest)
	caller.backoff = nil
	if answer := caller.call(context.Background(), "/capabilities/unknown", testRunCredential); answer.outcome != capabilityUnavailable || answer.status != 0 {
		t.Fatalf("answer = %+v, want unavailable without an answer", answer)
	}
	if answer := caller.post(context.Background(), nativePublicationPublishPath, testRunCredential, []byte("not a plan"), nil); answer.outcome != capabilityUnavailable {
		t.Fatalf("answer = %+v, want unavailable for a plan that does not decode", answer)
	}
	if calls.Load() != 0 {
		t.Fatalf("Delivery calls = %d, want 0", calls.Load())
	}
}

// TestCapabilityCallerKeepsAnAnswerOutsideTheContract: a 201 whose body the
// generated contract rejects still reaches the caller as a grant with its
// exact bytes, so the caller's own proof refuses it, as before the contract.
func TestCapabilityCallerKeepsAnAnswerOutsideTheContract(t *testing.T) {
	var calls atomic.Int64
	ingest := deliveryInstall(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"protocolVersion":"one"}`))
	})
	caller := newCapabilityCaller(ingest)
	answer := caller.call(context.Background(), installCapabilityPath, testRunCredential)
	if answer.outcome != capabilityGranted || answer.status != http.StatusCreated || string(answer.body) != `{"protocolVersion":"one"}` {
		t.Fatalf("answer = %+v %q", answer, answer.body)
	}
}

func TestBoundedDiagnosticText(t *testing.T) {
	if got := boundedDiagnosticText("a\tb\x7fc�d\xffe secret", 64, "secret"); got != "a b c d e <redacted>" {
		t.Fatalf("got %q", got)
	}
	if got := boundedDiagnosticText(strings.Repeat("é", 10), 5); got != "éé" {
		t.Fatalf("got %q, want two whole runes", got)
	}
}

func assertNoSecret(t *testing.T, stderr string) {
	t.Helper()
	for _, secret := range []string{testRunCredential, testInstallBearer} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("stderr carries a secret: %q", stderr)
		}
	}
}
