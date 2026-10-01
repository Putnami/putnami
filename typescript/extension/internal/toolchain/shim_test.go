package toolchain

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckShimArgs_RefusesWhatCmdExeWouldReinterpret(t *testing.T) {
	shim := `C:\ws\node_modules\.bin\biome.cmd`
	for _, arg := range []string{
		// Go leaves these unquoted: every metacharacter is live.
		`--config-path=C:\R&D\biome.json`,
		`--config-path=C:\100%PATH%\biome.json`,
		`packages/a|b`,
		`a^b`, `a<b`, `a>b`, `a!b`, `a(b`, `a)b`, `a"b`, "a\rb", "a\nb",
		// Go quotes these: cmd.exe still expands and still ends the quotes.
		`--config-path=C:\100 %PATH%\biome.json`,
		`--config-path=C:\Hi there!\biome.json`,
		`--config-path=C:\say "hi" & exit\biome.json`,
		"a b\nc",
	} {
		err := CheckShimArgsOn("windows", shim, []string{"check", arg})
		if !errors.Is(err, ErrShimArgument) {
			t.Errorf("argument %q: err = %v, want ErrShimArgument", arg, err)
			continue
		}
		if !strings.Contains(err.Error(), "cmd.exe") || !strings.Contains(err.Error(), shim) {
			t.Errorf("argument %q: error %q does not name cmd.exe and the shim", arg, err)
		}
	}
}

func TestCheckShimArgs_RefusesAShimPathCmdExeWouldReinterpret(t *testing.T) {
	err := CheckShimArgsOn("windows", `C:\R&D\node_modules\.bin\biome.CMD`, []string{"check"})
	if !errors.Is(err, ErrShimArgument) {
		t.Fatalf("err = %v, want ErrShimArgument for the shim path itself", err)
	}
}

func TestCheckShimArgs_AcceptsPlainArguments(t *testing.T) {
	args := []string{
		"check", "--reporter=json", `--config-path=C:\Users\Jo Doe\ws\biome.json`, "packages/app",
		// Go quotes an argument that holds a space; cmd.exe leaves these
		// characters alone inside quotes.
		`--config-path=C:\Program Files (x86)\ws\biome.json`,
		`--config-path=C:\Tom & Jerry\biome.json`,
		`C:\a b\x^y|z<w>v`,
	}
	for _, shim := range []string{`C:\ws\node_modules\.bin\biome.cmd`, `C:\ws\tools\biome.bat`} {
		if err := CheckShimArgsOn("windows", shim, args); err != nil {
			t.Errorf("%s: err = %v, want nil", shim, err)
		}
	}
}

func TestCheckShimArgs_OnlyBatchFilesOnWindows(t *testing.T) {
	args := []string{`--config-path=C:\Tom & Jerry\biome.json`}
	if err := CheckShimArgsOn("windows", `C:\ws\node_modules\.bin\biome.exe`, args); err != nil {
		t.Errorf("an executable: err = %v, want nil", err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := CheckShimArgsOn(goos, "/ws/tools/biome.cmd", args); err != nil {
			t.Errorf("%s: err = %v, want nil", goos, err)
		}
	}
}
