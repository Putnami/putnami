//go:build !linux

package credentialcustody

// Outside Linux a repository process of the same user cannot read another
// process's environment or memory the way /proc allows, and cannot ptrace it
// without privileges the OS withholds (macOS restricts task_for_pid; the spec
// names denying inspection on macOS a nonGoal). So there are no extra probes:
// the environment and file probes carry the check, and the Linux /proc and
// ptrace probes run on Linux CI only.

// platformSearchProbes names no probe outside Linux.
func platformSearchProbes() []string { return nil }

// platformProbes contributes no finding outside Linux.
func platformProbes(string) []finding { return nil }
