package shared

import (
	"os"
	"path/filepath"
	"strings"
)

// GoCommandEnv returns os.Environ with any inherited GOWORK dropped and, when a
// go.work governs moduleDir, GOWORK pointed explicitly at it — so `go get`
// resolves the workspace's `replace` directives (and a leaked GOWORK=off cannot
// force single-module resolution).
func GoCommandEnv(moduleDir string) []string {
	return GoCommandEnvFrom(os.Environ(), moduleDir)
}

// GoCommandEnvFrom is GoCommandEnv over base instead of os.Environ: the
// environment a resolved go command runs in.
func GoCommandEnvFrom(base []string, moduleDir string) []string {
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "GOWORK=") {
			continue
		}
		out = append(out, e)
	}
	if gowork := FindGoWork(moduleDir); gowork != "" {
		out = append(out, "GOWORK="+gowork)
	}
	return out
}

// FindGoWork walks up from dir to the nearest go.work, mirroring `go`'s own
// GOWORK=auto discovery, and returns its absolute path or "".
func FindGoWork(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(abs, "go.work")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}
