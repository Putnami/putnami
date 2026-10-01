package gocacheprog

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
	cache "go.putnami.dev/protocol/cache"
)

// The end-to-end proof: a REAL `go build`, backed by this helper, against the
// shared fake provider socket.
//
// Every other test in this package drives the protocol itself. This one lets
// the go command drive it, which is the only way to know that the framing, the
// DiskPath contract and the output-id bookkeeping hold against the
// implementation that actually consumes them. It is also the only test that can
// show the point of the feature: a second build whose local cache is EMPTY
// compiles nothing, because the objects come back from the provider.

// helperProcessEnv marks the re-executed test binary as the cache helper. The
// binary serves as its own GOCACHEPROG program, which avoids linking a second
// one for a test and keeps the code under test identical to the shipped path.
const helperProcessEnv = "PUTNAMI_GOCACHEPROG_TEST_HELPER"

// TestHelperProcessIsTheCacheProg is not a test: it is the GOCACHEPROG program
// the build below runs. It exits before the testing framework can write
// anything to stdout, which the protocol reserves for JSON responses.
func TestHelperProcessIsTheCacheProg(t *testing.T) {
	if os.Getenv(helperProcessEnv) == "" {
		t.Skip("not the helper process")
	}
	if err := Run(os.Stdin, os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "gocacheprog helper: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// goBinary locates a Go toolchain for the child build.
func goBinary(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("GOROOT"); root != "" {
		candidate := filepath.Join(root, "bin", "go")
		if runtime.GOOS == "windows" {
			candidate += ".exe"
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	binary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain to build with: %v", err)
	}
	return binary
}

// TestARealGoBuildSharesItsObjectsThroughTheProvider builds a module twice. The
// first build has nothing anywhere and fills the provider. The second starts
// from an EMPTY local cache and must be SERVED by the provider instead of
// recompiling — the helper reports how many objects it traded, which is the
// only way to tell the two apart: both produce the same binary and the same
// local cache either way.
func TestARealGoBuildSharesItsObjectsThroughTheProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this test compiles a module twice")
	}
	goBin := goBinary(t)
	server := startObjectCache(t)
	module := writeModule(t)

	firstBinary := filepath.Join(t.TempDir(), "first")
	firstServed, firstOffered := runGoBuild(t, goBin, module, firstBinary, t.TempDir(), server.Path())
	if firstOffered == 0 {
		t.Fatal("the first build stored nothing in the object cache")
	}
	if firstServed != 0 {
		t.Fatalf("the first build was served %d objects by an empty provider", firstServed)
	}
	if _, puts := server.Stats(); puts == 0 {
		t.Fatal("the provider received no object-put")
	}

	// A fresh cache root is a machine that has never compiled anything: no
	// GOCACHE, no helper store, nothing but the provider.
	secondBinary := filepath.Join(t.TempDir(), "second")
	secondServed, secondOffered := runGoBuild(t, goBin, module, secondBinary, t.TempDir(), server.Path())
	if secondServed < minimumServedObjects {
		t.Fatalf("the second build was served only %d objects from the provider; a cold local cache should have been served the whole compilation", secondServed)
	}
	if secondOffered >= firstOffered {
		t.Fatalf("the second build offered %d objects against the first build's %d; it recompiled what the provider already held",
			secondOffered, firstOffered)
	}
	t.Logf("second build: %d objects served by the provider, %d offered back", secondServed, secondOffered)

	first, err := os.ReadFile(firstBinary)
	if err != nil {
		t.Fatalf("read the first binary: %v", err)
	}
	second, err := os.ReadFile(secondBinary)
	if err != nil {
		t.Fatalf("read the second binary: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("the two builds produced different binaries (%d and %d bytes); cached objects must reproduce the same output",
			len(first), len(second))
	}
}

// minimumServedObjects is the floor the second build has to clear. Compiling
// this module from nothing takes well over a hundred actions — the standard
// library packages it reaches, plus its own two — so a floor of twenty is far
// below "served the whole compilation" while staying indifferent to how many
// packages a future Go release splits the standard library into.
const minimumServedObjects = 20

// TestARealGoBuildWorksWithoutAProvider is the same build with no socket at
// all: the helper is then an ordinary local cache, and the second build must
// still succeed and reuse it.
func TestARealGoBuildWorksWithoutAProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this test compiles a module twice")
	}
	goBin := goBinary(t)
	module := writeModule(t)
	cacheRoot := t.TempDir()

	binary := filepath.Join(t.TempDir(), "first")
	served, offered := runGoBuild(t, goBin, module, binary, cacheRoot, "")
	if served != 0 || offered != 0 {
		t.Fatalf("a build with no provider exchanged %d/%d objects", served, offered)
	}
	if countEntries(t, filepath.Join(cacheRoot, progDirName)) == 0 {
		t.Fatal("the helper stored nothing locally")
	}
	again := filepath.Join(t.TempDir(), "second")
	runGoBuild(t, goBin, module, again, cacheRoot, "")
}

