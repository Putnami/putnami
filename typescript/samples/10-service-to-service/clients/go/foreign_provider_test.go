package itemsclient_test

// TS→Go cell of the cross-language REST JSON matrix: a real TypeScript Putnami provider
// runs in its own process, and this Go consumer calls it through the client
// generated from that provider's own contract.
//
// Harness shape (D0.5): the provider is a subprocess this test owns, and the
// test learns the listening endpoint from the reserved `putnami.ready` log
// marker (protocols/runtime/ready_marker.go) — no port file, no scan, no sleep.
// It kills and reaps the process in its cleanup, so no child survives the task.
//
// One deviation from D0.5 §3 remains:
//
//  §3.1 asks for the artifact `build` produced. A TypeScript project's
//     transpiled bundle is not runnable from the per-command output directory:
//     workspace packages are installed into each project's own node_modules and
//     Bun resolves them from the entry file upward. The test therefore launches
//     the same entrypoint `putnami serve` launches — `bun <project>/src/serve.ts`,
//     the project's `./serve` export — with the `bun` that `exec.LookPath`
//     returns, which is what the TypeScript extension's toolchain.ResolveBun
//     resolves too.
//
// The provider also consumes its own generated client through `/proxy`. A
// loopback reverse proxy keeps its listener while Bun binds PORT=0 and forwards
// to the endpoint announced by the ready marker. That supplies a live client
// binding before provider startup without guessing or releasing Bun's port.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/ts-items-client"
	"go.putnami.dev/inject"
	runtimeprotocol "go.putnami.dev/protocol/runtime"
)

const (
	providerProject  = "typescript/samples/10-service-to-service"
	providerReadyTTL = 60 * time.Second
	serviceID        = "catalog.items"
)

// workspaceRoot walks up from the test's working directory to the directory
// that carries the workspace manifest.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no putnami.workspace.json above %s", dir)
		}
		dir = parent
	}
}

// startForeignProvider runs the TypeScript provider as a subprocess and returns
// its base URL. It fails with the provider's own output — never by silent
// timeout — when the process dies before announcing readiness.
func startForeignProvider(t *testing.T) string {
	t.Helper()
	return startForeignProviderInstance(t).baseURL
}

// foreignProvider is one running TypeScript provider process: its base URL and
// the graceful stop that drains it.
type foreignProvider struct {
	baseURL string
	process *os.Process
	exited  <-chan struct{}
}

// drain is the instance replacement a continuation exists for: SIGTERM is the
// provider's graceful stop, which ends every negotiated stream with no terminal
// before the process exits.
func (provider *foreignProvider) drain(t *testing.T) {
	t.Helper()
	if err := provider.process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("drain the provider: %v", err)
	}
	select {
	case <-provider.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider did not exit after its graceful stop")
	}
}

