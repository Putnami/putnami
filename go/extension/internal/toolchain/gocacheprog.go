package toolchain

import (
	"os"
	"strings"
	"sync"

	cache "go.putnami.dev/protocol/cache"
)

// GOCACHEPROG wiring: pointing the `go` command's build cache at this
// extension's own helper.
//
// GOCACHEPROG names a program the go command starts as a child and asks to back
// its build cache instead of using GOCACHE's directory directly (Go 1.24+). The
// helper is `putnami-go gocacheprog` (internal/gocacheprog): it keeps a local
// cache and additionally reads and writes the run's shared object cache, so a
// machine that starts cold can serve compiled packages another machine already
// built.
//
// The helper's local cache keeps its action records in its own directory,
// <root>/prog, and its compiled objects in GOCACHE's directory, <root>/build
// (GoBuildCacheDir), in the go command's own data-file format. The gating below
// means one machine runs the go command both ways: through the helper in a job
// of a provider-backed run, and without it everywhere else. Under GOCACHEPROG
// the go command writes nothing to GOCACHE, so sharing that one directory is
// what keeps a single copy of each object for both kinds. The
// helper is NOT turned on for the other kind to get there: the conditions below
// are the contract for when a job may talk to the provider at all.
//
// Two conditions gate it, and both must hold:
//
//  1. the job environment carries the provider's object-cache socket
//     (cache.ObjectCacheSocketEnv). Core exports it only for a live
//     provider-backed run, so an ordinary local build, a --no-cache run, and a
//     run under trust "none" never see it and behave exactly as before;
//  2. the `remote-build-cache` option is on, which is the user's off switch for
//     a run that has a provider but wants the plain local cache.
//
// A resolved Go cache root is required as well: the helper's local store lives
// under it, and without one there is no DiskPath to answer a put with.
//
// GOCACHE and GOMODCACHE stay set either way. GOMODCACHE is still the module
// cache — the helper has nothing to do with downloads. The go command ignores
// GOCACHE for lookups once GOCACHEPROG is set, but the helper's objects live in
// that directory, so a later go command without the helper reads them there.

const (
	// GoCacheProgSubcommand is the hidden putnami-go argument that runs the
	// helper. It is declared here rather than in internal/gocacheprog because
	// that package resolves its cache root through this one, and the value has
	// to be identical on both sides of the exec.
	GoCacheProgSubcommand = "gocacheprog"

	// goCacheProgEnv is the variable the go command reads the helper command
	// line from.
	goCacheProgEnv = "GOCACHEPROG"
)

// executablePath resolves this process's own binary. It is a package variable
// so a test can exercise the failure branch, which is otherwise unreachable.
var executablePath = os.Executable

// remoteBuildCache records the job's `remote-build-cache` option.
//
// The option reaches this package the same way the `registries` entry does: the
// dispatch table records it once, before the job builds its first Go
// environment (see UseRemoteBuildCacheOption). The alternative — threading a
// parameter through GoCommandEnv — would touch every one of the dozen call
// sites that build a Go environment, including the ones inside toolchain
// resolution that have no job context at all.
var (
	remoteBuildCacheMu      sync.Mutex
	remoteBuildCacheEnabled = true
)

// UseRemoteBuildCacheOption records whether this job may back the Go build
// cache with the run's object cache. The default is ON: a job that never calls
// this — a control call, a test — keeps the behavior the socket's presence
// alone decides.
func UseRemoteBuildCacheOption(enabled bool) {
	remoteBuildCacheMu.Lock()
	defer remoteBuildCacheMu.Unlock()
	remoteBuildCacheEnabled = enabled
}

func remoteBuildCacheOption() bool {
	remoteBuildCacheMu.Lock()
	defer remoteBuildCacheMu.Unlock()
	return remoteBuildCacheEnabled
}

// withGoCacheProg points GOCACHEPROG at this binary's helper when the run has
// an object cache and the option allows it.
//
// A caller that set GOCACHEPROG itself keeps it: naming a cache helper is an
// explicit choice about where compiled output comes from, and overriding it
// would be this extension deciding it knows better.
func withGoCacheProg(env []string) []string {
	if strings.TrimSpace(envValue(env, goCacheProgEnv)) != "" {
		return env
	}
	if strings.TrimSpace(envValue(env, cache.ObjectCacheSocketEnv)) == "" {
		return env
	}
	if !remoteBuildCacheOption() {
		return env
	}
	if ResolveGoCacheRoot(EnvLookup(env)) == "" {
		// The helper keeps its local store under the same root as GOCACHE; with
		// no root it has nowhere to write the DiskPath every put requires, and
		// a helper that refuses puts fails the build. Leave the go command on
		// its own default cache instead.
		return env
	}
	binary, err := executablePath()
	if err != nil || strings.TrimSpace(binary) == "" {
		return env
	}
	quoted, ok := quoteGoCacheProgArg(binary)
	if !ok {
		// A path that cannot be quoted would make the go command fail to parse
		// GOCACHEPROG and abort the build. Leaving the variable unset costs a
		// shared cache; setting a broken one costs the build.
		return env
	}
	return setEnvValue(env, goCacheProgEnv, quoted+" "+GoCacheProgSubcommand)
}

// quoteGoCacheProgArg quotes one GOCACHEPROG argument the way the go command
// parses it.
//
// The parser is cmd/internal/quoted.Split, and it does NO unescaping inside
// quotes: it takes everything up to the matching quote literally. Backslash
// escaping — what strconv.Quote produces — would therefore arrive in the
// argument rather than disappearing from it, so the rules below mirror
// quoted.Join instead: leave a plain argument alone, wrap one that contains a
// space in single quotes, use double quotes when it contains a single quote.
// An argument holding both quote characters cannot be expressed at all, and
// reports false rather than producing something that parses into a different
// path.
func quoteGoCacheProgArg(arg string) (string, bool) {
	sawSpace := strings.ContainsAny(arg, " \t\n\r")
	sawSingle := strings.Contains(arg, "'")
	sawDouble := strings.Contains(arg, `"`)
	switch {
	case !sawSpace && !sawSingle && !sawDouble:
		return arg, true
	case !sawSingle:
		return "'" + arg + "'", true
	case !sawDouble:
		return `"` + arg + `"`, true
	default:
		return "", false
	}
}
