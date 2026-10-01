//go:build windows

package ownerperm

import (
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// restrictACL replaces the DACL of path with a protected one holding a single
// entry: full control (FA) for the current user. Protected means the entries
// of the parent directory no longer flow in. On a directory the entry is
// inherited by files (OI) and directories (CI) created in it later.
func restrictACL(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return &os.PathError{Op: "restrict", Path: path, Err: err}
	}
	inherit := ""
	if info.IsDir() {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + user.String() + ")")
	if err != nil {
		return &os.PathError{Op: "restrict", Path: path, Err: err}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return &os.PathError{Op: "restrict", Path: path, Err: err}
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return &os.PathError{Op: "restrict", Path: path, Err: err}
	}
	return nil
}

// ownerOnly reports whether every entry of the DACL of path allows the
// current user. A null DACL grants everyone everything.
func ownerOnly(path string, _ fs.FileInfo) (bool, error) {
	user, err := currentUser()
	if err != nil {
		return false, err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, &os.PathError{Op: "read access list", Path: path, Err: err}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, &os.PathError{Op: "read access list", Path: path, Err: err}
	}
	if dacl == nil {
		return false, nil
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, &os.PathError{Op: "read access list", Path: path, Err: err}
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue // a deny entry grants nothing
		}
		if !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user) {
			return false, nil
		}
	}
	return true, nil
}

// currentUser is the user the process token runs as.
func currentUser() (*windows.SID, error) {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return token.User.Sid, nil
}
