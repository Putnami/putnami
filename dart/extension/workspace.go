package dart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/observe/detect/dart"
	"go.putnami.dev/tooling/stacks"
)

const (
	// InstallJobName is the job that resolves a component's stack root before
	// its task.
	InstallJobName = "install"
	// WorkspaceFetchJobName is the job that fills the pub cache.
	WorkspaceFetchJobName = "workspace-fetch"
	// WorkspaceInstallJobName is the job that resolves each stack root.
	WorkspaceInstallJobName = "workspace-install"
	// DepsUpgradeJobName is the job of the deps-upgrade command.
	DepsUpgradeJobName = "deps-upgrade"
)

// Diagnostic codes of the workspace jobs.
const (
	CodePubGetFailed     = "dart.pub_get_failed"
	CodePubUpgradeFailed = "dart.pub_upgrade_failed"
)

const (
	pubspecLock   = "pubspec.lock"
	packageConfig = ".dart_tool/package_config.json"
	// installMarker records the resolution the last pub get wrote.
	installMarker = ".dart_tool/putnami-install"
)

// InstallJob resolves the stack root of the job's component with pub get
// --enforce-lockfile before its lint, test or build, unless its resolution is
// current: a task never writes the lockfile it reads, so a root with no
// pubspec.lock fails with pub's own message. An offline run resolves nothing:
// workspace-install resolved every root from the pub cache before.
func InstallJob(stack stacks.Stack) cli.JobFunc {
	install := stack.RootJob(func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		if offline() {
			emit.Info("offline dependencies: pub get does not run")
			return nil
		}
		return pubGet(ctx, root, emit, false, false)
	})
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		// The root of a pub workspace below the workspace root resolves with
		// its members' SDK: its members' install steps resolve it.
		var meta stacks.Metadata
		if json.Unmarshal(ctx.Project.Metadata[ExtensionName], &meta) == nil && meta.Component == "" && meta.Root != "" {
			emit.Info(ctx.Project.Path + " is the root of a pub workspace: its members resolve it")
			return "OK", nil, nil
		}
		return install(ctx, emit, args)
	}
}

// WorkspaceFetchJob runs pub get --enforce-lockfile at each stack root, which
// downloads the locked packages into the pub cache and runs none of their
// code. A hosted run calls it before any repository process, with the network:
// a root with no pubspec.lock fails it, as it fails a task.
func WorkspaceFetchJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		if err := runPub(ctx, root, emit, "pub", "get", "--enforce-lockfile"); err != nil {
			emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodePubGetFailed)
			return fmt.Errorf("%s: %w", CodePubGetFailed, err)
		}
		return nil
	})
}

// WorkspaceInstallJob runs pub get at each stack root of the workspace, which
// resolves a pubspec.lock for a root that has none. A root the selected SDK
// resolved last is current and skipped, unless the force flag is set.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return pubGet(ctx, root, emit, force, true)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// DepsUpgradeJob runs pub upgrade at each stack root of the workspace.
func DepsUpgradeJob(stack stacks.Stack) cli.JobFunc {
	return stack.WorkspaceRootJob(stacks.FailOnMissingToolchain, pubUpgrade)
}

// pubGet resolves the stack root with pub get --enforce-lockfile, unless its
// resolution is current and force is unset. With writeLock set, a root with no
// pubspec.lock resolves one.
func pubGet(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force, writeLock bool) error {
	tool := rootTool(root.Dir)
	if !force && current(root, tool) {
		emit.Info(root.Dir + " is current: the selected " + tool + " resolved its pubspecs and " + pubspecLock + " last")
		return nil
	}
	args := []string{"pub", "get"}
	// The install keeps the locked versions.
	if !writeLock || hasLock(root.Dir) {
		args = append(args, "--enforce-lockfile")
	}
	if offline() {
		args = append(args, "--offline")
	}
	if err := runPub(ctx, root, emit, args...); err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodePubGetFailed)
		return fmt.Errorf("%s: %w", CodePubGetFailed, err)
	}
	if digest, ok := resolution(root, tool); ok {
		_ = os.WriteFile(filepath.Join(root.Dir, filepath.FromSlash(installMarker)), []byte(digest+"\n"), 0o644) //nolint:gosec // a marker in the root's own .dart_tool
	}
	return nil
}

