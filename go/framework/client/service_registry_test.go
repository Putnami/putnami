package client

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/app"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/inject"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// startedApplication builds an application whose runner returns at once, so
// Start walks every phase and returns instead of blocking on shutdown.
func startedApplication(t *testing.T, name string) *app.Application {
	t.Helper()
	return app.New(name).Run(func(context.Context) error { return nil })
}

func resolveRegistry(t *testing.T, container *inject.ContainerContext) *ServiceBindings {
	t.Helper()
	value, err := container.Get(inject.TokenOf[*ServiceBindings]())
	if err != nil {
		t.Fatal(err)
	}
	registry, ok := value.(*ServiceBindings)
	if !ok {
		t.Fatalf("resolved registry = %T", value)
	}
	return registry
}

func registryOptions(provider TokenSource) ServicesOptions {
	return ServicesOptions{
		ClientID: "consumer.workload",
		Services: map[string]ServiceBinding{
			"inventory": {
				URL:         "https://inventory.example",
				Credentials: map[string]CredentialBinding{"service": {Provider: provider}},
			},
		},
	}
}

func blockingTokenSource(started chan<- struct{}, observed *atomic.Int32) TokenSource {
	var once sync.Once
	return TokenSourceFunc(func(ctx context.Context, _ CredentialRequest) (Credential, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		if observed != nil {
			observed.Add(1)
		}
		return Credential{}, ctx.Err()
	})
}

func TestApplicationStopClosesTheServiceRegistryItPublished(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "registry-lifetime", "the-application-stop-phase-closes-the-registry-it-published")
	var acquisitions atomic.Int32
	plugin := Services(registryOptions(freshSource("service-token", &acquisitions)))
	application := startedApplication(t, "consumer")
	application.Use(plugin)
	if err := application.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry := resolveRegistry(t, application.Container())
	if _, err := registry.For("inventory"); err != nil {
		t.Fatalf("running application refused a binding: %v", err)
	}
	if err := application.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.For("inventory"); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("binding after application stop = %v", err)
	}
	// Stopping is the only owner action: a second stop must stay silent rather
	// than report an already-closed registry as a shutdown error.
	if err := plugin.Stop(t.Context(), nil); err != nil {
		t.Fatalf("repeated stop = %v", err)
	}
}

func TestServiceRegistryIsSharedInsideOneApplicationAndIsolatedBetweenTwo(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "application-scoped-registry", "modules-of-one-application-share-one-registry-and-two-applications-share-none")
	var first atomic.Int32
	var second atomic.Int32
	orders := app.NewModule("orders")
	firstApplication := startedApplication(t, "consumer")
	firstApplication.Use(Services(registryOptions(freshSource("first-token", &first)))).Use(orders)
	if err := firstApplication.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstApplication.Stop(context.Background()) }()

	fromApplication := resolveRegistry(t, firstApplication.Container())
	fromModule := resolveRegistry(t, orders.Container())
	if fromApplication != fromModule {
		t.Fatal("a module of the same application resolved a second registry")
	}

	secondApplication := startedApplication(t, "other-consumer")
	secondApplication.Use(Services(registryOptions(freshSource("second-token", &second))))
	if err := secondApplication.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondApplication.Stop(context.Background()) }()
	other := resolveRegistry(t, secondApplication.Container())
	if other == fromApplication {
		t.Fatal("two applications resolved the same registry")
	}

	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer.workload", Profile: "service"}
	firstBinding, err := fromApplication.For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	otherBinding, err := other.For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	// The same identity resolved twice inside one application is acquired once;
	// the other application acquires its own, because the cache hangs off the
	// registry rather than off the package.
	for range 2 {
		if _, err := fromApplication.credentials.acquire(t.Context(), request, firstBinding.Credentials["service"], time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fromModule.credentials.acquire(t.Context(), request, firstBinding.Credentials["service"], time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := other.credentials.acquire(t.Context(), request, otherBinding.Credentials["service"], time.Second); err != nil {
		t.Fatal(err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("acquisitions: first=%d second=%d", first.Load(), second.Load())
	}

	// Stopping one application must not disturb the other.
	if err := firstApplication.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fromApplication.For("inventory"); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("stopped registry = %v", err)
	}
	if _, err := other.For("inventory"); err != nil {
		t.Fatalf("unrelated application registry was closed: %v", err)
	}
}

func TestServiceRegistryCloseIsIdempotentAndCancelsRefreshesInFlight(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "registry-lifetime", "closing-the-registry-cancels-the-refreshes-in-flight-and-keeps-no-credential")
	started := make(chan struct{})
	var observedCancel atomic.Int32
	registry, err := newServiceBindings(registryOptions(blockingTokenSource(started, &observedCancel)))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := registry.For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer.workload", Profile: "service"}

	// Several callers wait on the same single acquisition. The provider only
	// returns when its context is canceled, so nothing here sleeps.
	results := make(chan error, 4)
	var waiting sync.WaitGroup
	for range 4 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			_, acquireErr := registry.credentials.acquire(context.Background(), request, binding.Credentials["service"], time.Minute)
			results <- acquireErr
		}()
	}
	<-started

	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	waiting.Wait()
	close(results)
	for acquireErr := range results {
		if !perrors.Is(acquireErr, CodeClientClosed) {
			t.Fatalf("waiter error = %v", acquireErr)
		}
	}
	if observedCancel.Load() != 1 {
		t.Fatalf("the in-flight acquisition was not canceled: %d", observedCancel.Load())
	}
	registry.credentials.mu.Lock()
	cached := len(registry.credentials.cache)
	inFlight := len(registry.credentials.calls)
	registry.credentials.mu.Unlock()
	if cached != 0 || inFlight != 0 {
		t.Fatalf("closed registry kept state: cache=%d calls=%d", cached, inFlight)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if err := (*ServiceBindings)(nil).Close(); err != nil {
		t.Fatalf("closing an absent registry = %v", err)
	}
}

