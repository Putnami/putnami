//go:build windows

package workspace

import "testing"

// The same table as dirlink's TestWin32Path: both packages turn a final path
// into the same Win32 spelling.
func TestWin32Path(t *testing.T) {
	for final, want := range map[string]string{
		`\\?\C:\work\a`:           `C:\work\a`,
		`\\?\UNC\server\share\a`:  `\\server\share\a`,
		`\\?\Volume{0000}\work\a`: `\\?\Volume{0000}\work\a`,
		`C:\already\plain`:        `C:\already\plain`,
	} {
		if got := win32Path(final); got != want {
			t.Errorf("win32Path(%q) = %q, want %q", final, got, want)
		}
	}
}
