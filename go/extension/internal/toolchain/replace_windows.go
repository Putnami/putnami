//go:build windows

package toolchain

// replaceExecutable moves staged to dest through replaceAside: dest may be a
// running program, which Windows lets a caller rename but not replace.
func replaceExecutable(staged, dest string) error { return replaceAside(osFileOps, staged, dest) }
