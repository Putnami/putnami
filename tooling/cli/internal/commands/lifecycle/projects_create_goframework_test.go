package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// goProxyRequest is a request a fake module proxy got.
type goProxyRequest struct {
	path      string
	userAgent string
	// login and password are the basic authentication credentials, empty when
	// the request carried none.
	login, password string
}

// fakeGoProxy is a module proxy that answers every request with one status and
// body, and records the requests it gets.
type fakeGoProxy struct {
	*httptest.Server
	mu       sync.Mutex
	requests []goProxyRequest
}

// newFakeGoProxy starts a module proxy that answers status and body, over TLS
// when tls is set.
func newFakeGoProxy(t *testing.T, status int, body string, tls bool) *fakeGoProxy {
	t.Helper()
	proxy := &fakeGoProxy{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, _ := r.BasicAuth()
		proxy.mu.Lock()
		proxy.requests = append(proxy.requests, goProxyRequest{
			path: r.URL.Path, userAgent: r.Header.Get("User-Agent"), login: login, password: password,
		})
		proxy.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	if tls {
		proxy.Server = httptest.NewTLSServer(handler)
	} else {
		proxy.Server = httptest.NewServer(handler)
	}
	t.Cleanup(proxy.Close)
	return proxy
}

// got returns the requests the proxy got so far.
func (p *fakeGoProxy) got() []goProxyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// frameworkLatest is the answer of a proxy that serves the Go framework at
// version.
func frameworkLatest(version string) string {
	return `{"Version":"` + version + `","Time":"2026-09-20T10:00:00Z"}`
}

// goLookupEnv is the environment a lookup test gives the lookup: values, over
// no go env file, a netrc file that does not exist and GOPROXY=off, so no
// setting of the host running the test reaches the lookup and a test that
// names no proxy never reaches the network.
func goLookupEnv(t *testing.T, values map[string]string) func(string) string {
	t.Helper()
	env := map[string]string{
		"GOENV":   "off",
		"GOPROXY": "off",
		"NETRC":   filepath.Join(t.TempDir(), "no-netrc"),
	}
	maps.Copy(env, values)
	return func(key string) string { return env[key] }
}

// The version the proxies answer is the one the new project pins; a 404 or a
// 410 passes the query to the next proxy, which is how an anonymous consumer
// whose first proxy does not serve the framework still gets it; any other
// failure does only after a "|".
func TestResolveGoFrameworkVersionWalksGOPROXYTheWayTheGoCommandDoes(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-resolves-the-go-framework-version",
		"the-lookup-passes-a-404-to-the-next-proxy-as-go-does")
	const version = "v0.1.0-feb66161"
	answers := newFakeGoProxy(t, http.StatusOK, frameworkLatest(version), false)
	notFound := newFakeGoProxy(t, http.StatusNotFound, "not found", false)
	gone := newFakeGoProxy(t, http.StatusGone, "gone", false)
	broken := newFakeGoProxy(t, http.StatusInternalServerError, "boom", false)
	locked := newFakeGoProxy(t, http.StatusUnauthorized, "who are you", false)

	tests := []struct {
		name    string
		goProxy func() string
		// want is the proxy that answers, nil when the lookup fails.
		want *fakeGoProxy
		// wantErr lists what the failure names.
		wantErr []string
		// unasked are the proxies the lookup must not ask.
		unasked []*fakeGoProxy
	}{
		{name: "the first proxy answers", goProxy: func() string { return answers.URL + "," + broken.URL },
			want: answers, unasked: []*fakeGoProxy{broken}},
		{name: "a 404 passes to the next proxy", goProxy: func() string { return notFound.URL + "," + answers.URL },
			want: answers},
		{name: "a 410 passes to the next proxy", goProxy: func() string { return gone.URL + "," + answers.URL },
			want: answers},
		{name: "a 500 after a comma stops", goProxy: func() string { return broken.URL + "," + answers.URL },
			wantErr: []string{broken.URL, "HTTP 500", "Set GOPROXY"}, unasked: []*fakeGoProxy{answers}},
		{name: "a 500 after a pipe passes on", goProxy: func() string { return broken.URL + "|" + answers.URL },
			want: answers},
		{name: "a 401 names the netrc file", goProxy: func() string { return locked.URL + "," + answers.URL },
			wantErr: []string{locked.URL, "HTTP 401", "netrc"}, unasked: []*fakeGoProxy{answers}},
		{name: "every proxy answering 404 fails naming each", goProxy: func() string { return notFound.URL + "," + gone.URL },
			wantErr: []string{notFound.URL, "HTTP 404", gone.URL, "HTTP 410", "go.putnami.dev/app"}},
		{name: "off stops", goProxy: func() string { return "off" },
			wantErr: []string{"GOPROXY=off"}},
		{name: "a proxy that is not http is not asked", goProxy: func() string { return "file:///srv/goproxy," + answers.URL },
			wantErr: []string{"file:///srv/goproxy", "not an http or https URL"}, unasked: []*fakeGoProxy{answers}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := map[*fakeGoProxy]int{}
			for _, proxy := range tt.unasked {
				before[proxy] = len(proxy.got())
			}
			got, from, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
				goLookupEnv(t, map[string]string{"GOPROXY": tt.goProxy()}))
			if tt.want != nil {
				if err != nil || got != version || from != tt.want.URL {
					t.Fatalf("resolveGoFrameworkVersion() = %q from %q, %v; want %s from %s", got, from, err, version, tt.want.URL)
				}
			} else {
				if err == nil {
					t.Fatalf("resolveGoFrameworkVersion() = %q from %q, want an error", got, from)
				}
				if got != "" {
					t.Errorf("resolveGoFrameworkVersion() version = %q on failure, want none", got)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to name %q", err, want)
					}
				}
			}
			for _, proxy := range tt.unasked {
				if n := len(proxy.got()); n != before[proxy] {
					t.Errorf("proxy %s was asked %d times, want never", proxy.URL, n-before[proxy])
				}
			}
		})
	}
}

