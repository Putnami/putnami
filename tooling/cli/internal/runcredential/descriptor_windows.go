//go:build windows

package runcredential

import "errors"

// readDescriptor fails on Windows, which names a handle rather than a
// descriptor. capture refuses Flag there before it reads anything.
func readDescriptor(int) ([]byte, error) {
	return nil, errors.New("descriptors are not supported on Windows")
}