// startForeignProviderInstance runs one TypeScript provider process, owned by
// the test, and returns it once it announced readiness.
func startForeignProviderInstance(t *testing.T) *foreignProvider {
	t.Helper()
	bunBin, err := exec.LookPath("bun")
	if err != nil {
		t.Skipf("bun is not on PATH: %v", err)
	}
	root := workspaceRoot(t)
	entrypoint := filepath.Join(root, providerProject, "src", "serve.ts")
	if _, err := os.Stat(entrypoint); err != nil {
		t.Fatalf("provider entrypoint %s: %v", entrypoint, err)
	}

	var forward atomic.Pointer[httputil.ReverseProxy]
	selfClient := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		proxy := forward.Load()
		if proxy == nil {
			http.Error(response, "provider is not ready", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(response, request)
	}))
	t.Cleanup(selfClient.Close)
	configData, err := json.Marshal(map[string]any{
		"clients": map[string]any{
			"clientId": "cross-language-provider",
			"services": map[string]any{
				serviceID: map[string]any{
					"url": selfClient.URL, "allowInsecure": true,
					"credentials": map[string]any{
						"catalog-key": map[string]any{"source": "static", "value": catalogAPIKey},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, bunBin, entrypoint)
	command.Dir = filepath.Join(root, providerProject)
	command.Env = append(os.Environ(),
		"PORT=0",
		"APP_ENV=test",
		"CONFIG_DATA="+string(configData),
		// The TypeScript runtime resolves the project root from
		// PUTNAMI_PROJECT_ROOT, else from the PWD *environment variable*
		// (typescript/framework/utils/src/server/workspace.utils.ts:41-51).
		// A subprocess started with a working directory does not rewrite PWD, so
		// without this the route auto-scan reads the parent shell's directory,
		// registers zero routes and answers 404 to everything with no diagnostic.
		"PUTNAMI_PROJECT_ROOT="+filepath.Join(root, providerProject),
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start provider: %v", err)
	}

	transcript := &syncBuffer{}
	exited := make(chan struct{})
	var waitErr error
	found := make(chan runtimeprotocol.ReadyEndpoint, 1)
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(stdout, transcript))
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		announced := false
		for scanner.Scan() {
			if announced {
				continue
			}
			if endpoint, ok := readyEndpoint(scanner.Bytes()); ok {
				announced = true
				found <- endpoint
			}
		}
	}()
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("the provider subprocess did not exit after cancellation")
		}
		if t.Failed() {
			t.Logf("provider transcript:\n%s", transcript.String())
		}
	})

	select {
	case endpoint := <-found:
		baseURL := endpoint.URL()
		target, err := url.Parse(baseURL)
		if err != nil {
			t.Fatalf("parse provider ready endpoint %q: %v", baseURL, err)
		}
		forward.Store(httputil.NewSingleHostReverseProxy(target))
		return &foreignProvider{baseURL: baseURL, process: command.Process, exited: exited}
	case <-exited:
		t.Fatalf("provider exited before announcing readiness (%v); output:\n%s", waitErr, transcript.String())
	case <-time.After(providerReadyTTL):
		t.Fatalf("provider never announced %q within %s; output:\n%s",
			runtimeprotocol.ReadyLogKey, providerReadyTTL, transcript.String())
	}
	return nil
}

// syncBuffer collects the provider transcript from the scanner goroutine while
// the test goroutine may read it on failure.
type syncBuffer struct {
	mu    sync.Mutex
	bytes []byte
}

func (buffer *syncBuffer) Write(chunk []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.bytes = append(buffer.bytes, chunk...)
	return len(chunk), nil
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.bytes)
}

// readyEndpoint extracts the bound address from one structured log line. It
// comes from the listener the provider actually bound, not configuration.
func readyEndpoint(line []byte) (runtimeprotocol.ReadyEndpoint, bool) {
	record := map[string]any{}
	if err := json.Unmarshal(line, &record); err != nil {
		return runtimeprotocol.ReadyEndpoint{}, false
	}
	ready, ok := runtimeprotocol.ReadyMarkerFromLogRecord(record)
	if !ok || len(ready.Endpoints) == 0 {
		return runtimeprotocol.ReadyEndpoint{}, false
	}
	return ready.Endpoints[0], true
}

// boundClient resolves the generated client the way an application does. The
// consumer declares one binding; it writes no URL, no header, no interceptor.
func boundClient(t *testing.T, baseURL string) *itemsclient.ItemsClient {
	t.Helper()
	module := app.NewModule("ts-items-consumer")
	module.Use(client.Services(client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {URL: baseURL}},
	}))
	itemsclient.RegisterItemsClient(module)
	return startBoundConsumer(t, module)
}

// startBoundConsumer starts the consumer application and resolves the generated
// client from its container, the way a deployment does.
func startBoundConsumer(t *testing.T, module *app.Module) *itemsclient.ItemsClient {
	t.Helper()
	application := app.New("cross-language-consumer")
	application.Use(module)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- application.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !application.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !application.IsRunning() {
		cancel()
		t.Fatalf("consumer application did not start: %v", <-done)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("consumer app run: %v", err)
		}
		if err := application.Stop(context.Background()); err != nil {
			t.Errorf("consumer app stop: %v", err)
		}
	})
	value, err := application.Context().Get(inject.TokenOf[*itemsclient.ItemsClient]())
	if err != nil {
		t.Fatal(err)
	}
	generated, ok := value.(*itemsclient.ItemsClient)
	if !ok {
		t.Fatalf("resolved generated client has type %T", value)
	}
	return generated
}

func crossLanguageClient(t *testing.T) *itemsclient.ItemsClient {
	t.Helper()
	return boundClient(t, startForeignProvider(t))
}

