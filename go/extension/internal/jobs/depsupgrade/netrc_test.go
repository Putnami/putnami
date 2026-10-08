package depsupgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
)

// privateOrigin is an https module origin that serves the canary channel of
// every module only to login "ci" with password "secret". It answers an
// anonymous request with its anonymous status, a wrong credential with 401,
// and any other path with 404.
type privateOrigin struct {
	server *httptest.Server
	host   string

	mu        sync.Mutex
	anonymous int
}

const privateChannelVersion = "v0.0.0-20260928052023-c2cd30187"

func newPrivateOrigin(t *testing.T, anonymousStatus int) *privateOrigin {
	t.Helper()
	origin := &privateOrigin{}
	origin.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, ok := r.BasicAuth()
		switch {
		case !ok:
			origin.mu.Lock()
			origin.anonymous++
			origin.mu.Unlock()
			w.WriteHeader(anonymousStatus)
		case login != "ci" || password != "secret":
			w.WriteHeader(http.StatusUnauthorized)
		case !strings.HasSuffix(r.URL.Path, "/@v/canary.info"):
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte(`{"Version":"` + privateChannelVersion + `"}`))
		}
	}))
	t.Cleanup(origin.server.Close)
	origin.host = strings.TrimPrefix(origin.server.URL, "https://")
	return origin
}

func (o *privateOrigin) anonymousRequests() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.anonymous
}

// channelUpgrade is an upgrade that resolves the canary channel against
// originURL, with the job environment environ and a client that trusts the
// certificate of trust.
func channelUpgrade(t *testing.T, originURL string, trust *httptest.Server, environ ...string) (*upgrade, *jobtest.Recorder) {
	t.Helper()
	j, rec := optionsJob(t, environ...)
	f := newFetcher(j)
	if trust != nil {
		transport := trust.Client().Transport
		f.client.Transport = transport
		f.probe.Transport = transport
	}
	return &upgrade{
		Job:      j,
		Options:  Options{Version: "canary", Origin: originURL},
		fetcher:  f,
		resolved: map[string]string{},
	}, rec
}

func writeNetrc(t *testing.T, content string) string {
	t.Helper()
	return jobtest.WriteFile(t, t.TempDir(), ".netrc", content)
}

// A private origin serves its channel to the credential the go command would
// send it, and hides it from an anonymous request.
func TestChannelQuerySendsTheNetrcCredential(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "channel-query-credentials", "a-channel-behind-basic-auth-resolves-with-a-netrc-entry")
	origin := newPrivateOrigin(t, http.StatusNotFound)

	netrc := writeNetrc(t, "machine other.example login x password y\nmachine "+origin.host+"\n  login ci\n  password secret\n")
	u, rec := channelUpgrade(t, origin.server.URL, origin.server, "NETRC="+netrc)
	if !u.reportResolved("canary", []string{"go.putnami.dev/api", "go.putnami.dev/app"}) {
		t.Fatalf("the channel did not resolve with a netrc entry:\n%s", rec.Transcript())
	}
	if !rec.Contains("go.putnami.dev/*  " + privateChannelVersion + "  revision c2cd30187") {
		t.Errorf("the resolved channel is not reported:\n%s", rec.Transcript())
	}
	if n := origin.anonymousRequests(); n != 0 {
		t.Errorf("%d request(s) went out without the credential", n)
	}

	for name, environ := range map[string][]string{
		// The home's netrc file is the default, as for the go command.
		"home netrc": {homeVariable() + "=" + filepath.Dir(netrc)},
		// The entry for the longest path prefix wins over the host's.
		"path prefix": {"NETRC=" + writeNetrc(t,
			"machine "+origin.host+" login ci password stale\n"+
				"machine "+origin.host+"/go.putnami.dev login ci password secret\n")},
	} {
		u, rec := channelUpgrade(t, origin.server.URL, origin.server, environ...)
		if _, ok := u.targetVersion("go.putnami.dev/api"); !ok {
			t.Errorf("%s: the channel did not resolve:\n%s", name, rec.Transcript())
		}
	}
}

