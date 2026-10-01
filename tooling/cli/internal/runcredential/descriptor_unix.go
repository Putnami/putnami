//go:build !windows

package runcredential

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// readDescriptor reads fd to its end, at most MaxBytes+1 bytes, and closes
// it. fd must be a pipe, a socket or a regular file: any other descriptor,
// such as one the Go runtime opened for itself, is left alone. A descriptor
// set non-blocking is waited on until it is readable. The writer must close
// its end: reading stops only at the end of the data or past MaxBytes.
func readDescriptor(fd int) ([]byte, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		if errors.Is(err, unix.EBADF) {
			return nil, fmt.Errorf("descriptor %d is not open", fd)
		}
		return nil, fmt.Errorf("inspect descriptor %d: %w", fd, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFREG:
	default:
		return nil, fmt.Errorf("descriptor %d is not a pipe, a socket or a regular file", fd)
	}
	data, err := readAtMost(fd, MaxBytes+1)
	if err != nil {
		return nil, fmt.Errorf("read descriptor %d: %w", fd, err)
	}
	if err := unix.Close(fd); err != nil {
		clear(data)
		return nil, fmt.Errorf("close descriptor %d: %w", fd, err)
	}
	return data, nil
}

func readAtMost(fd, limit int) ([]byte, error) {
	buf := make([]byte, limit)
	n := 0
	for n < limit {
		read, err := unix.Read(fd, buf[n:])
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EAGAIN):
			if err := waitReadable(fd); err != nil {
				clear(buf)
				return nil, err
			}
			continue
		case err != nil:
			clear(buf)
			return nil, err
		case read == 0:
			return buf[:n], nil
		}
		n += read
	}
	return buf[:n], nil
}

func waitReadable(fd int) error {
	for {
		_, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, -1)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// Exec replaces the process image with path, as syscall.Exec does. When the
// process holds the run credential, the new image receives it on a fresh pipe
// and argv names that pipe with Flag: the value of an existing occurrence is
// replaced, otherwise the flag is appended. The pipe gets a new number rather
// than the one Capture closed, because that number may name another file of
// this process by now.
//
// The pipe is created close-on-exec, and its read end loses that flag while
// syscall.ForkLock is held for reading, which a fork must hold for writing: no
// child another goroutine starts inherits it. The bearer is written into the
// pipe before the exec, so it must fit: Linux and macOS take at least 64 KiB
// into an empty pipe, four times MaxBytes, and a pipe that takes less fails
// the exec with an error rather than blocking it. When the exec fails, the
// pipe is closed and the error returned.
//
// The new image holds the credential, so once this process started
// repository code Exec fails with a *CustodyError and replaces nothing
// (StartHolder).
func Exec(path string, argv, env []string) error {
	c := held.Load()
	if c == nil {
		//nolint:gosec // G702: path is the putnami binary launch resolved, the workspace-pinned CLI or the one upgrade just installed; replacing this process with it is the point. Args go straight to exec — no shell.
		return syscall.Exec(path, argv, env)
	}
	return StartHolder("the CLI "+path, func() error {
		fd, err := credentialPipe(c.bearer)
		if err != nil {
			return err
		}
		next := withDescriptor(argv, fd)
		syscall.ForkLock.RLock()
		_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0)
		if err == nil {
			//nolint:gosec // G702: the same binary as above; next is argv with only the --credential-fd value set. Args go straight to exec — no shell.
			err = syscall.Exec(path, next, env)
		}
		_ = unix.Close(fd)
		syscall.ForkLock.RUnlock()
		return err
	})
}

// credentialPipe returns the read end of a close-on-exec pipe that holds
// bearer and whose write end is closed.
func credentialPipe(bearer string) (int, error) {
	var p [2]int
	syscall.ForkLock.RLock()
	err := syscall.Pipe(p[:])
	if err == nil {
		syscall.CloseOnExec(p[0])
		syscall.CloseOnExec(p[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, fmt.Errorf("hand the run credential on: %w", err)
	}
	err = writeAll(p[1], bearer)
	if closeErr := unix.Close(p[1]); err == nil {
		err = closeErr
	}
	if err == nil && p[0] < firstDescriptor {
		err = fmt.Errorf("the pipe opened as standard stream %d", p[0])
	}
	if err != nil {
		_ = unix.Close(p[0])
		return -1, fmt.Errorf("hand the run credential on: %w", err)
	}
	return p[0], nil
}

// writeAll writes bearer into the empty pipe fd without blocking.
func writeAll(fd int, bearer string) error {
	if err := unix.SetNonblock(fd, true); err != nil {
		return err
	}
	data := []byte(bearer)
	defer clear(data)
	for written := 0; written < len(data); {
		n, err := unix.Write(fd, data[written:])
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EAGAIN):
			return fmt.Errorf("the credential (%d bytes) does not fit in an empty pipe", len(data))
		case err != nil:
			return err
		}
		written += n
	}
	return nil
}
