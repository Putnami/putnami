package projectsetup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// frameworkModule is the module the lookup tests resolve.
const frameworkModule = "go.putnami.dev/app"

// proxyRequest is a request a fake module proxy got.
type proxyRequest struct {
	path      string
	userAgent string
	// login and password are the basic authentication credentials, empty when
	// the request carried none.
	login, password string
}

// fakeProxy is a module proxy that answers every request with one status and
// body, and records the requests it gets.
type fakeProxy struct {
	*httptest.Server
	mu       sync.Mutex
	requests []proxyRequest
}

// newFakeProxy starts a module proxy that answers status and body, over TLS
// when tls is set.
func newFakeProxy(t *testing.T, status int, body string, tls bool) *fakeProxy {
	t.Helper()
	proxy := &fakeProxy{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, _ := r.BasicAuth()
		proxy.mu.Lock()
		proxy.requests = append(proxy.requests, proxyRequest{
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
func (p *fakeProxy) got() []proxyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// hostPort is the host and port of a test server, the netrc machine go 1.24
// and later match a request to it with.
func hostPort(server *httptest.Server) string {
	return server.Listener.Addr().String()
}

// latestAnswer is the answer of a proxy that serves a module at version.
func latestAnswer(version string) string {
	return `{"Version":"` + version + `","Time":"2026-09-20T10:00:00Z"}`
}

// newTestLookup is a lookup over client with settings, whose credentials come
// from the environment values: NETRC, HOME. A test that names no NETRC reads a
// netrc file that does not exist.
func newTestLookup(t *testing.T, client *http.Client, settings goSettings, values ...string) *lookup {
	t.Helper()
	env := workspacejob.NewEnv(append([]string{"NETRC=" + filepath.Join(t.TempDir(), "no-netrc"), "HOME=" + t.TempDir()}, values...))
	return &lookup{
		ctx:       context.Background(),
		client:    client,
		userAgent: "putnami-cli/dev",
		settings:  settings,
		credential: func(goAuth string, target *url.URL) (string, string, bool) {
			return depsupgrade.NetrcCredential(env, goAuth, target)
		},
	}
}

// The version the proxies answer is the one the project pins; a 404 or a 410
// passes the query to the next proxy, which is how an anonymous consumer whose
// first proxy does not serve the framework still gets it; any other failure
// does only after a "|". A file:// proxy without the answer is a 404.
func TestLookupWalksGOPROXYTheWayTheGoCommandDoes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"the-lookup-passes-a-404-to-the-next-proxy-as-go-does")
	const version = "v0.1.0-feb66161"
	answers := newFakeProxy(t, http.StatusOK, latestAnswer(version), false)
	notFound := newFakeProxy(t, http.StatusNotFound, "not found", false)
	gone := newFakeProxy(t, http.StatusGone, "gone", false)
	broken := newFakeProxy(t, http.StatusInternalServerError, "boom", false)
	locked := newFakeProxy(t, http.StatusUnauthorized, "who are you", false)
	fileProxy := jobtest.WriteModuleProxy(t)
	jobtest.WriteLatestInfo(t, fileProxy, frameworkModule, version)
	emptyFileProxy := jobtest.FileURL(t.TempDir())

	tests := []struct {
		name    string
		goProxy string
		// want is the source that answers, "" when the lookup fails.
		want string
		// wantErr lists what the failure names.
		wantErr []string
		// unasked are the proxies the lookup must not ask.
		unasked []*fakeProxy
	}{
		{name: "the first proxy answers", goProxy: answers.URL + "," + broken.URL, want: answers.URL, unasked: []*fakeProxy{broken}},
		{name: "a 404 passes to the next proxy", goProxy: notFound.URL + "," + answers.URL, want: answers.URL},
		{name: "a 410 passes to the next proxy", goProxy: gone.URL + "," + answers.URL, want: answers.URL},
		{name: "a 500 after a comma stops", goProxy: broken.URL + "," + answers.URL,
			wantErr: []string{broken.URL, "HTTP 500", "Set GOPROXY"}, unasked: []*fakeProxy{answers}},
		{name: "a 500 after a pipe passes on", goProxy: broken.URL + "|" + answers.URL, want: answers.URL},
		{name: "a 401 names the netrc file", goProxy: locked.URL + "," + answers.URL,
			wantErr: []string{locked.URL, "HTTP 401", "netrc"}, unasked: []*fakeProxy{answers}},
		{name: "every proxy answering 404 fails naming each", goProxy: notFound.URL + "," + gone.URL,
			wantErr: []string{notFound.URL, "HTTP 404", gone.URL, "HTTP 410", frameworkModule}},
		{name: "off stops", goProxy: "off", wantErr: []string{"GOPROXY=off"}},
		{name: "a file proxy answers", goProxy: jobtest.FileURL(fileProxy) + "," + broken.URL,
			want: jobtest.FileURL(fileProxy), unasked: []*fakeProxy{broken}},
		{name: "a file proxy without the module passes on", goProxy: emptyFileProxy + "," + answers.URL, want: answers.URL},
		{name: "a proxy of another scheme is not asked", goProxy: "ftp://proxy.example," + answers.URL,
			wantErr: []string{"ftp://proxy.example", "not an http, https or file URL"}, unasked: []*fakeProxy{answers}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := map[*fakeProxy]int{}
			for _, proxy := range tt.unasked {
				before[proxy] = len(proxy.got())
			}
			got, from, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: tt.goProxy}).resolve(frameworkModule, latestChannel)
			if tt.want != "" {
				if err != nil || got != version || from != tt.want {
					t.Fatalf("resolve() = %q from %q, %v; want %s from %s", got, from, err, version, tt.want)
				}
			} else {
				if err == nil || got != "" {
					t.Fatalf("resolve() = %q from %q, %v; want no version and an error", got, from, err)
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
// module path, with the job's User-Agent, and a path on the proxy URL
// prefixes the module path.
func TestLookupAsksTheLatestEndpointUnderTheProxyPath(t *testing.T) {
	t.Parallel()
	proxy := newFakeProxy(t, http.StatusOK, latestAnswer("v0.4.2"), false)
	if _, from, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: proxy.URL + "/"}).resolve(frameworkModule, ""); err != nil || from != proxy.URL+"/" {
		t.Fatalf("resolve() from %q, %v; want %s/", from, err, proxy.URL)
	}
	if _, from, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: proxy.URL + "/mirror/go"}).resolve(frameworkModule, ""); err != nil || from != proxy.URL+"/mirror/go" {
		t.Fatalf("resolve() from %q, %v; want %s/mirror/go", from, err, proxy.URL)
	}
	want := []proxyRequest{
		{path: "/go.putnami.dev/app/@latest", userAgent: "putnami-cli/dev"},
		{path: "/mirror/go/go.putnami.dev/app/@latest", userAgent: "putnami-cli/dev"},
	}
	if got := proxy.got(); !slices.Equal(got, want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
}

// A channel other than latest is asked as the go version query
// @v/<channel>.info; latest, and no channel, ask @latest. A proxy that does
// not serve the channel passes the query on, and when no proxy serves it the
// lookup fails naming the channel: it never answers a channel with latest.
func TestLookupOnAChannelAsksTheChannelQuery(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"the-framework-version-is-the-one-the-channel-names")
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"a-missing-channel-fails-instead-of-resolving-latest")
	const latestPath, candidatePath = "/go.putnami.dev/app/@latest", "/go.putnami.dev/app/@v/candidate.info"
	newChannelProxy := func(answers map[string]string) (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			version, ok := answers[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, latestAnswer(version))
		}))
		t.Cleanup(server.Close)
		return server, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(paths) }
	}
	for _, tt := range []struct{ channel, want, path string }{
		{"candidate", "v0.4.0", candidatePath},
		{"latest", "v0.3.0", latestPath},
		{"", "v0.3.0", latestPath},
	} {
		proxy, asked := newChannelProxy(map[string]string{latestPath: "v0.3.0", candidatePath: "v0.4.0"})
		version, source, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: proxy.URL}).resolve(frameworkModule, tt.channel)
		if err != nil || version != tt.want || source != proxy.URL {
			t.Fatalf("resolve(%q) = %q from %q, %v; want %s from %s", tt.channel, version, source, err, tt.want, proxy.URL)
		}
		if got := asked(); !slices.Equal(got, []string{tt.path}) {
			t.Fatalf("channel %q asked %v, want only %s", tt.channel, got, tt.path)
		}
	}

	onlyLatest, askedLatest := newChannelProxy(map[string]string{latestPath: "v0.3.0"})
	serves, _ := newChannelProxy(map[string]string{candidatePath: "v0.4.0"})
	version, source, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: onlyLatest.URL + "," + serves.URL}).resolve(frameworkModule, "candidate")
	if err != nil || version != "v0.4.0" || source != serves.URL {
		t.Fatalf("resolve() = %q from %q, %v; want v0.4.0 from the proxy that serves the channel", version, source, err)
	}
	version, _, err = newTestLookup(t, http.DefaultClient, goSettings{Proxy: onlyLatest.URL}).resolve(frameworkModule, "candidate")
	if err == nil || version != "" {
		t.Fatalf("resolve() = %q, %v; want an error and no version", version, err)
	}
	for _, want := range []string{frameworkModule, "on channel candidate", onlyLatest.URL, "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if got := askedLatest(); slices.Contains(got, latestPath) {
		t.Fatalf("the proxy was asked %v: a channel is never answered with latest", got)
	}
}