// The lookup asks the module proxy protocol's @latest endpoint for the
// framework's module path, with the Putnami CLI's User-Agent.
func TestResolveGoFrameworkVersionAsksTheLatestEndpoint(t *testing.T) {
	proxy := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	if _, _, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL + "/"})); err != nil {
		t.Fatalf("resolveGoFrameworkVersion: %v", err)
	}
	want := []goProxyRequest{{path: "/go.putnami.dev/app/@latest", userAgent: "putnami-cli/dev"}}
	if got := proxy.got(); !slices.Equal(got, want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
}

// A GOPROXY entry without a scheme is an https proxy, and a path on the
// proxy URL prefixes the module path.
func TestResolveGoFrameworkVersionKeepsTheProxyPath(t *testing.T) {
	proxy := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	if _, from, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL + "/mirror/go"})); err != nil || from != proxy.URL+"/mirror/go" {
		t.Fatalf("resolveGoFrameworkVersion() from %q, %v; want %s/mirror/go", from, err, proxy.URL)
	}
	if got := proxy.got(); len(got) != 1 || got[0].path != "/mirror/go/go.putnami.dev/app/@latest" {
		t.Fatalf("requests = %+v, want one for /mirror/go/go.putnami.dev/app/@latest", got)
	}
}

// The environment sets a Go setting first, the go env file second, and Go's
// default applies after both.
func TestResolveGoFrameworkVersionReadsTheGoEnvFile(t *testing.T) {
	fromFile := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.2.0"), false)
	fromEnv := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.3.0"), false)
	envFile := filepath.Join(t.TempDir(), "env")
	writeRawFile(t, envFile, "# written by go env -w\ngoproxy=https://ignored.example\nGOPROXY="+fromFile.URL)

	version, _, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOENV": envFile, "GOPROXY": ""}))
	if err != nil || version != "v0.2.0" {
		t.Fatalf("with GOPROXY in the go env file: %q, %v; want v0.2.0", version, err)
	}
	version, _, err = resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOENV": envFile, "GOPROXY": fromEnv.URL}))
	if err != nil || version != "v0.3.0" {
		t.Fatalf("with GOPROXY in the environment too: %q, %v; want v0.3.0", version, err)
	}

	want := []goProxyEntry{{url: "https://proxy.golang.org"}, {url: "direct"}}
	if got := parseGoProxyList(defaultGoProxy); !slices.Equal(got, want) {
		t.Fatalf("Go's default GOPROXY parses as %+v, want %+v", got, want)
	}
}

