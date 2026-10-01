// Package procguard keeps the memory and environment of a process that holds
// a credential out of reach of the other processes its user runs.
//
// On Linux, DenyInspection marks the calling process non-dumpable with
// prctl(PR_SET_DUMPABLE, 0). The kernel then refuses a process of the same
// user without CAP_SYS_PTRACE when it reads /proc/<pid>/environ or
// /proc/<pid>/mem, lists /proc/<pid>/fd, or attaches with ptrace, and the
// process writes no core dump. The flag belongs to the process and holds for
// the rest of its life. It resets when a process executes a program, so a
// child the process starts is inspectable again: a child must never receive
// the credential, in its environment, its arguments or a descriptor. A process
// that replaces its own image keeps its pid but loses the mark too, so the new
// program calls DenyInspection again before it reads a credential handed on.
//
// On every other operating system DenyInspection does nothing and returns nil.
// macOS has no /proc. A process of the same user still reads another one's
// arguments and environment there through sysctl(KERN_PROCARGS2), which no
// flag of the target prevents, so a credential stays out of both on every
// platform. PT_DENY_ATTACH is not used: it only refuses ptrace, and it makes a
// process that runs under a debugger exit.
package procguard
