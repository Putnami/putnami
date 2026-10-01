//go:build !windows

package doctor

// readLongPathsEnabled is nil off Windows: LongPathsEnabled is a Windows
// registry value, and checkWorkstation runs the long-path checks only when the
// host is Windows.
var readLongPathsEnabled func() (bool, error)
