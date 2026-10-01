package mcp

import "os/exec"

// extensionToolFilePresent reports whether exec.Command would find a program
// at a resolved command path. On Windows it completes a path without an
// extension from PATHEXT, so a declared bin/tool runs bin\tool.exe; LookPath on
// a path with a separator applies exactly that completion and searches no PATH.
func extensionToolFilePresent(path string) bool {
	_, err := exec.LookPath(path)
	return err == nil
}
