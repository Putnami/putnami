//go:build unix

package proctree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// platformTree holds nothing on Unix: the kernel addresses a group by its id.
type platformTree struct{}

func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// suspend has nothing to do: Setpgid takes effect before the child runs.
func suspend(*exec.Cmd) {}

// attach has nothing to do: Setpgid made the child the leader of a group whose
// id is its pid before it ran.
func (t *Tree) attach() error { return nil }

// StartDetached starts cmd so that it keeps running after the caller exits and
// after a tree the caller runs in is closed, and returns the command that
// started. On Unix closing a tree ends nothing, so it starts cmd as cmd.Start
// does: in the caller's process group, where Terminate and Kill of a tree the
// caller runs in still reach it.
func StartDetached(cmd *exec.Cmd) (*exec.Cmd, error) { return cmd, cmd.Start() }

// Relay forwards a stop request the caller received to process, a child that
// runs in the caller's process group: it sends the child SIGTERM.
func Relay(process *os.Process) error { return process.Signal(syscall.SIGTERM) }

func (t *Tree) terminate() error { return signalGroup(t.id, syscall.SIGTERM) }

func (t *Tree) kill() error { return signalGroup(t.id, syscall.SIGKILL) }

// release forgets the platform state, as on Windows; it ends no process.
func (t *Tree) release() error {
	t.sys = platformTree{}
	return nil
}

func register(*Tree) {}

func unregister(*Tree) {}

// TerminateGroup sends SIGTERM to every process of the group id. It does nothing
// for an id <= 0, which kill(2) would read as the caller's own group or every
// process it may signal, and a group that is already gone is not an error.
func TerminateGroup(id int) error { return signalGroup(id, syscall.SIGTERM) }

// KillGroup sends SIGKILL to every process of the group id, under the same
// rules as TerminateGroup.
func KillGroup(id int) error { return signalGroup(id, syscall.SIGKILL) }

// GroupAlive reports whether a process of the group id still runs. A group
// owned by another user counts as running.
func GroupAlive(id int) bool {
	if id <= 0 {
		return false
	}
	err := syscall.Kill(-id, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ProcessAlive reports whether pid names a running process. A process owned by
// another user counts as running: a caller never takes a process it cannot
// inspect for dead.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalGroup(id int, sig syscall.Signal) error {
	if id <= 0 {
		return nil
	}
	err := syscall.Kill(-id, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
