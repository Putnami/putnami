package stacks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// stagingPattern names the directories an install downloads into before it
// moves the result in place.
const stagingPattern = "download-*"

// LockedToolsHome returns the folder of a stack's tools under the Putnami home,
// <home>/tools/<stack>, holding the lock every install of that stack on the
// machine takes, and the function that releases it. Under the lock, a staging
// directory (download-*) belongs to an install that stopped, and is removed.
func LockedToolsHome(stack string) (string, func(), error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", nil, fmt.Errorf("no Putnami home: set %s", putnamihome.Env)
	}
	base := filepath.Join(home, "tools", stack)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(base, ".lock"), true, false)
	if err != nil {
		return "", nil, fmt.Errorf("lock %s: %w", base, err)
	}
	if stale, err := filepath.Glob(filepath.Join(base, stagingPattern)); err == nil {
		for _, dir := range stale {
			_ = os.RemoveAll(dir)
		}
	}
	return base, func() { _ = lock.Release() }, nil
}

// ExecutableSuffix is the suffix of an executable on platform, an os/arch pair
// such as windows/amd64: .exe on Windows, none elsewhere.
func ExecutableSuffix(platform string) string {
	if strings.HasPrefix(platform, "windows/") {
		return ".exe"
	}
	return ""
}

// CompareVersions orders two dotted numeric versions: -1 when a sorts before
// b, 0 when they are equal, 1 after. A missing or non-numeric component reads
// as zero.
func CompareVersions(a, b string) int {
	return compareRelease(numbers(a), numbers(b))
}

// Accepted returns the candidates whose version accepts reports true, in their
// order.
func Accepted[R any](candidates []R, version func(R) string, accepts func(string) bool) []R {
	return slices.DeleteFunc(slices.Clone(candidates), func(r R) bool { return !accepts(version(r)) })
}

// Choose picks among accepted releases, oldest first: the first of preferred
// that is accepted, else the newest. It reports false when none is accepted.
func Choose[R any](accepted []R, version func(R) string, preferred ...string) (R, bool) {
	var none R
	if len(accepted) == 0 {
		return none, false
	}
	for _, want := range preferred {
		if want == "" {
			continue
		}
		if i := slices.IndexFunc(accepted, func(r R) bool { return version(r) == want }); i >= 0 {
			return accepted[i], true
		}
	}
	return accepted[len(accepted)-1], true
}

// PinResolver answers the lock pin of a stack's toolchain for the workspace at
// root, or false to pin nothing, which leaves the lock without it.
type PinResolver func(root string, request wsproto.ToolchainResolveRequest) (wsproto.LockToolchainEntry, bool)

// ServeToolchainResolve serves the toolchain-resolve control call of an
// extension that pins one toolchain identity in the workspace lock, for the
// workspace at the current directory. The bool reports whether args name that
// call.
func ServeToolchainResolve(args []string, stdin io.Reader, stdout io.Writer, extension, identity string, resolve PinResolver) (bool, error) {
	return wsproto.ServeToolchainResolve(args, stdin, stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve the workspace root: %w", err)
		}
		return AnswerToolchainResolve(root, request, extension, identity, resolve), nil
	})
}

// AnswerToolchainResolve answers one toolchain-resolve request against the
// workspace at root: the pin resolve answers for identity, a problem for any
// other identity, and nothing for an identity it pins nothing for.
func AnswerToolchainResolve(root string, request wsproto.ToolchainResolveRequest, extension, identity string, resolve PinResolver) wsproto.ToolchainResolveResult {
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  extension,
		Toolchains: map[string]wsproto.ToolchainResolution{},
	}
	for _, requested := range request.Identities {
		if requested != identity {
			result.Toolchains[requested] = wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
				Message: fmt.Sprintf("%s pins the %s toolchain only, not %q", extension, identity, requested),
			}}
			continue
		}
		if pin, ok := resolve(root, request); ok {
			result.Toolchains[requested] = wsproto.ToolchainResolution{Pin: &pin}
		}
	}
	return result
}
