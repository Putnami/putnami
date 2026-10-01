package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// writeProviderTool places an empty file with the executable bit at dir/name.
// The Windows lookup reads no bit and no content; the Unix one needs the bit.
func writeProviderTool(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func pathList(dirs ...string) string {
	return strings.Join(dirs, string(os.PathListSeparator))
}

// A bare provider command resolves against the provider's own PATH, not this
// process's; a command with a separator is used as written.
func TestProviderExecutableResolvesAgainstTheProviderPath(t *testing.T) {
	dir := t.TempDir()
	want := writeProviderTool(t, dir, pkgmeta.ExecutableName(runtime.GOOS, "provider-tool"))

	got, err := ProviderExecutable("provider-tool", []string{"PATH=" + pathList(t.TempDir(), dir)})
	if err != nil || got != want {
		t.Fatalf("ProviderExecutable = %q, %v; want %q from the provider PATH", got, err, want)
	}
	if _, err := ProviderExecutable("provider-tool", []string{"PATH=" + t.TempDir()}); err == nil || !strings.Contains(err.Error(), `"provider-tool"`) {
		t.Fatalf("a name missing from the provider PATH = %v, want an error naming it", err)
	}
	written := filepath.Join("bin", "provider-tool")
	if got, err := ProviderExecutable(written, nil); err != nil || got != written {
		t.Fatalf("ProviderExecutable(%q) = %q, %v; want it unchanged", written, got, err)
	}
}

// On Windows a bare provider name resolves the way os/exec resolves it there:
// PATH directories in order, PATHEXT extensions in order within each one, and
// variable names in any case. The lookup is injected with goos, so every host
// runs it.
func TestProviderExecutableFollowsPathextOnWindows(t *testing.T) {
	cases := []struct {
		name    string
		command string   // "tool" when empty
		files   []string // relative to the case's PATH directories d0, d1
		env     func(d0, d1 string) []string
		want    string // relative path, empty when nothing resolves
	}{
		{
			name:  "a cmd shim with the default PATHEXT",
			files: []string{"d0/tool.cmd"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + d0} },
			want:  "d0/tool.cmd",
		},
		{
			name:  "extension order within one directory",
			files: []string{"d0/tool.cmd", "d0/tool.exe", "d0/tool.bat"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + d0} },
			want:  "d0/tool.exe",
		},
		{
			name:  "directory order before extension order",
			files: []string{"d0/tool.cmd", "d1/tool.exe"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + pathList(d0, d1)} },
			want:  "d0/tool.cmd",
		},
		{
			name:  "a declared PATHEXT in upper case, one entry without its dot",
			files: []string{"d0/tool.exe", "d0/tool.bat"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + d0, "PATHEXT=BAT;.EXE"} },
			want:  "d0/tool.bat",
		},
		{
			name:  "variable names in the system spelling",
			files: []string{"d1/tool.bat"},
			env:   func(d0, d1 string) []string { return []string{"Path=" + pathList(d0, d1), "PathExt=.bat"} },
			want:  "d1/tool.bat",
		},
		{
			name:    "a name that has its extension",
			command: "tool.cmd",
			files:   []string{"d0/tool.cmd", "d0/tool.exe"},
			env:     func(d0, d1 string) []string { return []string{"PATH=" + d0} },
			want:    "d0/tool.cmd",
		},
		{
			name:  "a directory is not an executable",
			files: []string{"d0/tool.exe/inner", "d1/tool.cmd"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + pathList(d0, d1)} },
			want:  "d1/tool.cmd",
		},
		{
			name:  "no match",
			files: []string{"d0/tool.ps1"},
			env:   func(d0, d1 string) []string { return []string{"PATH=" + d0} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range tc.files {
				writeProviderTool(t, root, filepath.FromSlash(file))
			}
			command := tc.command
			if command == "" {
				command = "tool"
			}
			got, err := providerExecutable(command, tc.env(filepath.Join(root, "d0"), filepath.Join(root, "d1")), "windows")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("resolved %q, want no match", got)
				}
				return
			}
			if want := filepath.Join(root, filepath.FromSlash(tc.want)); err != nil || got != want {
				t.Fatalf("resolved %q, %v; want %q", got, err, want)
			}
		})
	}
}

// Off Windows, PATHEXT means nothing: a bare name resolves only to a file of
// exactly that name.
func TestProviderExecutableIgnoresPathextOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix lookup is not reachable on a Windows host")
	}
	dir := t.TempDir()
	writeProviderTool(t, dir, "tool.cmd")
	if got, err := providerExecutable("tool", []string{"PATH=" + dir, "PATHEXT=.CMD"}, runtime.GOOS); err == nil {
		t.Fatalf("resolved %q through PATHEXT on %s", got, runtime.GOOS)
	}
}
