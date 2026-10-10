package credentialprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/sdk/extension/proctree"
	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

const fakeProviderEnv = "PUTNAMI_TEST_FAKE_CREDENTIAL_PROVIDER"

// TestMain re-executes this test binary as a credential provider when
// fakeProviderEnv is set, so the out-of-process tests run a real provider
// process over real pipes. The host-keyed seam is stubbed: no test may reach
// a cloud installed on the machine.
func TestMain(m *testing.M) {
	// An extension runtime (fixtureproc.Binary) is this binary answering the
	// CLI's runtime handshake with the document its environment carries.
	if info := os.Getenv(fakeRuntimeInfoEnv); info != "" && len(os.Args) == 3 &&
		os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		fmt.Println(info)
		os.Exit(0)
	}
	if os.Getenv(fakeProviderEnv) == "1" {
		config := fakeConfig{
			bearer:          os.Getenv("FAKE_BEARER"),
			hosts:           strings.Split(os.Getenv("FAKE_HOSTS"), ","),
			lifetime:        time.Hour,
			callsFile:       os.Getenv("FAKE_CALLS_FILE"),
			exitAfterAnswer: os.Getenv("FAKE_EXIT_AFTER_ANSWER") == "1",
			initializeFile:  os.Getenv("FAKE_INITIALIZE_FILE"),
		}
		if envFile := os.Getenv("FAKE_ENV_FILE"); envFile != "" {
			_ = os.WriteFile(envFile, []byte(strings.Join(os.Environ(), "\n")), 0o600)
		}
		if argsFile := os.Getenv("FAKE_ARGS_FILE"); argsFile != "" {
			_ = os.WriteFile(argsFile, []byte(strings.Join(os.Args, "\n")), 0o600)
		}
		if os.Getenv(ProvidersEnv) != "" {
			config.refuse = "providers_env_inherited"
		}
		os.Exit(serveFake(os.Stdin, os.Stdout, config))
	}
	internalextension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	// A copy of this race-enabled binary that a test starts, such as a native
	// runtime, exits without the race runtime's 1 s exit sleep.
	_ = os.Setenv("GORACE", "atexit_sleep_ms=0")
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

// fakeConfig steers the fake provider.
type fakeConfig struct {
	bearer    string
	hosts     []string
	lifetime  time.Duration // expiresAt = clock() + lifetime
	clock     func() time.Time
	absent    bool   // answer credential with no credential member
	refuse    string // answer credential with this refusal code
	malformed bool   // answer credential with a line the protocol refuses
	hang      bool   // read credential requests and never answer
	crash     bool   // exit without answering the first credential request
	strayID   int64  // answer credential requests under this id instead
	calls     *atomic.Int32
	callsFile string // out of process: append one line per credential call
	// exitAfterAnswer exits as soon as the first credential answer is written.
	exitAfterAnswer bool
	// initializeFile, out of process, receives the initialize request line;
	// initialized, in process, receives it too.
	initializeFile string
	initialized    chan<- string
	// refuseInitialize refuses initialize with this message.
	refuseInitialize string
}

