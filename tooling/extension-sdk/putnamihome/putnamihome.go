// Package putnamihome resolves the Putnami home of an extension job and the
// directories under it that hold the toolchains Putnami installs for itself.
//
// The CLI resolves the same home for the putnami-home candidates of a runtime
// toolchain: PUTNAMI_HOME, else .putnami under the user's home directory, else
// .putnami under the workspace root. An extension that installs a release
// under ToolchainRoot therefore writes it where the candidate
// toolchains/<name>/<name>-{version}/... names it.
package putnamihome

import (
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// Env relocates the whole Putnami home.
	Env = "PUTNAMI_HOME"
	// DirName is the Putnami home under a user's home directory, and under a
	// workspace root when no home directory is set.
	DirName = ".putnami"
	// ToolchainsDirName is the subtree of a Putnami home that holds the
	// toolchains Putnami installs for itself, one <name>/<name>-<version>
	// directory each.
	ToolchainsDirName = "toolchains"
)

// UserHomeEnv names the variable that holds the user's home directory on this
// platform: USERPROFILE on Windows, HOME elsewhere.
func UserHomeEnv() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

// Resolve returns the Putnami home the environment names, or "" when it names
// none: PUTNAMI_HOME, else .putnami under the user's home directory. lookup
// reads one variable. A value that is only white space names nothing.
func Resolve(lookup func(string) string) string {
	if lookup == nil {
		return ""
	}
	if home := strings.TrimSpace(lookup(Env)); home != "" {
		return home
	}
	if userHome := strings.TrimSpace(lookup(UserHomeEnv())); userHome != "" {
		return filepath.Join(userHome, DirName)
	}
	return ""
}

// ToolchainRoot returns the directory that holds the releases of the toolchain
// name, one <name>-<version> directory each, shared by every workspace of the
// machine: toolchains/<name> under the Putnami home. When the environment
// names no home, the Putnami home is .putnami under workspaceRoot.
func ToolchainRoot(lookup func(string) string, workspaceRoot, name string) string {
	home := Resolve(lookup)
	if home == "" {
		home = filepath.Join(workspaceRoot, DirName)
	}
	return filepath.Join(home, ToolchainsDirName, name)
}

// IsToolchainInstall reports whether dir has the place and the name of a
// release of the toolchain name that Putnami installed:
// toolchains/<name>/<name>-<version>. It reads no file.
func IsToolchainInstall(dir, name string) bool {
	if strings.TrimSpace(dir) == "" || name == "" {
		return false
	}
	dir = filepath.Clean(dir)
	release, versioned := strings.CutPrefix(filepath.Base(dir), name+"-")
	parent := filepath.Dir(dir)
	return versioned && release != "" &&
		filepath.Base(parent) == name &&
		filepath.Base(filepath.Dir(parent)) == ToolchainsDirName
}
