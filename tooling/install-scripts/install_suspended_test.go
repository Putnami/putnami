package installscript

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// suspendedNotice is the reason install-suspended.ps1 fails with.
const suspendedNotice = "Putnami does not support Windows for now"

func installSuspendedPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(installScriptPath(t)), "install-suspended.ps1")
}

// psRequest matches a PowerShell command or a program that reaches the network
// or starts a process.
var psRequest = regexp.MustCompile(`(?i)(^|[^\w.-])(invoke-webrequest|invoke-restmethod|irm|iwr|curl(\.exe)?|wget|start-process|tar(\.exe)?)([^\w.-]|$)`)

// While Windows support is suspended, the site serves install-suspended.ps1 at
// /install.ps1: an ASCII script that requests nothing, installs nothing, keeps
// the one placeholder line the site bakes ?run= into, and fails with the
// reason.
func TestTheSiteServesAWindowsNoticeThatInstallsNothing(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-is-suspended", "the-site-serves-a-notice-that-installs-nothing")

	src := string(readFile(t, installSuspendedPath(t)))
	for i := 0; i < len(src); i++ {
		if src[i] >= 0x80 {
			t.Fatalf("install-suspended.ps1 holds a non-ASCII byte at %d: Windows PowerShell 5.1 reads it in the ANSI code page", i)
		}
	}
	if got := strings.Count(src, "\n"+ps1RunPlaceholderLine+"\n"); got != 1 {
		t.Errorf("install-suspended.ps1 holds %d placeholder lines %q, want 1", got, ps1RunPlaceholderLine)
	}
	var code []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		code = append(code, regexp.MustCompile(`'[^']*'`).ReplaceAllString(line, "''"))
	}
	if len(code) != 2 || code[0] != ps1RunPlaceholderLine || code[1] != "throw ''" {
		t.Errorf("install-suspended.ps1 code = %q, want the placeholder line and one throw", code)
	}
	if psRequest.MatchString(strings.Join(code, "\n")) {
		t.Errorf("install-suspended.ps1 requests or starts something: %q", code)
	}
	for _, want := range []string{suspendedNotice, "no Windows build is published", "https://putnami.dev/install.sh"} {
		if !strings.Contains(src, want) {
			t.Errorf("install-suspended.ps1 does not say %q", want)
		}
	}

	// The site copies this script, not the installer, to /install.ps1.
	var site struct {
		Options struct {
			Generate struct {
				Assets []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"assets"`
			} `json:"generate"`
		} `json:"options"`
	}
	sitePath := filepath.Join(filepath.Dir(installScriptPath(t)), "..", "..", "..", "sites", "putnami.dev", "putnami.json")
	if err := json.Unmarshal(readFile(t, sitePath), &site); err != nil {
		t.Fatal(err)
	}
	var served []string
	for _, asset := range site.Options.Generate.Assets {
		if asset.To == "public/install.ps1" {
			served = append(served, asset.From)
		}
	}
	if len(served) != 1 || served[0] != "/tooling/cli/scripts/install-suspended.ps1" {
		t.Errorf("the site serves %v at /install.ps1, want /tooling/cli/scripts/install-suspended.ps1", served)
	}

	// Under PowerShell, where it is installed, the script fails with the
	// reason, whatever the run value, and writes nothing.
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		return
	}
	dir := t.TempDir()
	baked := strings.Replace(src, ps1RunPlaceholderLine, "$RunCommandDefault = 'deploy'", 1)
	script := filepath.Join(dir, "install.ps1")
	if err := os.WriteFile(script, []byte(baked), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install-suspended.ps1 succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), suspendedNotice) {
		t.Errorf("install-suspended.ps1 output does not say %q:\n%s", suspendedNotice, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("install-suspended.ps1 wrote into its directory: %v, %v", entries, err)
	}
}
