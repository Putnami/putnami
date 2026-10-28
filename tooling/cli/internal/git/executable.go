package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// appleGitTrampoline is the git macOS ships in /usr/bin. It runs no git
// itself: each call asks xcrun where the developer tools' git is, then runs
// that git, which makes a call about four times slower than a direct one.
const appleGitTrampoline = "/usr/bin/git"

// Executable names the git program to run. When the PATH lookup lands on the
// macOS trampoline, it returns the git the trampoline forwards to, found once
// per developer directory. Otherwise it returns "git", found on PATH as
// before, so a git placed earlier on PATH still wins.
func Executable() string {
	return resolveExecutable(runtime.GOOS, exec.LookPath, os.Getenv("DEVELOPER_DIR"), developerGit)
}

var developerGits sync.Map // developer directory -> resolved git, "" when unresolved

func resolveExecutable(goos string, lookPath func(string) (string, error), developerDir string, find func() string) string {
	if goos != "darwin" {
		return "git"
	}
	if path, err := lookPath("git"); err != nil || path != appleGitTrampoline {
		return "git"
	}
	resolved, ok := developerGits.Load(developerDir)
	if !ok {
		resolved, _ = developerGits.LoadOrStore(developerDir, find())
	}
	if resolved == "" {
		return "git"
	}
	return resolved.(string)
}

// developerGit asks xcrun for the developer tools' git, the one the
// trampoline runs. It returns "" when xcrun names no executable file.
func developerGit() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/xcrun", "--find", "git").Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) || path == appleGitTrampoline {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return path
}
