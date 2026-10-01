//go:build !windows

package ownerperm

import "io/fs"

// restrictACL has nothing to add on Unix: the permission bits Restrict set
// are the whole access check.
func restrictACL(string) error {
	return nil
}

func ownerOnly(_ string, info fs.FileInfo) (bool, error) {
	return info.Mode().Perm()&0o077 == 0, nil
}
