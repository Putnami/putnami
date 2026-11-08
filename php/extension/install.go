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
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
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

// composerSHA256 is the digest of the pinned composer.phar. Tests pin the file
// their local server publishes.
var composerSHA256 = "6f1b891e1d4927d62740276c540fb490e47689e19e102c49e8da849794882f8c"

// composerURL is where composer.phar is published. Tests point it at a local
// server.
var composerURL = "https://getcomposer.org/download/" + composerVersion + "/composer.phar"

// InstallJob is the job of a component's install step: composer install at its
// stack root. A run whose dependencies are offline installs nothing.
func InstallJob(stack stacks.Stack) cli.JobFunc {
	job := stack.RootJob(installDependencies)
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if os.Getenv(extensionproto.OfflineDependenciesEnv) == "1" {
			emit.Info("offline dependencies: composer install does not run")
			return "OK", nil, nil
		}
		return job(ctx, emit, args)
	}
}

func installDependencies(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if vendorIsCurrent(root.Dir) {
		return nil
	}
	phar, err := provideComposer(ctx)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	emit.Info("$ composer install in " + root.Dir)
	cmd := exec.CommandContext(ctx, root.Program("php"), phar, "install", "--no-interaction", "--no-progress") //nolint:gosec // the pinned Composer
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	// The job's stdout carries its protocol.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		emit.DiagnosticWithCode("error", fmt.Sprintf("composer install in %s: %v", root.Dir, err), "", 0, 0, CodeComposerInstallFailed)
		return fmt.Errorf("%s: %w", CodeComposerInstallFailed, err)
	}
	return nil
}

// vendorIsCurrent reports whether vendor/composer/installed.json is newer than
// composer.json and composer.lock at dir.
func vendorIsCurrent(dir string) bool {
	installed, err := os.Stat(filepath.Join(dir, "vendor", "composer", "installed.json"))
	if err != nil {
		return false
	}
	for _, name := range []string{"composer.json", "composer.lock"} {
		if manifest, err := os.Stat(filepath.Join(dir, name)); err == nil && !installed.ModTime().After(manifest.ModTime()) {
			return false
		}
	}
	return true
}

// provideComposer returns the path of the pinned composer.phar under the
// Putnami home, downloading it when its digest does not match.
func provideComposer(ctx context.Context) (string, error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", fmt.Errorf("no Putnami home: set %s", putnamihome.Env)
	}
	path := filepath.Join(home, "tools", "php", "composer-"+composerVersion, "composer.phar")
	if digest, err := fileSHA256(path); err == nil && digest == composerSHA256 {
		return path, nil
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
	return os.Rename(stage, path)
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