// fakeOrigin is a module's origin over TLS: the pages its go-import meta tags
// are read from, keyed by request path, and a module proxy under /proxy that
// answers @latest and the candidate channel for every module.
type fakeOrigin struct {
	*httptest.Server
	mu       sync.Mutex
	pages    map[string]string
	requests []proxyRequest
}

// newFakeOrigin starts an origin whose /proxy module proxy answers version.
func newFakeOrigin(t *testing.T, version string) *fakeOrigin {
	t.Helper()
	origin := &fakeOrigin{pages: map[string]string{}}
	origin.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, _ := r.BasicAuth()
		origin.mu.Lock()
		defer origin.mu.Unlock()
		origin.requests = append(origin.requests, proxyRequest{
			path: r.URL.RequestURI(), userAgent: r.Header.Get("User-Agent"), login: login, password: password,
		})
		switch {
		case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, "/@latest"):
			_, _ = io.WriteString(w, latestAnswer(version))
		case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, "/@v/candidate.info"):
			_, _ = io.WriteString(w, latestAnswer(version+"-candidate"))
		case r.URL.Query().Get("go-get") == "1" && origin.pages[r.URL.Path] != "":
			_, _ = io.WriteString(w, origin.pages[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	return origin
}

// module is the module path app on the origin.
func (o *fakeOrigin) module() string { return hostPort(o.Server) + "/app" }

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
func (o *fakeOrigin) got() []proxyRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.requests)
}