// Without GOENV, the go env file is go/env under the user configuration
// directory, as the go command reads it.
func TestReadGoEnvFileDefaultsToTheUserConfigDirectory(t *testing.T) {
	home := hometest.Temp(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	configDir, err := os.UserConfigDir()
	if err != nil || !strings.HasPrefix(configDir, home) {
		t.Fatalf("os.UserConfigDir() = %q, %v; want a directory under %s", configDir, err, home)
	}
	writeRawFile(t, filepath.Join(configDir, "go", "env"), "GOPRIVATE=example.com/*")

	if got := readGoEnvFile(func(string) string { return "" })["GOPRIVATE"]; got != "example.com/*" {
		t.Fatalf("GOPRIVATE from the default go env file = %q, want example.com/*", got)
	}
	if got := readGoEnvFile(func(key string) string {
		if key == "GOENV" {
			return "off"
		}
		return ""
	}); got != nil {
		t.Fatalf("GOENV=off reads %v, want no go env file", got)
	}
}

// fakeOrigin is a module's origin over TLS: the pages its go-import meta tags
// are read from, keyed by request path, and a module proxy under /proxy that
// answers @latest for every module.
type fakeOrigin struct {
	*httptest.Server
	mu       sync.Mutex
	pages    map[string]string
	requests []goProxyRequest
}

// newFakeOrigin starts an origin whose /proxy module proxy answers version.
func newFakeOrigin(t *testing.T, version string) *fakeOrigin {
	t.Helper()
	origin := &fakeOrigin{pages: map[string]string{}}
	origin.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, _ := r.BasicAuth()
		origin.mu.Lock()
		defer origin.mu.Unlock()
		origin.requests = append(origin.requests, goProxyRequest{
			path: r.URL.RequestURI(), userAgent: r.Header.Get("User-Agent"), login: login, password: password,
		})
		switch {
		case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, "/@latest"):
			_, _ = io.WriteString(w, frameworkLatest(version))
		case r.URL.Query().Get("go-get") == "1" && origin.pages[r.URL.Path] != "":
			_, _ = io.WriteString(w, origin.pages[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	return origin
}

// host is the origin's host and port, the first element of its module paths.
func (o *fakeOrigin) host() string { return o.Listener.Addr().String() }

// module is the module path app on the origin.
func (o *fakeOrigin) module() string { return o.host() + "/app" }

// serve makes path answer an HTML page with one go-import tag per content.
func (o *fakeOrigin) serve(path string, contents ...string) {
	var page strings.Builder
	page.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta http-equiv=\"Content-Type\" content=\"text/html; charset=utf-8\"/>\n")
	for _, content := range contents {
		page.WriteString(`<meta name="go-import" content="` + content + "\">\n")
	}
	page.WriteString("</head>\n<body>\nRedirecting&hellip;\n</body>\n</html>\n")
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pages[path] = page.String()
}

// got returns the requests the origin got so far.
func (o *fakeOrigin) got() []goProxyRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.requests)
}

// A module GONOPROXY or GOPRIVATE matches, and a GOPROXY that reaches
// "direct", resolve from the module's origin, as the go command reads it: the
// go-import meta tag, then the module proxy a "mod" tag names, with the netrc
// entry for the origin's host.
func TestResolveModuleLatestReadsTheOriginAsGoDoes(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-resolves-the-go-framework-version",
		"the-lookup-reads-a-private-module-from-its-origin-as-go-does")
	origin := newFakeOrigin(t, "v0.9.0-0123abcd")
	origin.serve("/app", origin.module()+" mod "+origin.URL+"/proxy")
	unasked := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.0.1"), false)
	notFound := newFakeGoProxy(t, http.StatusNotFound, "", false)
	netrc := filepath.Join(t.TempDir(), "netrc")
	writeRawFile(t, netrc, "machine 127.0.0.1 login alice password s3cret-origin")

	tests := []struct {
		name   string
		values map[string]string
	}{
		{"GOPROXY=direct", map[string]string{"GOPROXY": "direct"}},
		{"GOPRIVATE matches the module", map[string]string{"GOPROXY": unasked.URL, "GOPRIVATE": origin.host()}},
		{"GONOPROXY matches the module even with GOPROXY=off", map[string]string{"GOPROXY": "off", "GONOPROXY": origin.module()}},
		{"a 404 reaches direct", map[string]string{"GOPROXY": notFound.URL + ",direct"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(origin.got())
			values := map[string]string{"NETRC": netrc}
			maps.Copy(values, tt.values)
			version, source, err := resolveModuleLatest(context.Background(), origin.Client(), goLookupEnv(t, values), origin.module())
			if err != nil || version != "v0.9.0-0123abcd" || source != "direct ("+origin.URL+"/proxy)" {
				t.Fatalf("resolveModuleLatest() = %q from %q, %v; want v0.9.0-0123abcd from direct (%s/proxy)", version, source, err, origin.URL)
			}
			want := []goProxyRequest{
				{path: "/app?go-get=1", userAgent: "putnami-cli/dev", login: "alice", password: "s3cret-origin"},
				{path: "/proxy/" + origin.module() + "/@latest", userAgent: "putnami-cli/dev", login: "alice", password: "s3cret-origin"},
			}
			if got := origin.got()[before:]; !slices.Equal(got, want) {
				t.Fatalf("the origin got %+v, want %+v", got, want)
			}
		})
	}
	if got := unasked.got(); len(got) != 0 {
		t.Fatalf("GOPROXY's proxy got %d requests for a private module, want none", len(got))
	}
}