// A typed 200 across the language boundary, with the declared query string and
// the declared request header the provider echoes back.
func TestTypeScriptProviderAnswersTheGeneratedGoClient(t *testing.T) {
	generated := crossLanguageClient(t)

	all, err := generated.ListItems(t.Context(), itemsclient.ListItemsInput{
		Query:  itemsclient.ListItemsQuery{Search: "", Limit: 10},
		Header: itemsclient.ListItemsHeader{XCatalogTenant: "tenant-go"},
	})
	if err != nil {
		var remote *client.RemoteError
		if errors.As(err, &remote) {
			t.Fatalf("ListItems: %v status=%d payload=%s", err, remote.StatusCode, string(remote.Payload))
		}
		t.Fatalf("ListItems: %v", err)
	}
	if all.Tenant != "tenant-go" {
		t.Fatalf("tenant = %q, want tenant-go (the declared header did not cross)", all.Tenant)
	}
	if len(all.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(all.Items))
	}

	filtered, err := generated.ListItems(t.Context(), itemsclient.ListItemsInput{
		Query:  itemsclient.ListItemsQuery{Search: "Gadget", Limit: 10},
		Header: itemsclient.ListItemsHeader{XCatalogTenant: "tenant-go"},
	})
	if err != nil {
		t.Fatalf("ListItems filtered: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Name != "Gadget" {
		t.Fatalf("filtered items = %+v, want only Gadget", filtered.Items)
	}
}

// The provider's generated self-client uses its live proxy binding after Bun
// announces the actual ephemeral endpoint; no reserved port is handed to Bun.
func TestTypeScriptProviderSelfClientUsesReadyEndpoint(t *testing.T) {
	baseURL := startForeignProvider(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/proxy", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("call provider self-client: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("provider self-client status = %d, body = %s", response.StatusCode, body)
	}
	var body struct {
		Message string            `json:"message"`
		Tenant  string            `json:"tenant"`
		Items   []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode provider self-client response: %v", err)
	}
	if body.Message != "Fetched items through the generated service binding" || body.Tenant != "sample-tenant" || len(body.Items) < 3 {
		t.Fatalf("provider self-client response = %+v", body)
	}
}

// A typed body in, a typed nested model out, and the path parameter the client
// substitutes. Absence stays absence over the wire.
func TestTypeScriptProviderRoundTripsATypedBodyAndNestedModel(t *testing.T) {
	generated := crossLanguageClient(t)

	created, err := generated.CreateItems(t.Context(), itemsclient.CreateItemsInput{
		Body: itemsclient.CreateItemsBody{Name: "Sprocket", Price: 3.5, Stock: 7},
	})
	if err != nil {
		t.Fatalf("CreateItems: %v", err)
	}
	if created.Item.Name != "Sprocket" || created.Item.Price != 3.5 || created.Item.Stock != 7 {
		t.Fatalf("created item = %+v", created.Item)
	}
	if created.Item.DiscontinuedAt != nil {
		t.Fatalf("an absent optional property arrived present: %+v", created.Item)
	}

	fetched, err := generated.GetItems(t.Context(), itemsclient.GetItemsInput{
		Path: itemsclient.GetItemsPath{Id: created.Item.Id},
	})
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	if fetched.Item != created.Item {
		t.Fatalf("read back %+v, want %+v", fetched.Item, created.Item)
	}

	discontinued, err := generated.GetItems(t.Context(), itemsclient.GetItemsInput{
		Path: itemsclient.GetItemsPath{Id: "item-2"},
	})
	if err != nil {
		t.Fatalf("GetItems item-2: %v", err)
	}
	if discontinued.Item.DiscontinuedAt == nil || *discontinued.Item.DiscontinuedAt != "2026-01-31" {
		t.Fatalf("a present optional property was lost: %+v", discontinued.Item)
	}
}

// The TypeScript provider declares 404 → not_found; the Go client narrows it to
// its generated type and keeps the stable code (D0.1).
func TestTypeScriptProviderDeclaredErrorArrivesTypedInGo(t *testing.T) {
	generated := crossLanguageClient(t)

	_, err := generated.GetItems(t.Context(), itemsclient.GetItemsInput{
		Path: itemsclient.GetItemsPath{Id: "missing"},
	})
	var notFound *itemsclient.GetItemsNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %T %v, want *GetItemsNotFoundError", err, err)
	}
	if notFound.Remote.Code() != "not_found" {
		t.Fatalf("RemoteError.Code() = %q, want not_found", notFound.Remote.Code())
	}
	if notFound.Remote.Service() != serviceID || notFound.Remote.Operation() != "getItems_id" {
		t.Fatalf("remote identity = %q/%q", notFound.Remote.Service(), notFound.Remote.Operation())
	}
}

