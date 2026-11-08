package php

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// InstallJobName is the job of a component's install step.
const InstallJobName = "install"

// CodeComposerInstallFailed is the diagnostic of a failed composer install.
const CodeComposerInstallFailed = "php.composer_install_failed"

const (
	composerVersion = "2.11.1"
	// composerDownloadTimeout bounds the download of composer.phar.
	composerDownloadTimeout = 5 * time.Minute
)

// installedMarker is the file under vendor/ that holds the digest of the
// manifests of the last successful install.
const installedMarker = ".putnami-composer-install"

// hostOS is the operating system the extension runs on. Tests name another.
var hostOS = runtime.GOOS

// composerSHA256 is the digest of the pinned composer.phar. Tests pin the file
// their local server publishes.
var composerSHA256 = "6f1b891e1d4927d62740276c540fb490e47689e19e102c49e8da849794882f8c"

// composerURL is where composer.phar is published. Tests point it at a local
// server.
var composerURL = "https://getcomposer.org/download/" + composerVersion + "/composer.phar"

// InstallJob is the job of a component's install step: composer install at its
// stack root.
func InstallJob(stack stacks.Stack) cli.JobFunc {
	return stack.RootJob(installDependencies)
}

// installDependencies prepares the selected PHP, then runs composer install
// unless the dependencies are offline or the last install is current.
func installDependencies(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if err := prepareWindowsPHP(root, emit); err != nil {
		return err
	}
	if offlineDependencies() {
		emit.Info("offline dependencies: composer install does not run")
		return nil
	}
	return composerInstall(ctx, root, emit, composerEnv(root.Env), false)
}

func offlineDependencies() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}

func prepareWindowsPHP(root stacks.RootStep, emit *jsonl.Emitter) error {
	if err := enableWindowsExtensions(root.Program("php")); err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	return nil
}

// composerEnv returns env with Composer's package cache in the directory this
// extension owns on the machine, which workspace-fetch fills and every install
// reads, and with the network off when the dependencies are offline.
func composerEnv(env []string) []string {
	out := slices.Clip(env)
	if cache := os.Getenv(extensionproto.MachineCacheRootEnv); cache != "" {
		out = append(out, "COMPOSER_CACHE_DIR="+filepath.Join(cache, "composer"))
	}
	if offlineDependencies() {
		out = append(out, "COMPOSER_DISABLE_NETWORK=1")
	}
	return out
}

// composerInstall runs composer install at the stack root with env, unless the
// last install is current and force is false.
func composerInstall(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, env []string, force bool) error {
	if !force && vendorIsCurrent(root.Dir) {
		return nil
	}
	phar, err := provideComposer(ctx)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	// A run that stops partway must not pass for the last finished install.
	if err := os.Remove(filepath.Join(root.Dir, "vendor", installedMarker)); err != nil && !os.IsNotExist(err) {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	emit.Info("$ composer install in " + root.Dir)
	if err := runComposer(ctx, root, env, phar, "install", "--no-interaction", "--no-progress"); err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("composer install in %s: %v", root.Dir, err), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	if err := recordInstall(root.Dir); err != nil {
		// Composer's vendor directory is configurable: the next run installs again.
		emit.DiagnosticWithCode("warning", "the install is not recorded, so the next task installs again: "+err.Error(), "", 0, 0, CodeComposerInstallFailed)
	}
	return nil
}

// runComposer runs the pinned Composer at the stack root. The job's stdout
// carries its protocol, so Composer's output goes to stderr.
func runComposer(ctx context.Context, root stacks.RootStep, env []string, phar string, args ...string) error {
	cmd := exec.CommandContext(ctx, root.Program("php"), append([]string{phar}, args...)...) //nolint:gosec // the pinned Composer
	cmd.Dir = root.Dir
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// vendorIsCurrent reports whether the last successful install at dir was made
// from its current composer.json and composer.lock.
func vendorIsCurrent(dir string) bool {
	recorded, err := os.ReadFile(filepath.Join(dir, "vendor", installedMarker)) //nolint:gosec // inside the stack root
	if err != nil {
		return false
	}
	digest, err := manifestsDigest(dir)
	return err == nil && string(recorded) == digest
}

// recordInstall records the manifests a successful install used.
func recordInstall(dir string) error {
	digest, err := manifestsDigest(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "vendor", installedMarker), []byte(digest), 0o644) //nolint:gosec // inside the stack root
}

func manifestsDigest(dir string) (string, error) {
	hash := sha256.New()
	for _, name := range []string{"composer.json", "composer.lock"} {
		content, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // inside the stack root
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		_, _ = hash.Write(content)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// enableWindowsExtensions writes the php.ini of a pinned Windows PHP, whose
// archive ships none: without one, PHP loads no shared extension, and Composer
// and PHPUnit stop on the missing openssl and mbstring.
func enableWindowsExtensions(program string) error {
	if hostOS != "windows" || !filepath.IsAbs(program) {
		return nil
	}
	dir := filepath.Dir(program)
	path := filepath.Join(dir, "php.ini")
	ini := "extension_dir = \"" + filepath.Join(dir, "ext") + "\"\n"
	for _, name := range []string{"openssl", "mbstring", "curl", "zip", "fileinfo"} {
		ini += "extension=" + name + "\n"
	}
	if current, err := os.ReadFile(path); err == nil && string(current) == ini { //nolint:gosec // the pinned toolchain's own folder
		return nil
	}
	// The folder is shared by every stack root of the machine: a reader must
	// never see a partial file.
	return robustio.WriteFile(path, []byte(ini), 0o644, nil)
}

// provideComposer returns the path of the pinned composer.phar under the
// Putnami home, downloading it when its digest does not match. Offline, a
// missing Composer is an error.
func provideComposer(ctx context.Context) (string, error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", fmt.Errorf("no Putnami home: set %s", putnamihome.Env)
	}
	path := filepath.Join(home, "tools", "php", "composer-"+composerVersion, "composer.phar")
	if digest, err := fileSHA256(path); err == nil && digest == composerSHA256 {
		return path, nil
	}
	if offlineDependencies() {
		return "", fmt.Errorf("offline dependencies: Composer %s is not at %s, and this run cannot download it", composerVersion, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, downloadComposer(ctx, path)
}

// downloadComposer writes the published composer.phar to path, only when its
// digest is the pinned one. pinnedarchive.Download does not open the file, so
// the format it requires says nothing of a .phar.
func downloadComposer(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, composerDownloadTimeout)
	defer cancel()
	staging, err := os.MkdirTemp(filepath.Dir(path), "download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	stage := filepath.Join(staging, "composer.phar")
	pin := pinnedarchive.Pin{URL: composerURL, SHA256: composerSHA256, Format: pinnedarchive.Zip}
	if err := pinnedarchive.Download(ctx, pin, stage, pinnedarchive.Options{}); err != nil {
		return fmt.Errorf("download Composer %s: %w", composerVersion, err)
	}
	return robustio.Rename(stage, path)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // the pinned Composer under the Putnami home
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