// The origin is followed only to a module proxy over https that a tag the go
// command would pick names.
func TestResolveModuleLatestRefusesAnOriginItCannotFollow(t *testing.T) {
	tests := []struct {
		name    string
		pages   map[string][]string
		wantErr string
	}{
		{"a git repository", map[string][]string{"/app": {"{module} git https://git.example/app"}},
			"names a git repository"},
		{"a module proxy over http", map[string][]string{"/app": {"{module} mod http://{host}/proxy"}},
			"not an https URL"},
		{"no page", map[string][]string{}, "HTTP 404"},
		{"no tag for the module", map[string][]string{"/app": {"{host}/other mod {url}/proxy"}},
			"no go-import tag covers"},
		{"two tags of version control", map[string][]string{"/app": {"{module} git https://a.example", "{module} hg https://b.example"}},
			"several go-import tags"},
		{"a prefix page that disagrees", map[string][]string{
			"/app": {"{host} mod {url}/proxy"},
			"/":    {"{host} mod {url}/elsewhere"},
		}, "disagree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origin := newFakeOrigin(t, "v1.0.0")
			replacer := strings.NewReplacer("{module}", origin.module(), "{host}", origin.host(), "{url}", origin.URL)
			for path, contents := range tt.pages {
				for i := range contents {
					contents[i] = replacer.Replace(contents[i])
				}
				origin.serve(path, contents...)
			}
			version, _, err := resolveModuleLatest(context.Background(), origin.Client(),
				goLookupEnv(t, map[string]string{"GOPROXY": "direct"}), origin.module())
			if err == nil || version != "" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolveModuleLatest() = %q, %v; want no version and an error containing %q", version, err, tt.wantErr)
			}
		})
	}

	// A prefix page that states the same tag is followed.
	origin := newFakeOrigin(t, "v1.0.0")
	origin.serve("/app", origin.host()+" mod "+origin.URL+"/proxy")
	origin.serve("/", origin.host()+" mod "+origin.URL+"/proxy")
	if version, _, err := resolveModuleLatest(context.Background(), origin.Client(),
		goLookupEnv(t, map[string]string{"GOPROXY": "direct"}), origin.module()); err != nil || version != "v1.0.0" {
		t.Fatalf("with an agreeing prefix page: %q, %v; want v1.0.0", version, err)
	}
}

// The framework is private for a consumer whose GOPRIVATE matches it: the
// lookup reads go.putnami.dev, never GOPROXY's proxies, and a failure names
// the netrc file and GONOPROXY=none.
func TestResolveGoFrameworkVersionReadsGoPutnamiDevForAPrivateFramework(t *testing.T) {
	proxy := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	var requested []string
	var mu sync.Mutex
	offline := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, r.URL.String())
		mu.Unlock()
		return nil, errors.New("offline")
	})}
	envFile := filepath.Join(t.TempDir(), "env")
	writeRawFile(t, envFile, "GOPRIVATE=go.putnami.dev/*")

	version, _, err := resolveGoFrameworkVersion(context.Background(), offline,
		goLookupEnv(t, map[string]string{"GOENV": envFile, "GOPROXY": proxy.URL}))
	if err == nil || version != "" {
		t.Fatalf("resolveGoFrameworkVersion() = %q, %v; want an error", version, err)
	}
	for _, want := range []string{"go.putnami.dev/app", "offline", "netrc", "GONOPROXY=none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if want := []string{"https://go.putnami.dev/app?go-get=1"}; !slices.Equal(requested, want) {
		t.Fatalf("requests = %v, want %v", requested, want)
	}
	if got := proxy.got(); len(got) != 0 {
		t.Fatalf("GOPROXY's proxy got %d requests, want none", len(got))
	}

	// GONOPROXY=none hands the module back to GOPROXY's proxies.
	version, _, err = resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOENV": envFile, "GOPROXY": proxy.URL, "GONOPROXY": "none"}))
	if err != nil || version != "v0.4.2" {
		t.Fatalf("with GONOPROXY=none: %q, %v; want v0.4.2 from GOPROXY", version, err)
	}
}

// stubModuleOriginCredential records the hosts create asks @putnami/cloud to
// refresh a credential for, and answers outcome.
func stubModuleOriginCredential(t *testing.T, outcome registrycred.Outcome) *[]string {
	t.Helper()
	var asked []string
	original := ensureModuleOriginCredential
	ensureModuleOriginCredential = func(_ context.Context, workspaceRoot, host string) registrycred.Outcome {
		asked = append(asked, workspaceRoot+" "+host)
		return outcome
	}
	t.Cleanup(func() { ensureModuleOriginCredential = original })
	return &asked
}

