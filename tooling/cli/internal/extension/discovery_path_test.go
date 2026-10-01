package extension

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestNormalizeWorkspaceExtensionPathRefusesAVolume pins the workspace-relative
// form of an extension reference. A reference that names a volume has none on
// Windows; everywhere else a volume name does not exist, so the same strings
// keep their Unix meaning byte for byte.
func TestNormalizeWorkspaceExtensionPathRefusesAVolume(t *testing.T) {
	windows := runtime.GOOS == "windows"
	cases := []struct {
		ref     string
		want    string
		ok      bool
		onlyWin bool // the result differs on Windows, where the ref names a volume
	}{
		{ref: "/go/extension", want: filepath.FromSlash("go/extension"), ok: true},
		{ref: `\go\extension`, want: filepath.FromSlash("go/extension"), ok: true},
		{ref: "tooling/../go/extension", want: filepath.FromSlash("go/extension"), ok: true},
		{ref: "../outside", ok: false},
		{ref: "/", ok: false},
		{ref: `C:\ext\one`, want: filepath.FromSlash("C:/ext/one"), ok: true, onlyWin: true},
		{ref: "C:/ext/one", want: filepath.FromSlash("C:/ext/one"), ok: true, onlyWin: true},
		{ref: "C:ext", want: "C:ext", ok: true, onlyWin: true},
		{ref: `\\server\share\ext`, want: filepath.FromSlash("server/share/ext"), ok: true, onlyWin: true},
		{ref: "//server/share/ext", want: filepath.FromSlash("server/share/ext"), ok: true, onlyWin: true},
	}
	for _, tc := range cases {
		want, wantOK := tc.want, tc.ok
		if windows && tc.onlyWin {
			want, wantOK = "", false
		}
		got, ok := normalizeWorkspaceExtensionPath(tc.ref)
		if got != want || ok != wantOK {
			t.Errorf("normalizeWorkspaceExtensionPath(%q) = %q, %v; want %q, %v", tc.ref, got, ok, want, wantOK)
		}
	}
}
