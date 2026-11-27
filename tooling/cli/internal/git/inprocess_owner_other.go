//go:build !unix

package git

// ownedByCurrentUser refuses every directory where ownership is not read the
// Unix way: Git answers there.
func ownedByCurrentUser(dir string) error {
	return unsupported("ownership of %s is not read on this platform", dir)
}

// fileStamp is never compared here: ownedByCurrentUser refuses every
// repository on such a platform, so no configuration reading is kept.
func fileStamp(string) string { return "unread" }

func deviceOf(path string) (uint64, error) {
	return 0, unsupported("devices of %s are not read on this platform", path)
}
