package depsupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/workspacejob"
)

// A channel is resolved PER MODULE, through that module's own
// `@v/<channel>.info` projection: impacted publication advances the channel of
// every member of a release, but each member keeps the version of the
// publication that last changed it, so two modules on one channel legitimately
// differ.
//
// Those versions are commit-stamped semver pre-releases, and asking the go
// command for "@latest" picks the lexically largest one from /@v/list rather
// than the newest publication. The origin's /@latest endpoint orders by
// publication time, so "latest" is resolved there directly, per module.

// requestTimeout bounds one request to the module origin.
const requestTimeout = 30 * time.Second

// fetcher asks the module origin for a module's channel projection, with the
// netrc credential the go command would send to that origin.
type fetcher struct {
	ctx       context.Context
	userAgent string
	env       *workspacejob.Env
	// goAuth is the GOAUTH the go command applies. It defaults to the job
	// environment's; run asks the go command, which also reads its env file.
	goAuth func() string
	// netrc is read on the first request, so it sees the job's final
	// environment.
	netrc *netrcCredentials
	// client follows redirects, as the script's `curl -L` did, except from
	// https to another scheme.
	client *http.Client
	// probe reports the status of the first answer without following a
	// redirect, as the script's `curl -w '%{http_code}'` did.
	probe *http.Client
}

func newFetcher(j *workspacejob.Job) *fetcher {
	return &fetcher{
		ctx:       j.Ctx,
		userAgent: j.UserAgent(),
		env:       j.Env,
		goAuth:    func() string { return j.Env.Get("GOAUTH") },
		client:    &http.Client{Timeout: requestTimeout, CheckRedirect: refuseDowngrade},
		probe: &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

var errHTTPStatus = errors.New("unsuccessful HTTP status")

// maxRedirects is net/http's own limit on one request's redirects.
const maxRedirects = 10

// refuseDowngrade stops a redirect chain that starts on https and leaves it,
// as the go command does, so a credential never travels in clear text.
func refuseDowngrade(request *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
		return fmt.Errorf("refused a redirect from https to %s", request.URL.Scheme)
	}
	return nil
}

// fetch returns the body at rawURL: an http(s) answer below 400, or the file a
// file:// URL names.
func (f *fetcher) fetch(rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "file":
		return os.ReadFile(fileURLPath(parsed))
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	response, err := f.get(f.client, rawURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%w %d", errHTTPStatus, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// status is the HTTP status the origin answers rawURL with, for the error
// message only, or "000" when nothing answered over HTTP.
func (f *fetcher) status(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "000"
	}
	response, err := f.get(f.probe, rawURL)
	if err != nil {
		return "000"
	}
	_ = response.Body.Close()
	return fmt.Sprintf("%03d", response.StatusCode)
}

func (f *fetcher) get(client *http.Client, rawURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", f.userAgent)
	// The go command sends a netrc credential over https only, and one the URL
	// carries wins. The client keeps the Authorization header only on a
	// redirect to the same host or a subdomain of it, and never leaves https.
	if request.URL.Scheme == "https" && request.URL.User == nil {
		if login, password, ok := f.credentials().lookup(httpsLocation(request.URL)); ok {
			request.SetBasicAuth(login, password)
		}
	}
	return client.Do(request) //nolint:gosec // G704: rawURL is on the module origin the workspace configures
}

// credentials is the job's netrc file, read once.
func (f *fetcher) credentials() netrcCredentials {
	if f.netrc == nil {
		credentials := loadNetrc(f.env, f.goAuth())
		f.netrc = &credentials
	}
	return *f.netrc
}

// missingCredential is why an https request to rawURL goes out anonymous, or
// "" when it carries a credential (one in the URL, or a netrc entry for its
// host) or is not https, where no credential ever applies.
func (f *fetcher) missingCredential(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" {
		return ""
	}
	credentials := f.credentials()
	if _, _, ok := credentials.lookup(httpsLocation(parsed)); ok {
		return ""
	}
	return credentials.missing(parsed.Host)
}

// httpsLocation is an https URL without its scheme, as the go command keys
// the credentials it read.
func httpsLocation(parsed *url.URL) string {
	return strings.TrimPrefix(parsed.String(), "https://")
}

// fileURLPath is the local path of a file:// URL.
func fileURLPath(parsed *url.URL) string {
	path := parsed.Path
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path)
}

