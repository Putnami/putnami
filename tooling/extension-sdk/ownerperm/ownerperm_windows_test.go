//go:build windows

package ownerperm

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// soleEntry returns the one entry of the DACL of path and whether that DACL is
// protected from its parent's entries.
func soleEntry(t *testing.T, path string) (*windows.ACCESS_ALLOWED_ACE, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("DACL of %s: %v (nil %v)", path, err, dacl == nil)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("%s: %d entries in %s, want 1", path, dacl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user) {
		t.Fatalf("%s: the entry of %s does not allow the current user %s", path, sd, user)
	}
	return ace, control&windows.SE_DACL_PROTECTED != 0
}

// Restrict gives a directory a protected, inheritable entry for the current
// user, and a file a protected entry of its own. A file created in the
// directory afterwards inherits the entry; a file moved in keeps the list it
// had until Restrict replaces it.
func TestRestrictWritesAProtectedDACLForTheCurrentUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Restrict(dir, 0o700); err != nil {
		t.Fatalf("Restrict(dir): %v", err)
	}
	ace, protected := soleEntry(t, dir)
	if !protected {
		t.Error("the directory's DACL still takes its parent's entries")
	}
	const inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	if ace.Header.AceFlags&inherit != inherit {
		t.Errorf("the directory's entry has flags %#x, want object and container inherit", ace.Header.AceFlags)
	}

	created := filepath.Join(dir, "created")
	writeFile(t, created)
	if !ownerOnlyOrFail(t, created) {
		t.Error("a file created in a restricted directory did not inherit its entry")
	}

	moved := filepath.Join(dir, "moved")
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside)
	if err := os.Rename(outside, moved); err != nil {
		t.Fatal(err)
	}
	if ownerOnlyOrFail(t, moved) {
		t.Skip("the temporary directory already grants only the current user, so a moved file proves nothing here")
	}
	if err := Restrict(moved, 0o600); err != nil {
		t.Fatalf("Restrict(moved): %v", err)
	}
	ace, protected = soleEntry(t, moved)
	if !protected {
		t.Error("the moved file's DACL is not protected")
	}
	if ace.Header.AceFlags != 0 {
		t.Errorf("the file's entry has flags %#x, want none", ace.Header.AceFlags)
	}
}