// A module GONOPROXY matches, and a GOPROXY that reaches "direct", resolve
// from the module's origin, as the go command reads it: the go-import meta
// tag, then the module proxy a "mod" tag names, with the netrc entry for the
// origin, on latest as on a channel.
func TestLookupReadsTheOriginAsGoDoes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"the-lookup-reads-a-private-module-from-its-origin-as-go-does")
	origin := newFakeOrigin(t, "v0.9.0-0123abcd")
	origin.serve("/app", origin.module()+" mod "+origin.URL+"/proxy")
	unasked := newFakeProxy(t, http.StatusOK, latestAnswer("v0.0.1"), false)
	notFound := newFakeProxy(t, http.StatusNotFound, "", false)
	netrc := jobtest.WriteFile(t, t.TempDir(), "netrc", "machine "+hostPort(origin.Server)+" login alice password s3cret-origin\n")

	tests := []struct {
		name     string
		settings goSettings
		channel  string
		want     string
	}{
		{"GOPROXY=direct", goSettings{Proxy: "direct"}, "", "v0.9.0-0123abcd"},
		{"GONOPROXY matches the module", goSettings{Proxy: unasked.URL, NoProxy: hostPort(origin.Server)}, "", "v0.9.0-0123abcd"},
		{"GONOPROXY matches the module even with GOPROXY=off", goSettings{Proxy: "off", NoProxy: origin.module()}, "", "v0.9.0-0123abcd"},
		{"a 404 reaches direct", goSettings{Proxy: notFound.URL + ",direct"}, "", "v0.9.0-0123abcd"},
		{"a channel", goSettings{Proxy: "direct"}, "candidate", "v0.9.0-0123abcd-candidate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(origin.got())
			version, source, err := newTestLookup(t, origin.Client(), tt.settings, "NETRC="+netrc).resolve(origin.module(), tt.channel)
			if err != nil || version != tt.want || source != "direct ("+origin.URL+"/proxy)" {
				t.Fatalf("resolve() = %q from %q, %v; want %s from direct (%s/proxy)", version, source, err, tt.want, origin.URL)
			}
			endpoint := "/@latest"
			if tt.channel != "" {
				endpoint = "/@v/" + tt.channel + ".info"
			}
			want := []proxyRequest{
				{path: "/app?go-get=1", userAgent: "putnami-cli/dev", login: "alice", password: "s3cret-origin"},
				{path: "/proxy/" + origin.module() + endpoint, userAgent: "putnami-cli/dev", login: "alice", password: "s3cret-origin"},
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
func TestLookupRefusesAnOriginItCannotFollow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pages   map[string][]string
		wantErr string
	}{
		{"a git repository", map[string][]string{"/app": {"{module} git https://git.example/app"}}, "names a git repository"},
		{"a module proxy over http", map[string][]string{"/app": {"{module} mod http://{host}/proxy"}}, "not an https URL"},
		{"no page", map[string][]string{}, "HTTP 404"},
		{"no tag for the module", map[string][]string{"/app": {"{host}/other mod {url}/proxy"}}, "no go-import tag covers"},
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
			replacer := strings.NewReplacer("{module}", origin.module(), "{host}", hostPort(origin.Server), "{url}", origin.URL)
			for path, contents := range tt.pages {
				replaced := make([]string, len(contents))
				for i, content := range contents {
					replaced[i] = replacer.Replace(content)
				}
				origin.serve(path, replaced...)
			}
			version, _, err := newTestLookup(t, origin.Client(), goSettings{Proxy: "direct"}).resolve(origin.module(), "")
			if err == nil || version != "" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolve() = %q, %v; want no version and an error containing %q", version, err, tt.wantErr)
			}
		})
	}

	// A prefix page that states the same tag is followed.
	origin := newFakeOrigin(t, "v1.0.0")
	origin.serve("/app", hostPort(origin.Server)+" mod "+origin.URL+"/proxy")
	origin.serve("/", hostPort(origin.Server)+" mod "+origin.URL+"/proxy")
	if version, _, err := newTestLookup(t, origin.Client(), goSettings{Proxy: "direct"}).resolve(origin.module(), ""); err != nil || version != "v1.0.0" {
		t.Fatalf("with an agreeing prefix page: %q, %v; want v1.0.0", version, err)
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The framework is private for a consumer whose GONOPROXY matches it: the
// lookup reads go.putnami.dev, never GOPROXY's proxies, and a failure names
// the netrc file and GONOPROXY=none.
func TestLookupReadsGoPutnamiDevForAPrivateFramework(t *testing.T) {
	t.Parallel()
	proxy := newFakeProxy(t, http.StatusOK, latestAnswer("v0.4.2"), false)
	var requested []string
	var mu sync.Mutex
	offline := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, r.URL.String())
		mu.Unlock()
		return nil, errors.New("offline")
	})}

	version, _, err := newTestLookup(t, offline, goSettings{Proxy: proxy.URL, NoProxy: "go.putnami.dev/*"}).resolve(frameworkModule, "")
	if err == nil || version != "" {
		t.Fatalf("resolve() = %q, %v; want an error", version, err)
	}
	for _, want := range []string{frameworkModule, "offline", "netrc", "GONOPROXY=none"} {
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
	version, _, err = newTestLookup(t, http.DefaultClient, goSettings{Proxy: proxy.URL, NoProxy: "none"}).resolve(frameworkModule, "")
	if err != nil || version != "v0.4.2" {
		t.Fatalf("with GONOPROXY=none: %q, %v; want v0.4.2 from GOPROXY", version, err)
	}
}