// catalogAPIKey and catalogKeyWithoutScope mirror src/workload-identity.ts: the
// provider accepts the first for the watch stream and authenticates the second
// without the scope the stream requires.
const (
	catalogAPIKey          = "sample-catalog-key"
	catalogKeyWithoutScope = "sample-catalog-key-no-scope"
)

// boundClientWithKey binds the api-key profile the watch stream declares. The
// consumer writes no header: the binding injects it from the credential.
// A failure the operation never declared reaches this consumer as an unknown
// remote error: a status it can act on, no narrowing to a declared type, and
// none of the provider's own prose.
func TestTypeScriptProviderUndeclaredFailureArrivesUntypedInGo(t *testing.T) {
	generated := crossLanguageClient(t)

	_, err := generated.GetItems(t.Context(), itemsclient.GetItemsInput{Path: itemsclient.GetItemsPath{Id: "boom"}})
	if err == nil {
		t.Fatal("an undeclared failure resolved")
	}
	var notFound *itemsclient.GetItemsNotFoundError
	if errors.As(err, &notFound) {
		t.Fatal("an undeclared failure narrowed to a declared error type")
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != 503 {
		t.Fatalf("error = %T %v, want a remote error carrying 503", err, err)
	}
	if strings.Contains(err.Error(), "shard") || strings.Contains(err.Error(), "replica") {
		t.Fatalf("the provider's prose reached the consumer: %v", err)
	}
}

func boundClientWithKey(t *testing.T, baseURL string, key string) *itemsclient.ItemsClient {
	t.Helper()
	module := app.NewModule("ts-items-consumer")
	module.Use(client.Services(client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {
			URL:         baseURL,
			Credentials: map[string]client.CredentialBinding{"catalog-key": {Source: client.CredentialSourceStatic, Value: key}},
		}},
	}))
	itemsclient.RegisterItemsClient(module)
	return startBoundConsumer(t, module)
}

// The declared server stream of a TypeScript provider, read by a generated Go
// client: the success path, the typed terminal, the admission refusals, and the
// consumer's own cancellation of a long-lived stream.
func TestTypeScriptProviderServerStreamReachesTheGeneratedGoClient(t *testing.T) {
	baseURL := startForeignProvider(t)
	generated := boundClientWithKey(t, baseURL, catalogAPIKey)

	t.Run("typed messages", func(t *testing.T) {
		stream, err := generated.GetItemsWatch(t.Context(), itemsclient.GetItemsWatchInput{
			Path:  itemsclient.GetItemsWatchPath{Id: "item-1"},
			Query: itemsclient.GetItemsWatchQuery{Follow: false},
		})
		if err != nil {
			t.Fatalf("GetItemsWatch: %v", err)
		}
		messages := make([]itemsclient.GetItemsWatchMessage, 0, 1)
		for message := range stream.Messages() {
			messages = append(messages, message)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("stream terminal: %v", err)
		}
		if len(messages) != 1 || messages[0].Id != "item-1" || messages[0].Name != "Widget" {
			t.Fatalf("messages = %+v", messages)
		}
	})

	t.Run("declared terminal error arrives typed", func(t *testing.T) {
		stream, err := generated.GetItemsWatch(t.Context(), itemsclient.GetItemsWatchInput{
			Path:  itemsclient.GetItemsWatchPath{Id: "missing"},
			Query: itemsclient.GetItemsWatchQuery{Follow: false},
		})
		if err != nil {
			t.Fatalf("GetItemsWatch open: %v", err)
		}
		for range stream.Messages() {
			t.Error("a refused item still delivered a message")
		}
		var notFound *itemsclient.GetItemsWatchNotFoundError
		if !errors.As(stream.Err(), &notFound) {
			t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
		}
		if notFound.Remote.StatusCode != 404 {
			t.Fatalf("terminal status = %d", notFound.Remote.StatusCode)
		}
	})

	t.Run("a refused credential reaches the consumer before any message", func(t *testing.T) {
		for name, key := range map[string]string{"unauthenticated": "not-the-catalog-key", "forbidden": catalogKeyWithoutScope} {
			t.Run(name, func(t *testing.T) {
				refused := boundClientWithKey(t, baseURL, key)
				stream, err := refused.GetItemsWatch(t.Context(), itemsclient.GetItemsWatchInput{
					Path:  itemsclient.GetItemsWatchPath{Id: "item-1"},
					Query: itemsclient.GetItemsWatchQuery{Follow: false},
				})
				if err == nil {
					t.Fatalf("a refused credential opened a stream: %+v", stream)
				}
				if stream != nil {
					t.Fatal("a refused admission returned a stream handle")
				}
				var remote *client.RemoteError
				if !errors.As(err, &remote) || (remote.StatusCode != 401 && remote.StatusCode != 403) {
					t.Fatalf("admission error = %T %v", err, err)
				}
			})
		}
	})

	t.Run("a followed stream ends on consumer cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream, err := generated.GetItemsWatch(ctx, itemsclient.GetItemsWatchInput{
			Path:  itemsclient.GetItemsWatchPath{Id: "item-1"},
			Query: itemsclient.GetItemsWatchQuery{Follow: true},
		})
		if err != nil {
			t.Fatalf("GetItemsWatch follow: %v", err)
		}
		received := 0
		for range stream.Messages() {
			received++
			if received == 2 {
				cancel()
			}
		}
		if received < 2 {
			t.Fatalf("messages before cancellation = %d", received)
		}
		if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
			t.Fatalf("terminal after cancellation = %T %v", err, err)
		}
	})
}

