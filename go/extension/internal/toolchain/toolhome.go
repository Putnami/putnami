package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/go/extension/tools"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// Machine tool home resolution: where the pinned development tools
// (golangci-lint, staticcheck) live once per machine.
//
// `putnami install` warms this directory from the `putnami-go
// workspace-install` job, which resolves the root through this function, and
// `putnami lint` reads it from the lint job. Two paths that disagreed would
// make install compile a tool lint never finds, which is exactly the defect
// this resolver replaces (install wrote
// .putnami/extensions/@putnami-go/bin/tools while lint read
// .putnami/bin/extensions/putnami-go/bin/tools).
//
// It deliberately does NOT read PUTNAMI_GO_CACHE_DIR. That variable relocates
// the Go BUILD and MODULE caches, which `putnami cache clean` empties on
// purpose; a pinned tool binary is managed state whose absence costs a 40 s
// rebuild, not a cache entry. Keeping the two roots separate is what lets the
// cache commands stay destructive without un-warming the machine.
//
// PUTNAMI_HOME relocates the whole ~/.putnami tree and therefore relocates the
// tool home with it. PUTNAMI_EXTENSION_CACHE_ROOT is the generic per-extension
// machine root the C5 contract provides, used only when no home directory is
// available at all.
const (
	// GoToolHomeDirName is the tool-home subtree of a Putnami home.
	GoToolHomeDirName = "tools"
	// GoToolHomeEcosystem scopes the tool home to this extension's ecosystem,
	// so a future extension can own <home>/tools/<its ecosystem> beside it.
	GoToolHomeEcosystem = "go"
)

// ResolveGoToolHomeRoot returns the machine-global root of the pinned Go tool
// binaries, or "" when no root can be resolved at all.
//
// Order: relocated putnami home, the ~/.putnami default, then the
// contract-provided extension cache root. lookup reads one variable, matching
// ResolveGoCacheRoot so both roots can be resolved against a built environment
// or against os.Getenv.
func ResolveGoToolHomeRoot(lookup func(string) string) string {
	if lookup == nil {
		return ""
	}
	if putnamiHome := ResolvePutnamiHome(lookup); putnamiHome != "" {
		return filepath.Join(putnamiHome, GoToolHomeDirName, GoToolHomeEcosystem)
	}
	if extensionCache := strings.TrimSpace(lookup(ExtensionCacheRootEnv)); extensionCache != "" {
		return filepath.Join(extensionCache, GoToolHomeEcosystem, GoToolHomeDirName)
	}
	return ""
}

// goToolHomePath is the machine path of ONE pinned tool binary.
//
// The key is the whole identity of the artifact: the tool, the pinned version,
// the local Go major.minor it serves, and the platform it runs on. A tool built
// with an older Go minor than the workspace uses panics type-checking newer
// sources (toolBuildMatchesCurrentGoVersion exists for exactly that), so the Go
// minor is part of the PATH rather than a property to re-probe — two
// workspaces on two Go minors keep two copies instead of evicting each other.
// The binary under a key is built with that minor or a newer one
// (ToolServesLocalGo): a prebuilt tool from a newer Go serves an older minor.
//
// Layout: <root>/<tool>/<version>/go<major.minor>/<goos>-<goarch>/<tool>. The
// file name is the executable name on goos, not on this host: a windows key
// ends in <tool>.exe and no other key does. Keep it identical to
// toolHomeBinary of the `putnami-go workspace-install` job
// (internal/jobs/workspaceinstall); TestToolHomeMatchesTheLintResolver pins
// the two.
func goToolHomePath(root, tool, version, goMajorMinorVersion, goos, goarch string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(
		root,
		tool,
		version,
		"go"+goMajorMinorVersion,
		goos+"-"+goarch,
		pkgmeta.ExecutableName(goos, tool),
	)
}

// ToolHome returns the machine path this process would install tool at, or ""
// when no machine root can be resolved and the legacy workspace location must
// be used instead.
//
// It probes the LOCAL Go toolchain for its version, the same way
// toolBuildMatchesCurrentGoVersion does, so the destination and the
// compatibility check can never disagree about which Go minor is current.
func ToolHome(tool string) string {
	spec, ok := tools.Lookup(tool)
	if !ok {
		return ""
	}
	root := ResolveGoToolHomeRoot(os.Getenv)
	if root == "" {
		return ""
	}
	return goToolHomePath(
		root,
		tool,
		spec.Version,
		goMajorMinor(localGoVersion(CurrentGoBinary())),
		runtime.GOOS,
		runtime.GOARCH,
	)
}

// toolBinaryName is the on-disk file name of a tool for the host platform.
func toolBinaryName(tool string) string {
	if runtime.GOOS == "windows" {
		return tool + ".exe"
	}
	return tool
}

// ExtensionArtifactToolPath is the prebuilt tool the RUNNING extension ships,
// or "" when this process runs from a source checkout with no artifact.
//
// An installed extension is materialized from its per-platform archive, which
// carries compiled/tools/<tool> beside compiled/<runtime binary> (see
// internal/jobs/pkg/archives.go). The archive is content-addressed and shared
// by every workspace on the machine, so it is a READ-ONLY source: InstallTool
// copies out of it and never writes into it.
func ExtensionArtifactToolPath(tool string) string {
	extensionRoot := strings.TrimSpace(os.Getenv(extensionRootEnvVar))
	if extensionRoot == "" {
		return ""
	}
	return filepath.Join(extensionRoot, "compiled", GoToolHomeDirName, toolBinaryName(tool))
}

// LegacyManagedToolPaths lists workspace tool locations accepted as read-only
// sources. The order matches _go_legacy_tool_binaries in the shell resolver.
func LegacyManagedToolPaths(tool, workspaceRoot string) []string {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	name := toolBinaryName(tool)
	return []string{
		filepath.Join(extRoot(workspaceRoot), "bin", "tools", name),
		filepath.Join(workspaceRoot, ".putnami", "extensions", "@putnami-go", "bin", "tools", name),
	}
}

// homeEnvName is the variable os.UserHomeDir reads on this platform, so a home
// resolved from a built environment and one resolved from the process agree.
func homeEnvName() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}