func TestParseGoImportTags(t *testing.T) {
	t.Parallel()
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
func TestLookupRefusesAnAnswerItCannotPin(t *testing.T) {
	t.Parallel()
	oversized := latestAnswer("v1.0.0") + strings.Repeat(" ", maxResponseSize)
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"an oversized answer", oversized, "exceeds"},
		{"not JSON", "v1.0.0\n", "invalid character"},
		{"no version", `{"Time":"2026-09-20T10:00:00Z"}`, "not a canonical module version"},
		{"a query, not a version", latestAnswer("latest"), "not a canonical module version"},
		{"a short version", latestAnswer("v1.0"), "not a canonical module version"},
		{"a version that carries a go.mod line", latestAnswer(`v1.0.0\nreplace go.putnami.dev/app => ../app`), "not a canonical module version"},
		{"a version with a space", latestAnswer("v1.0.0 // indirect"), "not a canonical module version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := newFakeProxy(t, http.StatusOK, tt.body, false)
			version, _, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: proxy.URL}).resolve(frameworkModule, "")
			if err == nil || version != "" || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolve() = %q, %v; want no version and an error containing %q", version, err, tt.wantErr)
			}
		})
	}
	for _, version := range []string{"v0.0.1", "v12.3.40", "v0.1.0-feb66161", "v1.2.3-rc.1+build.5", "v0.0.0-20260920100000-abcdefabcdef"} {
		if !canonicalVersion.MatchString(version) {
			t.Errorf("canonicalVersion refuses %q, a version the go command accepts", version)
		}
	}
}

