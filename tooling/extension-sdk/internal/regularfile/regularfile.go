// Package regularfile opens a file for reading only when it is a regular
// file, without following a symbolic link and without waiting on a FIFO.
//
// A process of the same user can replace a file between a check of its path
// and its open. Open therefore decides on the descriptor it opened, not on the
// path it checked: on Unix the open neither follows a symbolic link nor waits
// for a FIFO writer, so a replacement can make Open fail but never block.
package regularfile

import (
	"fmt"
	"os"
)

// Open opens name for reading. It refuses a name that is not a regular file
// when it is checked, a symbolic link, and a name replaced between the check
// and the open by anything but the file it checked. The returned file reads
// in blocking mode.
func Open(name string) (*os.File, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	file, err := openNoFollow(name)
	if err != nil {
		return nil, fmt.Errorf("%s changed while it was opened: %w", name, err)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("%s changed while it was opened", name)
	}
	if err := setBlocking(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	return file, nil
}