// A framework read from its origin gets its netrc credential refreshed first:
// the entry a past sign-in wrote expires within hours, and the origin answers
// 401 to an expired credential.
func TestRefreshGoFrameworkCredentialAsksForThePrivateOrigin(t *testing.T) {
	asked := stubModuleOriginCredential(t, registrycred.Outcome{
		Kind: registrycred.KindMaterialized, Level: "debug", Message: "refreshed the registry credential for go.putnami.dev",
	})
	envFile := filepath.Join(t.TempDir(), "env")
	writeRawFile(t, envFile, "GOPRIVATE=go.putnami.dev/*")

	message := refreshGoFrameworkCredential(context.Background(), "/ws", goLookupEnv(t, map[string]string{"GOENV": envFile}))
	if want := []string{"/ws go.putnami.dev"}; !slices.Equal(*asked, want) {
		t.Fatalf("asked = %v, want %v", *asked, want)
	}
	if message != "refreshed the registry credential for go.putnami.dev" {
		t.Fatalf("message = %q, want the outcome's message", message)
	}
}

// A framework read through GOPROXY's proxies needs no credential, so no cloud
// is asked for one, and GONOPROXY=none hands a private framework back to them.
func TestRefreshGoFrameworkCredentialAsksNothingForAProxiedFramework(t *testing.T) {
	asked := stubModuleOriginCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	envFile := filepath.Join(t.TempDir(), "env")
	writeRawFile(t, envFile, "GOPRIVATE=go.putnami.dev/*")

	for name, values := range map[string]map[string]string{
		"public":         {"GOPROXY": "https://proxy.golang.org"},
		"GONOPROXY=none": {"GOENV": envFile, "GONOPROXY": "none"},
	} {
		if message := refreshGoFrameworkCredential(context.Background(), "/ws", goLookupEnv(t, values)); message != "" {
			t.Errorf("%s: message = %q, want none", name, message)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("asked = %v, want no call", *asked)
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseGoImportTags(t *testing.T) {
	page := `<!DOCTYPE html><html><head>
<META NAME="go-import" CONTENT="example.com/a git https://git.example/a">
<meta name="go-source" content="example.com/a https://src.example">
<meta name="go-import" content="example.com/b git https://git.example/b">
<meta name="go-import" content="example.com/a mod https://proxy.example">
<meta name="go-import" content="example.com/c git https://git.example/c subdir">
<meta name="go-import" content="malformed">
</head><body>
<meta name="go-import" content="example.com/d mod https://ignored.example">
</body></html>`
	want := []goImportTag{
		{prefix: "example.com/a", vcs: "mod", repoRoot: "https://proxy.example"},
		{prefix: "example.com/b", vcs: "git", repoRoot: "https://git.example/b"},
		{prefix: "example.com/c", vcs: "git", repoRoot: "https://git.example/c"},
	}
	if got := parseGoImportTags([]byte(page)); !slices.Equal(got, want) {
		t.Fatalf("parseGoImportTags() = %+v, want %+v", got, want)
	}
}

// The resolved version is written into go.mod, so an answer that is not a
// canonical module version, or that is too large to be one, is refused.
func TestResolveGoFrameworkVersionRefusesAnAnswerItCannotPin(t *testing.T) {
	oversized := frameworkLatest("v1.0.0") + strings.Repeat(" ", maxGoProxyResponseSize)
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"an oversized answer", oversized, "exceeds"},
		{"not JSON", "v1.0.0\n", "invalid character"},
		{"no version", `{"Time":"2026-09-20T10:00:00Z"}`, "not a canonical module version"},
		{"a query, not a version", frameworkLatest("latest"), "not a canonical module version"},
		{"a short version", frameworkLatest("v1.0"), "not a canonical module version"},
		{"a version that carries a go.mod line", frameworkLatest(`v1.0.0\nreplace go.putnami.dev/app => ../app`), "not a canonical module version"},
		{"a version with a space", frameworkLatest("v1.0.0 // indirect"), "not a canonical module version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := newFakeGoProxy(t, http.StatusOK, tt.body, false)
			version, _, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
				goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL}))
			if err == nil || version != "" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolveGoFrameworkVersion() = %q, %v; want no version and an error containing %q", version, err, tt.wantErr)
			}
		})
	}
	for _, version := range []string{"v0.0.1", "v12.3.40", "v0.1.0-feb66161", "v1.2.3-rc.1+build.5", "v0.0.0-20260920100000-abcdefabcdef"} {
		if !canonicalModuleVersion.MatchString(version) {
			t.Errorf("canonicalModuleVersion refuses %q, a version the go command accepts", version)
		}
	}
}

