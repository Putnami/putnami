// Package proctree starts a child process as the root of its own process tree
// and stops that tree as a whole, the same way on every OS the CLI and the
// language extensions ship for.
//
// A tree is the child and every process it spawns. On Unix it is the child's
// process group: New sets Setpgid, and a signal sent to the negative group id
// reaches every member. On Windows it is two things at once: a console process
// group (CREATE_NEW_PROCESS_GROUP), so that a CTRL_BREAK_EVENT reaches every
// member attached to the caller's console, and a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, which holds every member whether it is
// attached to a console or not. The child is created suspended and joins the
// job before its first instruction runs, so no descendant leaves the job unless
// it is started with StartDetached.
//
// The platforms differ on purpose in one place. On Windows a tree ends when it
// is closed or when the process that started it exits, whichever comes first.
// On Unix a member still running after Close keeps running, and the next run
// reaps it.
package proctree

import (
	"os/exec"
	"sync"
)

// Tree is a child process started as the root of its own process tree. Its
// methods are safe for concurrent use, and they are no-ops on a nil Tree.
type Tree struct {
	cmd *exec.Cmd

	// mu orders Start against every stop request: os/exec runs cmd.Cancel as
	// soon as the process exists, which can be before Start has attached it.
	mu     sync.Mutex
	id     int
	closed bool
	sys    platformTree
}

// New prepares cmd to start as the root of a new tree and returns that tree.
// It keeps the other fields of an existing cmd.SysProcAttr. Start the command
// with Start or Run, never with cmd.Start: a command started any other way is
// outside the tree, which then has no id, so every stop does nothing.
func New(cmd *exec.Cmd) *Tree {
	prepare(cmd)
	return &Tree{cmd: cmd}
}

// Start starts the command and attaches it to the tree. It applies New's
// settings again first, so a caller may replace cmd.SysProcAttr after New. It
// returns cmd.Start's error unchanged. When the attachment fails, Start kills
// the child, waits for it and returns the attachment error: a child outside
// its tree could not be stopped as a whole.
func (t *Tree) Start() error {
	t.mu.Lock()
	prepare(t.cmd)
	suspend(t.cmd)
	if err := t.cmd.Start(); err != nil {
		t.mu.Unlock()
		return err
	}
	if err := t.attach(); err != nil {
		// Unlock before waiting: cmd.Wait waits for a cmd.Cancel that may be
		// blocked on mu.
		t.mu.Unlock()
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		return err
	}
	t.id = t.cmd.Process.Pid
	register(t)
	t.mu.Unlock()
	return nil
}

// Run starts the tree, waits for its root to exit and closes the tree. It
// returns what cmd.Run would.
func (t *Tree) Run() error {
	if err := t.Start(); err != nil {
		return err
	}
	err := t.cmd.Wait()
	_ = t.Close()
	return err
}

// ID is the tree's id: the process group id on Unix; on Windows the root's
// pid, which is also the id of its console process group. It is 0 until Start
// succeeded. TerminateGroup, KillGroup and GroupAlive address a tree by this
// id, including from another process.
func (t *Tree) ID() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

// Terminate asks every process of the tree to exit. On Unix it sends SIGTERM
// to the group. On Windows it sends CTRL_BREAK_EVENT to the process group,
// which a Go program receives as os.Interrupt; when no event can be delivered
// (the caller has no console, or shares none with the tree) it ends the tree
// as Kill does, since nothing else can ask the tree to exit. A tree that is
// already gone is not an error. Terminate does nothing before Start or after
// Close.
func (t *Tree) Terminate() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id <= 0 || t.closed {
		return nil
	}
	return t.terminate()
}

// Kill ends every process of the tree now: SIGKILL to the group on Unix,
// TerminateJobObject on Windows. A tree that is already gone is not an error.
// Kill does nothing before Start or after Close.
func (t *Tree) Kill() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id <= 0 || t.closed {
		return nil
	}
	return t.kill()
}

// Close releases the tree once the caller is done stopping it. On Windows it
// ends every process still in the job and returns once they exited, and once
// every process a Kill or Terminate already reached exited too, or after five
// seconds with an error that says how many still run, then closes the job; on
// Unix it leaves the processes alone. After Close, Terminate and Kill
// do nothing: the id may then name another tree. Close is idempotent.
func (t *Tree) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id <= 0 || t.closed {
		return nil
	}
	t.closed = true
	unregister(t)
	return t.release()
}