func serveFake(in io.Reader, out io.Writer, config fakeConfig) int {
	if config.clock == nil {
		config.clock = time.Now
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxCredentialLineBytes)
	write := func(response registry.CredentialResponse) {
		line, _ := json.Marshal(response)
		_, _ = out.Write(append(line, '\n'))
	}
	for scanner.Scan() {
		request, err := registry.ParseCredentialRequest(scanner.Bytes())
		if err != nil {
			return 3
		}
		switch request.Op {
		case registry.CredentialOpInitialize:
			if config.initializeFile != "" {
				_ = os.WriteFile(config.initializeFile, scanner.Bytes(), 0o600)
			}
			if config.initialized != nil {
				config.initialized <- scanner.Text()
			}
			if config.refuseInitialize != "" {
				write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: false, Error: &registry.CredentialRefusal{Code: "initialize_refused", Message: config.refuseInitialize}})
				continue
			}
			payload, _ := json.Marshal(registry.CredentialInitializeResult{ProtocolVersion: 1, ProviderName: "fake", Capabilities: []string{registry.CapabilityCredentialV1}})
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
		case registry.CredentialOpShutdown:
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true})
			return 0
		case registry.CredentialOpCredential:
			params, _ := registry.ParseCredentialParams(request.Payload)
			if config.calls != nil {
				config.calls.Add(1)
			}
			if config.callsFile != "" {
				file, err := os.OpenFile(config.callsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = file.WriteString(params.Purpose + "\n")
					_ = file.Close()
				}
			}
			switch {
			case config.hang:
				continue
			case config.crash:
				return 4
			case config.strayID != 0:
				_, _ = fmt.Fprintf(out, `{"protocolVersion":1,"id":%d,"ok":true,"payload":{}}`+"\n", config.strayID)
			case config.malformed:
				_, _ = fmt.Fprintf(out, `{"protocolVersion":1,"id":%d,"ok":true,"payload":{"credential":{"bearer":"a b","expiresAt":"x","hosts":[]}}}`+"\n", request.ID)
			case config.refuse != "":
				write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: false, Error: &registry.CredentialRefusal{Code: config.refuse, Message: "The account that owns this workspace is suspended."}})
			case config.absent:
				write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: json.RawMessage(`{}`)})
			default:
				credential := registry.Credential{
					Bearer:    config.bearer + "-" + params.Purpose,
					ExpiresAt: config.clock().Add(config.lifetime).UTC().Format(time.RFC3339),
					Hosts:     config.hosts,
				}
				payload, _ := json.Marshal(registry.CredentialResult{Credential: &credential})
				write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
			}
			if config.exitAfterAnswer {
				return 0
			}
		}
	}
	return 0
}

// inProcess returns an opener that serves config over pipes and counts how
// many times the provider was opened.
func inProcess(t *testing.T, config fakeConfig, opens *atomic.Int32) Opener {
	t.Helper()
	return func(ctx context.Context) (*Session, error) {
		opens.Add(1)
		requestReader, requestWriter := io.Pipe()
		responseReader, responseWriter := io.Pipe()
		go func() {
			serveFake(requestReader, responseWriter, config)
			_ = responseWriter.Close()
			_, _ = io.Copy(io.Discard, requestReader)
		}()
		session := Connect(requestWriter, responseReader)
		if _, err := session.Initialize(ctx, ""); err != nil {
			return nil, err
		}
		return session, nil
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	target, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestPurposesFollowTheInvocationProviders(t *testing.T) {
	t.Parallel()
	for providers, want := range map[string]string{
		"": "", "install": "read", "publish": "publish", "install,publish": "publish,read",
		"publish,install,install": "publish,read", "deploy": "",
	} {
		var list []string
		if providers != "" {
			list = strings.Split(providers, ",")
		}
		if got := strings.Join(Purposes(list), ","); got != want {
			t.Errorf("Purposes(%q) = %q, want %q", providers, got, want)
		}
	}
}

// With the providers flag off nothing is resolved, nothing is launched and
// no request is served: every consumer stays on its native credentials.
func TestFlagOffLaunchesNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "providers-are-opt-in", "flag-off-launches-nothing")
	declaring := providerExtension("@fixture/credentials", "/does/not/exist", nil)
	if broker := New(t.TempDir(), []*extension.ExtensionDescription{declaring, providerExtension("@fixture/other", "/nope", nil)}, nil, "", io.Discard); broker != nil {
		t.Fatalf("New without providers = %v; want no broker and no resolution", broker)
	}
	var opens atomic.Int32
	publishOnly := NewBroker(Purposes([]string{runner.InvocationProviderPublish}), inProcess(t, fakeConfig{bearer: "b", hosts: []string{"put.putnami.dev"}}, &opens))
	bearer, served, err := publishOnly.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/x"))
	if bearer != "" || served || err != nil || opens.Load() != 0 {
		t.Fatalf("a purpose the flag did not enable: bearer=%q served=%v err=%v opens=%d", bearer, served, err, opens.Load())
	}
	restore := publishOnly.InstallRead()
	restore()
	var nilBroker *Broker
	if nilBroker.Enabled(registry.PurposeRead) || nilBroker.Close() != nil {
		t.Fatal("a nil broker serves nothing and closes cleanly")
	}
}

// With the flag on and no loaded extension declaring the command, there is no
// broker and no error.
func TestProviderAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "one-provider-or-none", "absent-provider-stays-native")
	plain := &extension.ExtensionDescription{Name: "@fixture/plain", Version: "1.0.0", Commands: map[string]string{"build": "Build"}}
	if broker := New(t.TempDir(), []*extension.ExtensionDescription{plain}, []string{runner.InvocationProviderInstall}, "--providers", io.Discard); broker != nil {
		t.Fatalf("New with no declaring extension = %v; want no broker", broker)
	}
}

