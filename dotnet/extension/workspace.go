package dotnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/stacks"
)

const (
	// WorkspaceInstallJobName is the job that restores every project.
	WorkspaceInstallJobName = "workspace-install"
	// WorkspaceFetchJobName is the job that fills the NuGet cache.
	WorkspaceFetchJobName = "workspace-fetch"
	// DepsUpgradeJobName is the job of the deps-upgrade command.
	DepsUpgradeJobName = "deps-upgrade"
)

// Diagnostic codes of the workspace jobs.
const (
	CodeRestoreFailed       = "dotnet.restore_failed"
	CodePackageUpdateFailed = "dotnet.package_update_failed"
)

// packagesLock is the NuGet lock file a project restores in locked mode.
const packagesLock = "packages.lock.json"

// packageUpdateMajor is the first SDK major line that has dotnet package
// update.
const packageUpdateMajor = 10

// WorkspaceInstallJob runs dotnet restore on the file of every project of the
// workspace: a project with a packages.lock.json restores in locked mode, so
// the install keeps its locked versions. The force flag restores again what
// NuGet holds current.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		restore := func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return restoreAll(ctx, root, emit, force)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, restore)(ctx, emit, args)
	}
}

// readJobCredential reads the credential descriptor the engine hands
// workspace-fetch, closes it and removes its variable from the environment.
// Tests replace it.
var readJobCredential = registrycred.ReadJobCredential

// WorkspaceFetchJob runs dotnet restore on the file of every project of the
// workspace, which downloads the packages it references into the NuGet global
// packages folder. A hosted run calls it before any repository process, with
// the network, so the tasks then restore from that folder.
//
// A hosted run's engine may hand this job a credential descriptor. The hosts
// of that credential are the Putnami registries, which serve no NuGet package,
// so the job reads and closes the descriptor before it starts any process, the
// SDK check included, and uses nothing it held. A descriptor it cannot read
// fails the job with dotnet.restore_failed before any process starts.
func WorkspaceFetchJob(stack stacks.Stack) cli.JobFunc {
	fetch := stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		return restoreAll(ctx, root, emit, false)
	})
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if _, err := readJobCredential(); err != nil {
			err = fmt.Errorf("cannot read the registry credential the engine handed workspace-fetch: %w", err)
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRestoreFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeRestoreFailed, err)
		}
		return fetch(ctx, emit, args)
	}
}

// DepsUpgradeJob runs dotnet package update on the file of every project of
// the workspace, which moves each package reference to the newest version its
// sources hold and restores the project, its packages.lock.json included. It
// needs the .NET 10 SDK.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, packageUpdate)
}

func restoreAll(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	for _, manifest := range root.Manifests {
		args := []string{"restore", manifest}
		if hasLock(manifest) {
			args = append(args, "--locked-mode")
		}
		if force {
			args = append(args, "--force")
		}
		if err := runDotnet(ctx, root, emit, args...); err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeRestoreFailed)
			return fmt.Errorf("%s: %w", CodeRestoreFailed, err)
		}
	}
	return nil
}

func packageUpdate(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	err := errors.New("offline dependencies: dotnet package update needs the network")
	if !offline() {
		err = needsPackageUpdate(ctx, root)
	}
	for _, manifest := range root.Manifests {
		if err != nil {
			break
		}
		err = runDotnet(ctx, root, emit, "package", "update", "--project", manifest)
	}
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodePackageUpdateFailed)
		return fmt.Errorf("%s: %w", CodePackageUpdateFailed, err)
	}
	return nil
}

// needsPackageUpdate fails when the selected SDK predates dotnet package
// update.
func needsPackageUpdate(ctx context.Context, root stacks.RootStep) error {
	cmd := exec.CommandContext(ctx, root.Program("dotnet"), "--version") //nolint:gosec // the selected SDK
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("dotnet --version in %s: %w", root.Dir, err)
	}
	version := strings.TrimSpace(string(out))
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < packageUpdateMajor {
		return fmt.Errorf("dotnet package update needs the .NET %d SDK, and %s selects %s: raise the SDK global.json states", packageUpdateMajor, root.Dir, version)
	}
	return nil
}

// runDotnet runs the selected dotnet at the stack root.
func runDotnet(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, args ...string) error {
	emit.Info("$ dotnet " + strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, root.Program("dotnet"), args...) //nolint:gosec // the selected SDK
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	// The job's stdout carries its protocol.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dotnet %s in %s: %w", strings.Join(args, " "), root.Dir, err)
	}
	return nil
}

func hasLock(manifest string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(manifest), packagesLock))
	return err == nil
}

func offline() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}
