package toolchain

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const (
	goWorkEnvVar        = "GOWORK"
	workspaceRootEnvVar = "PUTNAMI_WORKSPACE_ROOT"
	extensionRootEnvVar = "PUTNAMI_EXTENSION_ROOT"
	goRegistryURLEnvVar = "GO_REGISTRY_URL"
	workspaceConfigFile = "putnami.workspace.json"
	defaultGoProxy      = "https://proxy.golang.org,direct"
)

// GoCommandEnv returns the environment shared by every native Go command and
// Go-built tool spawned by the extension, without mutating process-global
// state, so concurrent jobs remain isolated. The workspace lifecycle
// subcommands (`putnami-go workspace-install` and `putnami-go deps-upgrade`)
// export the same policy through internal/workspacejob.
//
// NETRC is left alone entirely, so Go reads the standard ~/.netrc (see below).
// Third-party GOPROXY values are preserved. An inherited GOPROXY that leads with
// the declared module origin is an unsafe legacy configuration and is normalized
// to the workspace's declared chain (`registries.go.proxy`, else the public
// default). Putnami's cache paths, local toolchain policy, and checksum
// exclusions are authoritative. When goBinary is one of the workspace-managed
// candidates, GOROOT and PATH are aligned with the binary's real installation
// root.
func GoCommandEnv(env []string, goBinary string) []string {
	out := append([]string(nil), env...)

	// The cache root is resolved by THIS extension (gocache.go), not read from a
	// variable core exports. An earlier migration deleted core's
	// PUTNAMI_GO_CACHE_DIR export for every job; without a local resolution the
	// Go commands would silently fall back to Go's own ~/Library/Caches and
	// ~/go/pkg/mod and abandon the machine-global Putnami cache.
	if cacheRoot := ResolveGoCacheRoot(EnvLookup(out)); cacheRoot != "" {
		buildCache := GoBuildCacheDir(cacheRoot)
		moduleCache := filepath.Join(cacheRoot, "mod")
		// The legacy shell setup treated cache creation as best effort. Keep
		// that behavior here: a read-only cache root is diagnosed by Go itself
		// without preventing commands that do not need the cache from running.
		_ = os.MkdirAll(buildCache, 0o755)
		_ = os.MkdirAll(moduleCache, 0o755)
		out = setEnvValue(out, "GOCACHE", buildCache)
		out = setEnvValue(out, "GOMODCACHE", moduleCache)
	}

	out = withGoCacheProg(out)

	// NETRC is deliberately NOT set here. Go reads ~/.netrc when NETRC is unset,
	// and ~/.netrc is where a per-user registry credential belongs: it is the
	// file `putnami cloud registry-token --host <host> --materialize` writes, the
	// file every other Go tool on the machine already reads, and the file a
	// developer edits by hand. Pointing Go at ~/.putnami/.netrc instead sent it
	// to a path nothing has ever written, so a private module fetch got no
	// credential at all while the user's real one sat unread. A caller-supplied
	// NETRC still wins, because we no longer touch the variable.

	out = setEnvValue(out, "GOTOOLCHAIN", "local")
	// The module origin never leads GOPROXY. GONOPROXY routes the workspace's
	// own modules around the public mirror; the vanity meta tag
	// `<meta name="go-import" content="<host>/x mod https://<host>">` names the
	// origin as that module's proxy, and NETRC supplies credentials.
	//
	// The host is DECLARED, never assumed: a workspace with no vanity server
	// declares nothing and keeps stock Go behavior. See moduleOriginHost.
	//
	// The GONOPROXY entry exists if and only if this function supplied the
	// default proxy or repaired an inherited origin-leading configuration. When
	// the caller names another proxy, only this function's own inherited entry
	// is dropped; patterns the caller owns are left alone.
	//
	// GONOSUMDB is set whenever an origin is declared, whichever proxy serves
	// the workspace's own modules.
	registries := declaredGoRegistries(out)
	originHost := moduleOriginHost(out)
	originPattern := moduleOriginPattern(originHost)
	// The declared chain is what we supply and what we repair to; a workspace
	// that declares no `registries.go.proxy` gets the public default.
	declaredProxy := goProxyChain(registries, originHost)
	proxy := strings.TrimSpace(envValue(out, "GOPROXY"))
	switch {
	case originPattern == "":
		if proxy == "" {
			out = setEnvValue(out, "GOPROXY", declaredProxy)
		}
	case proxy == "" || proxyLeadsWithOrigin(proxy, originHost):
		out = setEnvValue(out, "GOPROXY", declaredProxy)
		out = setEnvValue(
			out,
			"GONOPROXY",
			appendCommaValue(envValue(out, "GONOPROXY"), originPattern),
		)
	default:
		// Dropping our own inherited entry is NOT enough to hand the caller's
		// proxy the origin's modules. GOPRIVATE and GONOPROXY also live in Go's
		// own env file (`go env -w`), which this process environment cannot see
		// or edit, and Go falls GONOPROXY back to GOPRIVATE whenever GONOPROXY is
		// EMPTY — so both "unset" and "set to empty" silently re-enable origin
		// routing and bypass the proxy the caller just chose. Measured: a
		// developer machine with `GOPRIVATE=<origin>/*` sent a fixture-proxy job
		// straight to the real origin and it answered 401.
		//
		// Only a non-empty value overrides the file. Keep the caller's own
		// patterns, and fall back to Go's match-nothing sentinel when removing
		// ours leaves the list empty.
		remaining := removeCommaValue(envValue(out, "GONOPROXY"), originPattern)
		if remaining == "" {
			remaining = "none"
		}
		out = setEnvValue(out, "GONOPROXY", remaining)
	}
	if originPattern != "" {
		out = setEnvValue(
			out,
			"GONOSUMDB",
			appendCommaValue(envValue(out, "GONOSUMDB"), originPattern),
		)
	}

	// On a hosted run the workspace-fetch job already downloaded every module,
	// and no go command a task runs may download one (OfflineGoOverrides).
	// Without the signal nothing here changes.
	out = withOfflineDependencies(out)

	if goRoot := managedGoRoot(goBinary, out); goRoot != "" {
		out = setEnvValue(out, "GOROOT", goRoot)
		out = setEnvValue(out, "PATH", prependPath(envValue(out, "PATH"), filepath.Join(goRoot, "bin")))
	} else if explicitGoRoot := strings.TrimSpace(envValue(out, "GOROOT")); explicitGoRoot != "" {
		explicitBinary := filepath.Join(explicitGoRoot, "bin", goBinaryName())
		if filepath.Clean(explicitBinary) != filepath.Clean(goBinary) {
			// ResolveGo ignored this GOROOT because its compiler was not
			// executable. Do not let the stale value poison a valid PATH or
			// workspace-managed fallback selected afterward.
			out = removeEnv(out, "GOROOT")
		}
	}
	return out
}