// Two declaring extensions fail the first credential a consumer asks for,
// naming both and the source of the choice; building the broker does not
// fail, so a command that needs no credential still runs.
func TestTwoDeclaringExtensionsFailTheFirstCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "one-provider-or-none", "two-declarers-fail-the-first-download")
	first := providerExtension("@fixture/a", "/a", nil)
	second := providerExtension("@fixture/b", "/b", nil)
	broker := New(t.TempDir(), []*extension.ExtensionDescription{first, second}, []string{runner.InvocationProviderInstall}, "PUTNAMI_PROVIDERS", io.Discard)
	if broker == nil {
		t.Fatal("two declaring extensions built no broker")
	}
	t.Cleanup(func() { _ = broker.Close() })
	_, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/x"))
	if served || !errors.Is(err, extension.ErrProviderAmbiguous) {
		t.Fatalf("two declaring extensions: served=%v err=%v, want ErrProviderAmbiguous", served, err)
	}
	for _, want := range []string{"@fixture/a", "@fixture/b", "PUTNAMI_PROVIDERS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the ambiguity error does not name %s: %v", want, err)
		}
	}
}

// Many requests to many hosts, some concurrent, make one credential call per
// purpose while the answer is valid, and the bearer reaches only its hosts.
func TestOneCallPerPurposeAcrossHostsAndRequests(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "bearer-only-for-its-hosts", "one-call-per-purpose")
	var opens, calls atomic.Int32
	broker := NewBroker([]string{registry.PurposeRead, registry.PurposePublish}, inProcess(t, fakeConfig{
		bearer: "pat", hosts: []string{"put.putnami.dev", "registry.example.com:8443"}, lifetime: time.Hour, calls: &calls,
	}, &opens))
	t.Cleanup(func() { _ = broker.Close() })
	targets := []string{
		"https://put.putnami.dev/a", "https://put.putnami.dev/b", "https://registry.example.com:8443/c",
		"https://registry.example.com/d", "https://evil.example/put.putnami.dev", "http://put.putnami.dev/e",
	}
	var wait sync.WaitGroup
	for round := 0; round < 8; round++ {
		for _, raw := range targets {
			wait.Add(1)
			go func(raw string) {
				defer wait.Done()
				bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, raw))
				if err != nil {
					t.Errorf("%s: %v", raw, err)
					return
				}
				wantServed := raw != "https://registry.example.com/d" && raw != "https://evil.example/put.putnami.dev" && raw != "http://put.putnami.dev/e"
				if served != wantServed || (served && bearer != "pat-read") || (!served && bearer != "") {
					t.Errorf("%s: bearer=%q served=%v, want served=%v", raw, bearer, served, wantServed)
				}
			}(raw)
		}
	}
	wait.Wait()
	if calls.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("read over %d requests made %d credential calls and %d opens, want 1 and 1", 8*len(targets), calls.Load(), opens.Load())
	}
	bearer, served, err := broker.Bearer(context.Background(), registry.PurposePublish, mustURL(t, "https://put.putnami.dev/p"))
	if err != nil || !served || bearer != "pat-publish" || calls.Load() != 2 || opens.Load() != 1 {
		t.Fatalf("publish: bearer=%q served=%v err=%v calls=%d opens=%d", bearer, served, err, calls.Load(), opens.Load())
	}
}

