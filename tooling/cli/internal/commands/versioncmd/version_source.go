package versioncmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// cliVersionVar is the linker symbol the CLI stamps its version string into.
// It must match @putnami/cli's options."@putnami/go".version-var in
// tooling/cli/putnami.json; a fork that renames the package should publish
// through the normal channel instead of self-hosting from source.
const cliVersionVar = "go.putnami.dev/tooling/cli/internal/cli.Version"

// cliSourceModuleRel is the workspace-relative Go module that holds the CLI's
// main package (./cmd/putnami).
var cliSourceModuleRel = filepath.Join("tooling", "cli")

// VersionInstallFromSource builds the putnami CLI from the source in the
// current workspace and installs it as the active binary in binDir.
//
// This is the self-host escape hatch: when the download channel is broken or a
// merged fix is not yet published, a framework developer can adopt their
// working tree's CLI as the global binary without the manual `go build` +
// symlink swap. The build is stamped with a source-derived version
// (0.0.0-source-<sha>) so `putnami --version` plainly shows it is a local
// build, and is installed as a new inode followed by an atomic symlink swap —
// never rewriting the running binary's inode (see installBinary).
func VersionInstallFromSource(ctx context.Context, wsRoot, binDir string, dryRun bool) error {
	if wsRoot == "" {
		return cmderr.Usagef("upgrade --from-source must run inside a workspace that contains the putnami CLI source")
	}
	moduleDir := filepath.Join(wsRoot, cliSourceModuleRel)
	entry := filepath.Join(moduleDir, "cmd", "putnami")
	if _, err := os.Stat(entry); err != nil {
		if os.IsNotExist(err) {
			return cmderr.NotFoundf("putnami CLI source not found at %s; --from-source only works in the framework workspace", entry)
		}
		return fmt.Errorf("stat CLI source: %w", err)
	}

	version := sourceBuildVersion(ctx, wsRoot)
	// Mirror the putnami-go-<version> family that `upgrade` installs so
	// `version list`/`version use` treat a source build like any other.
	targetName := pkgmeta.ExecutableName(runtime.GOOS, "putnami-go-"+strings.TrimPrefix(version, "0.0.0-"))
	targetPath := filepath.Join(binDir, targetName)
	binLink := CLIPath(binDir)

	if dryRun {
		iox.Fprintf(os.Stdout, "  Would build %s from %s\n", version, entry)
		iox.Fprintf(os.Stdout, "  Would install %s and point %s at it\n", targetPath, binLink)
		return nil
	}

	iox.Fprintf(os.Stdout, "  Building %s from %s...\n", version, entry)
	built, cleanup, err := buildStampedCLI(ctx, moduleDir, cliVersionVar, version)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create bin directory: %w", err)
	}
	if err := installBinary(built, targetPath); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	iox.Fprintf(os.Stdout, "  Installed %s\n", targetPath)

	if err := activateCLI(targetName, binLink); err != nil {
		return fmt.Errorf("activate binary: %w", err)
	}
	iox.Fprintf(os.Stdout, "  Active: %s -> %s (%s)\n", binLink, targetName, version)
	return nil
}

// sourceBuildVersion derives a clearly-local version string from the
// workspace's git state, e.g. 0.0.0-source-1a2b3c4 (or ...-dirty when the
// tree has uncommitted changes). It never fails: a non-git workspace yields
// 0.0.0-source-local.
func sourceBuildVersion(ctx context.Context, wsRoot string) string {
	sha := "local"
	if out, err := gitOutput(ctx, wsRoot, "rev-parse", "--short", "HEAD"); err == nil {
		if s := strings.TrimSpace(out); s != "" {
			sha = s
		}
	}
	if out, err := gitOutput(ctx, wsRoot, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
		sha += "-dirty"
	}
	return "0.0.0-source-" + sha
}

// sourceBuildPattern is the os.CreateTemp pattern of the binary a source build
// writes for goos. It carries the executable suffix goos requires (.exe on
// Windows), which os.CreateTemp keeps after the random part.
func sourceBuildPattern(goos string) string {
	return pkgmeta.ExecutableName(goos, "putnami-source-build-*")
}

// buildStampedCLI compiles ./cmd/putnami in moduleDir with the version symbol
// stamped via -ldflags, writing the binary to a temp file. Build output is
// streamed to stderr so compile errors in the working tree are visible. The
// returned cleanup removes the temp file. Building the workspace's source
// counts as repository code (runcredential.MarkRepositoryCodeStarted).
func buildStampedCLI(ctx context.Context, moduleDir, versionVar, version string) (string, func(), error) {
	noop := func() {}
	runcredential.MarkRepositoryCodeStarted("go build ./cmd/putnami")
	out, err := os.CreateTemp("", sourceBuildPattern(runtime.GOOS))
	if err != nil {
		return "", noop, err
	}
	outPath := out.Name()
	out.Close()
	cleanup := func() { os.Remove(outPath) }

	ldflags := fmt.Sprintf("-X %s=%s", versionVar, version)
	//nolint:gosec // G702: in-workspace `go build`; ldflags carries only a git-derived 0.0.0-source-<sha> version and outPath is a temp file. Args go straight to exec — no shell.
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", outPath, "./cmd/putnami")
	cmd.Dir = moduleDir
	// Normalize GOWORK for the module being built. Putnami's Go test/build
	// runners deliberately point GOWORK at the repository workspace; inheriting
	// that value while compiling a standalone module outside the workspace makes
	// cmd/go reject the build. Conversely, source builds inside a workspace must
	// retain their governing go.work. goCommandEnv handles both cases.
	cmd.Env = shared.GoCommandEnv(moduleDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("go build ./cmd/putnami: %w", err)
	}
	return outPath, cleanup, nil
}