// ModuleDownloadsDisabled reports whether env forbids EVERY module download,
// which is Go's `GOPROXY=off` contract.
//
// The FIRST entry of the proxy list decides it. Go walks the list in order and
// stops at the `off` sentinel wherever it sits, so a list that leads with it can
// never reach a proxy. A module GONOPROXY matches still goes to its origin
// directly; OfflineGoOverrides sets GONOPROXY=none to close that route too.
//
// The comparison is exact, like Go's own. An unrecognized spelling resolves to
// "downloads are allowed", which is the fail-safe direction: the caller runs the
// command and reads the real outcome instead of assuming one.
//
// An inherited `off` reaches this predicate unchanged, because GoCommandEnv
// rewrites GOPROXY only when it is empty or leads with the declared module
// origin. The other `off` it sets comes from the offline signal
// (OfflineDependencies). That is what keeps the value a `go` command observes
// and the values a cache key hashes in agreement: build-tidy keys on GOPROXY
// and on the offline signal.
func ModuleDownloadsDisabled(env []string) bool {
	proxy := strings.TrimSpace(envValue(env, "GOPROXY"))
	if i := strings.IndexAny(proxy, ",|"); i >= 0 {
		proxy = proxy[:i]
	}
	return strings.TrimSpace(proxy) == "off"
}

// RegistryCredentialHost returns the one host a `go` command run with this
// environment needs a per-user credential for, or "" when there is none.
//
// That host is the DECLARED module origin, not the leading GOPROXY entry, and
// the difference is the whole point of the routing above. GOPROXY leads with the
// public mirror by construction — leading with the origin breaks Go's checksum
// probe — and GONOPROXY then routes the workspace's own modules PAST the mirror
// straight to the origin, which authenticates them. So the origin is the host
// `go` actually presents credentials to, and GOPROXY's host is a public mirror
// that wants none. A workspace that declares no origin has no private modules
// and needs no credential.
//
// workspaceRoot supplies the declaration source when the environment does not
// already carry PUTNAMI_WORKSPACE_ROOT — an extension job knows its root from
// its job context even when the variable is absent. env is not mutated.
func RegistryCredentialHost(env []string, workspaceRoot string) string {
	if root := strings.TrimSpace(workspaceRoot); root != "" {
		// envValue reads last-wins, so appending overrides an inherited value
		// without rewriting the caller's slice.
		env = append(append([]string(nil), env...), workspaceRootEnvVar+"="+root)
	}
	return moduleOriginHost(env)
}