// Credentials in a GOPROXY URL reach the proxy, as with the go command, and so
// does the netrc entry for an https proxy; neither reaches the output, not in
// the name of the proxy that answered nor in a failure.
func TestLookupNeverPrintsProxyCredentials(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"the-lookup-sends-the-credentials-go-would-and-prints-none")
	const secret = "hunter2-proxy-secret"
	withCredentials := func(rawURL string) string {
		return strings.Replace(rawURL, "://", "://gopher:"+secret+"@", 1)
	}

	answers := newFakeProxy(t, http.StatusOK, latestAnswer("v0.4.2"), false)
	_, from, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: withCredentials(answers.URL)}).resolve(frameworkModule, "")
	if err != nil || from != answers.URL {
		t.Fatalf("resolve() from %q, %v; want %s without its credentials", from, err, answers.URL)
	}
	if got := answers.got(); len(got) != 1 || got[0].login != "gopher" || got[0].password != secret {
		t.Fatalf("the proxy got %d requests, the first with login %q; want the GOPROXY URL's credentials", len(got), firstLogin(got))
	}

	refused := newFakeProxy(t, http.StatusServiceUnavailable, "", false)
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	for name, goProxy := range map[string]string{
		"an HTTP failure":     withCredentials(refused.URL),
		"no connection":       withCredentials(unreachable.URL),
		"a scheme not http":   "ftp://gopher:" + secret + "@proxy.example",
		"a 404 then off":      withCredentials(newFakeProxy(t, http.StatusNotFound, "", false).URL) + ",off",
		"a file:// with user": "file://gopher:" + secret + "@/nonexistent-proxy",
	} {
		_, _, err := newTestLookup(t, http.DefaultClient, goSettings{Proxy: goProxy}).resolve(frameworkModule, "")
		if err == nil {
			t.Fatalf("%s: resolve() succeeded, want an error", name)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gopher") {
			t.Fatalf("%s: the error prints the proxy credentials: %v", name, err)
		}
	}

	const netrcLogin, netrcSecret = "netrc-login-7f3a", "netrc-secret-5c1e"
	for name, status := range map[string]int{
		"an HTTP failure":      http.StatusServiceUnavailable,
		"a refused credential": http.StatusUnauthorized,
		"a forbidden request":  http.StatusForbidden,
		"a 404":                http.StatusNotFound,
	} {
		proxy := newFakeProxy(t, status, "", true)
		netrc := jobtest.WriteFile(t, t.TempDir(), "netrc", "machine "+hostPort(proxy.Server)+" login "+netrcLogin+" password "+netrcSecret+"\n")
		_, _, err := newTestLookup(t, proxy.Client(), goSettings{Proxy: proxy.URL}, "NETRC="+netrc).resolve(frameworkModule, "")
		if err == nil {
			t.Fatalf("netrc, %s: resolve() succeeded, want an error", name)
		}
		if got := proxy.got(); len(got) != 1 || got[0].login != netrcLogin || got[0].password != netrcSecret {
			t.Fatalf("netrc, %s: the proxy got login %q in %d requests; want the netrc entry in one", name, firstLogin(got), len(got))
		}
		if strings.Contains(err.Error(), netrcSecret) || strings.Contains(err.Error(), netrcLogin) {
			t.Fatalf("netrc, %s: the error prints the netrc credentials: %v", name, err)
		}
	}
}

