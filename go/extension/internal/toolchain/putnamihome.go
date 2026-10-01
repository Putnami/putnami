package toolchain

import (
	"path/filepath"
	"strings"
)

const (
	// putnamiHomeDirName is the Putnami home under a user's home directory,
	// and under a workspace root when no home directory is set.
	putnamiHomeDirName = ".putnami"
	// toolchainsDirName is the subtree of a Putnami home that holds the
	// toolchains Putnami installs for itself, one <name>/<name>-<version>
	// directory each.
	toolchainsDirName = "toolchains"
	// goToolchainName names the Go toolchain in that subtree.
	goToolchainName = "go"
)

// ResolvePutnamiHome returns the Putnami home the environment names, or ""
// when it names none: PUTNAMI_HOME, else .putnami under the user's home
// directory. lookup reads one variable.
func ResolvePutnamiHome(lookup func(string) string) string {
	if lookup == nil {
		return ""
	}
	if putnamiHome := strings.TrimSpace(lookup(PutnamiHomeEnv)); putnamiHome != "" {
		return putnamiHome
	}
	if home := strings.TrimSpace(lookup(homeEnvName())); home != "" {
		return filepath.Join(home, putnamiHomeDirName)
	}
	return ""
}

// ResolveGoToolchainRoot returns the directory that holds the Go releases
// `putnami install` installs, one go-<version> directory each, shared by every
// workspace of the machine: toolchains/go under the Putnami home.
//
// When the environment names no home, the Putnami home is .putnami under
// workspaceRoot. The CLI resolves the home the same way for the putnami-home
// candidates of a runtime toolchain, so a release installed here is the one
// the candidate toolchains/go/go-{version}/go/bin/go names.
func ResolveGoToolchainRoot(lookup func(string) string, workspaceRoot string) string {
	home := ResolvePutnamiHome(lookup)
	if home == "" {
		home = filepath.Join(workspaceRoot, putnamiHomeDirName)
	}
	return filepath.Join(home, toolchainsDirName, goToolchainName)
}