func pubUpgrade(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	err := errors.New("offline dependencies: pub upgrade needs the network")
	if !offline() {
		err = runPub(ctx, root, emit, "pub", "upgrade")
	}
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodePubUpgradeFailed)
		return fmt.Errorf("%s: %w", CodePubUpgradeFailed, err)
	}
	return nil
}

// runPub runs a pub command at the stack root with flutter when the root holds
// a Flutter package, which only flutter resolves, else with dart.
func runPub(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, args ...string) error {
	tool := rootTool(root.Dir)
	emit.Info("$ " + tool + " " + strings.Join(args, " ") + " in " + root.Dir)
	cmd := exec.CommandContext(ctx, root.Program(tool), args...) //nolint:gosec // the selected SDK
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	// The job's stdout carries its protocol.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s in %s: %w", tool, strings.Join(args, " "), root.Dir, err)
	}
	return nil
}

// holdsFlutter reports whether the stack root at dir resolves a Flutter
// package: the package at dir, or a member of the pub workspace it declares.
func holdsFlutter(dir string) bool {
	for _, c := range rootPackages(dir) {
		if c.Flutter {
			return true
		}
	}
	return false
}

// rootPackages are the packages the stack root at dir resolves: the package
// at dir and the members of the pub workspace it declares.
func rootPackages(dir string) []dart.Component {
	var out []dart.Component
	for _, c := range dart.Detect(dir).Components {
		if c.ID == "." || c.PubWorkspace == "." {
			out = append(out, c)
		}
	}
	return out
}

// rootTool is the SDK that resolves the stack root at dir: flutter when the
// root holds a Flutter package, which only flutter resolves, else dart.
func rootTool(dir string) string {
	if holdsFlutter(dir) {
		return "flutter"
	}
	return "dart"
}

// current reports whether the last pub get at the stack root ran with the
// selected SDK on the pubspecs, lockfile and package_config.json it holds now:
// a resolution another SDK or an editor wrote since is not current.
func current(root stacks.RootStep, tool string) bool {
	marker, err := os.ReadFile(filepath.Join(root.Dir, filepath.FromSlash(installMarker))) //nolint:gosec // the root's own marker
	if err != nil {
		return false
	}
	digest, ok := resolution(root, tool)
	return ok && strings.TrimSpace(string(marker)) == digest
}

// resolution digests what a pub get at the stack root depends on and writes:
// the SDK program, by path, size and time, the pubspec.yaml of the root and of
// each package it resolves, its pubspec.lock and its package_config.json.
func resolution(root stacks.RootStep, tool string) (string, bool) {
	program := root.Program(tool)
	if !filepath.IsAbs(program) {
		found, ok := stacks.LookPathIn(root.Env, program)
		if !ok {
			return "", false
		}
		program = found
	}
	info, err := os.Stat(program)
	if err != nil {
		return "", false
	}
	// A shim may switch SDKs behind one file: the version it answers names
	// the SDK.
	cmd := exec.Command(program, "--version") //nolint:gosec // the selected SDK
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	version, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", program, info.Size(), info.ModTime().UnixNano(), version)
	files := []string{dart.Pubspec, pubspecLock, packageConfig}
	for _, c := range rootPackages(root.Dir) {
		if c.ID != "." {
			files = append(files, c.ID+"/"+dart.Pubspec)
		}
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root.Dir, filepath.FromSlash(name))) //nolint:gosec // a file of the stack root
		if err != nil && !(errors.Is(err, os.ErrNotExist) && name == pubspecLock) {
			return "", false
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func hasLock(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, pubspecLock))
	return err == nil
}

func offline() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}