func TestServiceRegistryCloseClosesTheStreamSessionsItTracks(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "registry-lifetime", "closing-the-registry-closes-the-stream-sessions-it-tracks")
	registry, err := newServiceBindings(registryOptions(freshSource("service-token", nil)))
	if err != nil {
		t.Fatal(err)
	}
	observer := &recordingServiceTelemetry{}
	tracked, trackedCtx := newStreamSession(t.Context(), streamSessionConfig{
		ServiceID: "inventory", OperationID: "watchItems", Telemetry: observer,
	})
	releaseTracked, err := registry.TrackStream(tracked)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseTracked()

	// A stream the transport closes first detaches itself: the registry must
	// not close it a second time and emit a second call measurement.
	own, _ := newStreamSession(t.Context(), streamSessionConfig{
		ServiceID: "inventory", OperationID: "watchOwn", Telemetry: observer,
	})
	releaseOwn, err := registry.TrackStream(own)
	if err != nil {
		t.Fatal(err)
	}
	own.Complete()
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
	releaseOwn()
	releaseOwn()

	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if tracked.Phase() != StreamPhaseClosed {
		t.Fatalf("tracked session phase = %s", tracked.Phase())
	}
	if trackedCtx.Err() == nil {
		t.Fatal("closing the registry left the stream context live")
	}
	if !perrors.Is(tracked.Err(), CodeClientCanceled) {
		t.Fatalf("registry shutdown terminal = %v", tracked.Err())
	}
	observer.mu.Lock()
	calls := append([]ServiceCallResult(nil), observer.calls...)
	observer.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("call measurements = %#v", calls)
	}
	codes := map[string]int{}
	for _, call := range calls {
		codes[call.Code]++
	}
	if codes[string(CodeClientCanceled)] != 1 || codes[""] != 1 {
		t.Fatalf("measurement codes = %#v", codes)
	}
	if _, err := registry.TrackStream(tracked); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("tracking after close = %v", err)
	}
	if _, err := registry.TrackStream(nil); err == nil || perrors.Is(err, CodeClientClosed) {
		t.Fatalf("tracking an absent session = %v", err)
	}
}

func TestClosedServiceRegistryRefusesLateAcquisitionsWithOneStableCode(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "registry-lifetime", "a-late-acquisition-is-refused-with-one-stable-typed-error")
	var acquisitions atomic.Int32
	registry, err := newServiceBindings(registryOptions(freshSource("service-token", &acquisitions)))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := registry.For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.For("inventory"); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("late binding = %v", err)
	}
	if _, err := NewServiceClient(registry, testDescriptor(map[string]clientcontract.CredentialProfile{})); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("late client construction = %v", err)
	}
	request := CredentialRequest{ServiceID: "inventory", ClientID: "consumer.workload", Profile: "service"}
	if _, err := registry.credentials.acquire(t.Context(), request, binding.Credentials["service"], time.Second); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("late acquisition = %v", err)
	}
	if acquisitions.Load() != 0 {
		t.Fatalf("a closed registry still called the token source %d times", acquisitions.Load())
	}
	// The refusal is stable: it is the registry's code, not a bare context
	// cancellation the caller cannot distinguish from its own deadline.
	if _, err := registry.credentials.acquire(t.Context(), request, binding.Credentials["service"], time.Second); !perrors.Is(err, CodeClientClosed) ||
		perrors.Is(err, CodeClientCanceled) || perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("second late acquisition = %v", err)
	}
}

// TestPackageHoldsNoAmbientCredentialCache is the guard behind "one registry
// per application": a package-level cache would make two applications share
// bearer tokens whatever their config says, and would survive Close.
func TestPackageHoldsNoAmbientCredentialCache(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "application-scoped-registry", "the-package-holds-no-ambient-credential-cache")
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	owners := map[string]int{}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file, parseErr := parser.ParseFile(fileSet, filepath.Clean(name), nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, declaration := range file.Decls {
			switch typed := declaration.(type) {
			case *ast.GenDecl:
				if typed.Tok != token.VAR {
					continue
				}
				if source := declarationSource(t, name, fileSet, typed); credentialStateSource(source) {
					t.Errorf("%s declares package-level credential state: %s", name, source)
				}
			case *ast.FuncDecl:
				// The manager's own constructors are the seam the registry
				// calls; only their callers say who owns a cache.
				if strings.HasPrefix(typed.Name.Name, "newCredentialManager") {
					continue
				}
				ast.Inspect(typed, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if identifier, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(identifier.Name, "newCredentialManager") {
						owners[typed.Name.Name]++
					}
					return true
				})
			}
		}
	}
	if scanned == 0 {
		t.Fatal("the guard scanned no source file")
	}
	if len(owners) != 1 || owners["newServiceBindings"] == 0 {
		t.Fatalf("credential managers are built outside the application registry: %#v", owners)
	}
}

func declarationSource(t *testing.T, name string, fileSet *token.FileSet, declaration *ast.GenDecl) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatal(err)
	}
	start := fileSet.Position(declaration.Pos()).Offset
	end := fileSet.Position(declaration.End()).Offset
	return string(source[start:end])
}

func credentialStateSource(source string) bool {
	for _, marker := range []string{"credentialManager", "cachedCredential", "credentialCall", "Credential{"} {
		if strings.Contains(source, marker) {
			return true
		}
	}
	return false
}