// A cached credential is renewed before it expires: at most a minute before,
// and at most halfway through the lifetime it arrived with.
func TestExpiringCredentialIsRefreshedBeforeUse(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "bearer-only-for-its-hosts", "refresh-before-expiry")
	clock := &fakeClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	var opens, calls atomic.Int32
	config := fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: 10 * time.Minute, clock: clock.Now, calls: &calls}
	broker := NewBroker([]string{registry.PurposeRead}, inProcess(t, config, &opens), WithClock(clock.Now))
	t.Cleanup(func() { _ = broker.Close() })
	target := mustURL(t, "https://put.putnami.dev/x")
	use := func() {
		t.Helper()
		if _, served, err := broker.Bearer(context.Background(), registry.PurposeRead, target); err != nil || !served {
			t.Fatalf("served=%v err=%v", served, err)
		}
	}
	use()
	clock.Advance(8*time.Minute + 59*time.Second)
	use()
	if calls.Load() != 1 {
		t.Fatalf("a credential a minute from its refresh instant was fetched again: %d calls", calls.Load())
	}
	clock.Advance(time.Second)
	use()
	if calls.Load() != 2 {
		t.Fatalf("a credential within a minute of expiry was used without a refresh: %d calls", calls.Load())
	}

	short := fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: 30 * time.Second, clock: clock.Now, calls: &calls}
	shortBroker := NewBroker([]string{registry.PurposeRead}, inProcess(t, short, &opens), WithClock(clock.Now))
	t.Cleanup(func() { _ = shortBroker.Close() })
	calls.Store(0)
	for _, step := range []time.Duration{0, 14 * time.Second, time.Second} {
		clock.Advance(step)
		if _, _, err := shortBroker.Bearer(context.Background(), registry.PurposeRead, target); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("a 30-second credential is renewed halfway, 15 seconds in: %d calls", calls.Load())
	}

	expired := fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: -time.Second, clock: clock.Now}
	expiredBroker := NewBroker([]string{registry.PurposeRead}, inProcess(t, expired, &opens), WithClock(clock.Now))
	t.Cleanup(func() { _ = expiredBroker.Close() })
	if _, served, err := expiredBroker.Bearer(context.Background(), registry.PurposeRead, target); served || err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an already-expired credential: served=%v err=%v", served, err)
	}
}

// A refusal fails every request of its purpose with its bounded code,
// whatever the host, and holds for the process; absence is an ordinary answer
// that serves nothing.
func TestRefusalSurfacesItsCodeAndAbsenceServesNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "refusal-never-falls-back", "refusal-fails-every-request-of-its-purpose")
	var opens, calls atomic.Int32
	clock := &fakeClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	refusing := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{refuse: "account_suspended", calls: &calls}, &opens), WithClock(clock.Now))
	t.Cleanup(func() { _ = refusing.Close() })
	target := mustURL(t, "https://put.putnami.dev/x")
	for _, raw := range []string{"https://put.putnami.dev/x", "https://third-party.example/archive.tgz", "https://put.putnami.dev/y"} {
		_, served, err := refusing.Bearer(context.Background(), registry.PurposeRead, mustURL(t, raw))
		var refusal *RefusalError
		if served || !errors.As(err, &refusal) || refusal.Code != "account_suspended" || !strings.Contains(err.Error(), "account_suspended") {
			t.Fatalf("refusal for %s: served=%v err=%v", raw, served, err)
		}
		clock.Advance(FailureBackoff)
	}
	if calls.Load() != 1 {
		t.Fatalf("a refusal was asked %d times, want once", calls.Load())
	}

	absent := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{absent: true, calls: &calls}, &opens))
	t.Cleanup(func() { _ = absent.Close() })
	calls.Store(0)
	for range 3 {
		if bearer, served, err := absent.Bearer(context.Background(), registry.PurposeRead, target); bearer != "" || served || err != nil {
			t.Fatalf("absence: bearer=%q served=%v err=%v", bearer, served, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("absence was asked %d times, want once", calls.Load())
	}

	none := NewBroker([]string{registry.PurposeRead}, func(context.Context) (*Session, error) { return nil, nil })
	if _, served, err := none.Bearer(context.Background(), registry.PurposeRead, target); served || err != nil {
		t.Fatalf("no provider: served=%v err=%v", served, err)
	}
}

// A provider that breaks the protocol or stops answering fails the request;
// it never degrades to another credential.
func TestProviderFailureIsAnError(t *testing.T) {
	t.Parallel()
	var opens atomic.Int32
	target := mustURL(t, "https://put.putnami.dev/x")
	malformed := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{malformed: true}, &opens))
	t.Cleanup(func() { _ = malformed.Close() })
	if _, served, err := malformed.Bearer(context.Background(), registry.PurposeRead, target); served || err == nil || !strings.Contains(err.Error(), "invalid credential response") {
		t.Fatalf("malformed answer: served=%v err=%v", served, err)
	}
	hanging := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{hang: true}, &opens), WithOpTimeout(50*time.Millisecond))
	t.Cleanup(func() { _ = hanging.Close() })
	if _, served, err := hanging.Bearer(context.Background(), registry.PurposeRead, target); served || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging provider: served=%v err=%v", served, err)
	}
	failing := NewBroker([]string{registry.PurposeRead}, func(context.Context) (*Session, error) { return nil, errors.New("cannot start") })
	if _, _, err := failing.Bearer(context.Background(), registry.PurposeRead, target); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("unstartable provider: %v", err)
	}
	// A caller that gives up caches nothing: the next caller asks again.
	patient := NewBroker([]string{registry.PurposeRead}, func(ctx context.Context) (*Session, error) { return nil, ctx.Err() })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := patient.Bearer(canceled, registry.PurposeRead, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled caller: %v", err)
	}
	if _, served, err := patient.Bearer(context.Background(), registry.PurposeRead, target); served || err != nil {
		t.Fatalf("the caller after a canceled one: served=%v err=%v", served, err)
	}
	closed := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{bearer: "b", hosts: []string{"put.putnami.dev"}}, &opens))
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := closed.Bearer(context.Background(), registry.PurposeRead, target); err == nil {
		t.Fatal("a closed broker served a credential it never fetched")
	}
}

