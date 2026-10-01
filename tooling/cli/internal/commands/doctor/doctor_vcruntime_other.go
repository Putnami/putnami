//go:build !windows

package doctor

// vcRuntimeInstalled is nil off Windows: vcruntime140.dll is the Windows
// Visual C++ runtime, and checkWorkstation runs the runtime check only when the
// host is Windows.
var vcRuntimeInstalled func() (bool, error)
