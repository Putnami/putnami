// Package procstat counts the processes a Putnami process starts and the CPU
// they spend: by kind (job, batch, control, cli, git, other), git calls by
// subcommand, and the CPU of the counting process itself.
//
// The ledger is process-wide. The CLI engine counts what it starts, and a job
// built on the extension SDK counts what it starts and reports it in its result
// (go.putnami.dev/protocol/runtime ProcessReport), so a run can state its
// orchestration cost without an outside poller.
//
// A process is counted when it starts, through Run, Output, CombinedOutput or
// Start, and its CPU when its exit status is read, through those wrappers or
// Wait. A caller that starts a process another way records it with Started and
// Exited.
package procstat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// Kind classifies a started process. The values are protocol/runtime's
// ProcessKind* vocabulary.
type Kind string

// Process kinds.
const (
	// Job is a job process the engine starts for one task.
	Job Kind = runtimeproto.ProcessKindJob
	// Batch is a job process the engine starts for several tasks at once.
	Batch Kind = runtimeproto.ProcessKindBatch
	// Control is an extension process the engine starts for anything but a job:
	// a runtime handshake, a probe, a provider.
	Control Kind = runtimeproto.ProcessKindControl
	// CLI is a Putnami CLI process.
	CLI Kind = runtimeproto.ProcessKindCLI
	// Git is a git process.
	Git Kind = runtimeproto.ProcessKindGit
	// Other is any other program.
	Other Kind = runtimeproto.ProcessKindOther
)

// Kinds is every kind, in canonical order.
var Kinds = []Kind{Job, Batch, Control, CLI, Git, Other}

// counter is one kind's starts and the CPU of those that exited.
type counter struct {
	starts   atomic.Int64
	cpuNanos atomic.Int64
	measured atomic.Int64
}

// ledger is a set of counters by kind and git calls by subcommand.
type ledger struct {
	kinds [6]counter
	gitMu sync.Mutex
	git   map[string]int64
}

func (l *ledger) counter(kind Kind) *counter {
	index := slices.Index(Kinds, kind)
	if index < 0 {
		index = slices.Index(Kinds, Other)
	}
	return &l.kinds[index]
}

// global is the process-wide ledger.
var global ledger

// Started counts one process of kind whose argument vector is argv, the
// program first. A git process also counts its subcommand.
func Started(kind Kind, argv []string) {
	global.counter(kind).starts.Add(1)
	if kind == Git {
		subcommand := GitSubcommand(argv)
		global.gitMu.Lock()
		if global.git == nil {
			global.git = map[string]int64{}
		}
		global.git[subcommand]++
		global.gitMu.Unlock()
	}
}

// Exited adds the CPU an exited process of kind spent, user plus system, as
// its exit status accounts it: the process and the descendants it waited for.
// A nil state adds nothing.
func Exited(kind Kind, state *os.ProcessState) {
	if state == nil {
		return
	}
	c := global.counter(kind)
	c.cpuNanos.Add(int64(state.UserTime() + state.SystemTime()))
	c.measured.Add(1)
}

// Classify names the kind of a program from its path: git, the Putnami CLI
// (putnami or its putnamiw wrapper), or other.
func Classify(program string) Kind {
	// A Windows path names its program after a backslash, whatever the host.
	name := strings.ToLower(filepath.Base(program))
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "git":
		return Git
	case "putnami", "putnamiw":
		return CLI
	}
	return Other
}

// gitValueOptions are the global git options whose value is the next
// argument.
var gitValueOptions = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true, "--attr-source": true, "--super-prefix": true,
}

// GitSubcommand names the subcommand of a git argument vector, the program
// first: the first argument after the global options ("-C <dir>", "-c
// <name=value>", "--git-dir=<dir>"). A vector that names none counts under its
// first argument, and a vector with no argument, or an empty subcommand,
// under "git".
func GitSubcommand(argv []string) string {
	if len(argv) < 2 {
		return "git"
	}
	args := argv[1:]
	name := args[0]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if gitValueOptions[arg] {
			i++
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			name = arg
			break
		}
	}
	if name == "" {
		return "git"
	}
	return name
}

// kindOf is the explicit kind when one is given, otherwise the program's.
func kindOf(cmd *exec.Cmd, kind []Kind) Kind {
	if len(kind) > 0 {
		return kind[0]
	}
	return Classify(cmd.Path)
}

