package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/useragent"
)

// goFrameworkModule is the module whose version a template's
// goFrameworkVersion variable names.
const goFrameworkModule = "go.putnami.dev/app"

// defaultGoProxy is the GOPROXY the go command uses when neither the
// environment nor the go env file sets one.
const defaultGoProxy = "https://proxy.golang.org,direct"

// goProxyRequestTimeout bounds one request to a module proxy.
const goProxyRequestTimeout = 10 * time.Second

// maxGoProxyResponseSize caps a module proxy answer at 1 MiB. An @latest
// answer is a few dozen bytes; a larger one comes from a hostile or
// malfunctioning proxy, so the read is bounded (the same size-cap invariant as
// maxBinarySize / MaxArchiveSize / maxBinaryDownloadSize) and an oversized
// answer is refused rather than parsed truncated.
const maxGoProxyResponseSize = 1 << 20 // 1 MiB

// canonicalModuleVersion matches a canonical Go module version,
// vMAJOR.MINOR.PATCH with an optional pre-release and build suffix. The
// resolved version is written into go.mod, so a proxy answer that is anything
// else is refused.
var canonicalModuleVersion = regexp.MustCompile(
	`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// errGoProxyNotFound marks a proxy that answered 404 or 410: it does not serve
// the module, and the query passes to the next proxy whatever separator
// follows, as in the go command.
var errGoProxyNotFound = errors.New("the proxy does not serve the module")

// ensureModuleOriginCredential asks @putnami/cloud to write the netrc entry for
// a module origin. It starts a CLI that loads the workspace's extensions, so it
// records repository code first. A hosted run (runcredential.Hosted) asks
// nothing: that CLI would run without the run credential, and a hosted run
// starts no `putnami cloud registry-token`. A test replaces it so that no cloud
// CLI runs.
var ensureModuleOriginCredential = func(ctx context.Context, workspaceRoot, host string) registrycred.Outcome {
	if runcredential.Hosted() {
		return registrycred.Outcome{Kind: registrycred.KindSkipped}
	}
	runcredential.MarkRepositoryCodeStarted("putnami cloud registry-token for " + host)
	return registrycred.EnsureNativeCredential(ctx, workspaceRoot, host)
}

// refreshGoFrameworkCredential refreshes the netrc credential for the origin of
// go.putnami.dev/app before a lookup that reads the origin, as the Go extension
// does before every go command. The credential lives for hours, so the entry a
// past sign-in wrote is expired more often than not, and the origin answers
// 401 to an expired credential. A module the lookup reads through GOPROXY's
// proxies needs no credential, so nothing is asked for it. The refresh never
// fails the create: without a cloud or a session, the lookup runs with what the
// machine has and names the netrc fix when it fails.
func refreshGoFrameworkCredential(ctx context.Context, wsRoot string, getenv func(string) string) string {
	lookup := goEnvLookup(getenv)
	noProxy := lookup("GONOPROXY")
	if noProxy == "" {
		noProxy = lookup("GOPRIVATE")
	}
	if !matchGoPrefixPatterns(noProxy, goFrameworkModule) {
		return ""
	}
	host, _, _ := strings.Cut(goFrameworkModule, "/")
	return ensureModuleOriginCredential(ctx, wsRoot, host).Message
}

// resolveGoFrameworkVersion returns the version of go.putnami.dev/app a new Go
// project requires, and the source that named it, in a form that carries no
// user information (resolveModuleLatest).
func resolveGoFrameworkVersion(ctx context.Context, client *http.Client, getenv func(string) string) (version, source string, err error) {
	return resolveGoFrameworkVersionOn(ctx, client, getenv, "")
}

// resolveGoFrameworkVersionOn is resolveGoFrameworkVersion for the channel
// `putnami init` chose: the version the channel names for go.putnami.dev/app,
// asked of the same proxies and origin. An empty channel is latest. The
// template requires every framework module at that one version, so a channel
// that names different versions for them resolves the version of
// go.putnami.dev/app for all of them.
func resolveGoFrameworkVersionOn(ctx context.Context, client *http.Client, getenv func(string) string, channel string) (version, source string, err error) {
	version, source, err = resolveModuleChannel(ctx, client, getenv, goFrameworkModule, channel)
	if err != nil {
		if channel != "" && channel != latestChannel {
			return "", "", fmt.Errorf("resolve the version of %s on channel %s: %w", goFrameworkModule, channel, err)
		}
		return "", "", fmt.Errorf("resolve the version of %s: %w", goFrameworkModule, err)
	}
	return version, source, nil
}

// resolveModuleLatest returns module's newest version and the source that
// named it (resolveModuleChannel on latest).
func resolveModuleLatest(ctx context.Context, client *http.Client, getenv func(string) string, module string) (version, source string, err error) {
	return resolveModuleChannel(ctx, client, getenv, module, "")
}

// resolveModuleChannel returns the version channel names for module and the
// source that named it, the way the go command reaches the module: through
// the proxies of the effective GOPROXY in order, or from the module's origin
// ("direct") for a module GONOPROXY (or, when that is unset, GOPRIVATE)
// matches. getenv reads the environment the go command runs in; the go env
// file fills what it leaves unset, and Go's default applies after both. A
// proxy that answers 404 or 410 passes the query on; any other failure does
// only after a "|".
//
// An empty channel, or latest, asks each proxy for the module's @latest
// version, the endpoint the Go extension resolves a module's newest
// publication from: the highest version of @v/list is not the newest one for
// commit-stamped pre-releases (ADR 0018), and a public proxy can list a
// version it no longer serves. Any other channel asks for @v/<channel>.info,
// the Go version query the Go extension resolves a channel with (ADR 0020).
// The origin is read through its go-import meta tag, and only a "mod" tag,
// which names a module proxy, is followed (queryModuleOrigin).
//
// A request gets the credentials the go command would send it: the user
// information of its URL, and over https the netrc entry for its host
// (addGoProxyCredentials). No credential is ever printed. It fails naming every
// source it asked and the setting that fixes the lookup; it never returns a
// placeholder version, and it never answers a channel with latest.
func resolveModuleChannel(ctx context.Context, client *http.Client, getenv func(string) string, module, channel string) (version, source string, err error) {
	lookup := goEnvLookup(getenv)
	noProxy := lookup("GONOPROXY")
	if noProxy == "" {
		noProxy = lookup("GOPRIVATE")
	}
	goProxy := lookup("GOPROXY")
	if goProxy == "" {
		goProxy = defaultGoProxy
	}
	entries := parseGoProxyList(goProxy)
	private := matchGoPrefixPatterns(noProxy, module)
	if private {
		entries = []goProxyEntry{{url: "direct"}}
	}

	var asked []string
	for _, entry := range entries {
		switch entry.url {
		case "off":
			asked = append(asked, "off: GOPROXY=off disables module downloads")
			return "", "", moduleLookupError(module, asked, private)
		case "direct":
			version, origin, err := queryModuleOrigin(ctx, client, module, channel, lookup, getenv)
			if err == nil {
				return version, "direct (" + origin + ")", nil
			}
			asked = append(asked, "direct: "+err.Error())
			return "", "", moduleLookupError(module, asked, private)
		}
		display := redactedProxyURL(entry.url)
		version, err := queryGoProxyChannel(ctx, client, entry.url, module, channel, lookup, getenv)
		if err == nil {
			return version, display, nil
		}
		asked = append(asked, display+": "+err.Error())
		if !entry.fallBackOnError && !errors.Is(err, errGoProxyNotFound) {
			break
		}
	}
	return "", "", moduleLookupError(module, asked, private)
}

// moduleLookupError states a lookup nothing answered: every source it asked
// with its answer, and the setting that fixes it. private reports a module
// GONOPROXY or GOPRIVATE matches, which Go fetches from its origin only.
func moduleLookupError(module string, asked []string, private bool) error {
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

// queryModuleOrigin resolves the version channel names for module from its
// origin the way the go command's "direct" does for a module served by a
// module proxy: it reads the go-import meta tag that covers module and asks
// the module proxy a "mod" tag names (queryGoProxyChannel). It reads no
// version control repository; a tag of another kind fails naming it. The
// origin it returns carries no user information.
func queryModuleOrigin(ctx context.Context, client *http.Client, module, channel string, lookup, getenv func(string) string) (version, origin string, err error) {
	tag, err := fetchGoImportTag(ctx, client, module, module, lookup, getenv)
	if err != nil {
		return "", "", err
	}
	// A tag for a shorter prefix must be the one the prefix's own page states,
	// as the go command checks, or a page could claim a path above its own.
	if tag.prefix != module {
		prefixTag, err := fetchGoImportTag(ctx, client, tag.prefix, module, lookup, getenv)
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
	origin = redactedProxyURL(tag.repoRoot)
	version, err = queryGoProxyChannel(ctx, client, tag.repoRoot, module, channel, lookup, getenv)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", origin, err)
	}
	return version, origin, nil
}

// fetchGoImportTag reads the page https://<importPath>?go-get=1 and returns
// its go-import tag that covers module.
func fetchGoImportTag(ctx context.Context, client *http.Client, importPath, module string, lookup, getenv func(string) string) (goImportTag, error) {
	metaURL := "https://" + importPath + "?go-get=1"
	requestCtx, cancel := context.WithTimeout(ctx, goProxyRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, metaURL, nil)
	if err != nil {
		return goImportTag{}, fmt.Errorf("GET %s: the request cannot be built", metaURL)
	}
	useragent.Set(req)
	addGoProxyCredentials(req, lookup, getenv)
	resp, err := client.Do(req) //nolint:gosec // G704: the origin of the module path, as the go command reads it
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return goImportTag{}, fmt.Errorf("GET %s: %w", metaURL, err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxGoProxyResponseSize+1))
	if err != nil {
		return goImportTag{}, fmt.Errorf("GET %s: %w", metaURL, err)
	}
	if len(page) > maxGoProxyResponseSize {
		return goImportTag{}, fmt.Errorf("GET %s: the answer exceeds %d bytes", metaURL, maxGoProxyResponseSize)
	}
	// As in the go command, a page that is not a 200 still counts when it
	// carries a tag, and its status is the error when it does not.
	tags := parseGoImportTags(page)
	if len(tags) == 0 && resp.StatusCode != http.StatusOK {
		return goImportTag{}, fmt.Errorf("GET %s: HTTP %d", metaURL, resp.StatusCode)
	}
	tag, err := matchGoImport(tags, module)
	if err != nil {
		return goImportTag{}, fmt.Errorf("GET %s: %w", metaURL, err)
	}
	return tag, nil
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

// goProxyChannelEndpoint is the module proxy path that answers channel for
// module: /@latest for an empty channel or latest, which orders by publication
// time, and the Go version query /@v/<channel>.info for any other channel.
func goProxyChannelEndpoint(module, channel string) string {
	escaped := escapeGoModulePath(module)
	if channel == "" || channel == latestChannel {
		return "/" + escaped + "/@latest"
	}
	return "/" + escaped + "/@v/" + channel + ".info"
}

// queryGoProxyChannel asks the module proxy at base for the version channel
// names for module (goProxyChannelEndpoint), over the module proxy protocol.
// lookup reads the Go settings and getenv the environment, for the
// credentials the proxy gets.
func queryGoProxyChannel(ctx context.Context, client *http.Client, base, module, channel string, lookup, getenv func(string) string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("not an http or https URL; this lookup asks no other kind of proxy")
	}
	endpoint := *parsed
	endpoint.Path = strings.TrimSuffix(parsed.Path, "/") + goProxyChannelEndpoint(module, channel)
	endpoint.RawPath = ""
	shown := "GET " + redactedProxyURL(endpoint.String())

	requestCtx, cancel := context.WithTimeout(ctx, goProxyRequestTimeout)
	defer cancel()
	// The error of a request built from a URL can quote the URL, credentials
	// included, so it is not passed on.
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%s: the request cannot be built", shown)
	}
	useragent.Set(req)
	addGoProxyCredentials(req, lookup, getenv)
	resp, err := client.Do(req) //nolint:gosec // G704: the proxy the consumer's GOPROXY names
	if err != nil {
		// A *url.Error quotes the URL; its cause does not.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return "", fmt.Errorf("%s: %w", shown, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return "", fmt.Errorf("%s: HTTP %d: %w", shown, resp.StatusCode, errGoProxyNotFound)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%s: HTTP %d: the proxy wants credentials the go command would send it, "+
			"in the netrc file (NETRC, else %s in your home directory)", shown, resp.StatusCode, netrcFileName())
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%s: HTTP %d", shown, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGoProxyResponseSize+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", shown, err)
	}
	if len(body) > maxGoProxyResponseSize {
		return "", fmt.Errorf("%s: the answer exceeds %d bytes", shown, maxGoProxyResponseSize)
	}
	var info struct{ Version string }
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("%s: %w", shown, err)
	}
	if !canonicalModuleVersion.MatchString(info.Version) {
		return "", fmt.Errorf("%s: %q is not a canonical module version", shown, info.Version)
	}
	return info.Version, nil
}

// addGoProxyCredentials gives req the netrc credentials the go command sends
// a module proxy: over https only, the login and password of the first netrc
// entry whose machine is the request's host name. GOAUTH selects the go
// command's authentication methods; unset means netrc, and a GOAUTH that does
// not list netrc (such as off) sends none. The go command runs GOAUTH's git
// and command methods too; this lookup does not. User information in the
// GOPROXY URL needs nothing here: net/http sends it, as the go command does.
func addGoProxyCredentials(req *http.Request, lookup, getenv func(string) string) {
	if req.URL.Scheme != "https" || !goAuthUsesNetrc(lookup("GOAUTH")) {
		return
	}
	name := getenv("NETRC")
	if name == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		name = filepath.Join(home, netrcFileName())
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return
	}
	host := req.URL.Hostname()
	for _, entry := range parseNetrc(string(data)) {
		if entry.machine == host {
			req.SetBasicAuth(entry.login, entry.password)
			return
		}
	}
}

// goAuthUsesNetrc reports whether a GOAUTH value selects the netrc method:
// unset does, and so does a ";"-separated list that names it.
func goAuthUsesNetrc(goAuth string) bool {
	if strings.TrimSpace(goAuth) == "" {
		return true
	}
	for _, method := range strings.Split(goAuth, ";") {
		if strings.TrimSpace(method) == "netrc" {
			return true
		}
	}
	return false
}

// netrcFileName is the netrc file of the home directory the go command reads:
// _netrc on Windows, .netrc elsewhere.
func netrcFileName() string {
	if runtime.GOOS == "windows" {
		return "_netrc"
	}
	return ".netrc"
}

// netrcEntry is a machine entry of a netrc file with both a login and a
// password.
type netrcEntry struct {
	machine, login, password string
}

// parseNetrc reads the machine entries of a netrc file the way the go command
// does: tokens pair up per line, a machine token starts an entry, a macdef
// skips its macro up to the next empty line, and nothing after a default token
// is read.
func parseNetrc(data string) []netrcEntry {
	var entries []netrcEntry
	var entry netrcEntry
	inMacro := false
	for _, line := range strings.Split(data, "\n") {
		if inMacro {
			if line == "" {
				inMacro = false
			}
			continue
		}
		fields := strings.Fields(line)
		i := 0
		for ; i < len(fields)-1; i += 2 {
			switch fields[i] {
			case "machine":
				entry = netrcEntry{machine: fields[i+1]}
			case "login":
				entry.login = fields[i+1]
			case "password":
				entry.password = fields[i+1]
			case "macdef":
				inMacro = true
			}
			if entry.machine != "" && entry.login != "" && entry.password != "" {
				entries = append(entries, entry)
				entry = netrcEntry{}
			}
		}
		if i < len(fields) && fields[i] == "default" {
			break
		}
	}
	return entries
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

// redactedProxyURL is rawURL without its user information, which can carry a
// credential and is never printed.
func redactedProxyURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "(an unparsable proxy URL)"
	}
	parsed.User = nil
	return parsed.String()
}

// goEnvLookup reads a Go setting the way the go command does: the environment
// first, then the go env file `go env -w` writes (GOENV names it, else go/env
// under the user configuration directory, and GOENV=off reads none).
func goEnvLookup(getenv func(string) string) func(string) string {
	file := readGoEnvFile(getenv)
	return func(key string) string {
		if value := getenv(key); value != "" {
			return value
		}
		return file[key]
	}
}

// readGoEnvFile returns the settings of the go env file, or nil when there is
// none to read.
func readGoEnvFile(getenv func(string) string) map[string]string {
	name := getenv("GOENV")
	switch name {
	case "off":
		return nil
	case "":
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil
		}
		name = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || key[0] < 'A' || key[0] > 'Z' {
			continue
		}
		settings[key] = value
	}
	return settings
}

// matchGoPrefixPatterns reports whether any comma-separated glob of patterns
// matches a leading run of target's path elements, which is how GONOPROXY and
// GOPRIVATE select module paths.
func matchGoPrefixPatterns(patterns, target string) bool {
	elements := strings.Split(target, "/")
	for _, glob := range strings.Split(patterns, ",") {
		glob = strings.TrimSuffix(glob, "/")
		if glob == "" {
			continue
		}
		n := strings.Count(glob, "/") + 1
		if len(elements) < n {
			continue
		}
		if matched, _ := path.Match(glob, strings.Join(elements[:n], "/")); matched {
			return true
		}
	}
	return false
}

// escapeGoModulePath is the module proxy's case-encoded path: every uppercase
// letter becomes "!" and its lowercase spelling.
func escapeGoModulePath(module string) string {
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
