package store

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"testing"
)

// pinnedPOSIXKeyHash is TestCacheKey_HashFormatIsPinned's v7 golden: the key of
// pinnedFormatKey on every POSIX host.
const pinnedPOSIXKeyHash = "9386f2a81cee23d19ea159164f9d84bc28b5d91be3fb4fc6e700b6897ccc0297"

// pinnedWindowsKeyHash is the key of pinnedFormatKey on a Windows host,
// computed outside Go from the same preimage plus "osClass\0windows\0".
const pinnedWindowsKeyHash = "dc2d8964082c70ed4a0a502febc5d5ecf7f1d9f40699be43a5a27e2353a526b8"

// pinnedFormatPreimageDigest spells out, byte by byte, the stream
// ComputeHashUsing folds for pinnedFormatKey: every field followed by a NUL.
// optional lands right after the toolchain version, where the runtimeIdentity
// and osClass blocks sit, and before the task.
func pinnedFormatPreimageDigest(optional ...string) string {
	zeros := "0000000000000000000000000000000000000000000000000000000000000000"
	fields := []string{"v7", "ext", "1.2.3", "ed1:" + zeros, "go1.25.7"}
	fields = append(fields, optional...)
	fields = append(fields,
		"build~transpile", "tc1:"+zeros, "proj", "wsid1:"+zeros, "1.0.0", "1.0.0-abc",
		"selectedProjects", "a", "b",
		"", // hashParams of no params
		"up1", "up2",
	)
	h := sha256.New()
	for _, field := range fields {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// An empty OS class writes zero bytes, so every Linux and macOS key keeps its
// v7 address. The Windows class inserts exactly "osClass\0windows\0" after the
// runtime-identity block and before the task, and nothing else moves: a Windows
// key differs from the POSIX key of the same task by that marker alone.
func TestCacheKey_OSClassAddsOnlyItsMarker(t *testing.T) {
	cm := NewCacheManager(nil)
	if structural := pinnedFormatPreimageDigest(); structural != pinnedPOSIXKeyHash {
		t.Fatalf("the structural preimage drifted from the v7 golden: %s != %s", structural, pinnedPOSIXKeyHash)
	}

	posix, err := pinnedFormatKey().ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	if posix != pinnedPOSIXKeyHash {
		t.Fatalf("an empty OS class moved the key: got %s, want %s", posix, pinnedPOSIXKeyHash)
	}

	windows := pinnedFormatKey()
	windows.OSClass = OSClassWindows
	got, err := windows.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	if want := pinnedFormatPreimageDigest("osClass", "windows"); got != want {
		t.Fatalf("the Windows key is not the POSIX preimage plus the osClass marker: got %s, want %s", got, want)
	}
	if got != pinnedWindowsKeyHash {
		t.Fatalf("the Windows key moved: got %s, pinned %s", got, pinnedWindowsKeyHash)
	}

	declared := pinnedFormatKey()
	declared.RuntimeIdentity = []string{"hostPlatform=windows/amd64"}
	declared.OSClass = OSClassWindows
	got, err = declared.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	want := pinnedFormatPreimageDigest("runtimeIdentity", "hostPlatform=windows/amd64", "osClass", "windows")
	if got != want {
		t.Fatalf("the osClass marker is not written after the runtime identity: got %s, want %s", got, want)
	}
}

// The OS class names the file model, not the architecture: windows on any
// architecture is one class, and every POSIX system shares the empty class.
func TestOSClassFor(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": OSClassWindows,
		"linux":   "",
		"darwin":  "",
		"freebsd": "",
	} {
		if got := osClassFor(goos); got != want {
			t.Errorf("osClassFor(%q) = %q, want %q", goos, got, want)
		}
	}
}

// BuildCacheKey, the one producer of task keys, stamps the host's OS class, so
// the pinned task hashes to the POSIX golden on Linux and macOS and to the
// Windows golden on Windows.
func TestBuildCacheKey_HashesToTheHostPin(t *testing.T) {
	zeros := "0000000000000000000000000000000000000000000000000000000000000000"
	key := BuildCacheKey(
		"ext", "1.2.3", "ed1:"+zeros, "go1.25.7", "build~transpile", "tc1:"+zeros, "proj", "wsid1:"+zeros, "1.0.0", "1.0.0-abc",
		[]string{"a", "b"},
		nil,
		"", "",
		CacheKeyPolicy{},
		[]string{"up1", "up2"},
	)
	host := osClassFor(runtime.GOOS)
	if key.OSClass != host {
		t.Fatalf("BuildCacheKey OSClass = %q, want the host's %q", key.OSClass, host)
	}
	got, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := pinnedPOSIXKeyHash
	if host == OSClassWindows {
		want = pinnedWindowsKeyHash
	}
	if got != want {
		t.Fatalf("BuildCacheKey of the pinned task = %s, want %s on %s", got, want, runtime.GOOS)
	}
}