// proxyLeadsWithOrigin reports whether the first GOPROXY entry targets the
// declared module origin. The first entry owns Go's checksum-database probe, so
// it must never be the origin even when that legacy value came from go env. An
// undeclared origin matches nothing: there is no host to compare against, and
// guessing one would be the assumption this seam exists to avoid.
func proxyLeadsWithOrigin(proxy, host string) bool {
	if host == "" {
		return false
	}
	leader, _, _ := strings.Cut(strings.TrimSpace(proxy), ",")
	leader, _, _ = strings.Cut(leader, "|")
	parsed, err := url.Parse(strings.TrimSpace(leader))
	return err == nil && strings.EqualFold(parsed.Hostname(), host)
}

// moduleOriginPattern is the GONOPROXY/GONOSUMDB glob covering every module
// served by host, or "" when no origin is declared.
func moduleOriginPattern(host string) string {
	if host == "" {
		return ""
	}
	return host + "/*"
}

// goRegistries is the `registries.go` entry of the workspace document: where
// this workspace publishes its own modules, and which proxies serve module
// downloads. Its shape is the one the Go extension's ecosystem profile declares
// in putnami.extension.json, so the document and the reader cannot drift.
type goRegistries struct {
	// Origin is the vanity module server this workspace publishes to. It is the
	// host GONOPROXY routes the workspace's own modules to, and the host `go`
	// presents a credential to. Empty means the workspace has no vanity server.
	Origin string `json:"origin"`
	// Proxy is the GOPROXY chain, in order. Empty falls back to defaultGoProxy.
	Proxy []string `json:"proxy"`
}

// UseContextRegistries records the `registries` member a job context carries, so
// the environment builder resolves the PROJECT's effective entry rather than the
// workspace default a project may override.
//
// The entry travels through the job context and only through it (it is
// deliberately outside the resolved parameter bag the cache key hashes), and one
// extension process runs one job, so recording it once at dispatch is the whole
// seam. A context with no `registries` member leaves the workspace file in
// charge.
func UseContextRegistries(registries []byte) {
	entry, ok := goEntryOf(registries)
	if !ok {
		return
	}
	declaredGoRegistryMu.Lock()
	defer declaredGoRegistryMu.Unlock()
	contextGoRegistries = &entry
}

// goEntryOf decodes the `go` entry out of a raw `registries` map. A malformed
// document yields no entry rather than an error: this sits on the path of every
// go invocation, and a build must not fail because a key is unreadable.
func goEntryOf(registries []byte) (goRegistries, bool) {
	if len(registries) == 0 {
		return goRegistries{}, false
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(registries, &entries) != nil {
		return goRegistries{}, false
	}
	raw, ok := entries["go"]
	if !ok || len(raw) == 0 {
		return goRegistries{}, false
	}
	var entry goRegistries
	if json.Unmarshal(raw, &entry) != nil {
		return goRegistries{}, false
	}
	return entry, true
}

// moduleOriginHost resolves the host of the Go module origin this workspace
// publishes to — its vanity module server.
//
// It is DECLARED, not detected. Deriving it from the module paths found on disk
// would over-reach the moment a workspace hosts modules under a shared forge:
// two projects under github.com/acme and github.com/other share the first path
// element, and a pattern built from it would route every github.com module
// direct, breaking public dependency resolution.
//
// Resolution follows the same seam the publish job already reads, so a client
// declares the host exactly once:
//
//  1. GO_REGISTRY_URL, the environment form publish already honors;
//  2. registries.go.origin, from the job context's entry when there is one,
//     else from putnami.workspace.json.
//
// A workspace that declares neither has no vanity server. It resolves to "",
// no pattern is injected, and Go behaves exactly as it does outside Putnami.
func moduleOriginHost(env []string) string {
	raw := strings.TrimSpace(envValue(env, goRegistryURLEnvVar))
	if raw == "" {
		raw = declaredGoRegistries(env).Origin
	}
	return hostOfRegistryURL(raw)
}

// DeclaredGoOrigin returns the Go module origin this workspace publishes to, as
// the endpoint a publisher writes to — not just its host.
//
// It is the SAME resolution GoCommandEnv routes with, in the same order, so the
// registry a publish writes to and the host `go` presents a credential to can
// never be two different places. workspaceRoot supplies the declaration source
// when the environment does not already carry PUTNAMI_WORKSPACE_ROOT; env is
// not mutated.
func DeclaredGoOrigin(env []string, workspaceRoot string) string {
	if raw := strings.TrimSpace(envValue(env, goRegistryURLEnvVar)); raw != "" {
		return raw
	}
	if root := strings.TrimSpace(workspaceRoot); root != "" {
		env = append(append([]string(nil), env...), workspaceRootEnvVar+"="+root)
	}
	return strings.TrimSpace(declaredGoRegistries(env).Origin)
}

// hostOfRegistryURL is the lowercase hostname of a declared endpoint, "" when
// there is none. A bare host is the shape a human writes; url.Parse reads it as
// a path, so a scheme is supplied first.
func hostOfRegistryURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname()))
}

