//go:build windows

package recorded

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// executable writes the replay as a batch file, which CreateProcess runs
// through cmd.exe. type copies each recorded stream byte for byte and exit /b
// sets the status. A value cmd.exe would read differently than it is written
// fails tb instead of being replayed wrong: see batchLiteral and replayFile.
func executable(tb testing.TB, exchange Exchange, branches []Branch) string {
	tb.Helper()
	dir := tb.TempDir()
	var script strings.Builder
	// Delayed expansion reads a variable after the line is parsed, so a value
	// the caller's environment holds is compared, never parsed as batch text.
	script.WriteString("@echo off\r\nsetlocal EnableDelayedExpansion\r\n")
	for index, branch := range branches {
		if !envName.MatchString(branch.Env) {
			tb.Fatalf("recorded branch environment name %q is not a shell variable name", branch.Env)
		}
		fmt.Fprintf(&script, "if \"!%s!\"==\"%s\" (\r\n%s)\r\n",
			branch.Env, batchLiteral(tb, branch.Value), replayCommands(tb, dir, "branch-"+strconv.Itoa(index), branch.Exchange))
	}
	script.WriteString(replayCommands(tb, dir, "default", exchange))
	path := filepath.Join(dir, "replay.cmd")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

// replayCommands copies an exchange's streams next to the batch file, so the
// bytes the program writes are the recorded bytes and no batch parsing touches
// them.
func replayCommands(tb testing.TB, dir, name string, exchange Exchange) string {
	tb.Helper()
	stdout := replayFile(tb, filepath.Join(dir, name+".stdout"), exchange.stdout)
	stderr := replayFile(tb, filepath.Join(dir, name+".stderr"), exchange.stderr)
	return fmt.Sprintf("type \"%s\"\r\ntype \"%s\" 1>&2\r\nexit /b %d\r\n", stdout, stderr, exchange.exitCode)
}

// replayFile writes a recorded stream to path and returns the path. type ends
// a file at its first Ctrl+Z and converts one that starts with a UTF-16 byte
// order mark, so such a stream fails tb.
func replayFile(tb testing.TB, path string, stream []byte) string {
	tb.Helper()
	if bytes.IndexByte(stream, 0x1a) >= 0 || bytes.HasPrefix(stream, []byte{0xff, 0xfe}) || bytes.HasPrefix(stream, []byte{0xfe, 0xff}) {
		tb.Fatalf("recorded stream %s holds a Ctrl+Z or a UTF-16 byte order mark, which cmd.exe's type does not copy as recorded", filepath.Base(path))
	}
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		tb.Fatal(err)
	}
	return batchLiteral(tb, path)
}

// batchLiteral returns value for a double-quoted place in the batch file. A
// quote ends the place, cmd.exe expands a percent sign, and with delayed
// expansion it expands an exclamation mark and drops a caret, so a value
// holding one of them, or a line break, fails tb.
func batchLiteral(tb testing.TB, value string) string {
	tb.Helper()
	if strings.ContainsAny(value, "\"%!^\r\n") {
		tb.Fatalf("%q holds a character cmd.exe reads differently than it is written in a batch file", value)
	}
	return value
}
