//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killedExitCode is the exit code of a process the tree ends, the one
// os.Process.Kill uses.
const killedExitCode = 1

// platformTree is the tree's Job Object, a handle to its root, and a handle to
// each member the tree watches, by pid. The root handle keeps the root's pid
// from being reused while the tree is open, so the tree's id names its console
// process group and nothing else. A member handle is what proves that the
// member exited (see end).
type platformTree struct {
	job     windows.Handle
	root    windows.Handle
	members map[uint32]windows.Handle
}

// trees are the trees this process started and has not closed, by id, so that
// TerminateGroup, KillGroup and GroupAlive reach a tree's job through its id.
var trees = struct {
	sync.Mutex
	byID map[int]*Tree
}{byID: map[int]*Tree{}}

func register(t *Tree) {
	trees.Lock()
	defer trees.Unlock()
	trees.byID[t.id] = t
}

func unregister(t *Tree) {
	trees.Lock()
	defer trees.Unlock()
	if trees.byID[t.id] == t {
		delete(trees.byID, t.id)
	}
}

func lookup(id int) *Tree {
	trees.Lock()
	defer trees.Unlock()
	return trees.byID[id]
}

// prepare creates the child as the root of a new console process group. Ctrl+C
// is disabled in the new group, as a Unix background group does not receive
// the terminal's SIGINT: the caller relays the stop.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// suspend creates the child suspended, so that it joins its job before it can
// start a process of its own. Only Start sets it: a command started with
// cmd.Start by mistake then runs outside the tree, instead of never running.
func suspend(cmd *exec.Cmd) {
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// attach assigns the suspended child to a new job, then resumes it. On any
// error the job, if it holds the child, ends it when it is closed.
func (t *Tree) attach() error {
	pid := uint32(t.cmd.Process.Pid)
	root, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	job, err := newJob()
	if err != nil {
		_ = windows.CloseHandle(root)
		return err
	}
	if assignErr := windows.AssignProcessToJobObject(job, root); assignErr != nil {
		_ = windows.CloseHandle(job)
		_ = windows.CloseHandle(root)
		return fmt.Errorf("assign process %d to its job: %w", pid, assignErr)
	}
	if resumeErr := resume(root); resumeErr != nil {
		_ = windows.CloseHandle(job)
		_ = windows.CloseHandle(root)
		return fmt.Errorf("resume process %d: %w", pid, resumeErr)
	}
	t.sys = platformTree{job: job, root: root}
	return nil
}

// jobLimits end the job's processes when its last handle closes, and let a
// member start a process outside the job only when it asks to, with
// CREATE_BREAKAWAY_FROM_JOB, as StartDetached does. Every other process a
// member starts joins the job.
const jobLimits = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK

// newJob creates an anonymous job with jobLimits. The handle is not
// inheritable, so the job ends with this process.
func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: jobLimits,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("set job object limits: %w", err)
	}
	return job, nil
}

// ntResumeProcess is NtResumeProcess, which x/sys/windows does not declare.
var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// resume resumes every thread of process, a handle opened with
// PROCESS_SUSPEND_RESUME. A process created suspended has exactly one thread,
// and os/exec does not keep its handle; NtResumeProcess reaches it through the
// process handle, in a time that does not depend on how many threads the
// system runs.
func resume(process windows.Handle) error {
	if err := ntResumeProcess.Find(); err != nil {
		return err
	}
	if status, _, _ := syscall.SyscallN(ntResumeProcess.Addr(), uintptr(process)); status != 0 {
		return windows.NTStatus(status)
	}
	return nil
}

// terminate sends CTRL_BREAK_EVENT to the tree's process group. The event only
// reaches processes attached to the caller's console; when none can receive
// it, the tree is ended instead. The members that receive it are watched first,
// so Close can wait for them after they left the job.
func (t *Tree) terminate() error {
	t.watchMembers()
	if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(t.id)) == nil {
		return nil
	}
	return t.kill()
}

// kill watches the members, then ends the job. Once the job ended, its process
// list and its ActiveProcesses count no longer name a member that is still
// exiting, so Close can wait for it only through a handle taken here.
func (t *Tree) kill() error {
	t.watchMembers()
	if err := windows.TerminateJobObject(t.sys.job, killedExitCode); err != nil {
		return fmt.Errorf("terminate the job of process tree %d: %w", t.id, err)
	}
	return nil
}

// release ends every process still in the job, waits for them to exit
// (end), and closes the job.
func (t *Tree) release() error {
	endErr := t.end()
	err := windows.CloseHandle(t.sys.job)
	_ = windows.CloseHandle(t.sys.root)
	for _, member := range t.sys.members {
		_ = windows.CloseHandle(member)
	}
	t.sys = platformTree{}
	if err != nil {
		err = fmt.Errorf("close the job of process tree %d: %w", t.id, err)
	}
	return errors.Join(endErr, err)
}