// Credentials in a GOPROXY URL reach the proxy, as with the go command, and
// never the output: not in the name of the proxy that answered, nor in a
// failure.
func TestResolveGoFrameworkVersionNeverPrintsProxyCredentials(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-resolves-the-go-framework-version",
		"the-lookup-sends-the-credentials-go-would-and-prints-none")
	const secret = "hunter2-proxy-secret"
	withCredentials := func(rawURL string) string {
		return strings.Replace(rawURL, "://", "://gopher:"+secret+"@", 1)
	}

	answers := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	_, from, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOPROXY": withCredentials(answers.URL)}))
	if err != nil || from != answers.URL {
		t.Fatalf("resolveGoFrameworkVersion() from %q, %v; want %s without its credentials", from, err, answers.URL)
	}
	if got := answers.got(); len(got) != 1 || got[0].login != "gopher" || got[0].password != secret {
		t.Fatalf("the proxy got %d requests, the first with login %q; want the GOPROXY URL's credentials", len(got), firstLogin(got))
	}

	refused := newFakeGoProxy(t, http.StatusServiceUnavailable, "", false)
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	for name, goProxy := range map[string]string{
		"an HTTP failure":   withCredentials(refused.URL),
		"no connection":     withCredentials(unreachable.URL),
		"a scheme not http": "ftp://gopher:" + secret + "@proxy.example",
		"a 404 then off":    withCredentials(newFakeGoProxy(t, http.StatusNotFound, "", false).URL) + ",off",
	} {
		_, _, err := resolveGoFrameworkVersion(context.Background(), http.DefaultClient,
			goLookupEnv(t, map[string]string{"GOPROXY": goProxy}))
		if err == nil {
			t.Fatalf("%s: resolveGoFrameworkVersion() succeeded, want an error", name)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gopher") {
			t.Fatalf("%s: the error prints the proxy credentials: %v", name, err)
		}
	}

	// A netrc entry reaches the proxy as Basic auth, and no failure prints it.
	const netrcLogin, netrcSecret = "netrc-login-7f3a", "netrc-secret-5c1e"
	netrc := filepath.Join(t.TempDir(), "netrc")
	writeRawFile(t, netrc, "machine 127.0.0.1 login "+netrcLogin+" password "+netrcSecret+"\n")
	for name, status := range map[string]int{
		"an HTTP failure":      http.StatusServiceUnavailable,
		"a refused credential": http.StatusUnauthorized,
		"a forbidden request":  http.StatusForbidden,
		"a 404":                http.StatusNotFound,
	} {
		proxy := newFakeGoProxy(t, status, "", true)
		_, _, err := resolveGoFrameworkVersion(context.Background(), proxy.Client(),
			goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL, "NETRC": netrc}))
		if err == nil {
			t.Fatalf("netrc, %s: resolveGoFrameworkVersion() succeeded, want an error", name)
		}
		if got := proxy.got(); len(got) != 1 || got[0].login != netrcLogin || got[0].password != netrcSecret {
			t.Fatalf("netrc, %s: the proxy got login %q in %d requests; want the netrc entry in one", name, firstLogin(got), len(got))
		}
		if strings.Contains(err.Error(), netrcSecret) || strings.Contains(err.Error(), netrcLogin) {
			t.Fatalf("netrc, %s: the error prints the netrc credentials: %v", name, err)
		}
	}
}

func firstLogin(requests []goProxyRequest) string {
	if len(requests) == 0 {
		return ""
	}
	return requests[0].login
}

// Over https, a proxy gets the netrc entry for its host, as the go command's
// default GOAUTH sends it; over http, or with GOAUTH=off, it gets none.
func TestResolveGoFrameworkVersionSendsNetrcCredentialsOverHTTPS(t *testing.T) {
	secure := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), true)
	plain := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	netrc := filepath.Join(t.TempDir(), "netrc")
	writeRawFile(t, netrc, "machine example.com login other password nope\n"+
		"machine 127.0.0.1\n  login alice\n  password s3cret-netrc\n")

	tests := []struct {
		name      string
		proxy     *fakeGoProxy
		goAuth    string
		wantLogin string
	}{
		{"https with the default GOAUTH", secure, "", "alice"},
		{"https with GOAUTH listing netrc", secure, "git /srv/repos;netrc", "alice"},
		{"https with GOAUTH=off", secure, "off", ""},
		{"https with GOAUTH not listing netrc", secure, "git /srv/repos", ""},
		{"http", plain, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.proxy.got())
			version, _, err := resolveGoFrameworkVersion(context.Background(), tt.proxy.Client(),
				goLookupEnv(t, map[string]string{"GOPROXY": tt.proxy.URL, "NETRC": netrc, "GOAUTH": tt.goAuth}))
			if err != nil || version != "v0.4.2" {
				t.Fatalf("resolveGoFrameworkVersion() = %q, %v; want v0.4.2", version, err)
			}
			got := tt.proxy.got()[before:]
			if len(got) != 1 || got[0].login != tt.wantLogin {
				t.Fatalf("the proxy got login %q in %d requests, want %q in one", firstLogin(got), len(got), tt.wantLogin)
			}
			if tt.wantLogin != "" && got[0].password != "s3cret-netrc" {
				t.Fatal("the proxy got another password than the netrc entry's")
			}
		})
	}
}