// Without a credential the refusal names what is missing, not the channel.
func TestAnonymousRefusalNamesTheMissingCredential(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "channel-query-credentials", "an-anonymous-refusal-names-the-missing-credential")
	home := t.TempDir()
	withoutHost := writeNetrc(t, "machine other.example login x password y\n")
	unreadable := t.TempDir()

	for _, tc := range []struct {
		name            string
		anonymousStatus int
		environ         func(host string) []string
		want            func(host string) string
	}{
		{
			"no netrc entry", http.StatusNotFound,
			func(string) []string { return []string{homeVariable() + "=" + home} },
			func(host string) string {
				return "does not serve that channel to an anonymous request (HTTP 404): no credential for " + host + " in " + filepath.Join(home, ".netrc")
			},
		},
		{
			"NETRC names a file without the host", http.StatusUnauthorized,
			func(string) []string { return []string{"NETRC=" + withoutHost} },
			func(host string) string {
				return "refused an anonymous request (HTTP 401): no credential for " + host + " in " + withoutHost
			},
		},
		{
			"GOAUTH turns netrc off", http.StatusForbidden,
			func(host string) []string {
				return []string{"NETRC=" + writeNetrc(t, "machine "+host+" login ci password secret\n"), "GOAUTH=off"}
			},
			func(host string) string {
				return "refused an anonymous request (HTTP 403): no credential for " + host + ": GOAUTH does not include netrc"
			},
		},
		{
			"no home", http.StatusNotFound,
			func(string) []string { return nil },
			func(host string) string {
				return "(HTTP 404): no credential for " + host + ": no home directory names a netrc file"
			},
		},
		{
			"unreadable netrc", http.StatusNotFound,
			func(string) []string { return []string{"NETRC=" + unreadable} },
			func(host string) string {
				return "(HTTP 404): no credential for " + host + ": cannot read " + unreadable + ": "
			},
		},
		{
			"a refused credential is not a missing one", http.StatusNotFound,
			func(host string) []string {
				return []string{"NETRC=" + writeNetrc(t, "machine "+host+" login ci password stale\n")}
			},
			func(string) string { return "answered HTTP 401" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := newPrivateOrigin(t, tc.anonymousStatus)
			u, rec := channelUpgrade(t, origin.server.URL, origin.server, tc.environ(origin.host)...)
			if _, ok := u.targetVersion("go.putnami.dev/api"); ok {
				t.Fatalf("the channel resolved:\n%s", rec.Transcript())
			}
			u.emitMissing("go.putnami.dev/api")
			want := `Cannot resolve channel "canary" for go.putnami.dev/api: proxy ` + origin.host + " "
			if !rec.Contains(want) || !rec.Contains(tc.want(origin.host)) {
				t.Errorf("output does not contain %q and %q:\n%s", want, tc.want(origin.host), rec.Transcript())
			}
		})
	}
}

