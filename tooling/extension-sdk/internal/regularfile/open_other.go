//go:build !unix

package regularfile

import "os"

// openNoFollow opens name for reading. Outside Unix no open flag refuses a
// symbolic link, so Open relies on the identity check after the open.
func openNoFollow(name string) (*os.File, error) {
	return os.Open(name)
}

// setBlocking has nothing to clear: openNoFollow opens in blocking mode.
func setBlocking(*os.File) error {
	return nil
}