// Without NETRC, the netrc file is the one of the home directory the go
// command reads: .netrc, or _netrc on Windows.
func TestResolveGoFrameworkVersionReadsTheHomeNetrcFile(t *testing.T) {
	home := hometest.Temp(t)
	writeRawFile(t, filepath.Join(home, netrcFileName()), "machine 127.0.0.1 login alice password s3cret-netrc")
	proxy := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), true)
	getenv := goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL, "NETRC": ""})
	if _, _, err := resolveGoFrameworkVersion(context.Background(), proxy.Client(), getenv); err != nil {
		t.Fatalf("resolveGoFrameworkVersion: %v", err)
	}
	if got := proxy.got(); len(got) != 1 || got[0].login != "alice" {
		t.Fatalf("the proxy got login %q, want the home netrc entry's", firstLogin(got))
	}
}

func TestParseNetrc(t *testing.T) {
	data := strings.Join([]string{
		"machine one.example login a password 1",
		"machine two.example",
		"  login b",
		"  password 2",
		"machine incomplete.example login c",
		"macdef init",
		"machine inside.macro login m password 3",
		"",
		"machine three.example login d password 4",
		"default",
		"machine after.default login e password 5",
	}, "\n")
	want := []netrcEntry{
		{machine: "one.example", login: "a", password: "1"},
		{machine: "two.example", login: "b", password: "2"},
		{machine: "three.example", login: "d", password: "4"},
	}
	if got := parseNetrc(data); !slices.Equal(got, want) {
		t.Fatalf("parseNetrc() = %+v, want %+v", got, want)
	}
}

func TestParseGoProxyList(t *testing.T) {
	tests := []struct {
		value string
		want  []goProxyEntry
	}{
		{"", nil},
		{"off", []goProxyEntry{{url: "off"}}},
		{"direct,https://ignored.example", []goProxyEntry{{url: "direct"}}},
		{"https://a.example,https://b.example", []goProxyEntry{{url: "https://a.example"}, {url: "https://b.example"}}},
		{"https://a.example|https://b.example,direct", []goProxyEntry{
			{url: "https://a.example", fallBackOnError: true}, {url: "https://b.example"}, {url: "direct"},
		}},
		{"proxy.example/go, ,http://b.example", []goProxyEntry{{url: "https://proxy.example/go"}, {url: "http://b.example"}}},
		{"file:///srv/goproxy", []goProxyEntry{{url: "file:///srv/goproxy"}}},
	}
	for _, tt := range tests {
		if got := parseGoProxyList(tt.value); !slices.Equal(got, tt.want) {
			t.Errorf("parseGoProxyList(%q) = %+v, want %+v", tt.value, got, tt.want)
		}
	}
}

func TestMatchGoPrefixPatterns(t *testing.T) {
	tests := []struct {
		patterns string
		want     bool
	}{
		{"", false},
		{"none", false},
		{"go.putnami.dev", true},
		{"go.putnami.dev/", true},
		{"go.putnami.dev/app", true},
		{"go.putnami.dev/*", true},
		{"*.putnami.dev", true},
		{"example.com,go.putnami.dev", true},
		{"go.putnami.dev/app/extra", false},
		{"go.putnami.dev/other", false},
		{"putnami.dev", false},
		{"go.putnami", false},
	}
	for _, tt := range tests {
		if got := matchGoPrefixPatterns(tt.patterns, "go.putnami.dev/app"); got != tt.want {
			t.Errorf("matchGoPrefixPatterns(%q, go.putnami.dev/app) = %v, want %v", tt.patterns, got, tt.want)
		}
	}
}

func TestEscapeGoModulePath(t *testing.T) {
	if got := escapeGoModulePath("github.com/Azure/Go-SDK"); got != "github.com/!azure/!go-!s!d!k" {
		t.Fatalf("escapeGoModulePath() = %q", got)
	}
}