// A credential never follows a redirect from https to plain http.
func TestChannelQueryRefusesARedirectOffHTTPS(t *testing.T) {
	var (
		mu      sync.Mutex
		reached bool
	)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{"Version":"` + privateChannelVersion + `"}`))
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(secure.Close)
	host := strings.TrimPrefix(secure.URL, "https://")

	u, rec := channelUpgrade(t, secure.URL, secure, "NETRC="+writeNetrc(t, "machine "+host+" login ci password secret\n"))
	if version, ok := u.targetVersion("go.putnami.dev/api"); ok {
		t.Fatalf("the channel resolved through a redirect off https to %s:\n%s", version, rec.Transcript())
	}
	mu.Lock()
	defer mu.Unlock()
	if reached {
		t.Error("the redirect off https was followed")
	}
}

// The GOAUTH the go command applies includes its env file.
func TestGoCommandAuthReadsTheGoEnvFile(t *testing.T) {
	goBinary := jobtest.RequireGo(t)
	envFile := jobtest.WriteFile(t, t.TempDir(), "go.env", "GOAUTH=off\n")
	j, _ := optionsJob(t, "PATH="+filepath.Dir(goBinary), "GOENV="+envFile)
	j.GoBinary = goBinary
	if got := goCommandAuth(j); got != "off" {
		t.Errorf("goCommandAuth = %q, want the env file's %q", got, "off")
	}
	j.GoBinary = filepath.Join(t.TempDir(), "missing-go")
	j.Env.Set("GOAUTH", "netrc")
	if got := goCommandAuth(j); got != "netrc" {
		t.Errorf("goCommandAuth without a go command = %q, want the environment's %q", got, "netrc")
	}
}

// The go command sends a netrc credential over https only.
func TestPlainHTTPChannelQueryStaysAnonymous(t *testing.T) {
	var (
		mu         sync.Mutex
		authorized bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorized = authorized || r.Header.Get("Authorization") != ""
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	u, rec := channelUpgrade(t, server.URL, nil, "NETRC="+writeNetrc(t, "machine "+host+" login ci password secret\n"))
	if _, ok := u.targetVersion("go.putnami.dev/api"); ok {
		t.Fatal("the channel resolved")
	}
	u.emitMissing("go.putnami.dev/api")
	mu.Lock()
	defer mu.Unlock()
	if authorized {
		t.Error("a credential went out over plain http")
	}
	if want := "proxy " + host + " does not serve that channel (HTTP 404)"; !rec.Contains(want) {
		t.Errorf("output does not contain %q:\n%s", want, rec.Transcript())
	}
}

func TestParseNetrc(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       []netrcEntry
	}{
		{"one line", "machine a login u password p", []netrcEntry{{"a", "u", "p"}}},
		{"across lines", "machine a\nlogin u\npassword p\n", []netrcEntry{{"a", "u", "p"}}},
		{"crlf", "machine a\r\nlogin u\r\npassword p\r\n", []netrcEntry{{"a", "u", "p"}}},
		{"incomplete entry dropped", "machine a login u\nmachine b login v password q", []netrcEntry{{"b", "v", "q"}}},
		{
			"macdef body skipped to the empty line",
			"macdef init\nmachine hidden login x password y\n\nmachine a login u password p",
			[]netrcEntry{{"a", "u", "p"}},
		},
		{"nothing after default", "machine a login u password p\ndefault\nmachine b login v password q", []netrcEntry{{"a", "u", "p"}}},
		{"empty", "", nil},
	} {
		if got := parseNetrc(tc.data); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseNetrc = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNetrcLookup(t *testing.T) {
	credentials := netrcCredentials{entries: parseNetrc(
		"machine proxy.test login host password h\n" +
			"machine https://proxy.test/go/ login scoped password s\n" +
			"machine proxy.test login second password x\n")}
	for location, want := range map[string]string{
		"proxy.test/go/mod/@v/canary.info": "scoped",
		"proxy.test/other/@latest":         "host",
		"proxy.test":                       "host",
		"proxy.test:8443/go/x":             "",
		"other.test/go":                    "",
	} {
		login, _, ok := credentials.lookup(location)
		if login != want || ok != (want != "") {
			t.Errorf("lookup(%q) = %q, %v; want %q", location, login, ok, want)
		}
	}
}

func TestGoAuthUsesNetrc(t *testing.T) {
	for goAuth, want := range map[string]bool{
		"":                     true,
		"netrc":                true,
		"git /src; netrc":      true,
		"off":                  false,
		"git /src":             false,
		"/usr/bin/credhelper":  false,
		"netrc-helper --flags": false,
	} {
		if got := goAuthUsesNetrc(goAuth); got != want {
			t.Errorf("goAuthUsesNetrc(%q) = %v, want %v", goAuth, got, want)
		}
	}
}

func TestNetrcPath(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		goos    string
		environ []string
		want    string
	}{
		{"linux", []string{"NETRC=/pinned/netrc", "HOME=" + home}, "/pinned/netrc"},
		{"linux", []string{"HOME=" + home, "USERPROFILE=/elsewhere"}, filepath.Join(home, ".netrc")},
		{"windows", []string{"USERPROFILE=" + home, "HOME=/elsewhere"}, filepath.Join(home, ".netrc")},
		{"linux", nil, ""},
	} {
		got, err := netrcPath(workspacejob.NewEnv(tc.environ), tc.goos)
		if got != tc.want || err != nil {
			t.Errorf("netrcPath(%v, %s) = %q, %v; want %q", tc.environ, tc.goos, got, err, tc.want)
		}
	}
	legacy := jobtest.WriteFile(t, home, "_netrc", "")
	if got, err := netrcPath(workspacejob.NewEnv([]string{"USERPROFILE=" + home}), "windows"); got != legacy || err != nil {
		t.Errorf("netrcPath = %q, %v; want the existing %q", got, err, legacy)
	}
}

// A Windows _netrc whose existence cannot be established leaves the go
// command without a credential, so the job sends none either, even when a
// usable .netrc sits beside it.
func TestUnstatableWindowsNetrcSendsNoCredential(t *testing.T) {
	home := t.TempDir()
	jobtest.WriteFile(t, home, ".netrc", "machine proxy.test login ci password secret\n")
	legacy := filepath.Join(home, "_netrc")
	if err := os.Symlink("_netrc", legacy); err != nil {
		t.Skipf("cannot create a symlink loop here: %v", err)
	}

	credentials := loadNetrcOn(workspacejob.NewEnv([]string{"USERPROFILE=" + home}), "", "windows")
	if login, _, ok := credentials.lookup("proxy.test"); ok {
		t.Fatalf("the .netrc beside an unstatable _netrc supplied login %q", login)
	}
	if missing := credentials.missing("proxy.test"); !strings.HasPrefix(missing, "no credential for proxy.test: cannot read "+legacy+": ") ||
		strings.Count(missing, legacy) != 1 {
		t.Errorf("missing = %q, want the _netrc stat error naming the path once", missing)
	}
}

// homeVariable is the variable os.UserHomeDir reads on this platform.
func homeVariable() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}