// endBudget bounds how long Close waits for the processes it ends to exit.
const endBudget = 5 * time.Second

// maxWatchedMembers bounds how many member processes the tree watches by
// handle; the job's ActiveProcesses count covers the others.
const maxWatchedMembers = 64

// end terminates every process still in the job and waits, up to endBudget,
// until each has exited. TerminateJobObject, like closing a job that has
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, returns before the processes exit, so a
// caller that reuses a port or removes a directory right after Close could
// still meet one of them. The kernel lowers the job's ActiveProcesses count,
// and drops an exiting process from the job's process list, before the
// process handle is signaled, so neither proves an exit, and after a Kill or a
// stop request both can already read as empty. The wait therefore watches the
// process handle of the root and of every member the tree watched, at a stop
// request or here, whether a stop request came first or not; then the
// ActiveProcesses count, which also covers a member that started after the
// handles were taken or that the tree could not watch.
func (t *Tree) end() error {
	if t.activeProcesses() > 0 {
		t.watchMembers()
		if err := windows.TerminateJobObject(t.sys.job, killedExitCode); err != nil {
			return fmt.Errorf("terminate the job of process tree %d: %w", t.id, err)
		}
	}
	watched := t.watched()
	deadline := time.Now().Add(endBudget)
	for _, process := range watched {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		_, _ = windows.WaitForSingleObject(process, uint32(remaining.Milliseconds())+1)
	}
	for sleep := time.Millisecond; t.activeProcesses() > 0 && time.Until(deadline) > 0; sleep = min(2*sleep, 50*time.Millisecond) {
		time.Sleep(sleep)
	}
	running := t.activeProcesses()
	unexited := uint32(0)
	for _, process := range watched {
		if !exited(process) {
			unexited++
		}
	}
	if running = max(running, unexited); running > 0 {
		return fmt.Errorf("process tree %d: %d processes still run %s after the tree was ended", t.id, running, endBudget)
	}
	return nil
}

// watched returns the handles that prove the exit of the tree's processes: the
// root's, then each watched member's.
func (t *Tree) watched() []windows.Handle {
	handles := make([]windows.Handle, 0, 1+len(t.sys.members))
	handles = append(handles, t.sys.root)
	for _, member := range t.sys.members {
		handles = append(handles, member)
	}
	return handles
}

// activeProcesses is the number of processes in the job, or 0 when the job
// cannot be queried.
func (t *Tree) activeProcesses() uint32 {
	var info jobAccounting
	if err := windows.QueryInformationJobObject(t.sys.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	return info.ActiveProcesses
}

// jobProcessIDList is JOBOBJECT_BASIC_PROCESS_ID_LIST with room for
// maxWatchedMembers ids, which x/sys/windows does not declare.
type jobProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIDList             [maxWatchedMembers]uintptr
}

// procIsProcessInJob is IsProcessInJob, which x/sys/windows does not declare.
var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// watchMembers opens a SYNCHRONIZE handle on each process of the job that the
// tree does not watch yet, up to maxWatchedMembers in all, after it closed the
// handles of the members that already exited. A handle keeps its process's
// pid from being reused, so a pid the tree watches names the same process for
// as long as the tree watches it. A pid that no longer names a member, because
// the member exited and the pid was reused before the tree opened it, is left
// out. The caller holds t.mu.
func (t *Tree) watchMembers() {
	for pid, member := range t.sys.members {
		if exited(member) {
			_ = windows.CloseHandle(member)
			delete(t.sys.members, pid)
		}
	}
	var list jobProcessIDList
	// ERROR_MORE_DATA still fills the list with the first ids.
	if err := windows.QueryInformationJobObject(t.sys.job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return
	}
	if t.sys.members == nil {
		t.sys.members = make(map[uint32]windows.Handle)
	}
	count := min(int(list.NumberOfProcessIdsInList), maxWatchedMembers)
	for _, id := range list.ProcessIDList[:count] {
		pid := uint32(id)
		if _, watching := t.sys.members[pid]; watching || len(t.sys.members) >= maxWatchedMembers {
			continue
		}
		process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		if !memberOfJob(process, t.sys.job) {
			_ = windows.CloseHandle(process)
			continue
		}
		t.sys.members[pid] = process
	}
}

// memberOfJob reports whether process, opened with
// PROCESS_QUERY_LIMITED_INFORMATION, belongs to job.
func memberOfJob(process, job windows.Handle) bool {
	if procIsProcessInJob.Find() != nil {
		return false
	}
	var result int32
	ok, _, _ := syscall.SyscallN(procIsProcessInJob.Addr(), uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result)))
	return ok != 0 && result != 0
}

