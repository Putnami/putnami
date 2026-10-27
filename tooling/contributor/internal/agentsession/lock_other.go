//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package agentsession

import "errors"

// lockSessionFile needs a kernel file lock that the system releases with its
// holder. Without one, the hooks keep no session: every hook fails open.
func lockSessionFile(string) (*sessionLock, error) {
	return nil, errors.New("the session hooks need a Unix file lock")
}