// The first consumer: registry downloads ask the provider's read credential
// through AuthorizeRegistryRequest. The bearer goes only to the hosts it
// names; any other host stays on the host-keyed seam, and a refusal fails the
// download instead of falling back.
func TestAuthorizeRegistryRequestUsesTheReadCredentialOnlyForItsHosts(t *testing.T) {
	spectest.Proves(t, "cli/credential-provider", "bearer-only-for-its-hosts", "bearer-reaches-only-its-hosts")
	var opens, calls atomic.Int32
	broker := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: time.Hour, calls: &calls}, &opens))
	t.Cleanup(func() { _ = broker.Close() })
	restore := broker.InstallRead()
	t.Cleanup(restore)
	original := internalextension.ResolveRegistryToken
	var hostKeyed []string
	internalextension.ResolveRegistryToken = func(host string) (string, string) {
		hostKeyed = append(hostKeyed, host)
		return "host-keyed", ""
	}
	t.Cleanup(func() { internalextension.ResolveRegistryToken = original })

	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	for range 5 {
		inScope := request("https://put.putnami.dev/putnami/cli/download")
		if missing, err := internalextension.AuthorizeRegistryRequest(inScope); err != nil || missing != "" || inScope.Header.Get("Authorization") != "Bearer pat-read" {
			t.Fatalf("in scope: missing=%q err=%v", missing, err)
		}
	}
	outOfScope := request("https://mirror.example.com/putnami/cli/download")
	if missing, err := internalextension.AuthorizeRegistryRequest(outOfScope); err != nil || missing != "" || outOfScope.Header.Get("Authorization") != "Bearer host-keyed" {
		t.Fatalf("out of scope: missing=%q err=%v header=%q", missing, err, outOfScope.Header.Get("Authorization"))
	}
	if strings.Join(hostKeyed, ",") != "mirror.example.com" || calls.Load() != 1 {
		t.Fatalf("host-keyed seam asked for %v; provider calls %d", hostKeyed, calls.Load())
	}

	refusing := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{refuse: "account_suspended"}, &opens))
	t.Cleanup(func() { _ = refusing.Close() })
	restoreRefusing := refusing.InstallRead()
	refused := request("https://put.putnami.dev/putnami/cli/download")
	missing, err := internalextension.AuthorizeRegistryRequest(refused)
	restoreRefusing()
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Code != "account_suspended" || missing != "" || refused.Header.Get("Authorization") != "" {
		t.Fatalf("refusal: missing=%q err=%v header=%q", missing, err, refused.Header.Get("Authorization"))
	}
	if strings.Join(hostKeyed, ",") != "mirror.example.com" {
		t.Fatalf("a refusal fell back to the host-keyed seam: %v", hostKeyed)
	}

	restore()
	native := request("https://put.putnami.dev/putnami/cli/download")
	if _, err := internalextension.AuthorizeRegistryRequest(native); err != nil || native.Header.Get("Authorization") != "Bearer host-keyed" {
		t.Fatalf("with the provider removed: err=%v header=%q", err, native.Header.Get("Authorization"))
	}
}

// providerExtension declares the credential-provider command, launched as
// command with env.
func providerExtension(name, command string, env map[string]string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: name, Version: "1.0.0", Path: filepath.Dir(command),
		Commands: map[string]string{registry.CredentialProviderCommand: "Credentials"},
		Jobs: map[string]*extension.JobDefinition{registry.CredentialProviderCommand: {
			Name: registry.CredentialProviderCommand, Command: command, Env: env,
		}},
	}
}

