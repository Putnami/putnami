package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/envkeys"
)

// windowsLikePath joins host-style directories with the host list separator,
// so the Windows spelling of the variable can be exercised on any host.
func windowsLikePath(dirs ...string) string {
	return strings.Join(dirs, string(os.PathListSeparator))
}

func hostDir(parts ...string) string {
	return string(filepath.Separator) + filepath.Join(parts...)
}

// The golden path on Windows: the system environment block spells the variable
// "Path". The job environment prepends the CLI directory, then the resolved
// toolchain directory, exactly as prepareJobInvocation and
// applyRuntimeToolchains compose it. The child must keep every system entry
// behind them, under one PATH entry, and PATHEXT must not be mistaken for PATH.
func TestEnvKeysFoldKeepsTheWindowsSystemPath(t *testing.T) {
	system32 := hostDir("Windows", "system32")
	windowsDir := hostDir("Windows")
	bunDir := hostDir("Users", "dev", ".bun", "bin")
	cliDir := hostDir("Users", "dev", ".putnami", "bin")
	toolDir := hostDir("Users", "dev", ".putnami", "toolchains", "go", "bin")
	block := []string{
		"ALLUSERSPROFILE=" + hostDir("ProgramData"),
		"ComSpec=" + filepath.Join(system32, "cmd.exe"),
		"Path=" + windowsLikePath(system32, windowsDir, bunDir),
		"PATHEXT=.COM;.EXE;.BAT;.CMD",
		"SystemRoot=" + windowsDir,
		"USERPROFILE=" + hostDir("Users", "dev"),
	}

	keys := envkeys.Keys{Fold: true}
	env := prependPathWith(keys, append([]string(nil), block...), cliDir)
	env = keys.Set(env, "PATH", minimalRuntimePath(keys.Last(env, "PATH"), []string{toolDir}))

	var pathEntries []string
	for _, entry := range env {
		if _, ok := keys.Value(entry, "PATH"); ok {
			pathEntries = append(pathEntries, entry)
		}
	}
	want := "PATH=" + windowsLikePath(toolDir, cliDir, system32, windowsDir, bunDir)
	if len(pathEntries) != 1 || pathEntries[0] != want {
		t.Fatalf("child PATH entries = %q, want exactly [%q]", pathEntries, want)
	}
	if !slices.Contains(env, "PATHEXT=.COM;.EXE;.BAT;.CMD") {
		t.Fatalf("PATHEXT was dropped: %q", env)
	}
	if got := keys.Last(env, "path"); got != strings.TrimPrefix(want, "PATH=") {
		t.Fatalf("lower-case lookup = %q, want the composed PATH", got)
	}
	if kept := keys.Remove(env, "pathext"); slices.ContainsFunc(kept, func(e string) bool { return strings.HasPrefix(e, "PATHEXT=") }) {
		t.Fatalf("case-folded removal kept PATHEXT: %q", kept)
	}
}

// On Unix names are case-sensitive: "Path" is another variable, so the CLI
// directory becomes the whole PATH and "Path" passes through untouched.
func TestEnvKeysExactTreatsOtherSpellingsAsOtherVariables(t *testing.T) {
	cliDir := hostDir("opt", "putnami", "bin")
	other := "Path=" + windowsLikePath(hostDir("usr", "bin"))
	env := prependPathWith(envkeys.Keys{}, []string{other, "HOME=" + hostDir("home", "dev")}, cliDir)
	want := []string{other, "HOME=" + hostDir("home", "dev"), "PATH=" + cliDir}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
}
