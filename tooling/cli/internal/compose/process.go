package compose

import "go.putnami.dev/sdk/extension/proctree"

// A recorded process group is the process tree a member's job runs in
// (proctree): its id is the process group id on Unix and the pid of the tree's
// root on Windows, and processStartTime of that id confirms which tree it
// names. On Windows a tree also ends with the process that started it, so a
// dead owner leaves no group behind to reap.

// processAlive reports whether pid names a running process. A process owned by
// another user counts as running, so it is never reaped.
func processAlive(pid int) bool { return proctree.ProcessAlive(pid) }

// processGroupAlive reports whether any process of the tree id is still
// running.
func processGroupAlive(id int) bool { return proctree.GroupAlive(id) }

// terminateProcessGroup asks every process of the tree id to exit. A tree that
// is already gone is not an error.
func terminateProcessGroup(id int) error { return proctree.TerminateGroup(id) }

// killProcessGroup ends every process of the tree id. A tree that is already
// gone is not an error.
func killProcessGroup(id int) error { return proctree.KillGroup(id) }
