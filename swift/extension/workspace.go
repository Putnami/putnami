package swift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// Workspace job names.
const (
	WorkspaceInstallJobName = "workspace-install"
	DepsUpgradeJobName      = "deps-upgrade"
)

// Diagnostic codes of the workspace jobs.
const (
	CodeResolveFailed = "swift.resolve_failed"
	CodeUpdateFailed  = "swift.update_failed"
)

const (
	packageResolved = "Package.resolved"
	// resolvedMarker records, under .build, the manifests the last
	// successful resolve of a package used.
	resolvedMarker = "putnami-resolved"
)

// WorkspaceInstallJob resolves the packages of each component. The force flag
// resolves a package even when its last resolve is current.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return resolveRoot(run, root, emit, force)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// DepsUpgradeJob updates the dependencies of each package. An Xcode project
// keeps its pins, since Xcode owns their update, so the job leaves it out
// before its toolchain check.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	upgrade := stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, upgradeRoot)
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		packages := *ctx
		packages.WorkspaceProjects = nil
		for _, ref := range ctx.WorkspaceProjects {
			if isPackage(filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(ref.Path))) {
				packages.WorkspaceProjects = append(packages.WorkspaceProjects, ref)
			}
		}
		return upgrade(&packages, emit, args)
	}
}

// isPackage reports whether the component directory holds a Swift package;
// any other component of the stack is an Xcode project.
func isPackage(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "Package.swift"))
	return err == nil && !info.IsDir()
}

func resolveRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	// Offline, as in a hosted run, a resolve would reach the network: the
	// build resolves from what the machine already holds.
	if os.Getenv(extensionproto.OfflineDependenciesEnv) == "1" {
		emit.DiagnosticWithCode("warning", "offline dependencies: "+root.Dir+" is not resolved", "", 0, 0, CodeResolveFailed)
		return nil
	}
	if !isPackage(root.Dir) {
		emit.Info("$ xcodebuild -resolvePackageDependencies in " + root.Dir)
		return tool(ctx, root, emit, CodeResolveFailed, "xcodebuild", "-resolvePackageDependencies", "-onlyUsePackageVersionsFromResolvedFile")
	}
	digest, err := manifestsDigest(root.Dir)
	if err != nil {
		return fail(emit, CodeResolveFailed, err)
	}
	marker := filepath.Join(root.Dir, ".build", resolvedMarker)
	if recorded, err := os.ReadFile(marker); !force && err == nil && string(recorded) == digest { //nolint:gosec // inside the package
		return nil
	}
	// A resolve that stops partway must not pass for the last finished one.
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(emit, CodeResolveFailed, err)
	}
	emit.Info("$ swift package resolve in " + root.Dir)
	if err := tool(ctx, root, emit, CodeResolveFailed, root.Program("swift"), "package", "resolve", "--force-resolved-versions"); err != nil {
		return err
	}
	err = os.MkdirAll(filepath.Dir(marker), 0o755)
	if err == nil {
		err = os.WriteFile(marker, []byte(digest), 0o644) //nolint:gosec // inside the package's build directory
	}
	if err != nil {
		emit.DiagnosticWithCode("warning", "the resolve is not recorded, so the next install resolves again: "+err.Error(), "", 0, 0, CodeResolveFailed)
	}
	return nil
}

func upgradeRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if os.Getenv(extensionproto.OfflineDependenciesEnv) == "1" {
		return fail(emit, CodeUpdateFailed, errors.New("offline dependencies: swift package update needs the network"))
	}
	emit.Info("$ swift package update in " + root.Dir)
	return tool(ctx, root, emit, CodeUpdateFailed, root.Program("swift"), "package", "update")
}

// manifestsDigest is the digest of the files a resolve reads: Package.swift,
// its version-specific variants and Package.resolved.
func manifestsDigest(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "Package@swift-*.swift"))
	if err != nil {
		return "", err
	}
	files = append(files, filepath.Join(dir, "Package.swift"), filepath.Join(dir, packageResolved))
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // a manifest of the package
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.Base(f), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tool runs a program at the stack root; its output goes to stderr, since the
// job's stdout carries its protocol.
func tool(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, code, program string, args ...string) error {
	cmd := exec.CommandContext(ctx, program, args...) //nolint:gosec // the stack's own tools
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(emit, code, fmt.Errorf("%s in %s: %w", filepath.Base(program), root.Dir, err))
	}
	return nil
}

func fail(emit *jsonl.Emitter, code string, err error) error {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
	return fmt.Errorf("%s: %w", code, err)
}
