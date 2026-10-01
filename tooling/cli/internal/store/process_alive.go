package store

import "go.putnami.dev/sdk/extension/proctree"

// ProcessAlive reports whether pid names a running process. A process owned by
// another user counts as running: a caller never takes a process it cannot
// inspect for dead.
func ProcessAlive(pid int) bool { return proctree.ProcessAlive(pid) }
