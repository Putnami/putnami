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
//
// The forwarded git runs without the variables the trampoline adds to its
// environment (SDKROOT, CPATH, LIBRARY_PATH, MANPATH). A git verb that runs
// repository hooks or filters, which may build native code, keeps "git".
func Executable() string {
	return resolveExecutable(&developerGits, runtime.GOOS, exec.LookPath, os.Getenv("DEVELOPER_DIR"), developerGit, fileExists)
}

// developerGits maps a developer directory to its *developerGitEntry.
var developerGits sync.Map

// developerGitEntry is the git xcrun names for one developer directory; ""
// when it names none. Concurrent first calls share one lookup.
type developerGitEntry struct {
	once sync.Once
	path string
}

func resolveExecutable(cache *sync.Map, goos string, lookPath func(string) (string, error), developerDir string, find func() string, exists func(string) bool) string {
	if goos != "darwin" {
		return "git"
	}
	if path, err := lookPath("git"); err != nil || path != appleGitTrampoline {
		return "git"
	}
	value, _ := cache.LoadOrStore(developerDir, &developerGitEntry{})
	entry := value.(*developerGitEntry)
	entry.once.Do(func() { entry.path = find() })
	if entry.path == "" {
		return "git"
	}
	// The selected developer tools can move while a long process runs: look
	// again on the next call, and let the trampoline run this one.
	if !exists(entry.path) {
		cache.CompareAndDelete(developerDir, entry)
		return "git"
	}
	return entry.path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
