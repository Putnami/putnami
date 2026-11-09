package ruby

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// Job names of the Bundler jobs.
const (
	// InstallJobName is the job of a component's install step.
	InstallJobName = "install"
	// WorkspaceInstallJobName is the job that installs each stack root.
	WorkspaceInstallJobName = "workspace-install"
	// DepsUpgradeJobName is the job that updates each stack root's bundle.
	DepsUpgradeJobName = "deps-upgrade"
)

// Diagnostic codes of the Bundler jobs.
const (
	CodeBundleInstallFailed = "ruby.bundle_install_failed"
	CodeBundleUpdateFailed  = "ruby.bundle_update_failed"
)

// InstallJob is the job of a component's install step: bundle install at its
// stack root when its bundle is incomplete, or when the Bundler that runs is
// not the one its Gemfile.lock states.
func InstallJob(stack stacks.Stack) cli.JobFunc {
	bundler := bundlerCheck(stack)
	return stack.RootJob(func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		return bundleInstall(ctx, root, emit, bundler, false)
	})
}

// WorkspaceInstallJob runs bundle install at each stack root. The force flag
// installs a root whose bundle check passes as well.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	bundler := bundlerCheck(stack)
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return bundleInstall(ctx, root, emit, bundler, force)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// DepsUpgradeJob runs bundle update at each stack root.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, bundleUpdate)
}

// bundleInstall runs bundle install at the stack root, unless the root holds
// no Gemfile or, without force, bundle check accepts its bundle and the
// Bundler that runs is the locked one. bundle install installs the locked
// Bundler and switches to it, so the next task runs that one. With offline
// dependencies it installs from the gems already on the machine, and a
// Bundler other than the locked one alone installs nothing.
func bundleInstall(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, bundler stacks.Toolchain, force bool) error {
	if !hasGemfile(root.Dir) {
		emit.Info("no Gemfile in " + root.Dir + ": nothing to install")
		return nil
	}
	if !force && runBundle(ctx, root, nil, "check") == nil {
		if offlineDependencies() || !bundlerDiffers(ctx, root, bundler) {
			return nil
		}
		emit.Info("the Bundler that runs in " + root.Dir + " is not the one Gemfile.lock states: bundle install switches to it")
	}
	args := []string{"install"}
	if offlineDependencies() {
		args = append(args, "--local")
	}
	emit.Info(fmt.Sprintf("$ bundle %v in %s", args, root.Dir))
	if err := runBundle(ctx, root, os.Stderr, args...); err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("bundle install in %s: %v", root.Dir, err), "", 0, 0, CodeBundleInstallFailed)
		return fmt.Errorf("%s: %w", CodeBundleInstallFailed, err)
	}
	return nil
}

// bundleUpdate runs bundle update at the stack root; it needs the network.
func bundleUpdate(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if !hasGemfile(root.Dir) {
		emit.Info("no Gemfile in " + root.Dir + ": nothing to update")
		return nil
	}
	if offlineDependencies() {
		err := errors.New("offline dependencies: bundle update needs the network")
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeBundleUpdateFailed)
		return fmt.Errorf("%s: %w", CodeBundleUpdateFailed, err)
	}
	emit.Info("$ bundle update in " + root.Dir)
	if err := runBundle(ctx, root, os.Stderr, "update"); err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("bundle update in %s: %v", root.Dir, err), "", 0, 0, CodeBundleUpdateFailed)
		return fmt.Errorf("%s: %w", CodeBundleUpdateFailed, err)
	}
	return nil
}

// runBundle runs bundle with args at the stack root. The job's stdout carries
// its protocol, so Bundler's output goes to out, or nowhere when out is nil.
func runBundle(ctx context.Context, root stacks.RootStep, out *os.File, args ...string) error {
	cmd := exec.CommandContext(ctx, root.Program("bundle"), args...) //nolint:gosec // Bundler at the stack root
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}
	return cmd.Run()
}

// bundlerCheck is the further check of the profile that reads the Bundler
// version: the one whose command is bundle.
func bundlerCheck(stack stacks.Stack) stacks.Toolchain {
	for _, check := range stack.Profile.Toolchain.Also {
		if len(check.Command) > 0 && check.Command[0] == "bundle" {
			return check
		}
	}
	return stacks.Toolchain{}
}

// bundlerDiffers reports whether the Bundler that runs at the stack root
// prints a version other than the one its Gemfile.lock states. A lockfile
// that states none, or a Bundler that does not answer, differs from nothing.
func bundlerDiffers(ctx context.Context, root stacks.RootStep, check stacks.Toolchain) bool {
	if len(check.Command) == 0 {
		return false
	}
	expected, err := check.ExpectedVersion(root.Dir, ".", ".")
	if err != nil || expected.Version == "" {
		return false
	}
	check.Command = append([]string{root.Program(check.Command[0])}, check.Command[1:]...)
	running, err := check.MachineVersion(ctx, root.Dir, root.Env)
	return err == nil && check.Compare(expected, running).Code == stacks.CodeToolchainMismatch
}

func hasGemfile(dir string) bool {
	return isFile(filepath.Join(dir, "Gemfile"))
}

func offlineDependencies() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}
