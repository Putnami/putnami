package main

import (
	"encoding/json"
	"errors"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// A registry path segment npm percent-encodes: cmd.exe would expand %40...%.
const percentRegistry = "https://npm.example.test/%40scope/%2Fnpm"

// stubNPMLookPath makes npmLookPath find path, or nothing when path is "".
func stubNPMLookPath(t *testing.T, path string) *int {
	t.Helper()
	original := npmLookPath
	t.Cleanup(func() { npmLookPath = original })
	calls := 0
	npmLookPath = func(file string) (string, error) {
		calls++
		if file != "npm" {
			t.Fatalf("npmLookPath(%q), want npm", file)
		}
		if path == "" {
			return "", errors.New("executable file not found in %PATH%")
		}
		return path, nil
	}
	return &calls
}

func TestNPMCommand_IsNPMOnPATHElsewhereThanWindows(t *testing.T) {
	lookups := stubNPMLookPath(t, `C:\nodejs\npm.cmd`)
	for _, goos := range []string{"linux", "darwin"} {
		bin, err := npmCommand(goos, []string{"publish", "--registry", percentRegistry})
		if err != nil || bin != "npm" {
			t.Errorf("%s: npmCommand = (%q, %v), want (npm, nil)", goos, bin, err)
		}
	}
	if *lookups != 0 {
		t.Errorf("npmCommand looked npm up %d times off Windows, want none", *lookups)
	}
}

func TestNPMCommand_WindowsRunsTheNPMTheShellFinds(t *testing.T) {
	args := []string{"publish", "--access", "public", "--registry", "https://npm.example.test/"}
	for _, path := range []string{`C:\Program Files\nodejs\npm.cmd`, `C:\Volta\npm.exe`} {
		stubNPMLookPath(t, path)
		bin, err := npmCommand("windows", args)
		if err != nil || bin != path {
			t.Errorf("npmCommand = (%q, %v), want (%q, nil)", bin, err, path)
		}
	}
}

func TestNPMCommand_WindowsRefusesWhatCmdExeWouldReinterpret(t *testing.T) {
	stubNPMLookPath(t, `C:\nodejs\npm.cmd`)
	_, err := npmCommand("windows", []string{"publish", "--registry", percentRegistry})
	if !errors.Is(err, toolchain.ErrShimArgument) {
		t.Fatalf("npm.cmd with %q: err = %v, want ErrShimArgument", percentRegistry, err)
	}

	// A native npm takes the same arguments: no cmd.exe parses them.
	stubNPMLookPath(t, `C:\Volta\npm.exe`)
	if bin, err := npmCommand("windows", []string{"publish", "--registry", percentRegistry}); err != nil || bin != `C:\Volta\npm.exe` {
		t.Fatalf("npm.exe with %q: npmCommand = (%q, %v), want the native npm", percentRegistry, bin, err)
	}
}

// With no npm on PATH, exec reports the missing npm, as on every other OS.
func TestNPMCommand_WindowsLeavesAMissingNPMToExec(t *testing.T) {
	stubNPMLookPath(t, "")
	if bin, err := npmCommand("windows", []string{"view", "@test/pkg@1.0.0", "version"}); err != nil || bin != "npm" {
		t.Fatalf("npmCommand = (%q, %v), want (npm, nil)", bin, err)
	}
}

// An unmanaged publish on Windows runs npm view and npm publish through the npm
// npmCommand resolved, and refuses npm.cmd before any registry call when
// cmd.exe would reinterpret an argument.
func TestRunPublishNpm_UnmanagedRunsTheCmdExeSafeNPM(t *testing.T) {
	originalGOOS, originalRun, originalToken := npmGOOS, npmExecRun, npmResolveRegistryToken
	t.Cleanup(func() { npmGOOS, npmExecRun, npmResolveRegistryToken = originalGOOS, originalRun, originalToken })
	npmGOOS = "windows"
	npmResolveRegistryToken = func(string) (string, string) { return "", "" }

	publish := func(t *testing.T, npmPath, registry string) (status string, names []string, err error) {
		t.Helper()
		stubNPMLookPath(t, npmPath)
		npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
			names = append(names, name)
			if len(args) > 0 && args[0] == "view" {
				return &exec.Result{Success: false, ExitCode: 1}, nil
			}
			return &exec.Result{Success: true}, nil
		}
		ctx, dir := makeTestCtx(t)
		ctx.Params = pctx.Params{"npm": json.RawMessage(`true`), "registry": json.RawMessage(`"` + registry + `"`)}
		stageNPMFixture(t, dir)
		status, _, err = runPublishNpm(ctx, jsonl.New(), nil)
		return status, names, err
	}

	t.Run("refused npm.cmd", func(t *testing.T) {
		status, names, err := publish(t, `C:\nodejs\npm.cmd`, percentRegistry)
		if status != "FAILED" || !errors.Is(err, toolchain.ErrShimArgument) {
			t.Fatalf("publish = (%q, %v), want FAILED with ErrShimArgument", status, err)
		}
		if len(names) != 0 {
			t.Fatalf("npm ran %d times after the refusal, want none", len(names))
		}
	})
	t.Run("native npm", func(t *testing.T) {
		status, names, err := publish(t, `C:\Volta\npm.exe`, percentRegistry)
		if status != "OK" || err != nil {
			t.Fatalf("publish = (%q, %v), want OK", status, err)
		}
		if len(names) != 2 || names[0] != `C:\Volta\npm.exe` || names[1] != `C:\Volta\npm.exe` {
			t.Fatalf("npm runs = %q, want view and publish through the native npm", names)
		}
	})
}