// The engine resolves the declaring extension, launches its command as a
// real process, negotiates, and asks once per purpose however many requests
// need the credential. The provider does not inherit the providers variable.
func TestNewLaunchesTheDeclaringExtensionOutOfProcess(t *testing.T) {
	t.Setenv(ProvidersEnv, "install")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(t.TempDir(), "calls")
	declaring := providerExtension("@fixture/credentials", executable, map[string]string{
		fakeProviderEnv: "1", "FAKE_BEARER": "pat_out_of_process", "FAKE_HOSTS": "put.putnami.dev", "FAKE_CALLS_FILE": callsFile,
	})
	plain := &extension.ExtensionDescription{Name: "@fixture/plain", Version: "1.0.0", Commands: map[string]string{"build": "Build"}}
	broker := New(t.TempDir(), []*extension.ExtensionDescription{plain, declaring}, []string{runner.InvocationProviderInstall}, "--providers", os.Stderr)
	if broker == nil {
		t.Fatal("New built no broker for one declaring extension")
	}
	if _, err := os.Stat(callsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New launched the provider before any credential was needed: %v", err)
	}
	for _, raw := range []string{"https://put.putnami.dev/a", "https://put.putnami.dev/b", "https://other.example/c"} {
		bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if want := raw != "https://other.example/c"; served != want || (served && bearer != "pat_out_of_process-read") {
			t.Fatalf("%s: served=%v", raw, served)
		}
	}
	if _, served, err := broker.Bearer(context.Background(), registry.PurposePublish, mustURL(t, "https://put.putnami.dev/p")); served || err != nil {
		t.Fatalf("publish was not enabled: served=%v err=%v", served, err)
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(callsFile)
	if err != nil || string(recorded) != "read\n" {
		t.Fatalf("provider credential calls = %q, %v; want exactly one read", recorded, err)
	}
}

// initializeOverPipes serves config in process and runs Initialize with
// runCredential. It returns the initialize line the provider read, empty when
// none reached it, and the error Initialize returned.
func initializeOverPipes(t *testing.T, config fakeConfig, runCredential string) (string, error) {
	t.Helper()
	lines := make(chan string, 1)
	config.initialized = lines
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveFake(requestReader, responseWriter, config)
		_ = responseWriter.Close()
		_, _ = io.Copy(io.Discard, requestReader)
	}()
	session := Connect(requestWriter, responseReader)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := session.Initialize(ctx, runCredential)
	_ = session.Close()
	<-served
	select {
	case line := <-lines:
		return line, err
	default:
		return "", err
	}
}

// Without a run credential the initialize line has no runCredential field.
// With one, the line carries it and is the protocol's valid run-credential
// fixture. Both offer credential-v1 and publication-v1.
func TestInitializeCarriesTheRunCredentialOnlyWhenHeld(t *testing.T) {
	t.Parallel()
	for runCredential, want := range map[string]string{
		"":                           `{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1","publication-v1"]}}`,
		"prc_fixture-run-credential": `{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"capabilities":["credential-v1","publication-v1"],"runCredential":"prc_fixture-run-credential"}}`,
	} {
		line, err := initializeOverPipes(t, fakeConfig{}, runCredential)
		if err != nil || line != want {
			t.Errorf("run credential %q: initialize line\n%s\nwant\n%s\nerr %v", runCredential, line, want, err)
		}
	}
}

// A malformed run credential never reaches the provider: Initialize fails
// before it writes a line, and its error does not quote the value.
func TestAMalformedRunCredentialIsNeverSent(t *testing.T) {
	t.Parallel()
	for _, runCredential := range []string{"prc secret", "prc_\xff", strings.Repeat("a", registry.MaxRunCredentialBytes+1)} {
		line, err := initializeOverPipes(t, fakeConfig{}, runCredential)
		if err == nil || line != "" || strings.Contains(err.Error(), runCredential) {
			t.Errorf("a malformed run credential of %d bytes: initialize line %q, err %v", len(runCredential), line, err)
		}
	}
}

// A provider that refuses initialize and echoes the run credential in its
// message does not get the value into the engine's error.
func TestARefusedInitializeNeverQuotesTheRunCredential(t *testing.T) {
	t.Parallel()
	const secret = "prc_refused_run_credential"
	_, err := initializeOverPipes(t, fakeConfig{refuseInitialize: "no run matches " + secret}, secret)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("a refused initialize: %v", err)
	}
}

