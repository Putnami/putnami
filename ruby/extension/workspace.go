package ruby

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/stacks"
)

// WorkspaceFetchJobName is the job that fills the gem cache.
const WorkspaceFetchJobName = "workspace-fetch"

// CodeBundleFetchFailed is the diagnostic of a failed gem download.
const CodeBundleFetchFailed = "ruby.bundle_fetch_failed"

const gemfileLock = "Gemfile.lock"

// WorkspaceFetchJob downloads the gems of each stack root's Gemfile.lock into
// the extension's gem cache and runs none of their code. A hosted run's engine
// may hand this job a credential descriptor: Bundler reaches public
// registries, so the job reads and closes the descriptor before it starts any
// process and uses nothing it held.
func WorkspaceFetchJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if _, err := registrycred.ReadJobCredential(); err != nil {
			return "FAILED", nil, err
		}
		cache := gemCache(ctx.WorkspaceRoot)
		fetch := func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return fetchGems(run, root, emit, cache)
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, fetch)(ctx, emit, args)
	}
}

// fetchGems runs bundle cache with no install: Bundler downloads the locked
// gems, builds no native extension and copies the .gem files to the gem
// cache. Its install path and its local configuration are private
// directories it then removes, and the frozen lockfile is not rewritten, so
// it writes nothing in the repository. A root without a Gemfile.lock names
// no gem to download.
func fetchGems(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, cache string) error {
	if !hasGemfile(root.Dir) || !isFile(filepath.Join(root.Dir, gemfileLock)) {
		emit.Info("no " + gemfileLock + " in " + root.Dir + ": nothing to download")
		return nil
	}
	if cache == "" {
		return failFetch(emit, fmt.Errorf("no cache directory for %s", ExtensionName))
	}
	private, err := os.MkdirTemp("", "putnami-ruby-fetch-")
	if err != nil {
		return failFetch(emit, err)
	}
	defer func() { _ = os.RemoveAll(private) }()
	step := root
	step.Env = append(cacheEnv(root.Env, cache),
		"BUNDLE_GEMFILE="+filepath.Join(root.Dir, "Gemfile"),
		"BUNDLE_APP_CONFIG="+filepath.Join(private, "config"),
		"BUNDLE_PATH="+filepath.Join(private, "bundle"),
		"BUNDLE_FROZEN=true",
		"BUNDLE_NO_INSTALL=true",
		"TMPDIR="+private,
	)
	emit.Info("$ bundle cache --no-install in " + root.Dir + " to " + cache)
	if err := runBundle(ctx, step, os.Stderr, "cache"); err != nil {
		return failFetch(emit, fmt.Errorf("bundle cache in %s: %w", root.Dir, err))
	}
	return nil
}

// gemCache is the directory of the .gem files workspace-fetch downloads, under
// the extension's machine cache.
func gemCache(workspaceRoot string) string {
	root := os.Getenv(extensionproto.MachineCacheRootEnv)
	if root == "" {
		root = extensionproto.MachineCacheRoot(ExtensionName, workspaceRoot)
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "gems")
}

// cacheEnv points Bundler's gem cache at cache. Several stack roots share it,
// so Bundler keeps the gems another bundle names.
func cacheEnv(env []string, cache string) []string {
	return append(slices.Clip(env), "BUNDLE_CACHE_PATH="+cache, "BUNDLE_NO_PRUNE=true")
}

func failFetch(emit *jsonl.Emitter, err error) error {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeBundleFetchFailed)
	return fmt.Errorf("%s: %w", CodeBundleFetchFailed, err)
}
