//go:build unix

package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// probeLeakValue is the value of every variable the probed CLI must not see.
const probeLeakValue = "probe-must-not-see-this"

// probeLeakNames are variables a hosted run can hold that the probe must not
// hand the pinned CLI: a provider choice, a bound request, a capability.
var probeLeakNames = []string{"PUTNAMI_PROVIDERS", runner.BoundRequestEnv, extensionproto.CloudTokenEnv}

// fakePinnedCLI writes a pinned CLI that prints help and exits with code. It
// records its arguments, working directory, environment and open descriptors
// 3 to 9 in the file it returns. Neither path reaches it through the
// environment, which the probe replaces.
func fakePinnedCLI(t *testing.T, help string, code int) (path, record string) {
	t.Helper()
	dir := t.TempDir()
	helpFile := filepath.Join(dir, "help.txt")
	if err := os.WriteFile(helpFile, []byte(help), 0o600); err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"args=$*\"\n" +
		"  echo \"pwd=$(pwd -P)\"\n" +
		"  env\n" +
		"  for n in 3 4 5 6 7 8 9; do if [ -e /dev/fd/$n ]; then echo \"open-fd=$n\"; fi; done\n" +
		"} > " + ShellQuote(record) + "\n" +
		"cat " + ShellQuote(helpFile) + "\n" +
		"exit " + string(rune('0'+code)) + "\n"
	path = filepath.Join(dir, "putnami")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, record
}

// credentialLineWithoutLevel is the run credential's help line of a CLI that
// accepts the flag and advertises no custody level: the line an older build
// prints.
const credentialLineWithoutLevel = "  --credential-fd <n>          Read the run credential from descriptor <n>; keeps it out of every environment and file"

// topLevelHelp returns the top-level help the CLI prints today, which the
// surface golden pins, with the run credential's flag line replaced by what
// line returns for it. An empty replacement drops the line.
func topLevelHelp(t *testing.T, line func(current string) string) string {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "surface", "help-top-level.golden"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, current := range strings.Split(string(golden), "\n") {
		if strings.HasPrefix(strings.TrimSpace(current), runcredential.Flag+" ") {
			if current = line(current); current == "" {
				continue
			}
		}
		kept = append(kept, current)
	}
	return strings.Join(kept, "\n")
}

// The flag lines topLevelHelp writes: today's line, no line, the line of an
// older build that advertises no level, and today's line at another level.
func currentLine(line string) string { return line }
func withoutFlag(string) string      { return "" }
func withoutLevel(string) string     { return credentialLineWithoutLevel }
func atLevel(level int) func(string) string {
	return func(line string) string {
		return strings.Replace(line, "custody level "+strconv.Itoa(runcredential.CustodyLevel),
			"custody level "+strconv.Itoa(level), 1)
	}
}

// The probe reads the help surface the CLI ships: the golden lists the flag
// as the first word of a line, which advertises this CLI's custody level.
func TestCredentialProbeReadsTheHelpListing(t *testing.T) {
	t.Parallel()
	level, listed := runcredential.AdvertisedCustodyLevel([]byte(topLevelHelp(t, currentLine)))
	if !listed || level != runcredential.CustodyLevel {
		t.Fatalf("the top-level help golden advertises custody level %d (flag listed %t), want %d",
			level, listed, runcredential.CustodyLevel)
	}
	if _, listed := runcredential.AdvertisedCustodyLevel([]byte(topLevelHelp(t, withoutFlag))); listed {
		t.Fatal("help without the flag line still lists --credential-fd")
	}
	if level, listed := runcredential.AdvertisedCustodyLevel([]byte(topLevelHelp(t, withoutLevel))); !listed || level != 0 {
		t.Fatalf("a help line without a level advertises custody level %d (flag listed %t), want 0 and listed", level, listed)
	}
}