func argvOf(cmd *exec.Cmd) []string {
	if len(cmd.Args) > 0 {
		return cmd.Args
	}
	return []string{cmd.Path}
}

// Start starts cmd and counts it, under kind when one is given, otherwise
// under the kind its program classifies as. A command that fails to start is
// not counted.
func Start(cmd *exec.Cmd, kind ...Kind) error {
	k := kindOf(cmd, kind)
	if err := cmd.Start(); err != nil {
		return err
	}
	Started(k, argvOf(cmd))
	return nil
}

// Wait waits for cmd and adds its CPU, under the kind Start counted it as.
func Wait(cmd *exec.Cmd, kind ...Kind) error {
	err := cmd.Wait()
	Exited(kindOf(cmd, kind), cmd.ProcessState)
	return err
}

// Run is cmd.Run with the process counted.
func Run(cmd *exec.Cmd, kind ...Kind) error {
	if err := Start(cmd, kind...); err != nil {
		return err
	}
	return Wait(cmd, kind...)
}

// Output is cmd.Output with the process counted.
func Output(cmd *exec.Cmd, kind ...Kind) ([]byte, error) {
	out, err := cmd.Output()
	record(cmd, kind)
	return out, err
}

// CombinedOutput is cmd.CombinedOutput with the process counted.
func CombinedOutput(cmd *exec.Cmd, kind ...Kind) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	record(cmd, kind)
	return out, err
}

// Record counts cmd after another runner started and waited for it, such as
// a process tree: its start, and its CPU when its exit status was read. A
// command that never started is not counted.
func Record(cmd *exec.Cmd, kind ...Kind) { record(cmd, kind) }

// record counts a command that exec ran to completion: one that never started
// has no Process.
func record(cmd *exec.Cmd, kind []Kind) {
	if cmd.Process == nil {
		return
	}
	k := kindOf(cmd, kind)
	Started(k, argvOf(cmd))
	Exited(k, cmd.ProcessState)
}

// KindCount is one kind's starts and the CPU of those whose exit status was
// read.
type KindCount struct {
	Kind     Kind
	Starts   int64
	CPU      time.Duration
	Measured int64
}

// Snapshot is the ledger at one moment.
type Snapshot struct {
	// Kinds has one entry per kind, in Kinds order, started or not.
	Kinds []KindCount
	// Git maps a subcommand to its calls.
	Git map[string]int64
}

// Take reads the process-wide ledger.
func Take() Snapshot {
	snapshot := Snapshot{Kinds: make([]KindCount, len(Kinds)), Git: map[string]int64{}}
	for i, kind := range Kinds {
		c := &global.kinds[i]
		snapshot.Kinds[i] = KindCount{
			Kind:     kind,
			Starts:   c.starts.Load(),
			CPU:      time.Duration(c.cpuNanos.Load()),
			Measured: c.measured.Load(),
		}
	}
	global.gitMu.Lock()
	for subcommand, calls := range global.git {
		snapshot.Git[subcommand] = calls
	}
	global.gitMu.Unlock()
	return snapshot
}

// Of returns the entry of kind.
func (s Snapshot) Of(kind Kind) KindCount {
	for _, entry := range s.Kinds {
		if entry.Kind == kind {
			return entry
		}
	}
	return KindCount{Kind: kind}
}

// Report is the job-process report of s: the kinds started at least once, the
// git calls by subcommand, and the process's own CPU when the platform
// measures it.
func (s Snapshot) Report() *runtimeproto.ProcessReport {
	report := &runtimeproto.ProcessReport{}
	if self, ok := SelfCPU(); ok {
		ms := self.Milliseconds()
		report.SelfCPUMs = &ms
	}
	for _, entry := range s.Kinds {
		if entry.Starts == 0 {
			continue
		}
		start := runtimeproto.ProcessStarts{Kind: string(entry.Kind), Count: entry.Starts}
		if entry.Measured > 0 {
			ms := entry.CPU.Milliseconds()
			start.CPUMs = &ms
		}
		report.Starts = append(report.Starts, start)
	}
	subcommands := make([]string, 0, len(s.Git))
	for subcommand := range s.Git {
		subcommands = append(subcommands, subcommand)
	}
	slices.Sort(subcommands)
	for _, subcommand := range subcommands {
		report.Git = append(report.Git, runtimeproto.GitCalls{Subcommand: subcommand, Count: s.Git[subcommand]})
	}
	return report
}
