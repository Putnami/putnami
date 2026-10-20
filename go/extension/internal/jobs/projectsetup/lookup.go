package projectsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/module"

	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
)

// The lookup resolves a framework module's version the way the go command
// reaches the module: through the proxies of GOPROXY in order, or from the
// module's origin ("direct") for a module GONOPROXY matches. It is the port of
// the lookup `putnami projects create` ran before the Go extension set a
// created project up, and its messages are that lookup's.

// defaultGoProxy is the GOPROXY the go command uses when nothing sets one.
const defaultGoProxy = "https://proxy.golang.org,direct"

// latestChannel is the channel a project is created on when none is chosen.
const latestChannel = "latest"

// requestTimeout bounds one request to a module proxy or an origin.
const requestTimeout = 10 * time.Second

// maxResponseSize caps a module proxy answer at 1 MiB. An @latest answer is a
// few dozen bytes; a larger one comes from a hostile or malfunctioning proxy,
// so it is refused rather than parsed truncated.
const maxResponseSize = 1 << 20

// canonicalVersion matches a canonical Go module version,
// vMAJOR.MINOR.PATCH with an optional pre-release and build suffix. The
// resolved version is written into go.mod, so an answer that is anything else
// is refused.
var canonicalVersion = regexp.MustCompile(
	`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// errNotServed marks a proxy that answered 404 or 410, or a file:// proxy
// without the file: it does not serve the module, and the query passes to the
// next proxy whatever separator follows, as in the go command.
var errNotServed = errors.New("the proxy does not serve the module")

// netrcFileName is the netrc file of the home directory the go command reads:
// _netrc on Windows, .netrc elsewhere.
func netrcFileName() string {
	if runtime.GOOS == "windows" {
		return "_netrc"
	}
	return ".netrc"
}

// goSettings are the Go settings the lookup follows, as `go env` reports them:
// GONOPROXY there already falls back to GOPRIVATE.
type goSettings struct {
	Proxy   string `json:"GOPROXY"`
	NoProxy string `json:"GONOPROXY"`
	Auth    string `json:"GOAUTH"`
}

// lookup asks module proxies and origins for module versions.
type lookup struct {
	ctx       context.Context
	client    *http.Client
	userAgent string
	settings  goSettings
	// credential is the netrc login and password the go command sends to an
	// https URL under GOAUTH (depsupgrade.NetrcCredential).
	credential func(goAuth string, target *url.URL) (login, password string, ok bool)
}

// newClient is the HTTP client of the lookup: it follows redirects, except
// from https to another scheme, as the go command does.
func newClient() *http.Client {
	return &http.Client{CheckRedirect: depsupgrade.RefuseDowngrade}
}

// private reports whether the go command reads module from its origin only.
func (l *lookup) private(module string) bool {
	return matchPrefixPatterns(l.settings.NoProxy, module)
}

// resolve returns the version channel names for module and the source that
// named it, in a form that carries no user information. An empty channel, or
// latest, asks each proxy for the module's @latest version, which orders by
// publication time; any other channel asks for @v/<channel>.info, the go
// version query. It never answers a channel with latest, and it fails naming
// every source it asked and the setting that fixes the lookup.
func (l *lookup) resolve(module, channel string) (version, source string, err error) {
	version, source, err = l.walk(module, channel)
	if err != nil {
		if channel != "" && channel != latestChannel {
			return "", "", fmt.Errorf("resolve the version of %s on channel %s: %w", module, channel, err)
		}
		return "", "", fmt.Errorf("resolve the version of %s: %w", module, err)
	}
	return version, source, nil
}

// walk asks the sources of GOPROXY in order: a 404 or a 410 passes the query
// on, any other failure does only after a "|", and "off" and "direct" end the
// walk. A module GONOPROXY matches is read from its origin only.
func (l *lookup) walk(module, channel string) (version, source string, err error) {
	goProxy := l.settings.Proxy
	if goProxy == "" {
		goProxy = defaultGoProxy
	}
	entries := parseGoProxyList(goProxy)
	private := l.private(module)
	if private {
		entries = []goProxyEntry{{url: "direct"}}
	}

	var asked []string
	for _, entry := range entries {
		switch entry.url {
		case "off":
			asked = append(asked, "off: GOPROXY=off disables module downloads")
			return "", "", lookupError(module, asked, private)
		case "direct":
			version, origin, err := l.queryOrigin(module, channel)
			if err == nil {
				return version, "direct (" + origin + ")", nil
			}
			asked = append(asked, "direct: "+err.Error())
			return "", "", lookupError(module, asked, private)
		}
		display := redactedURL(entry.url)
		version, err := l.queryProxy(entry.url, module, channel)
		if err == nil {
			return version, display, nil
		}
		asked = append(asked, display+": "+err.Error())
		if !entry.fallBackOnError && !errors.Is(err, errNotServed) {
			break
		}
	}
	return "", "", lookupError(module, asked, private)
}

// lookupError states a lookup nothing answered: every source it asked with
// its answer, and the setting that fixes it. private reports a module
// GONOPROXY or GOPRIVATE matches, which Go fetches from its origin only.
func lookupError(module string, asked []string, private bool) error {
	if len(asked) == 0 {
		asked = []string{"GOPROXY names no module proxy"}
	}
	fix := fmt.Sprintf("Set GOPROXY to a module proxy that serves %s (Go's default is %s)", module, defaultGoProxy)
	if private {
		fix = fmt.Sprintf("GONOPROXY or GOPRIVATE matches %s, so Go reads it from its origin only: "+
			"put your credentials for the origin in the netrc file, or set GONOPROXY=none to use the proxies of GOPROXY", module)
	}
	return fmt.Errorf("no module source answered:\n  - %s\n%s", strings.Join(asked, "\n  - "), fix)
}

// queryOrigin resolves the version channel names for module from its origin
// the way the go command's "direct" does for a module served by a module
// proxy: it reads the go-import meta tag that covers module and asks the
// module proxy a "mod" tag names. It reads no version control repository; a
// tag of another kind fails naming it. The origin it returns carries no user
// information.
func (l *lookup) queryOrigin(module, channel string) (version, origin string, err error) {
	tag, err := l.fetchGoImportTag(module, module)
	if err != nil {
		return "", "", err
	}
	// A tag for a shorter prefix must be the one the prefix's own page states,
	// as the go command checks, or a page could claim a path above its own.
	if tag.prefix != module {
		prefixTag, err := l.fetchGoImportTag(tag.prefix, module)
		if err != nil {
			return "", "", err
		}
		if prefixTag != tag {
			return "", "", fmt.Errorf("the go-import tags of %s and %s disagree about %s", module, tag.prefix, tag.prefix)
		}
	}
	if tag.vcs != "mod" {
		return "", "", fmt.Errorf("the go-import tag of %s names a %s repository, which this lookup does not read", module, tag.vcs)
	}
	root, err := url.Parse(tag.repoRoot)
	if err != nil || root.Scheme != "https" || root.Host == "" {
		return "", "", fmt.Errorf("the go-import tag of %s names a module proxy that is not an https URL", module)
	}
	origin = redactedURL(tag.repoRoot)
	version, err = l.queryProxy(tag.repoRoot, module, channel)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", origin, err)
	}
	return version, origin, nil
}

// fetchGoImportTag reads the page https://<importPath>?go-get=1 and returns
// its go-import tag that covers module.
func (l *lookup) fetchGoImportTag(importPath, module string) (goImportTag, error) {
	metaURL := "https://" + importPath + "?go-get=1"
	page, status, err := l.get(metaURL)
	if err != nil {
		return goImportTag{}, fmt.Errorf("GET %s: %w", metaURL, err)
	}
	// As in the go command, a page that is not a 200 still counts when it
	// carries a tag, and its status is the error when it does not.
	tags := parseGoImportTags(page)
	if len(tags) == 0 && status != http.StatusOK {
		return goImportTag{}, fmt.Errorf("GET %s: HTTP %d", metaURL, status)
	}
	tag, err := matchGoImport(tags, module)
	if err != nil {
		return goImportTag{}, fmt.Errorf("GET %s: %w", metaURL, err)
	}
	return tag, nil
}

// get reads rawURL with the credentials the go command would send it and
// returns the body and the status. The error carries neither the URL nor a
// credential.
func (l *lookup) get(rawURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(l.ctx, requestTimeout)
	defer cancel()
	// The error of a request built from a URL can quote the URL, credentials
	// included, so it is not passed on.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, errors.New("the request cannot be built")
	}
	req.Header.Set("User-Agent", l.userAgent)
	if login, password, ok := l.credential(l.settings.Auth, req.URL); ok {
		req.SetBasicAuth(login, password)
	}
	resp, err := l.client.Do(req) //nolint:gosec // G704: a proxy GOPROXY names, or the origin of the module path, as the go command reads them
	if err != nil {
		// A *url.Error quotes the URL; its cause does not.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > maxResponseSize {
		return nil, resp.StatusCode, fmt.Errorf("the answer exceeds %d bytes", maxResponseSize)
	}
	return body, resp.StatusCode, nil
}

// goImportTag is a go-import meta tag: the import path prefix it covers, the
// kind of source ("mod" for a module proxy, else a version control system),
// and the source's URL.
type goImportTag struct {
	prefix, vcs, repoRoot string
}

// parseGoImportTags reads the go-import meta tags of an HTML page the way the
// go command does: up to the end of the head or the start of the body, with
// the tags of kind "mod" first and a tag of another kind dropped when a "mod"
// tag covers the same prefix.
func parseGoImportTags(page []byte) []goImportTag {
	decoder := xml.NewDecoder(bytes.NewReader(page))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	decoder.Entity = xml.HTMLEntity
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "utf-8", "ascii":
			return input, nil
		}
		return nil, fmt.Errorf("the page is in %s, not UTF-8", charset)
	}
	var tags []goImportTag
	for {
		token, err := decoder.RawToken()
		if err != nil {
			break
		}
		if start, ok := token.(xml.StartElement); ok && strings.EqualFold(start.Name.Local, "body") {
			break
		}
		if end, ok := token.(xml.EndElement); ok && strings.EqualFold(end.Name.Local, "head") {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "meta") || htmlAttribute(start.Attr, "name") != "go-import" {
			continue
		}
		// "prefix vcs repo-root", with an optional fourth field naming a
		// subdirectory of the repository.
		fields := strings.Fields(htmlAttribute(start.Attr, "content"))
		if len(fields) == 3 || len(fields) == 4 {
			tags = append(tags, goImportTag{prefix: fields[0], vcs: fields[1], repoRoot: fields[2]})
		}
	}
	modPrefixes := map[string]bool{}
	var ordered []goImportTag
	for _, tag := range tags {
		if tag.vcs == "mod" {
			modPrefixes[tag.prefix] = true
			ordered = append(ordered, tag)
		}
	}
	for _, tag := range tags {
		if tag.vcs != "mod" && !modPrefixes[tag.prefix] {
			ordered = append(ordered, tag)
		}
	}
	return ordered
}

// htmlAttribute is the value of the named attribute, matched without regard
// to case.
func htmlAttribute(attributes []xml.Attr, name string) string {
	for _, attribute := range attributes {
		if strings.EqualFold(attribute.Name.Local, name) {
			return attribute.Value
		}
	}
	return ""
}

// matchGoImport picks the tag whose prefix covers module, as the go command
// does: the first "mod" tag that covers it wins, and two other covering tags
// are ambiguous.
func matchGoImport(tags []goImportTag, module string) (goImportTag, error) {
	match := -1
	for i, tag := range tags {
		if module != tag.prefix && !strings.HasPrefix(module, tag.prefix+"/") {
			continue
		}
		if match >= 0 {
			if tags[match].vcs == "mod" && tag.vcs != "mod" {
				break
			}
			return goImportTag{}, fmt.Errorf("several go-import tags cover %s", module)
		}
		match = i
	}
	if match < 0 {
		return goImportTag{}, fmt.Errorf("no go-import tag covers %s", module)
	}
	return tags[match], nil
}

// channelEndpoint is the module proxy path that answers channel for module:
// /@latest for an empty channel or latest, and the go version query
// /@v/<channel>.info for any other channel. A channel is in the portable
// alphabet, which holds no uppercase letter, so the path carries it as is.
func channelEndpoint(module, channel string) string {
	escaped := escapeModulePath(module)
	if channel == "" || channel == latestChannel {
		return "/" + escaped + "/@latest"
	}
	return "/" + escaped + "/@v/" + channel + ".info"
}

// queryProxy asks the module proxy at base, an http, https or file URL, for
// the version channel names for module (channelEndpoint), over the module
// proxy protocol.
func (l *lookup) queryProxy(base, module, channel string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || !(parsed.Scheme == "file" || (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "") {
		return "", errors.New("not an http, https or file URL; this lookup asks no other kind of proxy")
	}
	endpoint := *parsed
	endpoint.Path = strings.TrimSuffix(parsed.Path, "/") + channelEndpoint(module, channel)
	endpoint.RawPath = ""
	if parsed.Scheme == "file" {
		return readFileProxy(&endpoint)
	}
	shown := "GET " + redactedURL(endpoint.String())
	body, status, err := l.get(endpoint.String())
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w", shown, err)
	case status == http.StatusNotFound || status == http.StatusGone:
		return "", fmt.Errorf("%s: HTTP %d: %w", shown, status, errNotServed)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", fmt.Errorf("%s: HTTP %d: the proxy wants credentials the go command would send it, "+
			"in the netrc file (NETRC, else %s in your home directory)", shown, status, netrcFileName())
	case status != http.StatusOK:
		return "", fmt.Errorf("%s: HTTP %d", shown, status)
	}
	return versionOf(shown, body)
}

// readFileProxy reads the answer of a file:// module proxy, where a missing
// file is a module the proxy does not serve, as the go command reads it.
func readFileProxy(endpoint *url.URL) (string, error) {
	shown := "read " + redactedURL(endpoint.String())
	body, err := os.ReadFile(depsupgrade.FileURLPath(endpoint))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", shown, errNotServed)
	}
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return "", fmt.Errorf("%s: %w", shown, err)
	}
	if len(body) > maxResponseSize {
		return "", fmt.Errorf("%s: the answer exceeds %d bytes", shown, maxResponseSize)
	}
	return versionOf(shown, body)
}

// versionOf is the canonical version an answer names.
func versionOf(shown string, body []byte) (string, error) {
	var info struct{ Version string }
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("%s: %w", shown, err)
	}
	if !canonicalVersion.MatchString(info.Version) {
		return "", fmt.Errorf("%s: %q is not a canonical module version", shown, info.Version)
	}
	return info.Version, nil
}

// goProxyEntry is one entry of a GOPROXY list.
type goProxyEntry struct {
	// url is a proxy URL, or the keyword "direct" or "off".
	url string
	// fallBackOnError reports a "|" after the entry: any failure passes the
	// query to the next entry, not only a 404 or 410.
	fallBackOnError bool
}

// parseGoProxyList splits a GOPROXY value the way the go command does. Entries
// are separated by "," or "|", empty entries are skipped, nothing after
// "direct" or "off" is read, and an entry that looks like a host gets an
// https:// scheme.
func parseGoProxyList(value string) []goProxyEntry {
	var entries []goProxyEntry
	for value != "" {
		entry, fallBackOnError := value, false
		if i := strings.IndexAny(value, ",|"); i >= 0 {
			entry, fallBackOnError, value = value[:i], value[i] == '|', value[i+1:]
		} else {
			value = ""
		}
		entry = strings.TrimSpace(entry)
		switch entry {
		case "":
			continue
		case "direct", "off":
			return append(entries, goProxyEntry{url: entry})
		}
		if strings.ContainsAny(entry, ".:/") && !strings.Contains(entry, ":/") &&
			!filepath.IsAbs(entry) && !path.IsAbs(entry) {
			entry = "https://" + entry
		}
		entries = append(entries, goProxyEntry{url: entry, fallBackOnError: fallBackOnError})
	}
	return entries
}

// redactedURL is rawURL without its user information, which can carry a
// credential and is never printed.
func redactedURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "(an unparsable proxy URL)"
	}
	parsed.User = nil
	return parsed.String()
}

// matchPrefixPatterns reports whether a glob of the comma-separated patterns
// matches a leading run of target's path elements, which is how GONOPROXY
// selects module paths.
func matchPrefixPatterns(patterns, target string) bool {
	return module.MatchPrefixPatterns(patterns, target)
}

// escapeModulePath is the module proxy's case-encoded path: every uppercase
// letter becomes "!" and its lowercase spelling.
func escapeModulePath(modulePath string) string {
	var out strings.Builder
	for _, r := range modulePath {
		if r >= 'A' && r <= 'Z' {
			out.WriteByte('!')
			out.WriteRune(r - 'A' + 'a')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
