package rust

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/stacks"
)

// Job names of the dependency jobs.
const (
	// WorkspaceFetchJobName is the job that downloads each stack root's locked
	// dependencies.
	WorkspaceFetchJobName = "workspace-fetch"
	// WorkspaceInstallJobName is the job that fetches each stack root's
	// dependencies, writing a missing Cargo.lock.
	WorkspaceInstallJobName = "workspace-install"
	// DepsUpgradeJobName is the job that updates each stack root's Cargo.lock.
	DepsUpgradeJobName = "deps-upgrade"
)

// Diagnostic codes of the dependency jobs.
const (
	CodeCargoFetchFailed  = "rust.cargo_fetch_failed"
	CodeCargoUpdateFailed = "rust.cargo_update_failed"
	// CodeLockfileMissing warns that a stack root with no Cargo.lock is left
	// out of a job that writes no lockfile.
	CodeLockfileMissing = "rust.lockfile_missing"
	// CodeTestToolsUnavailable warns that workspace-install could not install
	// the test tools ahead of the first test.
	CodeTestToolsUnavailable = "rust.test_tools_unavailable"
)

const (
	cargoLock = "Cargo.lock"
	// cargoNetOfflineEnv is cargo's net.offline setting: true keeps every cargo
	// command off the network, as --offline does.
	cargoNetOfflineEnv = "CARGO_NET_OFFLINE"
)

// onlineJobs are the jobs that keep the network when the run's dependencies
// are offline: cargo publish uploads to its registry.
var onlineJobs = map[string]bool{"publish-crates": true}

// readJobCredential reads the read credential the engine hands workspace-fetch.
// Tests replace it.
var readJobCredential = registrycred.ReadJobCredential

// OfflineCargo returns jobs where each job but the online ones turns cargo's
// network off when the run's dependencies are offline: every cargo it starts,
// the profile's steps included, inherits CARGO_NET_OFFLINE=true and reads the
// crates workspace-fetch downloaded. CARGO_HOME stays the environment's, so
// the fetch and the tasks read one cargo home.
func OfflineCargo(jobs map[string]cli.JobFunc) map[string]cli.JobFunc {
	out := make(map[string]cli.JobFunc, len(jobs))
	for name, job := range jobs {
		if onlineJobs[name] {
			out[name] = job
			continue
		}
		out[name] = func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
			if offlineDependencies() {
				if err := os.Setenv(cargoNetOfflineEnv, "true"); err != nil {
					return "FAILED", nil, err
				}
			}
			return job(ctx, emit, args)
		}
	}
	return out
}

// WorkspaceFetchJob runs cargo fetch --locked at each stack root, which
// downloads the crates and git dependencies its Cargo.lock names into cargo's
// home and runs none of their code: no build script and no procedural macro.
// A hosted run's engine may hand this job a credential descriptor. Its hosts
// are the Putnami registries, and none of them serves a Cargo registry: cargo
// authenticates to an alternative registry with its own credential providers.
// So the job reads and closes the descriptor before it starts any process,
// and uses nothing it held.
func WorkspaceFetchJob(stack stacks.Stack) cli.JobFunc {
	fetch := stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, fetchCrates)
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if _, err := readJobCredential(); err != nil {
			emit.DiagnosticWithCode("error", "read the credential the engine handed workspace-fetch: "+err.Error(), "", 0, 0, CodeCargoFetchFailed)
			return "FAILED", nil, fmt.Errorf("%s: %w", CodeCargoFetchFailed, err)
		}
		return fetch(ctx, emit, args)
	}
}

// WorkspaceInstallJob runs cargo fetch at each stack root, which writes a
// Cargo.lock a root lacks, then installs the test tools the PATH lacks. A
// root whose locked crates cargo's home already holds is current and
// skipped, unless the force flag is set. Offline, it checks each root's
// Cargo.lock against cargo's home and writes nothing.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return installRoot(run, root, emit, force)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// DepsUpgradeJob runs cargo update at each stack root, with the cargo the
// toolchain check selects: it moves each locked version to the newest one its
// semver requirement accepts. It needs the network.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, updateLockfile)
}