// New hands the run credential to the provider it launches in the initialize
// request and nowhere else: the provider's environment is the one it gets
// without a run credential, and no entry of it carries the value.
func TestNewHandsTheRunCredentialOnlyThroughInitialize(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "credential-provider-initialize-carries-it")
	t.Parallel()
	const secret = "prc_out_of_process_run_credential"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	initializeFile, envFile := filepath.Join(dir, "initialize"), filepath.Join(dir, "env")
	declaring := providerExtension("@fixture/credentials", executable, map[string]string{
		fakeProviderEnv: "1", "FAKE_BEARER": "pat", "FAKE_HOSTS": "put.putnami.dev",
		"FAKE_INITIALIZE_FILE": initializeFile, "FAKE_ENV_FILE": envFile,
	})
	workspace := t.TempDir()
	launch := func(options ...Option) (initialize, env string) {
		t.Helper()
		broker := New(workspace, []*extension.ExtensionDescription{declaring}, []string{runner.InvocationProviderInstall}, "--providers", io.Discard, options...)
		if bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/a")); !served || err != nil || bearer != "pat-read" {
			t.Fatalf("served=%v err=%v", served, err)
		}
		if err := broker.Close(); err != nil {
			t.Fatal(err)
		}
		initializeLine, err := os.ReadFile(initializeFile)
		if err != nil {
			t.Fatal(err)
		}
		environment, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(initializeLine), string(environment)
	}
	plainInitialize, plainEnv := launch()
	runInitialize, runEnv := launch(WithRunCredential(secret))
	if strings.Contains(plainInitialize, "runCredential") {
		t.Errorf("initialize without a run credential names one: %s", plainInitialize)
	}
	if !strings.Contains(runInitialize, `"runCredential":"`+secret+`"`) {
		t.Errorf("the provider did not receive the run credential in initialize: %s", runInitialize)
	}
	if strings.Contains(runEnv, secret) {
		t.Error("the run credential reached the provider's environment")
	}
	if runEnv != plainEnv {
		t.Errorf("the run credential changed the provider's environment:\n%s\nwant\n%s", runEnv, plainEnv)
	}
}

// A crash, a hang past the op timeout or a failed start answers for
// FailureBackoff; the first request after it asks again, closing the ended
// session and starting the provider anew. Within the backoff nothing is
// started again.
func TestAProviderFailureIsRetriedAfterTheBackoff(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "refusal-never-falls-back", "failure-retries-after-the-backoff")
	target := mustURL(t, "https://put.putnami.dev/x")
	for name, failure := range map[string]fakeConfig{
		"crash":        {crash: true},
		"hang":         {hang: true},
		"failed start": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
			var opens, healthyOpens atomic.Int32
			crashing := inProcess(t, failure, &healthyOpens)
			healthy := inProcess(t, fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: time.Hour, clock: clock.Now}, &healthyOpens)
			open := func(ctx context.Context) (*Session, error) {
				if opens.Add(1) > 1 {
					return healthy(ctx)
				}
				if failure.crash || failure.hang {
					return crashing(ctx)
				}
				return nil, errors.New("cannot start")
			}
			// One op timeout serves the hang and the healthy exchange after it, so it
			// stays long enough for a loaded machine to answer the healthy one.
			broker := NewBroker([]string{registry.PurposeRead}, open, WithClock(clock.Now), WithOpTimeout(2*time.Second))
			t.Cleanup(func() { _ = broker.Close() })
			for _, step := range []time.Duration{0, 0, FailureBackoff - time.Second} {
				clock.Advance(step)
				if _, served, err := broker.Bearer(context.Background(), registry.PurposeRead, target); served || err == nil {
					t.Fatalf("within the backoff: served=%v err=%v", served, err)
				}
			}
			if opens.Load() != 1 {
				t.Fatalf("the provider started %d times within the backoff, want once", opens.Load())
			}
			clock.Advance(time.Second)
			bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, target)
			if err != nil || !served || bearer != "pat-read" || opens.Load() != 2 {
				t.Fatalf("after the backoff: bearer=%q served=%v err=%v opens=%d", bearer, served, err, opens.Load())
			}
		})
	}
}

