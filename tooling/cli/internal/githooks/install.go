package githooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// Install writes putnami's git hook scripts to .git/hooks/. selfBinary is the
// absolute path to the putnami binary the commit-msg hook re-invokes for
// validation (empty skips the commit-msg hook). It is a no-op in CI (CI=true) and
// outside a git repository, so it is safe to call unconditionally on checkout.
func Install(selfBinary string) {
	if os.Getenv("CI") == "true" {
		return
	}

	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return // not a git repo
	}
	gitRoot := strings.TrimSpace(string(out))
	gitDir := filepath.Join(gitRoot, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return
	}

	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create hooks directory %s: %v\n", hooksDir, err)
		return
	}

	writeHook(hooksDir, "pre-commit", preCommitScript())
	writeHook(hooksDir, "pre-push", prePushScript())
	if target := hookTarget(gitRoot, selfBinary, runtime.GOOS); target != "" {
		writeHook(hooksDir, "commit-msg", commitMsgScript(target))
	}

	fmt.Println("Git hooks installed.")
}

// hookTarget picks the putnami the commit-msg hook re-invokes. The hook is
// DURABLE — it outlives the command that wrote it and every later CLI upgrade —
// so it must name a path that keeps resolving, and selfBinary often does not.
//
// `os.Executable()` reports the blob the kernel started. In a source workspace
// that is `$PUTNAMI_HOME/artifacts/cli-source/<key>/putnami`, which the artifact
// GC reaps by recency and `putnami cache clean --all` removes outright; baking it
// in makes `git commit` fail later with "no such file". On Linux this was already
// the case (`/proc/self/exe` resolves the link), and macOS now matches by
// exec'ing the blob directly, so both platforms need the same answer.
//
// `<gitRoot>/.putnami/bin/putnami` is that answer: a stable NAME that the wrapper
// (source workspace) or the installer (consumer workspace) keeps pointed at a
// current engine. Re-pointing it is exactly what must not break the hook, and a
// name is re-pointable where bytes are not — the opposite of what the launcher's
// own provenance proof needs, which is why the two do not share a target. On
// Windows the name is putnami.exe.
func hookTarget(gitRoot, selfBinary, goos string) string {
	link := filepath.Join(gitRoot, ".putnami", "bin", pkgmeta.ExecutableName(goos, "putnami"))
	if info, err := os.Stat(link); err == nil && !info.IsDir() {
		return link
	}
	return selfBinary
}

func writeHook(dir, name, content string) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // hook scripts are intentionally executable
		fmt.Fprintf(os.Stderr, "Warning: failed to write hook %s: %v\n", name, err)
	}
}

func preCommitScript() string {
	return `#!/bin/sh
# Installed by putnami — blocks commits on main, runs linting, rejects binaries/large files

branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" = "main" ]; then
  echo "Direct commits to main are blocked. Use a feature branch."
  exit 1
fi

# Reject binary files and large files (>1MB)
MAX_SIZE=1048576
errors=""
for file in $(git diff --cached --name-only --diff-filter=d); do
  size=$(git cat-file -s ":$file" 2>/dev/null) || continue
  if [ "$size" -gt "$MAX_SIZE" ]; then
    size_human=$(awk "BEGIN {printf \"%.1fMB\", $size / 1048576}")
    errors="${errors}\n  ${file} (${size_human} > 1MB)"
    continue
  fi
  if git diff --cached --numstat --diff-filter=d -- "$file" | grep -q "^-"; then
    errors="${errors}\n  ${file} (binary file)"
  fi
done
if [ -n "$errors" ]; then
  echo "Commit blocked — binary or large files (>1MB) detected:"
  printf "%b\n" "$errors"
  echo ""
  echo "Use git commit --no-verify to bypass."
  exit 1
fi

# Run linting on impacted files
putnami lint --impacted --no-color || exit 1
`
}

func prePushScript() string {
	return `#!/bin/sh
# Installed by putnami — blocks pushes on main

branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" = "main" ]; then
  echo "Direct pushes to main are blocked. Use a Pull Request."
  exit 1
fi
`
}

func commitMsgScript(selfBinary string) string {
	// Escape single quotes for safe shell embedding: ' → '\''
	escaped := strings.ReplaceAll(selfBinary, "'", "'\\''")
	return fmt.Sprintf(`#!/bin/sh
# Installed by putnami — validates conventional commit format
exec '%s' commit-msg "$1"
`, escaped)
}