func firstLogin(requests []proxyRequest) string {
	if len(requests) == 0 {
		return ""
	}
	return requests[0].login
}

// Over https, a proxy gets the netrc entry for its host and port, as the go
// command's default GOAUTH sends it, from NETRC or the home's netrc file; over
// http, or with a GOAUTH that does not list netrc, it gets none.
func TestLookupSendsNetrcCredentialsOverHTTPS(t *testing.T) {
	t.Parallel()
	secure := newFakeProxy(t, http.StatusOK, latestAnswer("v0.4.2"), true)
	plain := newFakeProxy(t, http.StatusOK, latestAnswer("v0.4.2"), false)
	entries := "machine example.com login other password nope\n" +
		"machine " + hostPort(secure.Server) + "\n  login alice\n  password s3cret-netrc\n"
	netrc := jobtest.WriteFile(t, t.TempDir(), "netrc", entries)
	home := t.TempDir()
	jobtest.WriteFile(t, home, ".netrc", entries)

	tests := []struct {
		name      string
		proxy     *fakeProxy
		goAuth    string
		values    []string
		wantLogin string
	}{
		{"https with GOAUTH=netrc", secure, "netrc", []string{"NETRC=" + netrc}, "alice"},
		{"https with the home netrc file", secure, "netrc", []string{"NETRC=", "HOME=" + home}, "alice"},
		{"https with GOAUTH listing netrc", secure, "git /srv/repos;netrc", []string{"NETRC=" + netrc}, "alice"},
		{"https with GOAUTH=off", secure, "off", []string{"NETRC=" + netrc}, ""},
		{"https with GOAUTH not listing netrc", secure, "git /srv/repos", []string{"NETRC=" + netrc}, ""},
		{"http", plain, "netrc", []string{"NETRC=" + netrc}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.proxy.got())
			l := newTestLookup(t, tt.proxy.Client(), goSettings{Proxy: tt.proxy.URL, Auth: tt.goAuth}, tt.values...)
			if version, _, err := l.resolve(frameworkModule, ""); err != nil || version != "v0.4.2" {
				t.Fatalf("resolve() = %q, %v; want v0.4.2", version, err)
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

func TestParseGoProxyList(t *testing.T) {
	t.Parallel()
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
		{defaultGoProxy, []goProxyEntry{{url: "https://proxy.golang.org"}, {url: "direct"}}},
	}
	for _, tt := range tests {
		if got := parseGoProxyList(tt.value); !slices.Equal(got, tt.want) {
			t.Errorf("parseGoProxyList(%q) = %+v, want %+v", tt.value, got, tt.want)
		}
	}
}

func TestMatchPrefixPatterns(t *testing.T) {
	t.Parallel()
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
		if got := matchPrefixPatterns(tt.patterns, frameworkModule); got != tt.want {
			t.Errorf("matchPrefixPatterns(%q, %s) = %v, want %v", tt.patterns, frameworkModule, got, tt.want)
		}
	}
}

func TestEscapeModulePath(t *testing.T) {
	t.Parallel()
	if got := escapeModulePath("github.com/Azure/Go-SDK"); got != "github.com/!azure/!go-!s!d!k" {
		t.Fatalf("escapeModulePath() = %q", got)
	}
}
