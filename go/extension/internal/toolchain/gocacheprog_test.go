package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

// withExecutable pins the binary path GOCACHEPROG is built from, and restores
// the real resolver afterwards.
func withExecutable(t *testing.T, path string, err error) {
	t.Helper()
	previous := executablePath
	executablePath = func() (string, error) { return path, err }
	t.Cleanup(func() { executablePath = previous })
}

// withRemoteBuildCache sets the recorded option for one test.
func withRemoteBuildCache(t *testing.T, enabled bool) {
	t.Helper()
	UseRemoteBuildCacheOption(enabled)
	t.Cleanup(func() { UseRemoteBuildCacheOption(true) })
}

func goCacheProgOf(env []string) string { return envValue(env, "GOCACHEPROG") }

// TestGoCacheProgIsSetOnlyForAProviderBackedRun is the gate that keeps this
// feature invisible everywhere else. Core exports the socket variable only for
// a live provider-backed run, so its absence has to leave the environment
// exactly as it was — including on a --no-cache run and a run under trust
// "none", which never carry it.
func TestGoCacheProgIsSetOnlyForAProviderBackedRun(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "the-helper-backs-the-go-build-cache-only-for-a-provider-backed-run")
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	base := []string{homeEnvName() + "=" + t.TempDir()}

	if got := goCacheProgOf(GoCommandEnv(base, "")); got != "" {
		t.Fatalf("GOCACHEPROG = %q with no object-cache socket, want unset", got)
	}

	withSocket := append(append([]string(nil), base...), cache.ObjectCacheSocketEnv+"=/tmp/exchange/objects.sock")
	got := goCacheProgOf(GoCommandEnv(withSocket, ""))
	if got != "/opt/putnami/putnami-go "+GoCacheProgSubcommand {
		t.Fatalf("GOCACHEPROG = %q, want the helper command", got)
	}
}

// TestAnEmptySocketValueIsNotASocket covers the variable being present but
// empty, which is how an environment clears an inherited value.
func TestAnEmptySocketValueIsNotASocket(t *testing.T) {
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	env := GoCommandEnv([]string{homeEnvName() + "=" + t.TempDir(), cache.ObjectCacheSocketEnv + "=  "}, "")
	if got := goCacheProgOf(env); got != "" {
		t.Fatalf("GOCACHEPROG = %q for an empty socket variable, want unset", got)
	}
}

// TestTheOptionTurnsTheHelperOff pins the user's off switch: a run that has a
// provider but asked for the plain local cache gets the environment it had
// before this feature existed.
func TestTheOptionTurnsTheHelperOff(t *testing.T) {
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	withRemoteBuildCache(t, false)
	env := GoCommandEnv([]string{
		homeEnvName() + "=" + t.TempDir(),
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
	}, "")
	if got := goCacheProgOf(env); got != "" {
		t.Fatalf("GOCACHEPROG = %q with remote-build-cache off, want unset", got)
	}
	if envValue(env, "GOCACHE") == "" || envValue(env, "GOMODCACHE") == "" {
		t.Fatal("turning the option off must leave the ordinary Go cache variables in place")
	}
}

// TestTheGoCachesStaySetAlongsideTheHelper pins that this change ADDS a
// variable rather than replacing two. GOMODCACHE is still the module cache the
// helper has nothing to do with, and GOCACHE keeps `go env` stable and the
// fallback one variable away.
func TestTheGoCachesStaySetAlongsideTheHelper(t *testing.T) {
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	root := t.TempDir()
	env := GoCommandEnv([]string{
		GoCacheDirEnv + "=" + root,
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
	}, "")
	if got := envValue(env, "GOCACHE"); got != filepath.Join(root, "build") {
		t.Fatalf("GOCACHE = %q, want %q", got, filepath.Join(root, "build"))
	}
	if got := envValue(env, "GOMODCACHE"); got != filepath.Join(root, "mod") {
		t.Fatalf("GOMODCACHE = %q, want %q", got, filepath.Join(root, "mod"))
	}
	if goCacheProgOf(env) == "" {
		t.Fatal("GOCACHEPROG is unset for a provider-backed run")
	}
}

// TestACallerSuppliedHelperIsPreserved keeps this extension out of a decision
// the caller already made: naming a cache helper is a choice about where
// compiled output comes from.
func TestACallerSuppliedHelperIsPreserved(t *testing.T) {
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	env := GoCommandEnv([]string{
		homeEnvName() + "=" + t.TempDir(),
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
		"GOCACHEPROG=/usr/local/bin/my-cache",
	}, "")
	if got := goCacheProgOf(env); got != "/usr/local/bin/my-cache" {
		t.Fatalf("GOCACHEPROG = %q, want the caller's own helper", got)
	}
}

