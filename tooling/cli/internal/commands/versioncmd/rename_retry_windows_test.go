//go:build windows

package versioncmd

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// The switch retries the refusals a scanner causes, and only those.
func TestTransientRenameError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_ACCESS_DENIED}, true},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_SHARING_VIOLATION}, true},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_FILE_NOT_FOUND}, false},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_PATH_NOT_FOUND}, false},
		{errors.New("other"), false},
	} {
		if got := transientRenameError(tc.err); got != tc.want {
			t.Errorf("transientRenameError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A file another process holds open without sharing delete access refuses the
// rename with ERROR_SHARING_VIOLATION, which the switch retries.
func TestTransientRenameError_ClassifiesAnOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + `\held.exe`
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold the file open: %v", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	err = os.Rename(path, dir+`\moved.exe`)
	if err == nil {
		t.Skip("precondition: renaming a file held open without FILE_SHARE_DELETE went through, so this host cannot show the refusal the switch retries")
	}
	if !transientRenameError(err) {
		t.Fatalf("transientRenameError(%v) = false, want true", err)
	}
}