// TestTheHelperAndAPlainGoCommandKeepOneCopyOfEachObject pins that one cache
// root keeps one copy of each object.
//
// One machine runs the go command two ways against ONE cache root: through this
// helper (a job of a run with a cache provider) and without it (everything
// else — a run with no provider, a --no-cache or nested run, a runtime prepare
// script, the wrapper's own bootstrap). The go command writes nothing to
// GOCACHE while the helper serves it, so the only way both kinds can share
// what they compiled is for the helper's bodies to live where the go command's
// own are. Both kinds compile the same packages to the same bytes, so the root
// must grow by about one copy of them, whichever kind ran first. With the
// bodies in a private tree it grew by two.
func TestTheHelperAndAPlainGoCommandKeepOneCopyOfEachObject(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this test compiles a module four times")
	}
	goBin := goBinary(t)
	module := writeModule(t)
	for _, order := range []struct {
		name        string
		helperFirst bool
	}{
		{name: "plain go command first", helperFirst: false},
		{name: "helper first", helperFirst: true},
	} {
		t.Run(order.name, func(t *testing.T) {
			root := t.TempDir()
			runGoTest(t, goBin, module, root, order.helperFirst)
			oneCopy := treeBytes(t, root)
			if oneCopy == 0 {
				t.Fatal("the first cold run stored nothing in the cache root")
			}
			runGoTest(t, goBin, module, root, !order.helperFirst)
			both := treeBytes(t, root)
			ratio := float64(both) / float64(oneCopy)
			t.Logf("cache root: %d bytes after the first run, %d after both (ratio %.3f)", oneCopy, both, ratio)
			if ratio >= maximumGrowthRatio {
				t.Fatalf("the cache root grew from %d to %d bytes (ratio %.2f, want < %.1f): the second kind of go command stored its own copy of objects the first already held",
					oneCopy, both, ratio, maximumGrowthRatio)
			}
		})
	}
}

// maximumGrowthRatio is the acceptance bound of that test. One copy plus the
// second kind's action records is a ratio just above 1; a second copy of the
// bodies is a ratio near 2.
const maximumGrowthRatio = 1.2

