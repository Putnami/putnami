package workspacejob

import "os"

// The external tests of this package reach its internals through these
// names: they import jobtest, which imports this package, so they cannot live
// inside it.

// InstallLockedGo runs the verified install of the Go release the lock pins.
func (j *Job) InstallLockedGo(requested string) (string, bool) { return j.installLockedGo(requested) }

// SetTrap replaces the job's signal trap.
func (j *Job) SetTrap(trap *Trap) { j.trap = trap }

// Receive delivers sig as the signal goroutine does.
func (t *Trap) Receive(sig os.Signal) { t.receive(sig) }

// Check ends the job when a signal was received, as every command does once it
// returns.
func (t *Trap) Check() { t.check() }

// Deliver puts sig on the armed trap's signal channel, as the runtime does when
// the process receives sig.
func (t *Trap) Deliver(sig os.Signal) {
	t.mu.Lock()
	signals := t.signals
	t.mu.Unlock()
	signals <- sig
}

// ExitCodeFor is the status the trap exits with after sig.
var ExitCodeFor = exitCodeFor

// GoInstallComplete is the completeness predicate of a managed Go install.
var GoInstallComplete = goInstallComplete

// ManagedGoBinary is where a managed install keeps its go command.
var ManagedGoBinary = managedGoBinary

// ManagedGoDir is the directory of a managed install.
var ManagedGoDir = managedGoDir

// WorkspaceGoRoot is the directory inside the workspace that holds Go
// releases.
func (j *Job) WorkspaceGoRoot() string { return j.workspaceGoRoot() }

// AppendCommaValue and RemoveCommaValue edit a comma-separated variable.
var (
	AppendCommaValue = appendCommaValue
	RemoveCommaValue = removeCommaValue
)

// MapLines and Field are the line filters the job ports from sed and awk.
var (
	MapLines = mapLines
	Field    = field
)
