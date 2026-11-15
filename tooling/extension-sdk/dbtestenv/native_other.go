//go:build !unix

package dbtestenv

import (
	"errors"
	"syscall"
)

// syscallNoFollow has no counterpart here: nothing is written off Unix.
const syscallNoFollow = 0

// errNativeUnsupported is what every native operation reports off Unix: the
// provider starts a detached server under another OS user, which this build
// does not implement.
var errNativeUnsupported = errors.New("a native postgres test server is only started on Unix")

// machineNativeHost finds no install, so the native provider is never
// available off Unix.
func machineNativeHost() nativeHost {
	unsupported := func(string) error { return errNativeUnsupported }
	return nativeHost{
		findBinDir:  func() (string, bool) { return "", false },
		freePort:    freeLoopbackPort,
		dial:        dialTCP,
		prepareRoot: unsupported,
		prepareDir:  unsupported,
		serviceIDs:  func() (int, int, error) { return -1, -1, errNativeUnsupported },
		alive:       func(int) bool { return false },
		lock:        func(string) (func(), error) { return nil, errNativeUnsupported },
	}
}

func nativeSysProcAttr(bool, bool) (*syscall.SysProcAttr, string, error) {
	return nil, "", errNativeUnsupported
}
