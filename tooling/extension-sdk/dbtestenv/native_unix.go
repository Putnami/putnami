//go:build unix

package dbtestenv

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// syscallNoFollow makes an open refuse a symbolic link in the last component.
const syscallNoFollow = syscall.O_NOFOLLOW

// machineNativeHost wires the native provider to this machine.
func machineNativeHost() nativeHost {
	return nativeHost{
		findBinDir:  findNativeBinDir,
		freePort:    freeLoopbackPort,
		dial:        dialTCP,
		prepareRoot: prepareNativeRoot,
		prepareDir:  prepareNativeDir,
		serviceIDs:  serviceIDs,
		alive:       processAlive,
		lock:        lockNativeState,
	}
}

// serviceUser is the OS user the server programs run as when this process is
// root: its credential, with its own groups in place of root's, and its home.
// It is nil when this process is not root.
func serviceUser() (*syscall.Credential, string, error) {
	if os.Geteuid() != 0 {
		return nil, "", nil
	}
	u, err := user.Lookup(nativeServiceUser)
	if err != nil {
		return nil, "", fmt.Errorf("postgres cannot run as root, and no %q OS user exists to run it as", nativeServiceUser)
	}
	uid, uerr := strconv.ParseUint(u.Uid, 10, 32)
	gid, gerr := strconv.ParseUint(u.Gid, 10, 32)
	if err := errors.Join(uerr, gerr); err != nil {
		return nil, "", fmt.Errorf("read the %q OS user: %w", nativeServiceUser, err)
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if g, err := strconv.ParseUint(id, 10, 32); err == nil {
				cred.Groups = append(cred.Groups, uint32(g))
			}
		}
	}
	return cred, u.HomeDir, nil
}

// serviceIDs is the service user's uid and gid under root, -1 and -1
// otherwise.
func serviceIDs() (int, int, error) {
	cred, _, err := serviceUser()
	if err != nil || cred == nil {
		return -1, -1, err
	}
	return int(cred.Uid), int(cred.Gid), nil
}

// nativeSysProcAttr runs a command as the service user, with that user's
// groups and home, when asService and this process is root, and in a session
// of its own when detach. It returns the HOME the command gets, "" to keep the
// inherited one.
func nativeSysProcAttr(asService, detach bool) (*syscall.SysProcAttr, string, error) {
	attr := &syscall.SysProcAttr{Setsid: detach}
	if !asService {
		return attr, "", nil
	}
	cred, home, err := serviceUser()
	if err != nil {
		return nil, "", err
	}
	attr.Credential = cred
	return attr, home, nil
}

// prepareNativeRoot creates the state root and refuses one it cannot trust: a
// symbolic link, something other than a directory, a directory another user
// owns, or one a group or others can write — any of which would let another
// local user plant files where this process writes as root. Under root it is
// 0711, so the service user reaches its own data directories without listing
// them.
func prepareNativeRoot(dir string) error {
	mode := os.FileMode(0o700)
	if os.Geteuid() == 0 {
		mode = 0o711
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s belongs to another user; remove it", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s can be written by other users; remove it", dir)
	}
	return os.Chmod(dir, mode)
}

// prepareNativeDir creates a 0700 data directory the service user owns.
func prepareNativeDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	uid, gid, err := serviceIDs()
	if err != nil || uid < 0 {
		return err
	}
	return os.Lchown(dir, uid, gid)
}

// processAlive reports whether a process runs under pid.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockNativeState takes an exclusive advisory lock on path, so two test tasks
// of one run never initialize or stop the same server at once.
func lockNativeState(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