// writeModule creates a small standalone module. It imports the standard
// library only, so the build needs no network and no module download.
func writeModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.test/cacheprog\n\ngo 1.24\n",
		"main.go": `package main

import (
	"fmt"
	"strings"

	"example.test/cacheprog/lib"
)

func main() {
	fmt.Println(strings.ToUpper("built through the object cache"), lib.Value())
}
`,
		"lib/lib.go": `package lib

// Value exists so the build compiles more than one package of its own.
func Value() int { return 42 }
`,
		"lib/lib_test.go": `package lib

import "testing"

// TestValue gives ` + "`go test`" + ` a test binary to compile and link.
func TestValue(t *testing.T) {
	if Value() != 42 {
		t.Fatal("Value changed")
	}
}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// goCommandEnv is the environment a go command runs with in these tests: one
// cache root laid out the way toolchain.GoCommandEnv lays it out, and — when
// helper is true — this package as its GOCACHEPROG program.
func goCommandEnv(t *testing.T, cacheRoot, socket string, helper bool) []string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GOCACHE=" + toolchain.GoBuildCacheDir(cacheRoot),
		"GOMODCACHE=" + filepath.Join(cacheRoot, "mod"),
		"GOPATH=" + filepath.Join(cacheRoot, "gopath"),
		"PUTNAMI_GO_CACHE_DIR=" + cacheRoot,
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOWORK=off",
		"CGO_ENABLED=0",
		// The helper reports what it traded only under the debug flag, which is
		// exactly how a user diagnoses the same question.
		"PUTNAMI_DEBUG=1",
	}
	if !helper {
		return env
	}
	// The helper command line is parsed by cmd/internal/quoted.Split, which
	// takes a quoted argument literally; the test binary lives under a
	// temporary directory, so this is the same quoting the toolchain package
	// applies to putnami-go's own path.
	self := os.Args[0]
	if !filepath.IsAbs(self) {
		absolute, err := filepath.Abs(self)
		if err != nil {
			t.Fatalf("resolve the test binary: %v", err)
		}
		self = absolute
	}
	env = append(env,
		"GOCACHEPROG='"+self+"' -test.run=^TestHelperProcessIsTheCacheProg$",
		helperProcessEnv+"=1",
	)
	if socket != "" {
		env = append(env,
			cache.ObjectCacheSocketEnv+"="+socket,
			cache.CacheTrustEnv+"=any",
		)
	}
	return env
}

// runGoTest runs the module's tests cold-or-warm against cacheRoot, with or
// without this package as the GOCACHEPROG helper.
func runGoTest(t *testing.T, goBin, module, cacheRoot string, helper bool) {
	t.Helper()
	command := exec.Command(goBin, "test", "-count=1", "./...") //nolint:gosec // the binary is a resolved Go toolchain
	command.Dir = module
	command.Env = goCommandEnv(t, cacheRoot, "", helper)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go test (helper %t) failed: %v\n%s", helper, err, out)
	}
}

// treeBytes is the total size of the regular files under root.
func treeBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return total
}

// runGoBuild compiles the module with this package as its GOCACHEPROG helper,
// and returns how many objects the helpers reported trading with the provider.
func runGoBuild(t *testing.T, goBin, module, output, cacheRoot, socket string) (served, offered int64) {
	t.Helper()
	command := exec.Command(goBin, "build", "-o", output, ".") //nolint:gosec // the binary is a resolved Go toolchain
	command.Dir = module
	command.Env = goCommandEnv(t, cacheRoot, socket, true)
	var out bytes.Buffer
	command.Stdout = &out
	command.Stderr = &out
	started := time.Now()
	if err := command.Run(); err != nil {
		t.Fatalf("go build failed after %s: %v\n%s", time.Since(started), err, out.String())
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("go build produced no binary: %v", err)
	}
	return sumExchange(t, out.String())
}

// exchangeLine matches the helper's debug summary. One `go build` starts a
// helper per go invocation, so the counts are summed rather than read once.
var exchangeLine = regexp.MustCompile(`object cache served (\d+) objects, offered (\d+)`)

func sumExchange(t *testing.T, output string) (served, offered int64) {
	t.Helper()
	for _, match := range exchangeLine.FindAllStringSubmatch(output, -1) {
		one, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatalf("parse served count %q: %v", match[1], err)
		}
		two, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			t.Fatalf("parse offered count %q: %v", match[2], err)
		}
		served += one
		offered += two
	}
	return served, offered
}

// countEntries reports how many files a cache tree holds.
func countEntries(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return count
}