// escapeModulePath is the module proxy's case-encoded path: every uppercase
// letter becomes "!" and its lowercase spelling, so a case-insensitive file
// system cannot serve two modules from one path.
func escapeModulePath(module string) string {
	var out strings.Builder
	for _, r := range module {
		if r >= 'A' && r <= 'Z' {
			out.WriteByte('!')
			out.WriteRune(r - 'A' + 'a')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// channelURL is the endpoint that answers selector for module: /@latest for
// "latest", which orders by publication time, and the native Go version query
// /@v/<selector>.info for any other channel.
func (u *upgrade) channelURL(module, selector string) string {
	escaped := escapeModulePath(module)
	if selector == "latest" {
		return u.Origin + "/" + escaped + "/@latest"
	}
	return u.Origin + "/" + escaped + "/@v/" + selector + ".info"
}

var urlHostPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://([^/]+)`)

// urlHost is the authority of rawURL without its user information, or rawURL
// itself when it has none (a file:// URL).
func urlHost(rawURL string) string {
	if match := urlHostPattern.FindStringSubmatch(rawURL); match != nil {
		host := match[1]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		return host
	}
	return rawURL
}

// resolveModule resolves the selector to module's own exact version.
func (u *upgrade) resolveModule(module, selector string) (string, bool) {
	if exactPattern.MatchString(selector) {
		return selector, true
	}
	if u.Origin == "" {
		return "", false
	}
	body, err := u.fetcher.fetch(u.channelURL(module, selector))
	if err != nil {
		return "", false
	}
	resolved, ok := versionField(body)
	if !ok || resolved == "<nil>" || !exactPattern.MatchString(resolved) {
		return "", false
	}
	return resolved, true
}

// versionField is what `jq -r '.Version // empty'` printed for body, one line
// per JSON document, and false where jq failed: on malformed JSON or on a
// document that is neither an object nor null.
func versionField(body []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var lines []string
	for {
		var document json.RawMessage
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", false
		}
		if isNull(document) {
			continue
		}
		fields, ok := object(document)
		if !ok {
			return "", false
		}
		version := bytes.TrimSpace(fields["Version"])
		if len(version) == 0 || string(version) == "null" || string(version) == "false" {
			continue
		}
		lines = append(lines, workspacejob.JQRawText(version))
	}
	return strings.Join(lines, "\n"), true
}

// targetVersion is the exact version the job pins module at: the release-set
// member, or the selector resolved once for the run and remembered, so every
// surface the run rewrites carries one answer per module.
func (u *upgrade) targetVersion(module string) (string, bool) {
	if u.ReleaseSet != nil {
		version := u.ReleaseSet.Versions[module]
		return version, version != ""
	}
	if version, ok := u.resolved[module]; ok {
		return version, true
	}
	version, ok := u.resolveModule(module, u.Version)
	if !ok {
		return "", false
	}
	u.resolved[module] = version
	return version, true
}

// reportUnresolved names the module, the channel, the origin and the HTTP
// status, so an operator knows whether to publish the channel, fix
// credentials or fix the proxy. An anonymous 401, 403 or 404 from an https
// origin names the missing credential.
func (u *upgrade) reportUnresolved(module string) {
	prefix := "Cannot resolve channel \"" + u.Version + "\" for " + module + ": "
	if u.Origin == "" {
		u.Emit.Diagnostic("error", prefix+"no module origin is declared (registries.go.origin)", "", 0)
		return
	}
	channelURL := u.channelURL(module, u.Version)
	host := urlHost(channelURL)
	anonymous := u.fetcher.missingCredential(channelURL)
	status := u.fetcher.status(channelURL)
	var detail string
	switch {
	case anonymous != "" && status == "404":
		detail = "proxy " + host + " does not serve that channel to an anonymous request (HTTP 404): " + anonymous
	case anonymous != "" && (status == "401" || status == "403"):
		detail = "proxy " + host + " refused an anonymous request (HTTP " + status + "): " + anonymous
	case status == "404" || status == "410":
		detail = "proxy " + host + " does not serve that channel (HTTP " + status + ")"
	case status == "" || status == "000":
		detail = "proxy " + host + " did not answer"
	case status == "200":
		detail = "proxy " + host + " answered HTTP 200 without a usable version"
	default:
		detail = "proxy " + host + " answered HTTP " + status
	}
	u.Emit.Diagnostic("error", prefix+detail, "", 0)
}

// emitMissing reports a module the job has no version for.
func (u *upgrade) emitMissing(module string) {
	if u.ReleaseSet != nil {
		u.Emit.Log("error", "Release set "+u.ReleaseSet.ID+" has no exact Go member for "+module)
		return
	}
	// Outside a release set every module answers for itself.
	u.reportUnresolved(module)
}

// sourceRevision is the pre-release suffix that names the commit a version was
// built from, or "-" for a release that carries none.
func sourceRevision(version string) string {
	if index := strings.LastIndex(version, "-"); index >= 0 {
		return version[index+1:]
	}
	return "-"
}

// reportResolved prints one row per module with its version and source
// revision; modules that all resolved to one version collapse into a single
// go.putnami.dev/* row. It reports false when a module has no version.
func (u *upgrade) reportResolved(selector string, modules []string) bool {
	if len(modules) == 0 {
		return true
	}
	first, uniform := "", true
	for _, module := range modules {
		version, ok := u.targetVersion(module)
		if !ok {
			return false
		}
		if first == "" {
			first = version
		} else if version != first {
			uniform = false
		}
	}
	u.Emit.Log("info", "Resolved go.putnami.dev/* from "+selector+":")
	if uniform {
		u.Emit.Log("info", "  go.putnami.dev/*  "+first+"  revision "+sourceRevision(first))
		return true
	}
	for _, module := range modules {
		version, _ := u.targetVersion(module)
		u.Emit.Log("info", "  "+module+"  "+version+"  revision "+sourceRevision(version))
	}
	return true
}

// publishedPinModules is the base list of modules pinned in go.work: the
// release set's Go members, sorted, or the published modules.
func (u *upgrade) publishedPinModules() []string {
	if u.ReleaseSet == nil {
		return PublishedModules
	}
	modules := make([]string, 0, len(u.ReleaseSet.Versions))
	for module := range u.ReleaseSet.Versions {
		if module != "" {
			modules = append(modules, module)
		}
	}
	sort.Strings(modules)
	return modules
}