// frameworkTemplateWorkspace writes a workspace whose extension offers two
// templates: fw-app renders the Go framework version, plain does not.
func frameworkTemplateWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"fw-ws"}`)
	for name, files := range map[string]map[string]string{
		"fw-app": {"framework.txt.template": "app <%= goFrameworkVersion %>"},
		"plain":  {"readme.txt.template": "<%= projectName %>"},
	} {
		dir := filepath.Join(root, "ext", "templates", name)
		writeRawFile(t, filepath.Join(dir, "putnami.template.json"),
			`{"name":"`+name+`","description":"A template","extension":"@acme/app"}`)
		for file, content := range files {
			writeRawFile(t, filepath.Join(dir, file), content)
		}
	}
	if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	return root
}

// setGoProxyEnv points the process's Go settings at goProxy alone.
func setGoProxyEnv(t *testing.T, goProxy string) {
	t.Helper()
	t.Setenv("GOPROXY", goProxy)
	t.Setenv("GOENV", "off")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOAUTH", "off")
}

// The project a template that renders the Go framework version creates pins
// the version the consumer's module proxy answers, whatever that proxy is.
func TestProjectsCreatePinsTheFrameworkVersionTheModuleProxyAnswers(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-resolves-the-go-framework-version",
		"create-pins-the-framework-version-goproxy-answers")
	root := frameworkTemplateWorkspace(t)
	asked := stubModuleOriginCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
	notFound := newFakeGoProxy(t, http.StatusNotFound, "", false)
	answers := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.1.0-feb66161"), false)
	setGoProxyEnv(t, notFound.URL+","+answers.URL)

	stdout, err := captureStdout(t, func() error {
		return ProjectsCreate(context.Background(), root, wsproto.Load(root),
			[]string{"api", "--template", "fw-app"}, true, LifecycleEnv{Out: io.Discard})
	})
	if err != nil {
		t.Fatalf("ProjectsCreate: %v\n%s", err, stdout)
	}
	data, err := os.ReadFile(filepath.Join(root, "api", "framework.txt"))
	if err != nil || string(data) != "app v0.1.0-feb66161\n" {
		t.Fatalf("framework.txt = %q, %v; want the version the proxy answered", data, err)
	}
	if want := "Resolved go.putnami.dev/app v0.1.0-feb66161 from " + answers.URL; !strings.Contains(stdout, want) {
		t.Fatalf("verbose output = %q, want %q", stdout, want)
	}
	if got := answers.got(); len(got) != 1 || got[0].path != "/go.putnami.dev/app/@latest" {
		t.Fatalf("the proxy got %+v, want one @latest request for go.putnami.dev/app", got)
	}
	if len(*asked) != 0 {
		t.Fatalf("create asked the cloud for a credential for %v, want none for a proxied framework", *asked)
	}
}

// When no module proxy answers, create writes nothing, names every proxy it
// asked, and names the command to run again once GOPROXY is fixed.
func TestProjectsCreateWritesNothingWhenNoModuleProxyAnswers(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-resolves-the-go-framework-version",
		"create-writes-nothing-and-names-the-fix-when-no-proxy-answers")
	root := frameworkTemplateWorkspace(t)
	configPath := filepath.Join(root, wsproto.WorkspaceConfigFilename)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	notFound := newFakeGoProxy(t, http.StatusNotFound, "", false)
	broken := newFakeGoProxy(t, http.StatusBadGateway, "", false)
	setGoProxyEnv(t, notFound.URL+","+broken.URL)

	_, err = captureStdout(t, func() error {
		return ProjectsCreate(context.Background(), root, wsproto.Load(root),
			[]string{"api", "--template", "fw-app", "--path", "apps/api"}, false, LifecycleEnv{Out: io.Discard})
	})
	if err == nil {
		t.Fatal("ProjectsCreate succeeded with no module proxy answering, want an error")
	}
	for _, want := range []string{"go.putnami.dev/app", notFound.URL, "HTTP 404", broken.URL, "HTTP 502", "Set GOPROXY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if next, want := protocolcli.SuggestedNext(err), "putnami projects create api --template fw-app --path apps/api"; next != want {
		t.Errorf("suggested next = %q, want %q", next, want)
	}
	if _, statErr := os.Stat(filepath.Join(root, "apps")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("create left %s behind (stat: %v), want nothing written", filepath.Join(root, "apps"), statErr)
	}
	if after, _ := os.ReadFile(configPath); !bytes.Equal(after, config) {
		t.Errorf("workspace config = %s, want it unchanged: %s", after, config)
	}
}

// A template that does not render the Go framework version asks no proxy.
func TestProjectsCreateAsksNoProxyForATemplateWithoutTheFrameworkVersion(t *testing.T) {
	root := frameworkTemplateWorkspace(t)
	proxy := newFakeGoProxy(t, http.StatusOK, frameworkLatest("v0.4.2"), false)
	setGoProxyEnv(t, proxy.URL)

	if out, err := captureStdout(t, func() error {
		return ProjectsCreate(context.Background(), root, wsproto.Load(root),
			[]string{"docs", "--template", "plain"}, true, LifecycleEnv{Out: io.Discard})
	}); err != nil {
		t.Fatalf("ProjectsCreate: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(root, "docs", "readme.txt")); err != nil || string(data) != "docs\n" {
		t.Fatalf("readme.txt = %q, %v; want the rendered project", data, err)
	}
	if got := proxy.got(); len(got) != 0 {
		t.Fatalf("the proxy got %d requests, want none", len(got))
	}
}