// running reports whether a process of the tree still runs.
func (t *Tree) running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return processRunning(uint32(t.id))
	}
	var info jobAccounting
	if err := windows.QueryInformationJobObject(t.sys.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return processRunning(uint32(t.id))
	}
	return info.ActiveProcesses > 0
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which x/sys/windows
// does not declare.
type jobAccounting struct {
	_               [4]int64 // user and kernel times
	_               uint32   // TotalPageFaultCount
	_               uint32   // TotalProcesses
	ActiveProcesses uint32
	_               uint32 // TotalTerminatedProcesses
}

// TerminateGroup asks every process of the tree id to exit, as Tree.Terminate
// does, for a tree this process started and has not closed. Any other tree is
// reached through its root, and only by the console event: the rest of it
// lives in a job that belongs to the process that started it and ends with
// that process. It does nothing for an id <= 0, and a tree whose root is gone
// is not an error.
func TerminateGroup(id int) error {
	if id <= 0 {
		return nil
	}
	if t := lookup(id); t != nil {
		return t.Terminate()
	}
	if !processRunning(uint32(id)) {
		return nil
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(id)); err != nil {
		return fmt.Errorf("send CTRL_BREAK_EVENT to process group %d: %w", id, err)
	}
	return nil
}

// KillGroup ends every process of the tree id, as Tree.Kill does, for a tree
// this process started and has not closed. Any other tree is reached through
// its root, which KillGroup terminates. It does nothing for an id <= 0, and a
// tree whose root is gone is not an error.
func KillGroup(id int) error {
	if id <= 0 {
		return nil
	}
	if t := lookup(id); t != nil {
		return t.Kill()
	}
	root, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(id))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("open process %d: %w", id, err)
	}
	defer func() { _ = windows.CloseHandle(root) }()
	if err := windows.TerminateProcess(root, killedExitCode); err != nil && !exited(root) {
		return fmt.Errorf("terminate process %d: %w", id, err)
	}
	return nil
}

// GroupAlive reports whether a process of the tree id still runs: any member
// of its job for a tree this process started and has not closed, its root for
// any other tree. A root owned by another user counts as running.
func GroupAlive(id int) bool {
	if id <= 0 {
		return false
	}
	if t := lookup(id); t != nil {
		return t.running()
	}
	return processRunning(uint32(id))
}

// ProcessAlive reports whether pid names a running process. A process owned by
// another user counts as running: a caller never takes a process it cannot
// inspect for dead. The exit is read from the process handle, not from its
// exit code, which a process that exited may share with STILL_ACTIVE.
func ProcessAlive(pid int) bool {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false
	}
	return processRunning(uint32(pid))
}

// processRunning reports whether pid names a running process. A process this
// caller may not open exists, and counts as running.
func processRunning(pid uint32) bool {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	return !exited(process)
}

// exited reports whether the process behind a handle opened with SYNCHRONIZE
// has exited.
func exited(process windows.Handle) bool {
	event, err := windows.WaitForSingleObject(process, 0)
	return err == nil && event == windows.WAIT_OBJECT_0
}

// Relay forwards a stop request the caller received to process, a child that
// runs in the caller's console process group. It sends nothing: a Windows
// process receives a stop request only as a console control event, and the
// console delivers that event to every process of the group, the child
// included, so the child already has it. A child that ignores it is bounded by
// the caller's grace period, as on Unix.
func Relay(*os.Process) error { return nil }

// detachedFlags give a detached process a console of its own that has no
// window. No event of the caller's console reaches it, closing the caller's
// console window does not end it, and a console program it starts shares its
// hidden console instead of opening a window. DETACHED_PROCESS, which leaves
// the process without any console, would open one window per such program.
const detachedFlags = windows.CREATE_NO_WINDOW

// StartDetached starts cmd so that it keeps running after the caller exits and
// after a tree the caller runs in is closed: the child breaks away from the
// caller's job, which every tree allows. A job this package did not create may
// forbid it; CreateProcess then fails with ERROR_ACCESS_DENIED, and
// StartDetached starts a copy of cmd inside that job, where it ends with the
// job. It returns the command that started, cmd or that copy.
func StartDetached(cmd *exec.Cmd) (*exec.Cmd, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= detachedFlags
	// A Cmd starts at most once, even after a failed Start: the copy that may
	// start inside the job is taken before cmd starts.
	inside := *cmd
	attr := *cmd.SysProcAttr
	inside.SysProcAttr = &attr
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	err := cmd.Start()
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return cmd, err
	}
	return &inside, inside.Start()
}
