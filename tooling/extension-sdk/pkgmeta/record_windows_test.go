//go:build windows

package pkgmeta

import (
	"testing"

	"golang.org/x/sys/windows"
)

// denyWrites lets the current user list and traverse dir but create nothing
// in it until the test ends: the read-only attribute os.Chmod sets on Windows
// does not keep files out of a directory, so dir gets a protected access list
// that grants read (FR) and execute (FX) alone.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	if err := setAccess(dir, "FRFX"); err != nil {
		t.Fatalf("deny writes to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := setAccess(dir, "FA"); err != nil {
			t.Errorf("restore writes to %s: %v", dir, err)
		}
	})
}

// setAccess gives dir a protected access list with one entry that grants the
// current user rights, inherited by what dir holds. The owner may always
// rewrite the list, so a narrower one can be widened again.
func setAccess(dir, rights string) error {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;" + rights + ";;;" + token.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
