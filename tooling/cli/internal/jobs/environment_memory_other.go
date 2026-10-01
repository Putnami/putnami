//go:build !windows

package jobs

// platformPhysicalMemoryBytes reads nothing outside Windows: Linux publishes
// the physical memory in /proc/meminfo and darwin through sysctl, which
// totalMemoryBytesAt reads first.
func platformPhysicalMemoryBytes() uint64 { return 0 }