// goProxyChain is the GOPROXY value this workspace declares, or defaultGoProxy
// when it declares none.
//
// An entry that targets the ORIGIN is dropped, wherever it sits in the list.
// Go asks only the FIRST proxy whether it relays the checksum database, and a
// vanity catch-all answers 200 on /sumdb/<name>/supported before serving 404 on
// every tile, which pins a dead base for the whole run; and the workspace's own
// modules already reach the origin directly through GONOPROXY, so an origin
// entry can only take routing away from that path. A list left empty by that
// filter falls back to the default chain.
func goProxyChain(registries goRegistries, originHost string) string {
	entries := make([]string, 0, len(registries.Proxy))
	for _, entry := range registries.Proxy {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if originHost != "" && strings.EqualFold(hostOfRegistryURL(entry), originHost) {
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return defaultGoProxy
	}
	return strings.Join(entries, ",")
}

// declaredGoRegistryCache memoizes the workspace declaration per root.
// GoCommandEnv runs for every child go invocation while the declaration changes
// only when the workspace file does, so one read per root per process is the
// right granularity. A root that declares nothing is cached too, so a workspace
// without the section does not re-read the file on every invocation.
var (
	declaredGoRegistryMu    sync.Mutex
	declaredGoRegistryCache = map[string]goRegistries{}
	contextGoRegistries     *goRegistries
)

// declaredGoRegistries resolves the effective `registries.go` entry: the job
// context's when a job recorded one, else the workspace document's.
func declaredGoRegistries(env []string) goRegistries {
	declaredGoRegistryMu.Lock()
	if contextGoRegistries != nil {
		entry := *contextGoRegistries
		declaredGoRegistryMu.Unlock()
		return entry
	}
	declaredGoRegistryMu.Unlock()
	return workspaceGoRegistries(strings.TrimSpace(envValue(env, workspaceRootEnvVar)))
}

func workspaceGoRegistries(workspaceRoot string) goRegistries {
	if workspaceRoot == "" {
		return goRegistries{}
	}
	declaredGoRegistryMu.Lock()
	defer declaredGoRegistryMu.Unlock()
	if cached, ok := declaredGoRegistryCache[workspaceRoot]; ok {
		return cached
	}
	var declared goRegistries
	// An unreadable or malformed workspace file yields no declaration rather
	// than an error: this is an environment helper on the path of every go
	// invocation, and a build must not fail because a key is absent.
	if raw, err := os.ReadFile(filepath.Join(workspaceRoot, workspaceConfigFile)); err == nil {
		var cfg struct {
			Registries json.RawMessage `json:"registries"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			if entry, ok := goEntryOf(cfg.Registries); ok {
				declared = entry
			}
		}
	}
	declaredGoRegistryCache[workspaceRoot] = declared
	return declared
}

// FindGoWork walks up from dir looking for a go.work file and returns the
// absolute path to the nearest one, or "" if none exists between dir and the
// filesystem root. This mirrors `go`'s own GOWORK=auto discovery so callers can
// name the workspace file explicitly instead of relying on an ambient GOWORK.
func FindGoWork(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(abs, "go.work")
		if fileExists(candidate) {
			return candidate
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// WorkspaceBuildEnv returns GoCommandEnv adjusted so a Go command for a module
// rooted at projectDir resolves its workspace `replace` directives.
//
// Any inherited GOWORK entry is dropped first, so a leaked GOWORK=off — e.g.
// from a parent process that deliberately built an extension or CLI standalone
// and then spawned this build (as the cloud deploy path once did) — cannot force
// single-module resolution and break a go.work-resolved workload whose siblings
// resolve through workspace replaces. When a go.work governs projectDir, GOWORK
// is then pointed at it explicitly; with no governing go.work the module is
// genuinely standalone and GOWORK is left unset (go's default).
//
// PWD is deliberately removed. The runtime can receive a logical Conductor
// workspace alias while its own inherited PWD names the physical checkout (or
// vice versa). os/exec preserves that stale value when Cmd.Env is explicit,
// and cmd/go then compares relative package patterns against a different path
// identity than GOWORK's use entries. Removing PWD makes the child derive its
// real working directory, while resolving projectDir below gives GOWORK the
// same physical identity.
//
// env is typically os.Environ(). A fresh slice is returned; env is not mutated.
func WorkspaceBuildEnv(env []string, projectDir, goBinary string) []string {
	out := removeEnv(GoCommandEnv(env, goBinary), goWorkEnvVar)
	out = removeEnv(out, "PWD")
	if resolved, err := filepath.EvalSymlinks(projectDir); err == nil {
		projectDir = resolved
	}
	if gowork := FindGoWork(projectDir); gowork != "" {
		out = setEnvValue(out, goWorkEnvVar, gowork)
	}
	return out
}

func managedGoRoot(goBinary string, env []string) string {
	return managedGoRootOn(runtime.GOOS, goBinary, env)
}

// managedGoRootOn is managedGoRoot on a host running goos. On Windows the go
// command of a managed install is a candidate too, since resolveGoOn returns
// it there in place of the bin/go link.
func managedGoRootOn(goos, goBinary string, env []string) string {
	if strings.TrimSpace(goBinary) == "" {
		return ""
	}
	binary := filepath.Clean(goBinary)
	binaryName := pkgmeta.ExecutableName(goos, "go")
	var candidates []string
	workspaceRoot := strings.TrimSpace(envValue(env, workspaceRootEnvVar))
	if workspaceRoot != "" {
		candidates = append(candidates,
			filepath.Join(workspaceRoot, ".putnami", "extensions", "@putnami-go", "bin", binaryName),
			filepath.Join(extRoot(workspaceRoot), "bin", binaryName),
		)
	}
	if extensionRoot := strings.TrimSpace(envValue(env, extensionRootEnvVar)); extensionRoot != "" {
		candidates = append(candidates, filepath.Join(extensionRoot, "bin", binaryName))
	}
	managed := false
	for _, candidate := range candidates {
		if binary == filepath.Clean(candidate) {
			managed = true
			break
		}
	}
	if !managed && goos == "windows" && workspaceRoot != "" {
		managed = isManagedGoInstall(workspaceRoot, binary, binaryName)
	}
	if !managed {
		return ""
	}

	actual := binary
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		actual = resolved
	}
	return filepath.Clean(filepath.Join(filepath.Dir(actual), ".."))
}

// isManagedGoInstall reports whether binary is the go command, named
// binaryName, of a Go release `putnami install` manages under workspaceRoot:
// libs/go-<version>/go/bin/<binaryName>.
func isManagedGoInstall(workspaceRoot, binary, binaryName string) bool {
	rel, err := filepath.Rel(managedGoLibs(workspaceRoot), binary)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 || parts[1] != "go" || parts[2] != "bin" || parts[3] != binaryName {
		return false
	}
	_, ok := managedGoVersion(parts[0])
	return ok
}

// envValue, setEnvValue and removeEnv match variable names the way this
// platform's process environment does (envkeys.Host).
func envValue(env []string, key string) string {
	return envkeys.Host.Last(env, key)
}

func setEnvValue(env []string, key, value string) []string {
	return envkeys.Host.Set(env, key, value)
}

func removeEnv(env []string, key string) []string {
	return envkeys.Host.Remove(env, key)
}

func appendCommaValue(current, value string) string {
	var values []string
	seen := make(map[string]struct{})
	for _, entry := range strings.Split(current, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		values = append(values, entry)
	}
	if _, ok := seen[value]; !ok {
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

// removeCommaValue drops every occurrence of value from a comma-separated list,
// keeping the remaining entries in order. It is the inverse of appendCommaValue.
func removeCommaValue(current, value string) string {
	var values []string
	for _, entry := range strings.Split(current, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == value {
			continue
		}
		values = append(values, entry)
	}
	return strings.Join(values, ",")
}

func prependPath(current, entry string) string {
	entry = filepath.Clean(entry)
	parts := filepath.SplitList(current)
	out := []string{entry}
	for _, part := range parts {
		if part == "" || filepath.Clean(part) == entry {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, string(os.PathListSeparator))
}