// TS→Go WebSocket cells: the same TypeScript provider, the same generated Go
// client, over the published first-party WebSocket wire. One provider process
// serves every subtest, so the cost is one boot, not six.
func TestTypeScriptProviderWebSocketStreamsReachTheGeneratedGoClient(t *testing.T) {
	baseURL := startForeignProvider(t)
	generated := boundClientWithKey(t, baseURL, catalogAPIKey)

	t.Run("a server stream follows the declared websocket-first order", func(t *testing.T) {
		// The TypeScript provider declares `websocket` before `sse` for this
		// operation and declares it resumable. This consumer says neither: it
		// calls the generated method, and the runtime opens the socket the
		// declaration named.
		stream, err := generated.GetItemsHistory(t.Context(), itemsclient.GetItemsHistoryInput{
			Path: itemsclient.GetItemsHistoryPath{Id: "item-1"},
		})
		if err != nil {
			t.Fatalf("GetItemsHistory: %v", err)
		}
		defer func() { _ = stream.Close() }()
		revisions := make([]string, 0, 3)
		for revision := range stream.Messages() {
			if revision.Id != "item-1" {
				t.Fatalf("revision = %+v, want item-1", revision)
			}
			revisions = append(revisions, revision.Revision)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("stream terminal: %v", err)
		}
		if fmt.Sprint(revisions) != "[1 2 3]" {
			t.Fatalf("revisions = %v, want the feed in order with nothing repeated", revisions)
		}
	})

	t.Run("a client stream returns its single declared result", func(t *testing.T) {
		stream, err := generated.GetItemsAdjust(t.Context(), itemsclient.GetItemsAdjustInput{
			Path: itemsclient.GetItemsAdjustPath{Id: "item-1"},
		})
		if err != nil {
			t.Fatalf("GetItemsAdjust: %v", err)
		}
		defer func() { _ = stream.Close() }()
		for _, delta := range []float64{5, -2, 7} {
			if err := stream.Send(t.Context(), itemsclient.GetItemsAdjustSend{Delta: delta}); err != nil {
				t.Fatalf("Send(%v): %v", delta, err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		total, err := stream.Result(t.Context())
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		if total.Id != "item-1" || total.Applied != 3 || total.Total != 10 {
			t.Fatalf("result = %+v", total)
		}
	})

	t.Run("a bidirectional stream outlives the consumer half-close", func(t *testing.T) {
		stream, err := generated.GetItemsNegotiate(t.Context(), itemsclient.GetItemsNegotiateInput{
			Path: itemsclient.GetItemsNegotiatePath{Id: "item-1"},
		})
		if err != nil {
			t.Fatalf("GetItemsNegotiate: %v", err)
		}
		defer func() { _ = stream.Close() }()
		for index, delta := range []float64{4, 6} {
			if err := stream.Send(t.Context(), itemsclient.GetItemsNegotiateSend{Delta: delta}); err != nil {
				t.Fatalf("Send(%v): %v", delta, err)
			}
			running, recvErr := stream.Recv(t.Context())
			if recvErr != nil {
				t.Fatalf("Recv after send %d: %v", index, recvErr)
			}
			if running.Applied != float64(index+1) {
				t.Fatalf("running total = %+v", running)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		terminal, err := stream.Recv(t.Context())
		if err != nil {
			t.Fatalf("terminal Recv: %v", err)
		}
		if terminal.Applied != 2 || terminal.Total != 10 {
			t.Fatalf("terminal value = %+v", terminal)
		}
		if _, err := stream.Recv(t.Context()); !errors.Is(err, io.EOF) {
			t.Fatalf("end of stream = %T %v", err, err)
		}
	})

	t.Run("a declared terminal error arrives typed", func(t *testing.T) {
		stream, err := generated.GetItemsAdjust(t.Context(), itemsclient.GetItemsAdjustInput{
			Path: itemsclient.GetItemsAdjustPath{Id: "missing"},
		})
		if err != nil {
			t.Fatalf("GetItemsAdjust open: %v", err)
		}
		defer func() { _ = stream.Close() }()
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		if _, err := stream.Result(t.Context()); err == nil {
			t.Fatal("a missing item returned a result")
		} else {
			var notFound *itemsclient.GetItemsAdjustNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("terminal error = %T %v", err, err)
			}
			if notFound.Remote.StatusCode != 404 {
				t.Fatalf("terminal status = %d", notFound.Remote.StatusCode)
			}
		}
	})

	t.Run("a refused credential reaches the consumer before any message", func(t *testing.T) {
		for name, key := range map[string]string{"unauthenticated": "not-the-catalog-key", "forbidden": catalogKeyWithoutScope} {
			t.Run(name, func(t *testing.T) {
				refused := boundClientWithKey(t, baseURL, key)
				stream, err := refused.GetItemsAdjust(t.Context(), itemsclient.GetItemsAdjustInput{
					Path: itemsclient.GetItemsAdjustPath{Id: "item-1"},
				})
				if err == nil {
					t.Fatalf("a refused credential opened a stream: %+v", stream)
				}
				if stream != nil {
					t.Fatal("a refused admission returned a stream handle")
				}
				var remote *client.RemoteError
				if !errors.As(err, &remote) || (remote.StatusCode != 401 && remote.StatusCode != 403) {
					t.Fatalf("admission error = %T %v", err, err)
				}
			})
		}
	})

	t.Run("a refused credential reaches the consumer before any revision or any turn", func(t *testing.T) {
		// Admission is decided per stream shape: the revision feed and the
		// conversation travel on the same socket as the client stream and are
		// refused the same way, before any frame.
		refused := boundClientWithKey(t, baseURL, "not-the-catalog-key")

		feed, err := refused.GetItemsHistory(t.Context(), itemsclient.GetItemsHistoryInput{
			Path: itemsclient.GetItemsHistoryPath{Id: "item-1"},
		})
		if err == nil {
			t.Fatalf("a refused credential opened a server stream: %+v", feed)
		}
		var remote *client.RemoteError
		if !errors.As(err, &remote) || remote.StatusCode != 401 {
			t.Fatalf("server-stream admission error = %T %v", err, err)
		}
		if feed != nil {
			t.Fatal("a refused admission returned a server-stream handle")
		}

		conversation, err := refused.GetItemsNegotiate(t.Context(), itemsclient.GetItemsNegotiateInput{
			Path: itemsclient.GetItemsNegotiatePath{Id: "item-1"},
		})
		if err == nil {
			t.Fatalf("a refused credential opened a conversation: %+v", conversation)
		}
		if !errors.As(err, &remote) || remote.StatusCode != 401 {
			t.Fatalf("bidi admission error = %T %v", err, err)
		}
		if conversation != nil {
			t.Fatal("a refused admission returned a bidi handle")
		}
	})

	t.Run("a bidirectional stream ends on consumer cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream, err := generated.GetItemsNegotiate(ctx, itemsclient.GetItemsNegotiateInput{
			Path: itemsclient.GetItemsNegotiatePath{Id: "item-1"},
		})
		if err != nil {
			t.Fatalf("GetItemsNegotiate: %v", err)
		}
		if err := stream.Send(ctx, itemsclient.GetItemsNegotiateSend{Delta: 1}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, err := stream.Recv(ctx); err != nil {
			t.Fatalf("Recv: %v", err)
		}
		cancel()
		select {
		case <-stream.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation left the conversation open")
		}
		if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
			t.Fatalf("terminal after cancellation = %T %v", err, err)
		}
	})
}

// --- Raw octet cell: Go consumer → TypeScript provider ---------------------

// nonUTF8Blob is neither valid UTF-8 nor valid JSON, so a text hop or a JSON
// re-encoding on either side of the boundary would corrupt it.
var nonUTF8Blob = []byte{0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a}

// TestTypeScriptProviderCarriesRawOctetsToTheGeneratedGoClient is the TS→Go
// raw octet cell: the declared echo, the declared bound, the declared
// credential and the declared error, all across the language boundary.
func TestTypeScriptProviderCarriesRawOctetsToTheGeneratedGoClient(t *testing.T) {
	baseURL := startForeignProvider(t)
	generated := boundClientWithKey(t, baseURL, catalogAPIKey)

	for name, payload := range map[string][]byte{
		"empty":    {},
		"non-utf8": nonUTF8Blob,
		"at-bound": bytes.Repeat([]byte{0x7f}, 4096),
	} {
		echoed, err := generated.CreateBlobsEcho(t.Context(), itemsclient.CreateBlobsEchoInput{
			Body: bytes.NewReader(payload),
		})
		if err != nil {
			t.Fatalf("echo %s: %v", name, err)
		}
		if !bytes.Equal(echoed.Body, payload) {
			t.Fatalf("%s round trip = %x, want %x", name, echoed.Body, payload)
		}
		if echoed.ContentType != "application/octet-stream" || echoed.Status != 200 {
			t.Fatalf("%s declared representation lost: %d %q", name, echoed.Status, echoed.ContentType)
		}
	}

	// The path parameter is typed and the declared credential is injected by
	// the binding: the consumer writes neither a URL nor a header.
	blob, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "1"}})
	if err != nil {
		t.Fatalf("GetBlobs: %v", err)
	}
	if !bytes.Equal(blob.Body, nonUTF8Blob) {
		t.Fatalf("stored blob = %x, want %x", blob.Body, nonUTF8Blob)
	}
	// Zero octets are octets: the empty stored payload arrives empty, not absent.
	empty, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "2"}})
	if err != nil {
		t.Fatalf("GetBlobs empty: %v", err)
	}
	if len(empty.Body) != 0 {
		t.Fatalf("empty stored blob = %x, want zero octets", empty.Body)
	}

	// The emitted bound fires before a socket exists.
	if _, err := generated.CreateBlobsEcho(t.Context(), itemsclient.CreateBlobsEchoInput{
		Body: bytes.NewReader(bytes.Repeat([]byte{0x02}, 4097)),
	}); !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("oversized payload = %v, want a client.request refusal", err)
	}

	// A binary success does not cost the endpoint its declared error.
	_, err = generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "absent"}})
	var notFound *itemsclient.GetBlobsNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("declared error on a binary endpoint = %T %v, want *GetBlobsNotFoundError", err, err)
	}
	if notFound.Remote.StatusCode != 404 || notFound.Remote.Code() != "not_found" {
		t.Fatalf("typed error = %d/%q, want 404/not_found", notFound.Remote.StatusCode, notFound.Remote.Code())
	}
}

// TestTypeScriptProviderRefusesARawOctetReadWithNoCredential proves the
// credential rule holds on a binary endpoint: an unbound declared profile
// stops the call before a socket exists.
func TestTypeScriptProviderRefusesARawOctetReadWithNoCredential(t *testing.T) {
	generated := crossLanguageClient(t)
	_, err := generated.GetBlobs(t.Context(), itemsclient.GetBlobsInput{Path: itemsclient.GetBlobsPath{Id: "1"}})
	if err == nil {
		t.Fatal("a binary read with no declared credential reached the provider")
	}
	if !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("unbound credential = %v, want a client.credential refusal", err)
	}
}
