//go:build !unix

package procstat

import "time"

// SelfCPU is the CPU this process spent itself. This platform does not
// measure it: ok is false.
func SelfCPU() (cpu time.Duration, ok bool) { return 0, false }

// ChildrenCPU is the CPU of the children this process waited for. This
// platform does not measure it: ok is false.
func ChildrenCPU() (cpu time.Duration, ok bool) { return 0, false }
