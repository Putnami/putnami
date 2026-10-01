// Package ownerperm restricts a file or a directory to its owner, which is
// what a private artifact needs on every platform Putnami builds for. On Unix
// the permission bits do it. On Windows they do not: os.Chmod only sets or
// clears the read-only attribute there, and a new file inherits the access
// control list of its directory, which commonly lets every authenticated
// user of the machine read it. There, Restrict also replaces the object's
// DACL (discretionary access control list, the list of who may access it)
// with a protected one that grants the current user full control and nobody
// else.
package ownerperm

import (
	"fmt"
	"io/fs"
	"os"
)

// Restrict applies mode to path, following a symbolic link as os.Chmod does.
// mode must grant nothing to group or others. On Windows, Restrict also gives
// path a protected DACL that grants the current user full control and nobody
// else. A directory passes that entry on to what is created in it later; a
// file moved in from elsewhere keeps its own list, so a caller restricts it
// again after a producer wrote it.
func Restrict(path string, mode fs.FileMode) error {
	if mode&0o077 != 0 {
		return &os.PathError{Op: "restrict", Path: path, Err: fmt.Errorf("mode %04o grants access beyond the owner", mode.Perm())}
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return restrictACL(path)
}

// OwnerOnly reports whether nobody but the owner may access path: on Unix,
// its permission bits grant nothing to group or others; on Windows, its DACL
// grants access to the current user and to nobody else.
func OwnerOnly(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return ownerOnly(path, info)
}