// A hosted relaunch probes the pinned CLI before it hands it
// the run credential, and refuses a CLI below this CLI's custody level with
// exit 2 and ErrPinBelowCustodyLevel: one whose help does not list the flag,
// lists it with no level, as an older build does, or advertises a lower
// level. It accepts a CLI at this level or above. The probe runs the pinned
// CLI in an empty directory with only PATH, HOME and the two opt-outs, and no
// descriptor beyond the standard streams. An unhosted relaunch never probes.
// The bootstrap provider serves only the resolve and is closed before the
// exec.
func TestHostedRelaunchProbesThePinnedCLI(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "older-pin-is-refused")
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bootstrap-provider-serves-only-the-locked-downloads")
	for _, name := range probeLeakNames {
		t.Setenv(name, probeLeakValue)
	}
	t.Setenv("HOME", t.TempDir())
	inherited := inheritedDescriptors(t)
	const refusedNext = "putnami pin <version>"
	for _, c := range []struct {
		name     string
		hosted   bool
		help     string
		code     int
		refused  bool
		below    bool
		wantExit int
		wantNext string
		wantText string
	}{
		{name: "hosted, pin without the flag", hosted: true, help: topLevelHelp(t, withoutFlag), refused: true, below: true,
			wantExit: protocolcli.ExitUsage, wantNext: refusedNext,
			wantText: `pins CLI "1.4.2", which does not accept --credential-fd`},
		{name: "hosted, pin that advertises no level", hosted: true, help: topLevelHelp(t, withoutLevel), refused: true, below: true,
			wantExit: protocolcli.ExitUsage, wantNext: refusedNext,
			wantText: `pins CLI "1.4.2", which advertises no custody level`},
		{name: "hosted, pin at a lower level", hosted: true, help: topLevelHelp(t, atLevel(runcredential.CustodyLevel-1)), refused: true, below: true,
			wantExit: protocolcli.ExitUsage, wantNext: refusedNext,
			wantText: fmt.Sprintf(`which advertises custody level %d; a hosted run hands the run credential only to a CLI at custody level %d or above`,
				runcredential.CustodyLevel-1, runcredential.CustodyLevel)},
		{name: "hosted, pin at this CLI's level", hosted: true, help: topLevelHelp(t, currentLine)},
		{name: "hosted, pin at a higher level", hosted: true, help: topLevelHelp(t, atLevel(runcredential.CustodyLevel+1))},
		{name: "hosted, help fails", hosted: true, help: topLevelHelp(t, currentLine), code: 3, refused: true,
			wantExit: protocolcli.ExitFailure, wantNext: "PUTNAMI_NO_RELAUNCH=1 putnami build --all",
			wantText: `could not check that pinned CLI "1.4.2"`},
		{name: "not hosted, pin that advertises no level", help: topLevelHelp(t, withoutLevel)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			writeCLILock(t, ws, "linux", "amd64", testSHA)
			pinned, record := fakePinnedCLI(t, c.help, c.code)
			cap := &capture{}
			var events []string
			cfg := baseConfig(cap, filepath.Join(t.TempDir(), "self"))
			cfg.hosted = c.hosted
			cfg.probe = credentialProbe(os.Getenv)
			cfg.serve = func() func() {
				events = append(events, "serve")
				return func() { events = append(events, "restore") }
			}
			cfg.beforeExec = func() { events = append(events, "close") }
			cfg.resolve = func(context.Context, *lockfile.LockEntry, string, string) (string, error) {
				events = append(events, "resolve")
				return pinned, nil
			}
			capturedExec := cfg.exec
			cfg.exec = func(path string, argv, env []string) error {
				events = append(events, "exec")
				return capturedExec(path, argv, env)
			}

			if c.refused {
				err := mustFailClosed(t, ws, cfg, cap, c.wantExit, c.wantNext)
				if !strings.Contains(err.Error(), c.wantText) {
					t.Errorf("error = %q, want %q", err, c.wantText)
				}
				if got := errors.Is(err, ErrPinBelowCustodyLevel); got != c.below {
					t.Errorf("errors.Is(%q, ErrPinBelowCustodyLevel) = %t, want %t", err, got, c.below)
				}
				if want := []string{"serve", "resolve", "restore"}; !slices.Equal(events, want) {
					t.Errorf("events = %v, want %v", events, want)
				}
			} else {
				launched, err := relaunch(context.Background(), ws, cfg)
				if err != nil || !launched || cap.calls != 1 || cap.path != pinned {
					t.Fatalf("relaunch = %v, %v; exec calls %d of %q, want one exec of %q", launched, err, cap.calls, cap.path, pinned)
				}
				if want := []string{"serve", "resolve", "restore", "close", "exec"}; !slices.Equal(events, want) {
					t.Errorf("events = %v, want %v", events, want)
				}
			}

			raw, err := os.ReadFile(record)
			if !c.hosted {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("an unhosted relaunch ran the pinned CLI to probe it: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the hosted relaunch did not probe the pinned CLI: %v", err)
			}
			checkProbeRecord(t, string(raw), inherited)
		})
	}
}

// openDescriptors returns the open-fd lines of a record.
func openDescriptors(record string) []string {
	var fds []string
	for _, line := range strings.Split(record, "\n") {
		if fd, ok := strings.CutPrefix(line, "open-fd="); ok {
			fds = append(fds, fd)
		}
	}
	return fds
}

// inheritedDescriptors returns the descriptors from 3 to 9 that any child of
// this process inherits: the ones this process itself inherited without
// close-on-exec, such as a descriptor the test runner holds open.
func inheritedDescriptors(t *testing.T) []string {
	t.Helper()
	path, record := fakePinnedCLI(t, "", 0)
	if out, err := exec.Command(path).CombinedOutput(); err != nil {
		t.Fatalf("run a plain child: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	return openDescriptors(string(raw))
}

// checkProbeRecord asserts what the probed CLI received. inherited are the
// descriptors every child of this process inherits; the probe adds none.
func checkProbeRecord(t *testing.T, record string, inherited []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(record), "\n")
	if lines[0] != "args=--help" {
		t.Errorf("the probe ran %q, want args=--help", lines[0])
	}
	for _, want := range []string{"PUTNAMI_NO_RELAUNCH=1", "PUTNAMI_NO_AUTO_INSTALL=1", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")} {
		if !slices.Contains(lines, want) {
			t.Errorf("the probed CLI's environment lacks %s:\n%s", want, record)
		}
	}
	if strings.Contains(record, probeLeakValue) {
		t.Errorf("the probed CLI saw a variable of this run:\n%s", record)
	}
	if got := openDescriptors(record); !slices.Equal(got, inherited) {
		t.Errorf("the probed CLI holds descriptors %v, want only the %v every child of this process inherits", got, inherited)
	}
	for _, line := range lines {
		if dir, ok := strings.CutPrefix(line, "pwd="); ok {
			if wd, _ := os.Getwd(); sameResolvedPath(dir, wd) {
				t.Errorf("the probe ran in this process's directory %s", dir)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the probe's directory %s outlived it: %v", dir, err)
			}
		}
	}
}
