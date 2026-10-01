//go:build !unix

package store

// sameFilesystem reports false on hosts without a device number to compare, so
// every restore there stages beside its destination.
func sameFilesystem(_, _ string) bool {
	return false
}

// crossDevice reports false: a restore that always stages beside its
// destination never renames between filesystems.
func crossDevice(error) bool {
	return false
}