// A waiter whose context ends while another caller's exchange is in flight
// returns at once and changes nothing: the exchange in flight answers, and
// its answer is cached for the next caller.
func TestACanceledWaiterLeavesTheCacheUntouched(t *testing.T) {
	t.Parallel()
	var opens, calls atomic.Int32
	healthy := inProcess(t, fakeConfig{bearer: "pat", hosts: []string{"put.putnami.dev"}, lifetime: time.Hour, calls: &calls}, &opens)
	entered, release := make(chan struct{}), make(chan struct{})
	broker := NewBroker([]string{registry.PurposeRead}, func(ctx context.Context) (*Session, error) {
		close(entered)
		<-release
		return healthy(ctx)
	})
	t.Cleanup(func() { _ = broker.Close() })
	target := mustURL(t, "https://put.putnami.dev/x")
	first := make(chan error, 1)
	go func() {
		_, _, err := broker.Bearer(context.Background(), registry.PurposeRead, target)
		first <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, _, err := broker.Bearer(ctx, registry.PurposeRead, target)
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the canceled waiter = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the canceled waiter did not return while another exchange was in flight")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the caller in flight: %v", err)
	}
	bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, target)
	if err != nil || !served || bearer != "pat-read" || calls.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("after the canceled waiter: bearer=%q served=%v err=%v calls=%d opens=%d", bearer, served, err, calls.Load(), opens.Load())
	}
}

// An answer to a request the session never sent ends the session: the call
// waiting fails, and so does every later call.
func TestAnAnswerToAnUnsentRequestEndsTheSession(t *testing.T) {
	t.Parallel()
	var opens atomic.Int32
	session, err := inProcess(t, fakeConfig{strayID: 99}, &opens)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Credential(context.Background(), registry.PurposeRead); err == nil || !strings.Contains(err.Error(), "never sent") {
		t.Fatalf("an answer to id 99 = %v, want the session to end naming the unsent request", err)
	}
	if !session.ended() {
		t.Fatal("the session survived an answer to a request it never sent")
	}
	if _, err := session.Credential(context.Background(), registry.PurposeRead); err == nil || !strings.Contains(err.Error(), "never sent") {
		t.Fatalf("a call after the session ended = %v", err)
	}
}

// A provider that writes its answer and exits at once is still heard: once
// the process is reaped, the reader gets a grace to drain its output.
func TestAnAnswerWrittenJustBeforeExitIsHeard(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "refusal-never-falls-back", "late-answer-before-exit-is-heard")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(withoutEnv(os.Environ(), ProvidersEnv), fakeProviderEnv+"=1", "FAKE_BEARER=pat", "FAKE_HOSTS=put.putnami.dev", "FAKE_EXIT_AFTER_ANSWER=1")
	for attempt := range 8 {
		session, err := Spawn(context.Background(), LaunchSpec{Command: executable, Env: env}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Initialize(context.Background(), ""); err != nil {
			_ = session.Close()
			t.Fatalf("attempt %d: initialize: %v", attempt, err)
		}
		credential, err := session.Credential(context.Background(), registry.PurposeRead)
		_ = session.Close()
		if err != nil || credential == nil || credential.Bearer != "pat-read" {
			t.Fatalf("attempt %d: the answer written before exit was lost: %v", attempt, err)
		}
	}
}

// A provider that exits while a process it started still holds its output
// ends the session within seconds, not at the caller's deadline, and the
// process it left behind is killed.
func TestAProviderExitIsSeenWhileItsChildHoldsItsOutput(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "refusal-never-falls-back", "exit-is-seen-while-a-child-holds-output")
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	provider := fixtureproc.Write(t, filepath.Join(dir, "provider"), fixtureproc.Program{HoldOutput: release, ReadLines: 1})
	session, err := Spawn(context.Background(), LaunchSpec{Command: provider, Env: os.Environ()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	holder := fixtureproc.Holder(t, release)
	// A copy that outlived a failed test lets go instead of waiting a minute.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := time.Now()
	_, err = session.Initialize(ctx, "")
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "output stayed open") {
		t.Fatalf("initialize after the provider exited = %v, want the session to end naming the held output", err)
	}
	// The copy also holds stderr, so Wait returns only after WaitDelay; the
	// bound leaves room for that and a loaded machine. The fault waits out
	// the one-minute deadline.
	if elapsed > 30*time.Second {
		t.Fatalf("the session ended %s after the provider exited, want within seconds", elapsed)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); proctree.ProcessAlive(holder); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the process the provider left holding its output (pid %d) outlived the session", holder)
		}
	}
}
