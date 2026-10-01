package runcredential

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A holder is a native executable only in the format of the
// system it runs on: ELF on Linux and the other Unix systems, Mach-O, thin or
// universal, on macOS, and PE on Windows. A PE or Mach-O file on Linux, or an
// ELF file on macOS, runs only through a handler that interprets it.
func TestNativeImageAcceptsOnlyTheSystemFormat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	images := map[string][]byte{
		"elf":           []byte("\x7fELF\x02\x01\x01"),
		"32-bit mach-o": {0xfe, 0xed, 0xfa, 0xce, 7},
		"64-bit mach-o": {0xcf, 0xfa, 0xed, 0xfe, 7},
		"universal":     {0xca, 0xfe, 0xba, 0xbe, 0},
		"universal 64":  {0xca, 0xfe, 0xba, 0xbf, 0},
		"pe":            []byte("MZ\x90\x00"),
		// A Java class file starts with the universal magic. macOS reads it
		// as a universal image and hands it to no interpreter.
		"java class": {0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 0x41},
	}
	accepted := map[string][]string{
		"linux":   {"elf"},
		"freebsd": {"elf"},
		"darwin":  {"32-bit mach-o", "64-bit mach-o", "universal", "universal 64", "java class"},
		"windows": {"pe"},
	}
	for name, body := range images {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, body, 0o755); err != nil { //nolint:gosec // executable test fixtures
			t.Fatal(err)
		}
		for goos, formats := range accepted {
			reason := nativeImage(path, goos)
			if want := slices.Contains(formats, name); (reason == "") != want {
				t.Errorf("nativeImage(%s, %s) = %q, want accepted %t", name, goos, reason, want)
			}
		}
	}
}

// On a hosted run a cache provider or credential-provider starts only as its
// extension's native runtime executable: a script, a file that is not an
// executable image, or a command that is not the runtime fails with a named
// error. Without a run credential every holder starts as before.
func TestRequireNativeHolder(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o755); err != nil { //nolint:gosec // executable test fixtures
			t.Fatal(err)
		}
		return path
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := write("launcher", []byte("#!/bin/sh\nexec \"$0.bin\" \"$@\"\n"))
	type holderCase struct {
		name                string
		executable, runtime string
		// want is a fragment of the reason, or "" when the holder starts.
		want string
	}
	var linked []holderCase
	// A link can name an interpreter, so even a link to a native image is
	// refused. Windows may not allow the test to create one.
	if link := filepath.Join(dir, "link"); os.Symlink(self, link) == nil {
		linked = append(linked, holderCase{name: "a link to a native executable", executable: link, runtime: link, want: "is not a regular file"})
	}
	// A file of this system's executable format, and one of another format.
	native := write("native", append(slices.Clone(nativeMagic(runtime.GOOS)[0]), 0, 0))
	foreignGOOS := "linux"
	if runtime.GOOS == "linux" {
		foreignGOOS = "darwin"
	}
	foreign := write("foreign", append(slices.Clone(nativeMagic(foreignGOOS)[0]), 0, 0))
	for _, tc := range append(linked, []holderCase{
		{name: "this test's own executable", executable: self, runtime: self},
		{name: "an image of this system's format", executable: native, runtime: native},
		{name: "an image of another system's format", executable: foreign, runtime: foreign, want: "is not a native executable of " + runtime.GOOS},
		{name: "the runtime named with a redundant separator", executable: dir + "//native", runtime: native},
		{name: "a shell launcher as the runtime", executable: script, runtime: script, want: "is a script (#!)"},
		{name: "a text file", executable: write("text", []byte("console.log(1)\n")), runtime: filepath.Join(dir, "text"), want: "is not a native executable"},
		{name: "an empty file", executable: write("empty", nil), runtime: filepath.Join(dir, "empty"), want: "is not a native executable"},
		{name: "a missing runtime", executable: filepath.Join(dir, "missing"), runtime: filepath.Join(dir, "missing"), want: "cannot be read"},
		{name: "a launcher beside a native runtime", executable: script, runtime: self, want: "is not its extension's runtime executable"},
		{name: "an interpreter that runs the entry", executable: self, runtime: native, want: "is not its extension's runtime executable"},
		{name: "an extension without a runtime", executable: self, want: "is not its extension's runtime executable"},
	}...) {
		t.Run(tc.name, func(t *testing.T) {
			if err := RequireNativeHolder("the cache provider of @a/cache", tc.executable, tc.runtime); err != nil {
				t.Fatalf("without a run credential: %v, want nil", err)
			}

			restore := SetForTest(testBearer)
			defer restore()
			err := RequireNativeHolder("the cache provider of @a/cache", tc.executable, tc.runtime)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("RequireNativeHolder = %v, want nil", err)
				}
				return
			}
			var refusal *NativeHolderError
			if !errors.As(err, &refusal) {
				t.Fatalf("RequireNativeHolder = %v, want a *NativeHolderError", err)
			}
			if refusal.Holder != "the cache provider of @a/cache" || !strings.Contains(refusal.Reason, tc.want) {
				t.Errorf("refusal = %+v, want the holder and a reason with %q", refusal, tc.want)
			}
			for _, part := range []string{Flag, "the cache provider of @a/cache", "{extensionRuntime}", tc.want} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err, part)
				}
			}
		})
	}
}
