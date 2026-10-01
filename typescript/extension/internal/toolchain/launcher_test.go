package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// noBun fails the test's bun resolution: a command that needs no bun never
// looks for one.
func noBun() (string, error) {
	return "", errors.New("bun not found")
}

func writeScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIsNodeScript(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"env node", "#!/usr/bin/env node\nconsole.log(1);\n", true},
		{"env with a flag", "#!/usr/bin/env -S node --no-warnings\n", true},
		{"node by path", "#!/usr/local/bin/node\n", true},
		{"shebang alone", "#!/usr/bin/env node", true},
		{"shell script", "#!/bin/sh\nexec node \"$0\"\n", false},
		{"another interpreter", "#!/usr/bin/env nodejs-wrapper\n", false},
		{"native executable", "\x7fELF\x02\x01\x01\x00node\n", false},
		{"node after the first line", "\n#!/usr/bin/env node\n", false},
		{"empty file", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNodeScript(writeScript(t, c.content)); got != c.want {
				t.Errorf("isNodeScript() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsNodeScript_MissingFile(t *testing.T) {
	if isNodeScript(filepath.Join(t.TempDir(), "missing")) {
		t.Error("isNodeScript() = true for a file that does not exist")
	}
}

func TestCommand_StartsEveryOtherFileAsItself(t *testing.T) {
	args := []string{"check", "."}
	for _, bin := range []string{
		writeScript(t, "#!/bin/sh\n"),
		writeScript(t, "\x7fELF"),
		filepath.Join(t.TempDir(), "missing"),
	} {
		program, got, err := commandWith(noBun, bin, args)
		if err != nil {
			t.Fatalf("commandWith(%q): %v", bin, err)
		}
		if program != bin || !slices.Equal(got, args) {
			t.Errorf("commandWith(%q) = %q %q, want the file and its arguments unchanged", bin, program, got)
		}
	}
}

func TestCommand_NodeScriptWithoutBunFails(t *testing.T) {
	launcher := writeScript(t, "#!/usr/bin/env node\n")

	_, _, err := commandWith(noBun, launcher, nil)
	if err == nil {
		t.Fatal("commandWith() succeeded without a bun to start the launcher")
	}
	if !strings.Contains(err.Error(), launcher) || !strings.Contains(err.Error(), "bun not found") {
		t.Errorf("commandWith() error = %q, want it to name the launcher and the missing bun", err)
	}
}

// Command starts a launcher with the bun ResolveBun finds.
func TestCommand_NodeScriptStartsThroughTheResolvedBun(t *testing.T) {
	bun, err := ResolveBun()
	if err != nil {
		t.Skip("bun is not installed")
	}
	launcher := writeScript(t, "#!/usr/bin/env node\n")

	program, args, err := Command(launcher, []string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	if program != bun || !slices.Equal(args, []string{launcher, "--version"}) {
		t.Errorf("Command() = %q %q, want %q started with the launcher", program, args, bun)
	}
}
