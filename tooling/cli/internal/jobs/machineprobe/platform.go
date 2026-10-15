package machineprobe

import "runtime"

// HostPlatform is the GOOS/GOARCH of the machine running the CLI, in the same
// `os/arch` spelling extensions use for platform requests.
func HostPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