// TestAnUnresolvableExecutableLeavesTheVariableUnset keeps a failure here from
// taking the build down: the go command aborts when it cannot start the program
// GOCACHEPROG names, so "no helper" has to beat "a helper we cannot name".
func TestAnUnresolvableExecutableLeavesTheVariableUnset(t *testing.T) {
	withExecutable(t, "", errors.New("no executable"))
	env := GoCommandEnv([]string{
		homeEnvName() + "=" + t.TempDir(),
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
	}, "")
	if got := goCacheProgOf(env); got != "" {
		t.Fatalf("GOCACHEPROG = %q when this binary's path is unknown, want unset", got)
	}
}

// TestTheHelperPathIsQuotedTheWayGoParsesIt is the quoting contract. The go
// command splits GOCACHEPROG with cmd/internal/quoted.Split, which does NO
// unescaping inside quotes — so backslash escaping (what strconv.Quote
// produces) would arrive in the path instead of disappearing from it, and a
// macOS home directory with a space in it is an ordinary case.
func TestTheHelperPathIsQuotedTheWayGoParsesIt(t *testing.T) {
	cases := []struct {
		name   string
		binary string
		want   string
		ok     bool
	}{
		{name: "plain", binary: "/opt/putnami/putnami-go", want: "/opt/putnami/putnami-go", ok: true},
		{name: "space", binary: "/Users/me/My Projects/putnami-go", want: "'/Users/me/My Projects/putnami-go'", ok: true},
		{name: "single quote", binary: "/Users/o'brien/putnami-go", want: `"/Users/o'brien/putnami-go"`, ok: true},
		{name: "double quote", binary: `/tmp/wei"rd/putnami-go`, want: `'/tmp/wei"rd/putnami-go'`, ok: true},
		{name: "both quotes", binary: `/tmp/o'wei"rd/putnami-go`, ok: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			quoted, ok := quoteGoCacheProgArg(testCase.binary)
			if ok != testCase.ok {
				t.Fatalf("quoteGoCacheProgArg(%q) ok = %t, want %t", testCase.binary, ok, testCase.ok)
			}
			if !ok {
				return
			}
			if quoted != testCase.want {
				t.Fatalf("quoteGoCacheProgArg(%q) = %q, want %q", testCase.binary, quoted, testCase.want)
			}
			if split := splitLikeGo(t, quoted+" "+GoCacheProgSubcommand); len(split) != 2 || split[0] != testCase.binary {
				t.Fatalf("go would parse %q as %q, want [%q %q]", quoted, split, testCase.binary, GoCacheProgSubcommand)
			}
		})
	}
}

// TestAnUnquotableHelperPathIsSkipped keeps the same failure closed end to end:
// a path that cannot be expressed leaves the variable unset rather than
// producing a value the go command would fail to parse.
func TestAnUnquotableHelperPathIsSkipped(t *testing.T) {
	withExecutable(t, `/tmp/o'wei"rd/putnami-go`, nil)
	env := GoCommandEnv([]string{
		homeEnvName() + "=" + t.TempDir(),
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
	}, "")
	if got := goCacheProgOf(env); got != "" {
		t.Fatalf("GOCACHEPROG = %q for an unquotable path, want unset", got)
	}
}

// splitLikeGo reproduces cmd/internal/quoted.Split, which is internal to the Go
// distribution: single or double quotes delimit a field and NOTHING inside them
// is unescaped.
func splitLikeGo(t *testing.T, value string) []string {
	t.Helper()
	var fields []string
	for len(value) > 0 {
		for len(value) > 0 && strings.ContainsRune(" \t\n\r", rune(value[0])) {
			value = value[1:]
		}
		if value == "" {
			break
		}
		if value[0] == '"' || value[0] == '\'' {
			quote := value[0]
			value = value[1:]
			end := strings.IndexByte(value, quote)
			if end < 0 {
				t.Fatalf("unterminated %c string", quote)
			}
			fields = append(fields, value[:end])
			value = value[end+1:]
			continue
		}
		end := strings.IndexAny(value, " \t\n\r")
		if end < 0 {
			end = len(value)
		}
		fields = append(fields, value[:end])
		value = value[end:]
	}
	return fields
}

// TestTheRealExecutableIsUsedByDefault keeps the default resolver wired: every
// other test replaces it, so nothing else would notice if it were dropped.
func TestTheRealExecutableIsUsedByDefault(t *testing.T) {
	binary, err := executablePath()
	if err != nil {
		t.Skipf("this platform does not report the running executable: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if binary != self {
		t.Fatalf("executablePath() = %q, want %q", binary, self)
	}
}

// TestNoCacheRootLeavesTheHelperUnset pins the third gate: the helper's local
// store lives under the Go cache root, and without one it has no DiskPath to
// answer a put with, so the go command keeps its own default cache.
func TestNoCacheRootLeavesTheHelperUnset(t *testing.T) {
	withExecutable(t, "/opt/putnami/putnami-go", nil)
	env := GoCommandEnv([]string{cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock"}, "")
	if got := goCacheProgOf(env); got != "" {
		t.Fatalf("GOCACHEPROG = %q with no resolvable cache root, want unset", got)
	}
}