// fetchCrates runs cargo fetch --locked at the stack root. A root with no
// Cargo.lock is left out: cargo fetch would write one. The fetch runs no
// rustc wrapper and no rustc the repository's Cargo configuration names: it
// runs before any repository process on a hosted run, and cargo asks rustc
// for the targets it knows.
func fetchCrates(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if !hasLock(root.Dir) {
		emit.DiagnosticWithCode("warning", "no "+cargoLock+" in "+root.Dir+": workspace-fetch leaves it out, since cargo fetch would write one; run putnami install and commit it", "", 0, 0, CodeLockfileMissing)
		return nil
	}
	step := root
	step.Env = append(slices.Clip(root.Env), "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER=", "RUSTC="+selectedRustc(root))
	return runCargo(ctx, step, emit, CodeCargoFetchFailed, "fetch", "--locked")
}

// installRoot fetches the stack root's dependencies, then installs the test
// tools. Offline, it checks the root's Cargo.lock against cargo's home: a
// root with no Cargo.lock has nothing it can check.
func installRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	if offlineDependencies() {
		if !hasLock(root.Dir) {
			emit.DiagnosticWithCode("warning", "no "+cargoLock+" in "+root.Dir+": an offline install has nothing to check, so it is left out", "", 0, 0, CodeLockfileMissing)
			return nil
		}
		return runCargo(ctx, root, emit, CodeCargoFetchFailed, "fetch", "--locked", "--offline")
	}
	if !force && fetched(ctx, root) {
		emit.Info(root.Dir + " is current: cargo's home holds every crate its " + cargoLock + " names")
	} else if err := runCargo(ctx, root, emit, CodeCargoFetchFailed, "fetch"); err != nil {
		return err
	}
	installTestTools(ctx, root, emit)
	return nil
}

// fetched reports whether the stack root's Cargo.lock is current and cargo's
// home holds every crate it names: cargo fetch --locked --offline passes.
func fetched(ctx context.Context, root stacks.RootStep) bool {
	if !hasLock(root.Dir) {
		return false
	}
	cmd := exec.CommandContext(ctx, root.Program("cargo"), "fetch", "--locked", "--offline", "--quiet") //nolint:gosec // the selected toolchain
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	return cmd.Run() == nil
}

// installTestTools installs the test task's tools the PATH lacks ahead of the
// first test. A failure is a warning: the first test installs them again.
func installTestTools(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) {
	if _, err := Tools(ctx, stacks.TaskTest, stacks.Metadata{}, root.Env, emit.Info); err != nil {
		emit.DiagnosticWithCode("warning", "the test tools are not installed ahead of the first test, which installs them: "+err.Error(), "", 0, 0, CodeTestToolsUnavailable)
	}
}

// updateLockfile runs cargo update at the stack root; it needs the network.
func updateLockfile(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if offlineDependencies() {
		err := errors.New("offline dependencies: cargo update needs the network to read the registries")
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeCargoUpdateFailed)
		return fmt.Errorf("%s: %w", CodeCargoUpdateFailed, err)
	}
	return runCargo(ctx, root, emit, CodeCargoUpdateFailed, "update")
}

// runCargo runs the selected cargo with args at the stack root and reports a
// failure under code. The job's stdout carries its protocol, so cargo's
// output goes to stderr.
func runCargo(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, code string, args ...string) error {
	emit.Info("$ cargo " + strings.Join(args, " ") + " in " + root.Dir)
	cmd := exec.CommandContext(ctx, root.Program("cargo"), args...) //nolint:gosec // the selected toolchain
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("cargo %s in %s: %v", strings.Join(args, " "), root.Dir, err), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	return nil
}

// selectedRustc is the absolute path of the rustc the toolchain check
// selected, else the bare name, which cargo looks up on PATH.
func selectedRustc(root stacks.RootStep) string {
	program := root.Program("rustc")
	if filepath.IsAbs(program) {
		return program
	}
	if found, ok := stacks.LookPathIn(root.Env, program); ok {
		return found
	}
	return program
}

func hasLock(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, cargoLock))
	return err == nil && !info.IsDir()
}

func offlineDependencies() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}
