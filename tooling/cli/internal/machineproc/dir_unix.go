//go:build linux || darwin

package machineproc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// Dir is the supervisor's directory: the first candidate that fits the socket
// path and is private to the user. With create, a missing candidate is
// created with mode 0700; without, only an existing one is returned.
func Dir(getenv func(string) string, create bool) (string, error) {
	uid := os.Getuid()
	for _, dir := range candidateDirs(getenv, runtime.GOOS, uid) {
		if len(filepath.Join(dir, socketName)) > maxSocketPath {
			continue
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				continue
			}
			if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				continue
			}
			info, err = os.Lstat(dir)
		}
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !isPrivateDir(info, st.Uid, uid) {
			continue
		}
		return dir, nil
	}
	return "", errNoDir
}
