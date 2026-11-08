package php

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/stacks"
)

const (
	// WorkspaceFetchJobName is the job that fills the Composer cache.
	WorkspaceFetchJobName = "workspace-fetch"
	// WorkspaceInstallJobName is the job that installs each stack root.
	WorkspaceInstallJobName = "workspace-install"
)

// CodeComposerFetchFailed is the diagnostic of a failed package download.
const CodeComposerFetchFailed = "php.composer_fetch_failed"

const composerLock = "composer.lock"

// WorkspaceFetchJob downloads the packages of each stack root's composer.lock
// and runs none of their code. A hosted run's engine may hand this job a
// credential descriptor: Composer reaches public registries, so the job reads
// and closes the descriptor before it starts any process and uses nothing it
// held.
func WorkspaceFetchJob(stack stacks.Stack) cli.JobFunc {
	fetch := stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, fetchPackages)
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if _, err := registrycred.ReadJobCredential(); err != nil {
			return "FAILED", nil, err
		}
		return fetch(ctx, emit, args)
	}
}

// WorkspaceInstallJob runs composer install at each stack root. The force flag
// installs a root even when its last install is current.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return installWorkspaceRoot(ctx, root, emit, force)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// fetchPackages runs composer install into a private vendor directory it then
// removes: the packages stay in the Composer cache. A root without a
// composer.lock names no package to download.
func fetchPackages(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	if !hasLock(root.Dir) {
		emit.Info("no " + composerLock + " in " + root.Dir + ": nothing to download")
		return nil
	}
	if err := prepareWindowsPHP(root, emit); err != nil {
		return err
	}
	phar, err := provideComposer(ctx)
	if err != nil {
		return failFetch(emit, err)
	}
	private, err := os.MkdirTemp("", "putnami-php-fetch-")
	if err != nil {
		return failFetch(emit, err)
	}
	defer func() { _ = os.RemoveAll(private) }()
	env := append(composerEnv(root.Env), "COMPOSER_VENDOR_DIR="+filepath.Join(private, "vendor"), "COMPOSER_BIN_DIR="+filepath.Join(private, "bin"), "TMPDIR="+private)
	emit.Info("$ composer install --no-scripts --no-plugins in " + root.Dir)
	if err := runComposer(ctx, root, env, phar, "install", "--no-interaction", "--no-progress", "--no-scripts", "--no-plugins", "--no-autoloader"); err != nil {
		return failFetch(emit, fmt.Errorf("composer install in %s: %w", root.Dir, err))
	}
	return nil
}

// installWorkspaceRoot installs one stack root. Offline, Composer reads its
// cache alone, so a root without a composer.lock has nothing it can install.
func installWorkspaceRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	if err := prepareWindowsPHP(root, emit); err != nil {
		return err
	}
	if offlineDependencies() && !hasLock(root.Dir) {
		emit.DiagnosticWithCode("warning", "no "+composerLock+" in "+root.Dir+": an offline install has nothing to install", "", 0, 0, CodeComposerInstallFailed)
		return nil
	}
	return composerInstall(ctx, root, emit, composerEnv(root.Env), force)
}

func hasLock(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, composerLock))
	return err == nil
}

func failFetch(emit *jsonl.Emitter, err error) error {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeComposerFetchFailed)
	return fmt.Errorf("%s: %w", CodeComposerFetchFailed, err)
}
