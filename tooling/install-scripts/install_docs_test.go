package installscript

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// oneLinerOnACommandLine matches a PowerShell command line that carries the
// install one-liner, such as powershell -c "irm <url> | iex". Microsoft
// Defender blocks that command line as Trojan:Win32/Commando.A!ml.
var oneLinerOnACommandLine = regexp.MustCompile(`(?i)\bpowershell(\.exe)?\s[^\n]*-c(ommand)?\s+["']?[^\n]*\birm\b`)

// fencedLines returns the lines of every fenced code block in a markdown
// document: the commands a reader copies.
func fencedLines(doc string) []string {
	var lines []string
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			lines = append(lines, line)
		}
	}
	return lines
}

// The install documents give a Windows user commands Defender lets run: the
// one-liner typed in a PowerShell window, and for cmd.exe a download run with
// -File. No copyable command passes the one-liner to powershell.
func TestInstallDocsGiveWindowsCommandsDefenderLetsRun(t *testing.T) {
	docDir := filepath.Join(filepath.Dir(installScriptPath(t)), "..", "doc")
	for _, name := range []string{"22-installing-the-cli.md", "14-version-management.md"} {
		data, err := os.ReadFile(filepath.Join(docDir, name))
		if err != nil {
			t.Fatal(err)
		}
		lines := fencedLines(string(data))
		for _, line := range lines {
			if oneLinerOnACommandLine.MatchString(line) {
				t.Errorf("%s gives a command Defender blocks: %s", name, line)
			}
		}
		if name != "22-installing-the-cli.md" {
			continue
		}
		code := strings.Join(lines, "\n")
		for _, want := range []string{
			"irm https://putnami.dev/install.ps1 | iex",
			"curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1",
			"powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1",
		} {
			if !strings.Contains(code, want) {
				t.Errorf("%s has no command block with %q", name, want)
			}
		}
	}
}

// The pattern catches every spelling the docs used, and lets the forms Defender
// lets run through.
func TestOneLinerOnACommandLineMatchesTheBlockedForms(t *testing.T) {
	for _, blocked := range []string{
		`powershell -ExecutionPolicy Bypass -c "irm https://putnami.dev/install.ps1 | iex"`,
		`powershell.exe -Command "irm https://putnami.dev/install.ps1 | iex"`,
		`cmd /c powershell -NoProfile -command 'irm https://putnami.dev/install.ps1 | iex'`,
	} {
		if !oneLinerOnACommandLine.MatchString(blocked) {
			t.Errorf("pattern misses %s", blocked)
		}
	}
	for _, allowed := range []string{
		"irm https://putnami.dev/install.ps1 | iex",
		"$env:PUTNAMI_VERSION = '1.2.0'; irm https://putnami.dev/install.ps1 | iex",
		"curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1",
		"powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1",
	} {
		if oneLinerOnACommandLine.MatchString(allowed) {
			t.Errorf("pattern matches %s", allowed)
		}
	}
}
