package installscript

// Evidence for scripts/install.ps1, the Windows installer served at
// https://putnami.dev/install.ps1. It keeps the trust model of install.sh.
//
// The static tests read the script and run on every host. The behavior tests
// run the real script under PowerShell 7 (pwsh) when it is installed, and skip
// otherwise: a harness loads the script's own statements, replaces the few
// functions that read Windows itself (the platform, tar.exe, the registry, the
// WM_SETTINGCHANGE broadcast and the byte-range lock), and runs the script's
// entry point against an httptest registry. install_ps1_windows_test.go runs
// the script unmodified, through Windows PowerShell 5.1, on a Windows host.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// installPS1Path returns the Windows installer under test. It is the same file
// the site publishes at /install.ps1 (sites/putnami.dev/putnami.json copies it).
func installPS1Path(t *testing.T) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(installScriptPath(t)), "install.ps1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("install.ps1 not found: %v", err)
	}
	return path
}

func readInstallPS1(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(installPS1Path(t))
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may carry CRLF line endings; PowerShell reads both.
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// --- A small PowerShell source reader for the static tests -----------------

// psSkipSingle returns the index after the single-quoted string at i.
func psSkipSingle(src string, i int) (int, error) {
	for j := i + 1; j < len(src); j++ {
		if src[j] == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return -1, fmt.Errorf("unterminated single-quoted string at %d", i)
}

// psSkipDouble returns the index after the double-quoted string at i. A $( )
// subexpression inside it is code, which may hold its own strings.
func psSkipDouble(src string, i int) (int, error) {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '`':
			j++
		case '"':
			if j+1 < len(src) && src[j+1] == '"' {
				j++
				continue
			}
			return j + 1, nil
		case '$':
			if j+1 < len(src) && src[j+1] == '(' {
				end, err := psMatch(src, j+1)
				if err != nil {
					return -1, err
				}
				j = end
			}
		}
	}
	return -1, fmt.Errorf("unterminated double-quoted string at %d", i)
}

// psSkipComment returns the index after the comment at i, or i when none
// starts there.
func psSkipComment(src string, i int) (int, error) {
	if strings.HasPrefix(src[i:], "<#") {
		end := strings.Index(src[i+2:], "#>")
		if end < 0 {
			return -1, fmt.Errorf("unterminated block comment at %d", i)
		}
		return i + 2 + end + 2, nil
	}
	if src[i] == '#' {
		end := strings.IndexByte(src[i:], '\n')
		if end < 0 {
			return len(src), nil
		}
		return i + end, nil
	}
	return i, nil
}

// psMatch returns the index of the bracket that closes the one at open,
// skipping comments, strings and escaped characters.
func psMatch(src string, open int) (int, error) {
	closer := map[byte]byte{'{': '}', '(': ')', '[': ']'}
	var stack []byte
	for i := open; i < len(src); {
		if next, err := psSkipComment(src, i); err != nil {
			return -1, err
		} else if next != i {
			i = next
			continue
		}
		switch c := src[i]; c {
		case '`':
			i += 2
			continue
		case '\'':
			next, err := psSkipSingle(src, i)
			if err != nil {
				return -1, err
			}
			i = next
			continue
		case '"':
			next, err := psSkipDouble(src, i)
			if err != nil {
				return -1, err
			}
			i = next
			continue
		case '{', '(', '[':
			stack = append(stack, closer[c])
		case '}', ')', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return -1, fmt.Errorf("unbalanced %q at %d", c, i)
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i, nil
			}
		}
		i++
	}
	return -1, fmt.Errorf("bracket at %d is never closed", open)
}

// psCode returns src with comments removed and every string reduced to its
// quotes, leaving what PowerShell runs as commands.
func psCode(src string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(src); {
		if next, err := psSkipComment(src, i); err != nil {
			return "", err
		} else if next != i {
			i = next
			continue
		}
		switch src[i] {
		case '`':
			end := min(i+2, len(src))
			out.WriteString(src[i:end])
			i = end
		case '\'':
			next, err := psSkipSingle(src, i)
			if err != nil {
				return "", err
			}
			out.WriteString("''")
			i = next
		case '"':
			next, err := psSkipDouble(src, i)
			if err != nil {
				return "", err
			}
			out.WriteString(`""`)
			i = next
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String(), nil
}

// psBlockAt returns the content of the brace block that opens at or after i.
func psBlockAt(src string, i int) (string, error) {
	open := strings.IndexByte(src[i:], '{')
	if open < 0 {
		return "", fmt.Errorf("no block after %d", i)
	}
	open += i
	end, err := psMatch(src, open)
	if err != nil {
		return "", err
	}
	return src[open+1 : end], nil
}

// psFunctionBody returns the body of the function called name.
func psFunctionBody(src, name string) (string, error) {
	loc := regexp.MustCompile(`(?m)^function ` + regexp.QuoteMeta(name) + `\b`).FindStringIndex(src)
	if loc == nil {
		return "", fmt.Errorf("function %s is missing", name)
	}
	i := loc[1]
	for i < len(src) && src[i] != '{' && src[i] != '(' {
		i++
	}
	if i < len(src) && src[i] == '(' {
		end, err := psMatch(src, i)
		if err != nil {
			return "", err
		}
		i = end + 1
	}
	return psBlockAt(src, i)
}

// --- Static evidence --------------------------------------------------------

// Windows PowerShell 5.1 reads a script without a byte order mark, and an
// `irm` response without a charset, in the ANSI code page. A UTF-8 dash or
// quote then becomes other characters, some of which PowerShell parses as
// syntax. ASCII reads the same in every code page.
func TestInstallPS1IsPlainASCII(t *testing.T) {
	src := readInstallPS1(t)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c > 0x7e || (c < 0x20 && c != '\n' && c != '\r' && c != '\t') {
			line := strings.Count(src[:i], "\n") + 1
			t.Fatalf("install.ps1:%d has byte 0x%02x; the script must be ASCII", line, c)
		}
	}
}

// The script is one script block called with the script's arguments, so that
// `irm | iex` defines nothing in the caller's session and StrictMode and
// $ErrorActionPreference apply to every statement. `exit` would close the
// window of a user who ran the one-liner, so a failure throws instead, and the
// one exit (checkPS1ExitsOnlyFromAFile) ends a run from a file.
func TestInstallPS1RunsInOneScopedBlock(t *testing.T) {
	src := readInstallPS1(t)
	code, err := psCode(src)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(code)
	if !strings.HasPrefix(trimmed, "& {") || !strings.HasSuffix(trimmed, "} @args") {
		t.Fatalf("install.ps1 code must be exactly `& { ... } @args`; it starts %q and ends %q", trimmed[:min(20, len(trimmed))], trimmed[max(0, len(trimmed)-20):])
	}
	open := strings.Index(src, "& {")
	end, err := psMatch(src, open+2)
	if err != nil {
		t.Fatal(err)
	}
	if rest := strings.TrimSpace(src[end+1:]); rest != "@args" {
		t.Fatalf("the script block is followed by %q, not by @args alone", rest)
	}
	body := strings.TrimSpace(src[open+3 : end])
	if !strings.HasPrefix(body, "Set-StrictMode -Version 3.0\n$ErrorActionPreference = 'Stop'\n") {
		t.Fatalf("the block must start with Set-StrictMode and $ErrorActionPreference = 'Stop':\n%s", body[:min(200, len(body))])
	}
	if !strings.HasSuffix(body, "\nInvoke-PutnamiInstaller ([string[]]$args)") {
		t.Fatal("the block must end with the entry point, Invoke-PutnamiInstaller ([string[]]$args)")
	}
	if err := checkPS1ExitsOnlyFromAFile(src); err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`@['"]\r?\n`).MatchString(src) {
		t.Fatal("install.ps1 uses a here-string, which the static reader in this file does not parse")
	}
}

// checkPS1VerifiesBeforeInstalling reports how src could install a download
// that nobody verified, or nil when it cannot.
func checkPS1VerifiesBeforeInstalling(src string) error {
	main, err := psFunctionBody(src, "Invoke-PutnamiInstaller")
	if err != nil {
		return err
	}
	guard := regexp.MustCompile(`if \(-not \(Confirm-DownloadIntegrity \$assetFile [^\n]*\)\) \{`)
	guards := guard.FindAllStringIndex(main, -1)
	if len(guards) != 1 || strings.Count(main, "Confirm-DownloadIntegrity") != 1 {
		return errors.New("Invoke-PutnamiInstaller must call Confirm-DownloadIntegrity once, as the condition of a refusal")
	}
	guardBlock, err := psBlockAt(main, guards[0][1]-1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(guardBlock) != "Stop-Install" {
		return errors.New("a failed integrity check does not stop the install")
	}
	verifiedAt := guards[0][0]
	if at := strings.Index(main, "Save-PutnamiDownload "); at < 0 || at > verifiedAt {
		return errors.New("the download is not verified after it is saved")
	}
	for _, later := range []string{"Expand-PutnamiAsset ", "Confirm-BinaryStamp ", "Install-VersionedBinary ", "Install-PlainBinary ", "Complete-PutnamiInstall "} {
		at := strings.Index(main, later)
		if at < 0 {
			return fmt.Errorf("Invoke-PutnamiInstaller no longer calls %s", strings.TrimSpace(later))
		}
		if at < verifiedAt {
			return fmt.Errorf("%s runs before the download is verified", strings.TrimSpace(later))
		}
	}

	verify, err := psFunctionBody(src, "Confirm-DownloadIntegrity")
	if err != nil {
		return err
	}
	if !strings.Contains(verify, "$actual = Get-FileSha256 $AssetFile") {
		return errors.New("Confirm-DownloadIntegrity does not hash the downloaded file")
	}
	const compare = "if ($actual -cne $expected) {"
	compareAt := strings.Index(verify, compare)
	if compareAt < 0 {
		return errors.New("Confirm-DownloadIntegrity does not compare the hash with the expected digest")
	}
	mismatch, err := psBlockAt(verify, compareAt+len(compare)-1)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(mismatch), "return $false") {
		return errors.New("a hash mismatch does not fail the check")
	}
	if at := strings.Index(verify, `Write-Success "Integrity verified`); at < compareAt {
		return errors.New("Confirm-DownloadIntegrity reports verification before it compares")
	}
	missingAt := strings.Index(verify, "if (-not $expected) {\n")
	if missingAt < 0 {
		return errors.New("Confirm-DownloadIntegrity does not handle a missing digest")
	}
	noDigest, err := psBlockAt(verify, missingAt)
	if err != nil {
		return err
	}
	refuse := strings.Index(noDigest, "if (-not $unsafe) {")
	if refuse < 0 {
		return errors.New("a missing digest is accepted without PUTNAMI_UNSAFE_INSTALL=1")
	}
	refusal, err := psBlockAt(noDigest, refuse)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(refusal), "return $false") {
		return errors.New("a missing digest does not fail the check")
	}

	hash, err := psFunctionBody(src, "Get-FileSha256")
	if err != nil {
		return err
	}
	for _, want := range []string{"[Security.Cryptography.SHA256]::Create()", "$stream = [IO.File]::OpenRead($Path)", "$hash = $algorithm.ComputeHash($stream)"} {
		if !strings.Contains(hash, want) {
			return fmt.Errorf("Get-FileSha256 no longer computes a SHA-256 of the file (%s)", want)
		}
	}
	return nil
}

// A download is hashed and compared with the digest the registry advertised,
// and a mismatch or a missing digest stops the install, before the download is
// extracted, run or installed. Each mutation below removes one part of that,
// and each must be caught.
func TestInstallPS1VerifiesTheDownloadBeforeUsingIt(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-trust", "the-download-is-verified-before-it-is-used")
	src := readInstallPS1(t)
	if err := checkPS1VerifiesBeforeInstalling(src); err != nil {
		t.Fatalf("install.ps1 can install an unverified download: %v", err)
	}

	guard := "if (-not (Confirm-DownloadIntegrity $assetFile $advertisedIntegrity $expectedSha256 $sourceLabel)) {\n            Stop-Install\n        }\n"
	extract := "        $binarySource = Expand-PutnamiAsset $assetFile $tempDir\n"
	mutations := []struct {
		name     string
		old, new string
	}{
		{"the hash is never computed", "$actual = Get-FileSha256 $AssetFile", "$actual = $expected"},
		{"the hash is never compared", "if ($actual -cne $expected) {", "if ($false) {"},
		{"a mismatch passes", "Nothing was installed.\"\n        return $false", "Nothing was installed.\"\n        return $true"},
		{"the result is ignored", guard, "$null = Confirm-DownloadIntegrity $assetFile $advertisedIntegrity $expectedSha256 $sourceLabel\n"},
		{"the check is removed", guard, ""},
		{"the check does not stop the install", guard, strings.Replace(guard, "Stop-Install", "Write-Info 'unverified'", 1)},
		{"the download is extracted first", guard + "\n" + extract, extract + "\n        " + guard},
		{"a missing digest passes", "if (-not $unsafe) {", "if ($false) {"},
		{"the hash covers no bytes", "$hash = $algorithm.ComputeHash($stream)", "$hash = $algorithm.ComputeHash([byte[]]@())"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: install.ps1 does not contain exactly one %q", m.old)
			}
			if err := checkPS1VerifiesBeforeInstalling(strings.Replace(src, m.old, m.new, 1)); err == nil {
				t.Fatal("the check accepted a script that installs an unverified download")
			}
		})
	}
}

// checkPS1RefusesBeforeDownloading reports how src could start a download on a
// host, an input or an install directory it refuses, or install a binary whose
// version stamp it refuses, or nil when it cannot.
func checkPS1RefusesBeforeDownloading(src string) error {
	main, err := psFunctionBody(src, "Invoke-PutnamiInstaller")
	if err != nil {
		return err
	}
	downloadAt := strings.Index(main, "$download = Save-PutnamiDownload ")
	if downloadAt < 0 || strings.Count(main, "Save-PutnamiDownload") != 1 {
		return errors.New("Invoke-PutnamiInstaller must download once, through Save-PutnamiDownload")
	}
	for _, refusal := range []string{
		"\n    Assert-SupportedPlatform (Get-PutnamiPlatform)\n",
		"\n    Assert-Prerequisites\n",
		"\n    if (-not $valid) { Stop-Install }\n",
		"\n    Assert-WritableInstallDir $installDir\n",
	} {
		at := strings.Index(main, refusal)
		if at < 0 {
			return fmt.Errorf("Invoke-PutnamiInstaller no longer runs %q unconditionally", strings.TrimSpace(refusal))
		}
		if at > downloadAt {
			return fmt.Errorf("%q runs after the download starts", strings.TrimSpace(refusal))
		}
	}
	const stamp = "if (-not (Confirm-BinaryStamp $binarySource $resolvedVersion)) {"
	stampAt := strings.Index(main, stamp)
	if stampAt < 0 || strings.Count(main, "Confirm-BinaryStamp") != 1 {
		return errors.New("Invoke-PutnamiInstaller must check the version stamp once, as the condition of a refusal")
	}
	refusal, err := psBlockAt(main, stampAt+len(stamp)-1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(refusal) != "Stop-Install" {
		return errors.New("a refused version stamp does not stop the install")
	}
	for _, install := range []string{"Install-VersionedBinary ", "Install-PlainBinary "} {
		if at := strings.Index(main, install); at < stampAt {
			return fmt.Errorf("%s runs before the version stamp is checked", strings.TrimSpace(install))
		}
	}
	return nil
}

// A refused host, input or install directory stops the script before any
// request, and a refused version stamp before anything is installed. Each
// mutation below removes one part of that, and each must be caught.
func TestInstallPS1RefusesBeforeDownloading(t *testing.T) {
	src := readInstallPS1(t)
	if err := checkPS1RefusesBeforeDownloading(src); err != nil {
		t.Fatalf("install.ps1 refuses too late: %v", err)
	}
	download := "        $download = Save-PutnamiDownload $downloadTarget $assetFile $MaxDownloadBytes\n"
	mutations := []struct{ name, old, new string }{
		{"the platform is checked after the download", "    Assert-SupportedPlatform (Get-PutnamiPlatform)\n", ""},
		{"the prerequisites are never checked", "    Assert-Prerequisites\n", ""},
		{"a refused URL is downloaded", "    if (-not $valid) { Stop-Install }\n", "    if (-not $valid) { Write-Info 'insecure' }\n"},
		{"the install directory is checked after the download", "    Assert-WritableInstallDir $installDir\n", ""},
		{"a refused stamp is installed", "        if (-not (Confirm-BinaryStamp $binarySource $resolvedVersion)) {\n            Stop-Install\n        }\n", "        $null = Confirm-BinaryStamp $binarySource $resolvedVersion\n"},
		{"a second download", download, download + download},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: install.ps1 does not contain exactly one %q", m.old)
			}
			mutated := strings.Replace(src, m.old, m.new, 1)
			if m.new == "" && strings.Contains(m.old, "Assert-") {
				// Moving the check after the download is the same finding as dropping it.
				mutated = strings.Replace(mutated, download, download+m.old, 1)
			}
			if err := checkPS1RefusesBeforeDownloading(mutated); err == nil {
				t.Fatal("the check accepted a script that refuses too late")
			}
		})
	}
}

// A one-line installer that prompts for elevation is one a user cannot audit
// before it runs. The script writes only the user's own registry hive, through
// one value, and broadcasts through the .NET user-scope write.
func TestInstallPS1NeverEscalatesPrivileges(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-trust", "the-installer-never-elevates")
	src := readInstallPS1(t)
	code, err := psCode(src)
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(code)
	for _, banned := range []string{"runas", "sudo", "gsudo", "start-process", "set-executionpolicy", "set-itemproperty", "new-itemproperty", "set-acl"} {
		if containsWord(lowered, banned) {
			t.Fatalf("install.ps1 runs %s", banned)
		}
	}
	for _, banned := range []string{"hklm:", "-verb"} {
		if strings.Contains(lowered, banned) {
			t.Fatalf("install.ps1 uses %s", banned)
		}
	}
	for needle, want := range map[string]int{
		".SetValue(":                 1,
		"CreateSubKey(":              1,
		"SetEnvironmentVariable(":    1,
		"LocalMachine":               1,
		"CurrentUser.CreateSubKey(":  1,
		"LocalMachine.OpenSubKey(":   1,
		"'Environment', $false)":     1,
		"'User')":                    1,
		"SetEnvironmentVariable('Pa": 0,
	} {
		if got := strings.Count(src, needle); got != want {
			t.Fatalf("install.ps1 contains %q %d times, want %d", needle, got, want)
		}
	}
	setter, err := psFunctionBody(src, "Set-UserPathValue")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(setter, "[Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')") || !strings.Contains(setter, "$key.SetValue('Path', $Value,") {
		t.Fatal("the only registry write must be the user's Path value in HKCU\\Environment")
	}
	machine, err := psFunctionBody(src, "Get-MachinePathValue")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine, "OpenSubKey('SYSTEM\\CurrentControlSet\\Control\\Session Manager\\Environment', $false)") {
		t.Fatal("the machine Path must be opened read-only")
	}
}

// Every reference points at putnami.dev/install.ps1; put.putnami.dev is the
// artifact registry and serves no script.
func TestInstallPS1AdvertisesTheServedURL(t *testing.T) {
	src := readInstallPS1(t)
	if strings.Contains(src, "put.putnami.dev/install") {
		t.Fatal("install.ps1 advertises a script URL on put.putnami.dev, which serves no script")
	}
	// Microsoft Defender blocks the one-liner passed to powershell on a
	// command line, so the script never advertises that form.
	if strings.Contains(src, `-c "irm`) || strings.Contains(src, `-Command "irm`) {
		t.Fatal("install.ps1 advertises the one-liner on a powershell command line, which Defender blocks")
	}
	for _, want := range []string{
		"# Usage, in a PowerShell window: irm https://putnami.dev/install.ps1 | iex",
		"curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1 && powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1",
		"#   & ([scriptblock]::Create((irm https://putnami.dev/install.ps1))) --version canary",
		`Write-OutLine "       & ([scriptblock]::Create((irm $InstallScriptUrl))) [options]" ''`,
		"$InstallScriptUrl = 'https://putnami.dev/install.ps1'",
		"$UnixInstallScriptUrl = 'https://putnami.dev/install.sh'",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("install.ps1 does not contain %q", want)
		}
	}
}

// checkPS1GuardsEveryRequest reports how src could make a request that skips
// the transport rules, or nil when it cannot. Every request goes through
// Save-PutnamiDownload, which asks for TLS 1.2 or later and clears any
// certificate callback the caller's session set, and puts both back afterwards.
// Redirects are followed by hand, each target passes the registry URL rule,
// credentials go only to the origin of the URL, and the integrity headers come
// from the response that carried the body.
func checkPS1GuardsEveryRequest(src string) error {
	code, err := psCode(src)
	if err != nil {
		return err
	}
	lowered := strings.ToLower(code)
	for _, client := range []string{"invoke-webrequest", "invoke-restmethod", "iwr", "irm", "curl", "wget"} {
		if containsWord(lowered, client) {
			return fmt.Errorf("install.ps1 makes a request through %s, outside Save-PutnamiDownload", client)
		}
	}
	for _, client := range []string{"webclient", "httpclient", "bitstransfer", "skipcertificatecheck"} {
		if strings.Contains(lowered, client) {
			return fmt.Errorf("install.ps1 uses %s, outside Save-PutnamiDownload", client)
		}
	}
	if got := strings.Count(code, "[Net.WebRequest]::Create("); got != 1 {
		return fmt.Errorf("install.ps1 creates %d web requests, want the one in Receive-PutnamiDownload", got)
	}
	if got := strings.Count(code, "Receive-PutnamiDownload"); got != 2 {
		return fmt.Errorf("Receive-PutnamiDownload appears %d times, want its definition and one call from Save-PutnamiDownload", got)
	}
	if got := strings.Count(code, "::SecurityProtocol ="); got != 2 {
		return fmt.Errorf("install.ps1 sets SecurityProtocol %d times, want the floor and the restore in Save-PutnamiDownload", got)
	}
	if got := strings.Count(code, "ServerCertificateValidationCallback"); got != 3 {
		return fmt.Errorf("install.ps1 names ServerCertificateValidationCallback %d times, want the save, the clear and the restore in Save-PutnamiDownload", got)
	}

	save, err := psFunctionBody(src, "Save-PutnamiDownload")
	if err != nil {
		return err
	}
	for _, want := range []string{
		"\n    $previousProtocols = [Net.ServicePointManager]::SecurityProtocol\n    $previousCallback = [Net.ServicePointManager]::ServerCertificateValidationCallback\n    try {\n",
		"\n        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType](ConvertTo-TlsFloor ([int]$previousProtocols) (Test-Tls13ByDefault))\n" +
			"        [Net.ServicePointManager]::ServerCertificateValidationCallback = $null\n" +
			"        return Receive-PutnamiDownload $current $OutFile $MaxBytes\n" +
			"    } finally {\n" +
			"        [Net.ServicePointManager]::SecurityProtocol = $previousProtocols\n" +
			"        [Net.ServicePointManager]::ServerCertificateValidationCallback = $previousCallback\n" +
			"    }\n",
	} {
		if !strings.Contains(save, want) {
			return fmt.Errorf("Save-PutnamiDownload does not set the TLS floor and clear the certificate callback for the request, then restore both:\n%s", want)
		}
	}

	receive, err := psFunctionBody(src, "Receive-PutnamiDownload")
	if err != nil {
		return err
	}
	if !strings.Contains(receive, "[Net.WebRequest]::Create(") {
		return errors.New("the request is not created in Receive-PutnamiDownload")
	}
	if !strings.Contains(receive, "\n        $request.AllowAutoRedirect = $false\n") {
		return errors.New("the transport follows redirects without the registry URL rule")
	}
	const redirectRule = "if (-not (Test-RegistryUrl $next.AbsoluteUri 'redirect target')) {"
	ruleAt := strings.Index(receive, redirectRule)
	if ruleAt < 0 {
		return errors.New("a redirect target is not checked against the registry URL rule")
	}
	refusal, err := psBlockAt(receive, ruleAt+len(redirectRule)-1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(refusal) != "return @{ Error = 'refused redirect'; Headers = @{} }" {
		return errors.New("a refused redirect target does not end the download")
	}
	if followAt := strings.Index(receive, "$current = $next"); followAt < ruleAt {
		return errors.New("a redirect is followed before its target is checked")
	}
	if got := strings.Count(receive, "$origin ="); got != 1 || !strings.Contains(receive, "\n    $origin = $current\n") {
		return errors.New("the credential origin must be the URL's own, set once before the first request")
	}
	const sameOrigin = "if ($authorization -and (Test-SameOrigin $origin $current)) {"
	originAt := strings.Index(receive, sameOrigin)
	if originAt < 0 || strings.Count(src, "Headers['Authorization']") != 1 {
		return errors.New("credentials are not limited to the origin of the URL")
	}
	credentials, err := psBlockAt(receive, originAt+len(sameOrigin)-1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(credentials) != "$request.Headers['Authorization'] = $authorization" {
		return errors.New("credentials are sent outside the same-origin check")
	}
	headersAt := strings.Index(receive, "foreach ($name in $response.Headers.AllKeys) {")
	if headersAt < 0 || headersAt < strings.Index(receive, "if ($status -ne 200) {") || headersAt < strings.Index(receive, "continue") {
		return errors.New("integrity headers are read from a response that did not carry the body")
	}
	return nil
}

// Windows PowerShell 5.1 can offer TLS 1.0 by default, and a session may carry
// a certificate callback that accepts anything. Each request asks for TLS 1.2
// or later, without lowering a stronger setting, and uses the certificate
// checks of the system. The caller's settings come back afterwards. Each
// mutation below removes one part of that, and each must be caught.
func TestInstallPS1DownloadsOverTLS12OrLater(t *testing.T) {
	src := readInstallPS1(t)
	if err := checkPS1GuardsEveryRequest(src); err != nil {
		t.Fatalf("install.ps1 can make a request outside its transport rules: %v", err)
	}
	floor, err := psFunctionBody(src, "ConvertTo-TlsFloor")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n    $tls12 = 3072\n    $tls13 = 12288\n",
		"\n    if ($Protocols -eq 0) {\n        if ($Tls13ByDefault) { return $tls12 -bor $tls13 }\n        return $tls12\n    }\n",
		"\n    if (($Protocols -band (-bnot ($tls12 -bor $tls13))) -eq 0) { return $Protocols }\n",
		"\n    return $tls12 -bor ($Protocols -band $tls13)\n",
	} {
		if !strings.Contains(floor, want) {
			t.Fatalf("ConvertTo-TlsFloor does not contain %q", want)
		}
	}

	setFloor := "        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType](ConvertTo-TlsFloor ([int]$previousProtocols) (Test-Tls13ByDefault))\n"
	clearCallback := "        [Net.ServicePointManager]::ServerCertificateValidationCallback = $null\n"
	sameOrigin := "if ($authorization -and (Test-SameOrigin $origin $current)) {"
	headers := "        $headers = @{}\n        foreach ($name in $response.Headers.AllKeys) {\n            $headers[$name.ToLowerInvariant()] = $response.Headers[$name]\n        }\n"
	status := "        $status = [int]$response.StatusCode\n"
	mutations := []struct {
		name   string
		mutate func(string) string
	}{
		{"a second client bypasses the guards", func(s string) string {
			return strings.Replace(s, "function Save-PutnamiDownload([string]$Url, [string]$OutFile, [long]$MaxBytes) {\n", "function Save-PutnamiDownload([string]$Url, [string]$OutFile, [long]$MaxBytes) {\n    Invoke-WebRequest -Uri $Url -OutFile $OutFile\n", 1)
		}},
		{"SystemDefault keeps TLS 1.0", func(s string) string {
			return strings.Replace(s, setFloor, "        if ([int]$previousProtocols -ne 0) {\n    "+setFloor+"        }\n", 1)
		}},
		{"the caller's protocols never come back", func(s string) string {
			return strings.Replace(s, "        [Net.ServicePointManager]::SecurityProtocol = $previousProtocols\n", "", 1)
		}},
		{"an inherited certificate callback stays", func(s string) string {
			return strings.Replace(s, clearCallback, "", 1)
		}},
		{"the caller's certificate callback never comes back", func(s string) string {
			return strings.Replace(s, "        [Net.ServicePointManager]::ServerCertificateValidationCallback = $previousCallback\n", "", 1)
		}},
		{"a request accepts any certificate", func(s string) string {
			return strings.Replace(s, "        $request.AllowAutoRedirect = $false\n", "        $request.AllowAutoRedirect = $false\n        $request.ServerCertificateValidationCallback = { $true }\n", 1)
		}},
		{"the transport follows redirects itself", func(s string) string {
			return strings.Replace(s, "$request.AllowAutoRedirect = $false", "$request.AllowAutoRedirect = $true", 1)
		}},
		{"a redirect target skips the URL rule", func(s string) string {
			return strings.Replace(s, "if (-not (Test-RegistryUrl $next.AbsoluteUri 'redirect target')) {", "if ($false) {", 1)
		}},
		{"a refused redirect is followed", func(s string) string {
			return strings.Replace(s, "return @{ Error = 'refused redirect'; Headers = @{} }", "Write-InstallWarning 'refused redirect'", 1)
		}},
		{"credentials go to every origin", func(s string) string {
			return strings.Replace(s, sameOrigin, "if ($authorization) {", 1)
		}},
		{"the credential origin follows redirects", func(s string) string {
			return strings.Replace(s, "            $current = $next\n", "            $current = $next\n            $origin = $next\n", 1)
		}},
		{"a redirect's headers count as integrity", func(s string) string {
			return strings.Replace(strings.Replace(s, headers, "", 1), status, status+headers, 1)
		}},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := m.mutate(src)
			if mutated == src {
				t.Fatal("the mutation no longer applies to install.ps1")
			}
			if err := checkPS1GuardsEveryRequest(mutated); err == nil {
				t.Fatal("the check accepted a script that makes a request outside its transport rules")
			}
		})
	}

	requirePwsh(t)
	registry, _, _ := defaultPS1Registry(t)
	// SecurityProtocolType: SystemDefault 0, Tls 192, Tls11 768, Tls12 3072, Tls13 12288.
	for _, tc := range []struct{ before, tls13, during string }{
		{"", "", "3072"},
		{"", "1", "15360"},
		{"960", "", "3072"},
		{"12480", "", "15360"},
		{"3072", "", "3072"},
		{"12288", "", "12288"},
		{"15360", "1", "15360"},
	} {
		t.Run("before="+tc.before+",tls13="+tc.tls13, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL).set("PUTNAMI_HARNESS_TLS", tc.before).set("PUTNAMI_HARNESS_TLS13", tc.tls13)
			res := e.run(t, "--no-agent-hosts")
			if res.exitCode != 0 {
				t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
			}
			after := tc.before
			if after == "" {
				after = "0"
			}
			if got, want := e.readState(t, "tls.log"), "during "+tc.during+"\nafter "+after+"\n"; got != want {
				t.Fatalf("security protocols = %q, want %q", got, want)
			}
		})
	}
	t.Run("an inherited certificate callback", func(t *testing.T) {
		t.Parallel()
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL).set("PUTNAMI_HARNESS_CERTIFICATE_CALLBACK", "1")
		res := e.run(t, "--no-agent-hosts")
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if got, want := e.readState(t, "certificate.log"), "during none\nafter kept\n"; got != want {
			t.Fatalf("certificate callback = %q, want %q", got, want)
		}
	})
}

// Both installers take the same options: every flag install.sh documents and
// every PUTNAMI_* variable it reads has the same name in install.ps1.
func TestInstallPS1TakesTheOptionsOfInstallSh(t *testing.T) {
	shell, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := readInstallPS1(t)
	usageAt := strings.Index(string(shell), "\nprint_usage() {")
	if usageAt < 0 {
		t.Fatal("install.sh has no print_usage")
	}
	usage, _, found := strings.Cut(string(shell)[usageAt:], "\n}\n")
	if !found {
		t.Fatal("install.sh print_usage has no end")
	}
	flags := regexp.MustCompile(`--[a-z0-9][a-z0-9-]*`).FindAllString(usage, -1)
	if len(flags) < 7 {
		t.Fatalf("found only %d flags in install.sh print_usage: %v", len(flags), flags)
	}
	ps1Usage, err := psFunctionBody(src, "Write-Usage")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range flags {
		if !strings.Contains(ps1Usage, flag) {
			t.Errorf("install.ps1 --help does not document %s, which install.sh takes", flag)
		}
	}
	var code strings.Builder
	for _, line := range strings.Split(string(shell), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code.WriteString(line + "\n")
		}
	}
	variables := regexp.MustCompile(`PUTNAMI_[A-Z0-9_]+|NO_COLOR`).FindAllString(code.String(), -1)
	if len(variables) < 10 {
		t.Fatalf("found only %d variables in install.sh: %v", len(variables), variables)
	}
	for _, variable := range variables {
		if !strings.Contains(src, variable) {
			t.Errorf("install.ps1 does not read %s, which install.sh reads", variable)
		}
	}
}

// install.ps1 switches binaries in %USERPROFILE%\.putnami\bin by the same
// contract as `putnami upgrade` and `putnami version use` on Windows: the same
// lock file, the same locked byte, and the same moved-aside names. A drift
// lets an install and an upgrade interleave in one directory.
func TestInstallPS1FollowsTheBinarySwitchContract(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "one-binary-switch-on-windows", "the-installer-follows-the-switch-contract")
	src := readInstallPS1(t)
	for _, want := range []string{
		"$SwitchLockName = '.putnami-switch.lock'",
		fmt.Sprintf("$SwitchLockOffset = [long]%d", uint64(1)<<62),
		"$AsideMarker = '.old-'",
		"$stream.Lock($SwitchLockOffset, 1)",
		"[IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("install.ps1 does not contain %q", want)
		}
	}

	root := filepath.Join(filepath.Dir(installScriptPath(t)), "..", "..")
	filelock, err := os.ReadFile(filepath.Join(root, "extension-sdk", "filelock", "filelock_windows.go"))
	if err != nil {
		t.Fatalf("read the Windows file lock: %v", err)
	}
	if !strings.Contains(string(filelock), "const lockOffset = 1 << 62") {
		t.Fatal("the Windows file lock no longer locks the byte at 1 << 62; update $SwitchLockOffset in install.ps1 with it")
	}
	if !strings.Contains(string(filelock), "windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE") {
		t.Fatal("the Windows file lock changed its share mode; check that install.ps1 can still open the lock file beside it")
	}

	// The switch in versioncmd, when this tree has it.
	switchSource, err := os.ReadFile(filepath.Join(root, "cli", "internal", "commands", "versioncmd", "version_switch.go"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`switchLockName\s*=\s*"\.putnami-switch\.lock"`, `asideMarker\s*=\s*"\.old-"`} {
		if !regexp.MustCompile(want).Match(switchSource) {
			t.Fatalf("versioncmd no longer declares %s; update install.ps1 with it", want)
		}
	}
}

// checkPS1SwitchKeepsTheOldBinary reports how src could lose the binary a
// switch moved aside, or nil when it cannot: every move goes through
// Move-InstallFile, a failed move-in moves the old binary back, a failed
// restore leaves it aside and names its path, and only a switch that moved the
// new binary in deletes the old one.
func checkPS1SwitchKeepsTheOldBinary(src string) error {
	code, err := psCode(src)
	if err != nil {
		return err
	}
	if got := strings.Count(code, "[IO.File]::Move("); got != 1 {
		return fmt.Errorf("install.ps1 moves files %d times outside Move-InstallFile, want only its own call", got-1)
	}
	mover, err := psFunctionBody(src, "Move-InstallFile")
	if err != nil {
		return err
	}
	if strings.TrimSpace(mover) != "[IO.File]::Move($From, $To)" {
		return errors.New("Move-InstallFile does more than rename the file")
	}
	swap, err := psFunctionBody(src, "Install-ByRenameAside")
	if err != nil {
		return err
	}
	asideAt := strings.Index(swap, "Move-InstallFile $Destination $aside")
	const moveIn = "\n        try {\n            Move-InstallFile $staging $Destination\n        } catch {"
	moveInAt := strings.Index(swap, moveIn)
	if asideAt < 0 || moveInAt < asideAt {
		return errors.New("Install-ByRenameAside does not move the current file aside, then the new one in")
	}
	failed, err := psBlockAt(swap, moveInAt+len(moveIn)-1)
	if err != nil {
		return err
	}
	const restore = "\n                try {\n                    Move-InstallFile $aside $Destination\n                } catch {"
	restoreAt := strings.Index(failed, restore)
	if restoreAt < 0 || !strings.HasSuffix(strings.TrimSpace(failed), "throw $moveIn") {
		return errors.New("a failed move-in does not move the old binary back and fail")
	}
	lost, err := psBlockAt(failed, restoreAt+len(restore)-1)
	if err != nil {
		return err
	}
	if !strings.Contains(lost, `Write-Hint "The previous $leaf is kept at $aside."`) || !strings.HasSuffix(strings.TrimSpace(lost), "Stop-Install") {
		return errors.New("a failed restore does not name the path of the old binary and stop")
	}
	deleteAt := strings.Index(swap, "[IO.File]::Delete($aside)")
	if strings.Count(code, "[IO.File]::Delete($aside)") != 1 || deleteAt < moveInAt+len(moveIn)+len(failed) {
		return errors.New("the old binary is deleted before the new one has moved in")
	}
	finallyAt := strings.LastIndex(swap, "} finally {")
	if finallyAt < 0 {
		return errors.New("Install-ByRenameAside does not clean up its staged copy")
	}
	cleanup, err := psBlockAt(swap, finallyAt+len("} finally {")-1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cleanup) != "if (Test-Path -LiteralPath $staging) { [IO.File]::Delete($staging) }" {
		return errors.New("the cleanup of Install-ByRenameAside does more than delete the staged copy")
	}
	return nil
}

// A switch that cannot move the new binary in moves the old one back. When
// that fails too, the old binary stays aside, and the error names its path, so
// the user can move it back before a later switch deletes it. Each mutation
// below breaks one part of that, and each must be caught.
func TestInstallPS1KeepsTheOldBinaryWhenARestoreFails(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "one-binary-switch-on-windows", "a-failed-restore-names-the-kept-binary")
	src := readInstallPS1(t)
	if err := checkPS1SwitchKeepsTheOldBinary(src); err != nil {
		t.Fatalf("install.ps1 can lose the binary it moved aside: %v", err)
	}
	restore := "                try {\n                    Move-InstallFile $aside $Destination\n"
	mutations := []struct{ name, old, new string }{
		{"a move bypasses the seam", "            Move-InstallFile $staging $Destination\n", "            [IO.File]::Move($staging, $Destination)\n"},
		{"a failed move-in keeps nothing", restore, "                try {\n"},
		{"a failed restore names nothing", `                    Write-Hint "The previous $leaf is kept at $aside."` + "\n", ""},
		{"a failed restore deletes the old binary", "                    Stop-Install\n                }\n", "                    [IO.File]::Delete($aside)\n                    Stop-Install\n                }\n"},
		{"a failed restore goes on", "                    Stop-Install\n                }\n", "                }\n"},
		{"a failed move-in is swallowed", "            throw $moveIn\n", "            return\n"},
		{"the cleanup deletes the old binary", "        if (Test-Path -LiteralPath $staging) { [IO.File]::Delete($staging) }\n", "        if (Test-Path -LiteralPath $staging) { [IO.File]::Delete($staging) }\n        if ($aside) { Remove-Item -LiteralPath $aside }\n"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: install.ps1 does not contain exactly one %q", m.old)
			}
			if err := checkPS1SwitchKeepsTheOldBinary(strings.Replace(src, m.old, m.new, 1)); err == nil {
				t.Fatal("the check accepted a script that can lose the binary it moved aside")
			}
		})
	}

	// The same failure, run: the move-in and the restore both fail.
	requirePwsh(t)
	registry, _, binary := defaultPS1Registry(t)
	e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
	if res := e.run(t, "--no-agent-hosts"); res.exitCode != 0 {
		t.Fatalf("first install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	versioned, err := filepath.Glob(filepath.Join(e.binDir(), "putnami-go-*.exe"))
	if err != nil || len(versioned) != 1 {
		t.Fatalf("versioned binaries = %v (%v), want one", versioned, err)
	}
	res := e.set("PUTNAMI_HARNESS_FAIL_MOVE", `\.new\.[0-9]+$|\.old-`).run(t, "--no-agent-hosts")
	if res.exitCode == 0 {
		t.Fatalf("an install whose move-in and restore fail succeeded:\n%s", res.output())
	}
	asides, err := filepath.Glob(filepath.Join(e.binDir(), "."+filepath.Base(versioned[0])+".old-*"))
	if err != nil || len(asides) != 1 {
		t.Fatalf("moved-aside binaries = %v (%v), want the previous one kept\n%s", asides, err, res.output())
	}
	if got := readFile(t, asides[0]); !bytes.Equal(got, binary) {
		t.Fatal("the kept binary is not the previous one")
	}
	if !res.contains("The previous " + filepath.Base(versioned[0]) + " is kept at " + asides[0] + ".") {
		t.Fatalf("the error does not name the kept binary %s:\n%s", asides[0], res.output())
	}
	if _, err := os.Stat(versioned[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed switch left %s in place (err %v)", versioned[0], err)
	}
	if staged, _ := filepath.Glob(filepath.Join(e.binDir(), "*.new.*")); len(staged) != 0 {
		t.Fatalf("the failed switch left staged copies %v", staged)
	}

	// Once the moves succeed, the next install switches in and deletes it.
	if res := e.set("PUTNAMI_HARNESS_FAIL_MOVE", "").run(t, "--no-agent-hosts"); res.exitCode != 0 {
		t.Fatalf("the install after the failure failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if got := readFile(t, versioned[0]); !bytes.Equal(got, binary) {
		t.Fatal("the install after the failure did not put the binary back")
	}
	if left, _ := filepath.Glob(filepath.Join(e.binDir(), ".*.old-*")); len(left) != 0 {
		t.Fatalf("moved-aside binaries = %v, want them deleted by the next install", left)
	}
}

// A switch whose move-in fails moves the old binary back and fails with the
// move-in error.
func TestInstallPS1RestoresTheOldBinaryWhenTheMoveInFails(t *testing.T) {
	t.Parallel()
	registry, _, binary := defaultPS1Registry(t)
	e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
	if res := e.run(t, "--no-agent-hosts"); res.exitCode != 0 {
		t.Fatalf("first install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	res := e.set("PUTNAMI_HARNESS_FAIL_MOVE", `\.new\.[0-9]+$`).run(t, "--no-agent-hosts")
	if res.exitCode == 0 || !res.contains("harness refuses to move") {
		t.Fatalf("an install whose move-in fails exited %d:\n%s", res.exitCode, res.output())
	}
	if res.contains("is kept at") {
		t.Fatalf("a restored binary is reported as kept aside:\n%s", res.output())
	}
	versioned, _ := filepath.Glob(filepath.Join(e.binDir(), "putnami-go-*.exe"))
	if len(versioned) != 1 || !bytes.Equal(readFile(t, versioned[0]), binary) {
		t.Fatalf("versioned binaries = %v, want the previous one restored", versioned)
	}
	if left, _ := filepath.Glob(filepath.Join(e.binDir(), ".*.old-*")); len(left) != 0 {
		t.Fatalf("moved-aside binaries = %v, want none after a restore", left)
	}
}

// --- Behavior under PowerShell ----------------------------------------------

func requirePwsh(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness stands in for Windows on other hosts; install_ps1_windows_test.go runs install.ps1 itself")
	}
	path, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed; install.ps1 runs under PowerShell on the Windows host (install_ps1_windows_test.go)")
	}
	return path
}

// ps1Harness runs install.ps1 on any host. It dot-sources every statement of
// the script's block except the entry point, replaces the functions that read
// Windows itself with ones backed by files in PUTNAMI_HARNESS_STATE, then calls
// the entry point with the harness arguments. PUTNAMI_HARNESS_FAIL_MOVE is a
// pattern: a binary-switch move of a file whose name matches it fails.
// PUTNAMI_HARNESS_FROM_FILE=1 runs the script as if read from a file, where run
// mode ends with exit; otherwise it runs as text. status.log records the
// $LASTEXITCODE the script leaves behind, and returned.log that the script
// returned to its caller rather than exiting.
const ps1Harness = `Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$harnessTokens = $null
$harnessErrors = $null
$harnessAst = [System.Management.Automation.Language.Parser]::ParseFile($env:PUTNAMI_HARNESS_SCRIPT, [ref]$harnessTokens, [ref]$harnessErrors)
if ($harnessErrors.Count -gt 0) { throw ('install.ps1 does not parse: ' + $harnessErrors[0].Message) }
$harnessTop = $harnessAst.EndBlock.Statements
$harnessBody = $harnessTop[$harnessTop.Count - 1].PipelineElements[0].CommandElements[0].ScriptBlock.EndBlock.Statements
$harnessEntry = $harnessBody[$harnessBody.Count - 1].Extent.Text
if ($harnessEntry -ne 'Invoke-PutnamiInstaller ([string[]]$args)') { throw ('unexpected entry point: ' + $harnessEntry) }
for ($harnessIndex = 0; $harnessIndex -lt $harnessBody.Count - 1; $harnessIndex++) {
    . ([scriptblock]::Create($harnessBody[$harnessIndex].Extent.Text))
}

$HarnessState = $env:PUTNAMI_HARNESS_STATE
$RunsFromFile = [string]$env:PUTNAMI_HARNESS_FROM_FILE -eq '1'
function Write-HarnessLog([string]$Name, [string]$Line) {
    [IO.File]::AppendAllText((Join-Path $HarnessState $Name), $Line + [Environment]::NewLine)
}
function Get-HarnessListing([string]$Directory) {
    return ((Get-ChildItem -LiteralPath $Directory -Force | ForEach-Object { $_.Name } | Sort-Object) -join ',')
}
function Get-PutnamiPlatform {
    $raw = [string]$env:PUTNAMI_HARNESS_ARCH
    if (-not $raw) { $raw = 'AMD64' }
    $arch = 'unknown'
    if ($raw -eq 'AMD64') { $arch = 'amd64' }
    return @{ OS = 'windows'; Arch = $arch; RawArch = $raw }
}
function Get-SystemTarPath {
    return [string](Get-Command -Name tar -CommandType Application | Select-Object -First 1).Path
}
function Get-UserPathValue {
    $valueFile = Join-Path $HarnessState 'hkcu-path.txt'
    if (-not (Test-Path -LiteralPath $valueFile)) { return @{ Value = $null; Kind = '' } }
    return @{ Value = [IO.File]::ReadAllText($valueFile); Kind = [IO.File]::ReadAllText((Join-Path $HarnessState 'hkcu-path.kind')) }
}
function Set-UserPathValue([string]$Value, [string]$Kind) {
    [IO.File]::WriteAllText((Join-Path $HarnessState 'hkcu-path.txt'), $Value)
    [IO.File]::WriteAllText((Join-Path $HarnessState 'hkcu-path.kind'), $Kind)
    Write-HarnessLog 'registry.log' ('set ' + $Kind)
}
function Get-MachinePathValue { return [string]$env:PUTNAMI_HARNESS_MACHINE_PATH }
function Send-EnvironmentChange { Write-HarnessLog 'registry.log' 'broadcast Environment' }
function Enter-SwitchLock([string]$Directory) {
    Write-HarnessLog 'lock.log' ('enter ' + (Get-HarnessListing $Directory))
    return $Directory
}
function Exit-SwitchLock($Lock) { Write-HarnessLog 'lock.log' ('exit ' + (Get-HarnessListing $Lock)) }
function Test-Tls13ByDefault { return [string]$env:PUTNAMI_HARNESS_TLS13 -eq '1' }
$HarnessMove = ${function:Move-InstallFile}
function Move-InstallFile([string]$From, [string]$To) {
    $name = [IO.Path]::GetFileName($From)
    if ($env:PUTNAMI_HARNESS_FAIL_MOVE -and ($name -match $env:PUTNAMI_HARNESS_FAIL_MOVE)) {
        throw (New-Object System.IO.IOException ('harness refuses to move ' + $name))
    }
    & $HarnessMove $From $To
}
$HarnessReceive = ${function:Receive-PutnamiDownload}
function Receive-PutnamiDownload([Uri]$Url, [string]$OutFile, [long]$MaxBytes) {
    Write-HarnessLog 'tls.log' ('during ' + [int][Net.ServicePointManager]::SecurityProtocol)
    $callback = 'set'
    if ($null -eq [Net.ServicePointManager]::ServerCertificateValidationCallback) { $callback = 'none' }
    Write-HarnessLog 'certificate.log' ('during ' + $callback)
    return & $HarnessReceive $Url $OutFile $MaxBytes
}

if ($env:PUTNAMI_HARNESS_TLS) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType][int]$env:PUTNAMI_HARNESS_TLS
}
$HarnessCallback = $null
if ($env:PUTNAMI_HARNESS_CERTIFICATE_CALLBACK -eq '1') {
    [Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
    $HarnessCallback = [Net.ServicePointManager]::ServerCertificateValidationCallback
}
try {
    Invoke-PutnamiInstaller ([string[]]$args)
} finally {
    Write-HarnessLog 'tls.log' ('after ' + [int][Net.ServicePointManager]::SecurityProtocol)
    $callback = 'changed'
    if ([object]::ReferenceEquals($HarnessCallback, [Net.ServicePointManager]::ServerCertificateValidationCallback)) { $callback = 'kept' }
    Write-HarnessLog 'certificate.log' ('after ' + $callback)
    Write-HarnessLog 'status.log' ([string](Get-Variable -Name LASTEXITCODE -Scope Global -ValueOnly -ErrorAction SilentlyContinue))
}
Write-HarnessLog 'returned.log' 'returned'
`

// stubPS1CLI stands in for putnami.exe. It records the relaunch variables of
// each --version run, which is how the installer asks the binary for its own
// stamp instead of a workspace-pinned one.
func stubPS1CLI(version string) []byte {
	return []byte("#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  if [ -n \"${PUTNAMI_TEST_STUB_LOG:-}\" ]; then\n" +
		"    printf 'no-relaunch=%s launched=%s\\n' \"${PUTNAMI_NO_RELAUNCH:-}\" \"${PUTNAMI_LAUNCHED:-}\" >> \"$PUTNAMI_TEST_STUB_LOG\"\n" +
		"  fi\n" +
		"  echo \"Putnami   " + version + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n")
}

// windowsArchive is the release archive shape B1 publishes for windows/amd64:
// a .tar.gz with the executable at compiled/<name> among other files.
func windowsArchive(t *testing.T, name string, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := []struct {
		name string
		mode int64
		body []byte
	}{
		{"compiled/README.md", 0o644, []byte("Putnami CLI\n")},
		{"compiled/" + name, 0o755, binary},
	}
	if err := tw.WriteHeader(&tar.Header{Name: "compiled/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type ps1Request struct {
	path          string
	query         url.Values
	authorization string
}

// ps1Registry serves the download endpoint the way the registry does, the
// installer itself at /install.ps1 the way the site does, and records every
// request.
type ps1Registry struct {
	*httptest.Server
	mu       sync.Mutex
	requests []ps1Request
}

type ps1RegistryOptions struct {
	registryOptions
	// redirect, when set, answers the download endpoint with a 302 to it.
	redirect string
	// mapStatus, when set, answers /install-commands.txt with this status and
	// the commandMap body, even an empty one.
	mapStatus int
}

func newPS1Registry(t *testing.T, opts ps1RegistryOptions) *ps1Registry {
	t.Helper()
	installer := []byte(readInstallPS1(t))
	registry := &ps1Registry{}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.mu.Lock()
		registry.requests = append(registry.requests, ps1Request{path: r.URL.Path, query: r.URL.Query(), authorization: r.Header.Get("Authorization")})
		registry.mu.Unlock()
		switch r.URL.Path {
		case "/install.ps1":
			servePS1InstallScript(w, r, installer)
			return
		case "/install-commands.txt":
			if opts.commandMap == "" && opts.mapStatus == 0 {
				http.NotFound(w, r)
				return
			}
			// Announce the size, as a static file server does.
			w.Header().Set("Content-Length", strconv.Itoa(len(opts.commandMap)))
			if opts.mapStatus != 0 {
				w.WriteHeader(opts.mapStatus)
			}
			_, _ = w.Write([]byte(opts.commandMap))
			return
		case "/putnami/cli/download":
			if opts.redirect != "" {
				http.Redirect(w, r, opts.redirect, http.StatusFound)
				return
			}
		case "/asset":
		default:
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("plain") == "1" {
			_, _ = w.Write(opts.body)
			return
		}
		if opts.integrity != "" {
			w.Header().Set("X-Integrity", opts.integrity)
		}
		if opts.digest != "" {
			w.Header().Set("Digest", opts.digest)
		}
		if opts.resolved != "" {
			w.Header().Set("X-Resolved-Version", opts.resolved)
		}
		status := opts.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(opts.body)
	}))
	t.Cleanup(registry.Close)
	return registry
}

func (r *ps1Registry) recorded() []ps1Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ps1Request(nil), r.requests...)
}

// defaultPS1Registry serves the Windows archive with a matching digest and
// stamp, the shape of a healthy release.
func defaultPS1Registry(t *testing.T) (*ps1Registry, []byte, []byte) {
	t.Helper()
	binary := stubPS1CLI(stubVersion)
	archive := windowsArchive(t, "putnami.exe", binary)
	return newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:      archive,
		integrity: "sha256:" + sha256Hex(archive),
		resolved:  stubVersion,
	}}), archive, binary
}

// psEnv is a hermetic PowerShell run: a temp profile directory (USERPROFILE
// and HOME), a temp TMPDIR, a PATH of system directories plus a fake bin
// directory, and the harness state directory that stands in for the registry.
type psEnv struct {
	pwsh     string
	root     string
	workDir  string // the directory pwsh starts in; defaults to home
	home     string
	tmp      string
	state    string
	fakeBin  string
	stubLog  string
	script   string
	pathDirs []string
	vars     map[string]string
}

func newPSEnv(t *testing.T) *psEnv {
	t.Helper()
	pwsh := requirePwsh(t)
	root := t.TempDir()
	e := &psEnv{
		pwsh:    pwsh,
		root:    root,
		home:    filepath.Join(root, "home"),
		tmp:     filepath.Join(root, "tmp"),
		state:   filepath.Join(root, "state"),
		fakeBin: filepath.Join(root, "fakebin"),
		stubLog: filepath.Join(root, "stub.log"),
		script:  installPS1Path(t),
		vars:    map[string]string{},
	}
	for _, dir := range []string{e.home, e.tmp, e.state, e.fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.pathDirs = []string{e.fakeBin, "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if err := os.WriteFile(filepath.Join(root, "harness.ps1"), []byte(ps1Harness), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *psEnv) set(key, value string) *psEnv {
	e.vars[key] = value
	return e
}

func (e *psEnv) binDir() string { return filepath.Join(e.home, ".putnami", "bin") }

func (e *psEnv) environ() []string {
	out := []string{
		"PATH=" + strings.Join(e.pathDirs, string(os.PathListSeparator)),
		"HOME=" + e.home,
		"USERPROFILE=" + e.home,
		"TMPDIR=" + e.tmp,
		"PUTNAMI_HARNESS_SCRIPT=" + e.script,
		"PUTNAMI_HARNESS_STATE=" + e.state,
		"PUTNAMI_TEST_STUB_LOG=" + e.stubLog,
		"POWERSHELL_TELEMETRY_OPTOUT=1",
		"POWERSHELL_UPDATECHECK=Off",
		"DOTNET_CLI_TELEMETRY_OPTOUT=1",
		"DOTNET_EnableDiagnostics=0",
	}
	for k, v := range e.vars {
		out = append(out, k+"="+v)
	}
	return out
}

type psResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func (r psResult) output() string { return r.stdout + r.stderr }

func (r psResult) contains(needle string) bool { return strings.Contains(r.output(), needle) }

func (e *psEnv) exec(t *testing.T, args ...string) psResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.pwsh, append([]string{"-NoLogo", "-NoProfile", "-NonInteractive"}, args...)...)
	cmd.Dir = e.home
	if e.workDir != "" {
		cmd.Dir = e.workDir
	}
	cmd.Env = e.environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run pwsh: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return psResult{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}
}

// run runs install.ps1 through the harness with args.
func (e *psEnv) run(t *testing.T, args ...string) psResult {
	t.Helper()
	return e.exec(t, append([]string{"-File", filepath.Join(e.root, "harness.ps1")}, args...)...)
}

func (e *psEnv) readState(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.state, name))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertNothingInstalled checks that a refusal left no binary and no PATH
// change behind.
func (e *psEnv) assertNothingInstalled(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(e.binDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "putnami") {
			t.Fatalf("a refused install left %s in %s", entry.Name(), e.binDir())
		}
	}
	if got := e.readState(t, "registry.log"); got != "" {
		t.Fatalf("a refused install changed the user PATH: %q", got)
	}
}

func (e *psEnv) assertNoScratchLeft(t *testing.T, output string) {
	t.Helper()
	entries, err := os.ReadDir(e.tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "putnami-install-") {
			t.Fatalf("installer left %s in the temp directory\n%s", entry.Name(), output)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func TestInstallPS1InstallsTheVersionedLayoutAfterVerifying(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	registry, archive, binary := defaultPS1Registry(t)
	e.set("PUTNAMI_REGISTRY_URL", registry.URL).set("PUTNAMI_LAUNCHED", "outer-launcher")

	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	for _, want := range []string{
		"Integrity verified (sha256:" + sha256Hex(archive) + ")",
		"Version stamp verified (" + stubVersion + ")",
		"Installed to " + filepath.Join(e.binDir(), "putnami-go-latest.exe"),
		"Active: putnami.exe (a copy of putnami-go-latest.exe)",
		"putnami " + stubVersion,
		"Added " + e.binDir() + " to your user PATH (HKCU\\Environment) for new terminals",
		"$env:Path = \"" + e.binDir() + ";$env:Path\"",
		"set \"PATH=" + e.binDir() + ";%PATH%\"",
	} {
		if !res.contains(want) {
			t.Fatalf("output does not contain %q:\n%s", want, res.output())
		}
	}
	if strings.Contains(res.output(), "\x1b[") {
		t.Fatalf("redirected output carries escape sequences:\n%q", res.output())
	}

	// The registry was asked for the Windows build, in GOOS/GOARCH words.
	requests := registry.recorded()
	if len(requests) != 1 {
		t.Fatalf("registry saw %d requests, want 1: %+v", len(requests), requests)
	}
	for key, want := range map[string]string{"channel": "latest", "os": "windows", "arch": "amd64"} {
		if got := requests[0].query.Get(key); got != want {
			t.Fatalf("download query %s = %q, want %q", key, got, want)
		}
	}

	// Both names hold the served executable; nothing staged is left.
	for _, name := range []string{"putnami-go-latest.exe", "putnami.exe"} {
		if got := readFile(t, filepath.Join(e.binDir(), name)); !bytes.Equal(got, binary) {
			t.Fatalf("%s is not the executable the archive carried", name)
		}
	}
	entries, err := os.ReadDir(e.binDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".new.") || strings.Contains(entry.Name(), ".old-") || strings.Contains(entry.Name(), "write-probe") {
			t.Fatalf("install left %s behind", entry.Name())
		}
	}

	// The binaries landed while the switch lock was held.
	lockLog := strings.Split(strings.TrimSpace(e.readState(t, "lock.log")), "\n")
	if len(lockLog) != 2 || !strings.HasPrefix(lockLog[0], "enter") || strings.Contains(lockLog[0], "putnami") ||
		!strings.Contains(lockLog[1], "putnami-go-latest.exe") || !strings.Contains(lockLog[1], "putnami.exe") {
		t.Fatalf("the binaries were not switched under the lock: %q", lockLog)
	}

	// The stamp check asked the binary itself, never a relaunch target.
	stubRuns := strings.Split(strings.TrimSpace(string(readFile(t, e.stubLog))), "\n")
	if stubRuns[0] != "no-relaunch=1 launched=" {
		t.Fatalf("the stamp check ran with %q, want relaunch disabled", stubRuns[0])
	}

	// The user's Path gained the directory once, as an expandable string, and
	// running programs were told.
	if got := e.readState(t, "hkcu-path.txt"); got != e.binDir() {
		t.Fatalf("user Path = %q, want %q", got, e.binDir())
	}
	if got := e.readState(t, "hkcu-path.kind"); got != "ExpandString" {
		t.Fatalf("user Path kind = %q, want ExpandString", got)
	}
	if got := e.readState(t, "registry.log"); got != "set ExpandString\nbroadcast Environment\n" {
		t.Fatalf("registry writes = %q", got)
	}
	e.assertNoScratchLeft(t, res.output())
}

// A second install replaces the binaries, keeps one Path entry, deletes what
// earlier switches moved aside, and leaves other hidden files alone.
func TestInstallPS1ReinstallsWithoutDuplicatingState(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	registry, _, binary := defaultPS1Registry(t)
	e.set("PUTNAMI_REGISTRY_URL", registry.URL)

	if res := e.run(t); res.exitCode != 0 {
		t.Fatalf("first install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	leftover := filepath.Join(e.binDir(), ".putnami.exe.old-1-2")
	foreign := filepath.Join(e.binDir(), ".notes.old-keep")
	for _, path := range []string{leftover, foreign} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.binDir(), "putnami.exe"), []byte("older build"), 0o755); err != nil {
		t.Fatal(err)
	}

	e.pathDirs = append([]string{e.binDir()}, e.pathDirs...)
	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("second install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if !res.contains(e.binDir() + " is already on your PATH") {
		t.Fatalf("a directory already on PATH was not recognized:\n%s", res.output())
	}
	if got := e.readState(t, "hkcu-path.txt"); got != e.binDir() {
		t.Fatalf("user Path = %q after a second install, want one entry", got)
	}
	if got := strings.Count(e.readState(t, "registry.log"), "set "); got != 1 {
		t.Fatalf("the user Path was written %d times across two installs", got)
	}
	if got := readFile(t, filepath.Join(e.binDir(), "putnami.exe")); !bytes.Equal(got, binary) {
		t.Fatal("putnami.exe was not replaced")
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a binary an earlier switch moved aside is still there: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("a hidden file the installer does not own was deleted: %v", err)
	}
}

// The user's Path keeps its registry kind, its other entries and their
// unexpanded %VARIABLES%; a directory the machine Path already carries is not
// added again.
func TestInstallPS1KeepsTheUserPath(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)

	t.Run("an existing value keeps its kind and entries", func(t *testing.T) {
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		existing := `%USERPROFILE%\tools;C:\Program Files\Other;`
		if err := os.WriteFile(filepath.Join(e.state, "hkcu-path.txt"), []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.state, "hkcu-path.kind"), []byte("String"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		want := e.binDir() + `;%USERPROFILE%\tools;C:\Program Files\Other`
		if got := e.readState(t, "hkcu-path.txt"); got != want {
			t.Fatalf("user Path = %q, want %q", got, want)
		}
		if got := e.readState(t, "hkcu-path.kind"); got != "String" {
			t.Fatalf("user Path kind = %q, want the existing String", got)
		}
	})

	t.Run("an entry on the machine Path counts", func(t *testing.T) {
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		e.set("PUTNAMI_HARNESS_MACHINE_PATH", `C:\Windows;`+e.binDir()+`\`)
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if got := e.readState(t, "registry.log"); got != "" {
			t.Fatalf("a directory on the machine Path was added to the user Path: %q", got)
		}
	})
}

func TestInstallPS1RefusesATamperedDownload(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	archive := windowsArchive(t, "putnami.exe", stubPS1CLI(stubVersion))
	registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:      archive,
		integrity: "sha256:" + strings.Repeat("a", 64),
		resolved:  stubVersion,
	}})
	e.set("PUTNAMI_REGISTRY_URL", registry.URL)

	res := e.run(t)
	if res.exitCode == 0 {
		t.Fatalf("a download that does not match its digest installed anyway:\n%s", res.output())
	}
	if !res.contains("Integrity check failed: expected " + strings.Repeat("a", 64) + ", got " + sha256Hex(archive)) {
		t.Fatalf("the mismatch was not named:\n%s", res.output())
	}
	if res.contains("Integrity verified") || res.contains("Version stamp verified") {
		t.Fatalf("installer claimed or went past verification:\n%s", res.output())
	}
	if _, err := os.Stat(e.stubLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the unverified download was run")
	}
	e.assertNothingInstalled(t)
	e.assertNoScratchLeft(t, res.output())
}

// The tamper test above has teeth: the same run against a copy of install.ps1
// that ignores the integrity result installs the tampered download.
func TestInstallPS1TamperTestCatchesAMissingDigestCheck(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	src := readInstallPS1(t)
	guard := "if (-not (Confirm-DownloadIntegrity $assetFile $advertisedIntegrity $expectedSha256 $sourceLabel)) {\n            Stop-Install\n        }\n"
	if strings.Count(src, guard) != 1 {
		t.Fatal("install.ps1 no longer has the integrity guard this test removes")
	}
	e.script = filepath.Join(e.root, "install-unguarded.ps1")
	if err := os.WriteFile(e.script, []byte(strings.Replace(src, guard, "$null = Confirm-DownloadIntegrity $assetFile $advertisedIntegrity $expectedSha256 $sourceLabel\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:      windowsArchive(t, "putnami.exe", stubPS1CLI(stubVersion)),
		integrity: "sha256:" + strings.Repeat("a", 64),
		resolved:  stubVersion,
	}})
	e.set("PUTNAMI_REGISTRY_URL", registry.URL)

	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("the unguarded copy refused the download, so the tamper test proves nothing (exit %d):\n%s", res.exitCode, res.output())
	}
	if _, err := os.Stat(filepath.Join(e.binDir(), "putnami.exe")); err != nil {
		t.Fatalf("the unguarded copy did not install the tampered download: %v", err)
	}
}

func TestInstallPS1RefusesAnUnadvertisedDigestUnlessUnsafe(t *testing.T) {
	t.Parallel()
	registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:     windowsArchive(t, "putnami.exe", stubPS1CLI(stubVersion)),
		resolved: stubVersion,
	}})

	t.Run("refused by default", func(t *testing.T) {
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		res := e.run(t)
		if res.exitCode == 0 {
			t.Fatalf("a download nobody vouched for installed:\n%s", res.output())
		}
		if !res.contains("the registry did not advertise an integrity hash; refusing to install an unverified binary.") || !res.contains("PUTNAMI_UNSAFE_INSTALL=1") {
			t.Fatalf("refusal did not name the cause and the override:\n%s", res.output())
		}
		e.assertNothingInstalled(t)
		e.assertNoScratchLeft(t, res.output())
	})

	t.Run("installed loudly with PUTNAMI_UNSAFE_INSTALL=1", func(t *testing.T) {
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL).set("PUTNAMI_UNSAFE_INSTALL", "1")
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("the documented override failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if !res.contains("Installing without integrity verification (PUTNAMI_UNSAFE_INSTALL=1)") {
			t.Fatalf("the override was silent:\n%s", res.output())
		}
		if res.contains("Integrity verified") {
			t.Fatalf("an unverified install claimed verification:\n%s", res.output())
		}
	})
}

func TestInstallPS1AcceptsAnRFC9530Digest(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	archive := windowsArchive(t, "putnami.exe", stubPS1CLI(stubVersion))
	registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:     archive,
		digest:   "sha-512=ignored, sha-256=" + sha256Hex(archive),
		resolved: stubVersion,
	}})
	e.set("PUTNAMI_REGISTRY_URL", registry.URL)

	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if !res.contains("Integrity verified (sha256:" + sha256Hex(archive) + ")") {
		t.Fatalf("the Digest header was not used:\n%s", res.output())
	}
}

func TestInstallPS1DownloadURLNeedsADigest(t *testing.T) {
	t.Parallel()
	registry, archive, _ := defaultPS1Registry(t)
	plainURL := registry.URL + "/asset?plain=1"
	metadataURL := registry.URL + "/asset"

	cases := []struct {
		name    string
		args    []string
		success bool
		want    string
	}{
		{"refused without --sha256", []string{"--download-url", plainURL}, false, "the download URL did not advertise an integrity hash"},
		{"installed with a matching --sha256", []string{"--download-url", plainURL, "--sha256", "sha256:" + sha256Hex(archive)}, true, "Integrity verified (sha256:" + sha256Hex(archive) + ")"},
		{"refused when --sha256 does not match", []string{"--download-url", plainURL, "--sha256", strings.Repeat("b", 64)}, false, "Integrity check failed"},
		{"refused when --sha256 disagrees with the advertised digest", []string{"--download-url", metadataURL, "--sha256", strings.Repeat("b", 64)}, false, "does not match the one the download URL advertised"},
		{"refused when --sha256 is not a digest", []string{"--download-url", plainURL, "--sha256", "not-a-digest"}, false, "expected 64-character hex SHA-256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t)
			res := e.run(t, tc.args...)
			if (res.exitCode == 0) != tc.success {
				t.Fatalf("exit %d, want success=%v:\n%s", res.exitCode, tc.success, res.output())
			}
			if !res.contains(tc.want) {
				t.Fatalf("output does not contain %q:\n%s", tc.want, res.output())
			}
			if !tc.success {
				e.assertNothingInstalled(t)
			}
		})
	}
}

func TestInstallPS1RefusesAStaleVersionStamp(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	archive := windowsArchive(t, "putnami.exe", stubPS1CLI("1.2.3"))
	registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:      archive,
		integrity: sha256Hex(archive),
		resolved:  "v9.9.9",
	}})
	e.set("PUTNAMI_REGISTRY_URL", registry.URL)

	res := e.run(t)
	if res.exitCode == 0 {
		t.Fatalf("a stale build installed under a fresh version:\n%s", res.output())
	}
	if !res.contains(`The registry resolved v9.9.9 but the downloaded binary reports "1.2.3"; refusing to install a stale build.`) {
		t.Fatalf("the stale stamp was not named:\n%s", res.output())
	}
	e.assertNothingInstalled(t)
}

// An asset whose archive holds no putnami.exe, or which is neither an archive
// nor a Windows executable, is refused after verification.
func TestInstallPS1RefusesAnAssetWithoutTheExecutable(t *testing.T) {
	t.Parallel()
	for name, body := range map[string][]byte{
		"archive without putnami.exe": windowsArchive(t, "putnami", stubPS1CLI(stubVersion)),
		"not an executable":           []byte("<html>not a release</html>"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			registry := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{body: body, integrity: sha256Hex(body)}})
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
			res := e.run(t)
			if res.exitCode == 0 {
				t.Fatalf("an asset without the executable installed:\n%s", res.output())
			}
			if !res.contains("Downloaded asset did not contain a 'putnami.exe' executable") {
				t.Fatalf("the missing executable was not named:\n%s", res.output())
			}
			e.assertNothingInstalled(t)
		})
	}
}

func TestInstallPS1RefusesUnauthenticatedRegistries(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token"
	cases := []struct {
		name, url, want string
	}{
		{"plaintext non-loopback", "http://user:" + secret + "@registry.invalid", `registry URL "http://***@registry.invalid" uses plaintext http://; refusing to fetch over an unauthenticated channel.`},
		{"unsupported scheme", "ftp://user:" + secret + "@example.com", `has unsupported scheme "ftp" (only https and http are accepted).`},
		{"no scheme", "registry.invalid", `Invalid registry URL "registry.invalid": no scheme`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", tc.url)
			res := e.run(t)
			if res.exitCode == 0 {
				t.Fatalf("an unauthenticated registry was accepted:\n%s", res.output())
			}
			if !res.contains(tc.want) {
				t.Fatalf("output does not contain %q:\n%s", tc.want, res.output())
			}
			if res.contains(secret) {
				t.Fatalf("registry credentials reached the output:\n%s", res.output())
			}
			if res.contains("Downloading") {
				t.Fatalf("installer started a download from a refused registry:\n%s", res.output())
			}
			if _, err := os.Stat(e.binDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("installer created the install directory for a refused registry")
			}
		})
	}
}

// Redirects are followed under the registry rule, and credentials from the
// registry URL go only to the registry's own origin.
func TestInstallPS1FollowsRedirectsUnderTheRegistryRule(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token"
	storage, archive, _ := defaultPS1Registry(t)

	t.Run("credentials stay with the registry origin", func(t *testing.T) {
		t.Parallel()
		registry := newPS1Registry(t, ps1RegistryOptions{redirect: storage.URL + "/asset"})
		withCredentials := strings.Replace(registry.URL, "http://", "http://user:"+secret+"@", 1)
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", withCredentials)

		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install through a redirect failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if !res.contains("Integrity verified (sha256:" + sha256Hex(archive) + ")") {
			t.Fatalf("the redirected download was not verified:\n%s", res.output())
		}
		if res.contains(secret) || !res.contains("***@") {
			t.Fatalf("registry credentials were not redacted:\n%s", res.output())
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+secret))
		if got := registry.recorded()[0].authorization; got != want {
			t.Fatalf("registry Authorization = %q, want %q", got, want)
		}
		for _, request := range storage.recorded() {
			if request.path == "/asset" && request.authorization != "" {
				t.Fatalf("registry credentials followed a redirect to another origin: %q", request.authorization)
			}
		}
	})

	t.Run("a redirect to plaintext http is refused", func(t *testing.T) {
		t.Parallel()
		registry := newPS1Registry(t, ps1RegistryOptions{redirect: "http://storage.invalid/asset"})
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		res := e.run(t)
		if res.exitCode == 0 {
			t.Fatalf("a redirect to plaintext http was followed:\n%s", res.output())
		}
		if !res.contains(`redirect target "http://storage.invalid/asset" uses plaintext http://`) {
			t.Fatalf("the refused redirect was not named:\n%s", res.output())
		}
		e.assertNothingInstalled(t)
	})
}

// --install-dir, in either spelling, installs the plain name there and no
// versioned copy.
func TestInstallPS1InstallDirUsesThePlainName(t *testing.T) {
	t.Parallel()
	registry, _, binary := defaultPS1Registry(t)
	for _, spelling := range []string{"--install-dir", "-InstallDir"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
			target := filepath.Join(e.root, "tools", "bin")
			res := e.run(t, spelling, target, "--version", "1.2.3", "--no-agent-hosts")
			if res.exitCode != 0 {
				t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
			}
			if got := readFile(t, filepath.Join(target, "putnami.exe")); !bytes.Equal(got, binary) {
				t.Fatal("putnami.exe in the install directory is not the served executable")
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "putnami-") {
					t.Fatalf("--install-dir wrote the versioned name %s", entry.Name())
				}
			}
			if _, err := os.Stat(e.binDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("--install-dir also wrote the default directory")
			}
			if got := registry.recorded(); got[len(got)-1].query.Get("channel") != "v1.2.3" {
				t.Fatalf("a semver --version was not sent as v1.2.3: %v", got[len(got)-1].query)
			}
			if got := e.readState(t, "hkcu-path.txt"); got != target {
				t.Fatalf("user Path = %q, want the install directory", got)
			}
		})
	}
}

func TestInstallPS1RefusesUnsupportedInput(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	// Nothing below reaches the registry. The check runs after the parallel
	// subtests finish.
	t.Cleanup(func() {
		if got := registry.recorded(); len(got) != 0 {
			t.Errorf("refusals reached the registry: %+v", got)
		}
	})
	cases := []struct {
		name string
		arch string
		args []string
		env  map[string]string
		want string
	}{
		{name: "arm64", arch: "ARM64", want: "Unsupported architecture: ARM64"},
		{name: "32-bit", arch: "x86", want: "Unsupported architecture: x86"},
		{name: "unknown variant flag", args: []string{"--variant", "rust"}, want: `Unknown variant "rust" (expected go or ts)`},
		{name: "unknown variant variable", env: map[string]string{"PUTNAMI_VARIANT": "Go"}, want: `Unknown variant "Go" (expected go or ts)`},
		{name: "unknown option", args: []string{"--force"}, want: "Unknown option: --force"},
		{name: "missing value", args: []string{"--version"}, want: "--version requires a value"},
		{name: "unwritable install directory", args: []string{"--install-dir", "/dev/null/bin"}, want: "Cannot create install directory /dev/null/bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL).set("PUTNAMI_HARNESS_ARCH", tc.arch)
			for k, v := range tc.env {
				e.set(k, v)
			}
			res := e.run(t, tc.args...)
			if res.exitCode == 0 {
				t.Fatalf("installer accepted it:\n%s", res.output())
			}
			if !res.contains(tc.want) {
				t.Fatalf("output does not contain %q:\n%s", tc.want, res.output())
			}
			if res.contains("Downloading") {
				t.Fatalf("installer downloaded before refusing:\n%s", res.output())
			}
		})
	}
}

// An older putnami earlier on PATH is reported as the shadow it is.
func TestInstallPS1ReportsAShadowingPutnami(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
	shadow := filepath.Join(e.fakeBin, "putnami")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, "--no-agent-hosts")
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	want := "putnami currently runs " + shadow + ", not the build just installed (" + filepath.Join(e.binDir(), "putnami.exe") + ")."
	if !res.contains(want) {
		t.Fatalf("the shadowing putnami was not reported:\n%s", res.output())
	}
}

func TestInstallPS1RegistersAgentHostsLikeInstallSh(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
	binary := filepath.Join(e.binDir(), "putnami.exe")
	claude := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "claude", false)
	codex := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "codex", false)

	first := e.run(t)
	if first.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", first.exitCode, first.output())
	}
	if calls := readHostCalls(t, claude); !strings.Contains(calls, "mcp add --scope user --env PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.} --transport stdio putnami -- "+binary+" mcp") {
		t.Fatalf("Claude registration differs from install.sh:\n%s", calls)
	}
	if calls := readHostCalls(t, codex); !strings.Contains(calls, "mcp add putnami -- "+binary+" mcp") {
		t.Fatalf("Codex registration differs from install.sh:\n%s", calls)
	}
	for _, want := range []string{
		"Claude Code MCP configuration and connection verified for new sessions",
		"Codex MCP configuration verified for new sessions",
	} {
		if !first.contains(want) {
			t.Fatalf("output does not contain %q:\n%s", want, first.output())
		}
	}

	second := e.run(t)
	if second.exitCode != 0 {
		t.Fatalf("repeat install failed (exit %d):\n%s", second.exitCode, second.output())
	}
	for _, host := range []fakeAgentHost{claude, codex} {
		if got := strings.Count(readHostCalls(t, host), "mcp add "); got != 1 {
			t.Fatalf("%s registration count = %d across two installs, want 1", host.name, got)
		}
	}
}

func TestInstallPS1RespectsExistingAgentHostDefinitions(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)

	t.Run("human definitions are preserved", func(t *testing.T) {
		t.Parallel()
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		binary := filepath.Join(e.binDir(), "putnami.exe")
		claude := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "claude", false)
		codex := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "codex", false)
		claudeHuman := "putnami:\n  Status: \u2714 Connected\n  Command: /human/claude-putnami\n  Args: mcp --custom\n  Environment:\n    HUMAN=value\n"
		codexHuman := "putnami\n  enabled: true\n  transport: stdio\n  command: /human/codex-putnami\n  args: mcp --custom\n  cwd: /human/workspace\n  env: -\n"
		if err := os.WriteFile(claude.state, []byte(claudeHuman), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(codex.state, []byte(codexHuman), 0o644); err != nil {
			t.Fatal(err)
		}
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		for _, host := range []fakeAgentHost{claude, codex} {
			if strings.Contains(readHostCalls(t, host), "mcp add ") {
				t.Fatalf("installer replaced the existing %s definition", host.name)
			}
		}
		for _, want := range []string{"human configuration was preserved", "human TOML and launcher were preserved"} {
			if !res.contains(want) {
				t.Fatalf("output does not contain %q:\n%s", want, res.output())
			}
		}
	})

	t.Run("a reformatted Codex definition is recognized", func(t *testing.T) {
		t.Parallel()
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		binary := filepath.Join(e.binDir(), "putnami.exe")
		codex := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "codex", false)
		reformatted := "{\n    \"enabled\": true,\n    \"name\": \"putnami\",\n    \"transport\": {\n        \"args\": [ \"mcp\" ],\n        \"command\": \"" + binary + "\",\n        \"cwd\": null,\n        \"type\": \"stdio\",\n        \"unreleased_future_field\": \"whatever\"\n    }\n}\n"
		if err := os.WriteFile(codex.state, []byte("putnami\n  enabled: true\n  transport: stdio\n  command: "+binary+"\n  args: mcp\n  cwd: -\n  env: -\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(codex.jsonState, []byte(reformatted), 0o644); err != nil {
			t.Fatal(err)
		}
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if !res.contains("Codex MCP configuration verified for new sessions") {
			t.Fatalf("a reformatted Putnami definition was not recognized:\n%s", res.output())
		}
	})

	t.Run("a lookalike Codex definition is not verified", func(t *testing.T) {
		t.Parallel()
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		binary := filepath.Join(e.binDir(), "putnami.exe")
		codex := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "codex", false)
		lookalike := "{\"name\":\"putnami\",\"enabled\":true,\"transport\":{\"type\":\"stdio\",\"command\":\"" + binary + "\",\"args\":[\"mcp\",\"--workspace\",\"/first\"],\"env\":null,\"env_vars\":[],\"cwd\":null},\"enabled_tools\":null,\"disabled_tools\":null}\n"
		if err := os.WriteFile(codex.state, []byte("putnami\n  enabled: true\n  transport: stdio\n  command: "+binary+"\n  args: mcp\n  cwd: -\n  env: -\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(codex.jsonState, []byte(lookalike), 0o644); err != nil {
			t.Fatal(err)
		}
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
		}
		if res.contains("Codex MCP configuration verified") || !res.contains("human TOML and launcher were preserved") {
			t.Fatalf("a lookalike definition was treated as the installer's own:\n%s", res.output())
		}
	})

	t.Run("a host policy refusal leaves the install usable", func(t *testing.T) {
		t.Parallel()
		e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
		binary := filepath.Join(e.binDir(), "putnami.exe")
		writeFakeAgentHost(t, e.state, e.fakeBin, binary, "claude", true)
		writeFakeAgentHost(t, e.state, e.fakeBin, binary, "codex", true)
		res := e.run(t)
		if res.exitCode != 0 {
			t.Fatalf("a host refusal failed the install (exit %d):\n%s", res.exitCode, res.output())
		}
		for _, host := range []string{"Claude Code", "Codex"} {
			if !res.contains("Could not register Putnami with " + host) {
				t.Fatalf("missing the %s policy warning:\n%s", host, res.output())
			}
		}
	})
}

func TestInstallPS1SkipsAgentHostsWhenDeclined(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	for name, decline := range map[string]func(e *psEnv) psResult{
		"flag":        func(e *psEnv) psResult { return e.run(t, "--no-agent-hosts") },
		"environment": func(e *psEnv) psResult { return e.set("PUTNAMI_NO_AGENT_HOSTS", "1").run(t) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newPSEnv(t).set("PUTNAMI_REGISTRY_URL", registry.URL)
			binary := filepath.Join(e.binDir(), "putnami.exe")
			claude := writeFakeAgentHost(t, e.state, e.fakeBin, binary, "claude", false)
			res := decline(e)
			if res.exitCode != 0 {
				t.Fatalf("declined install failed (exit %d):\n%s", res.exitCode, res.output())
			}
			if !res.contains("Skipping agent-host registration (PUTNAMI_NO_AGENT_HOSTS/--no-agent-hosts).") {
				t.Fatalf("the declined registration was not reported:\n%s", res.output())
			}
			if data, err := os.ReadFile(claude.log); err == nil && len(data) != 0 {
				t.Fatalf("declined install still called claude: %s", data)
			}
		})
	}
}

// The one-liner a user runs: `irm` returns the script served as
// application/octet-stream as text, and `iex` runs it. On a host that is not
// Windows it refuses, and in every case the caller's session keeps no
// function, variable, preference or strict mode from the script.
func TestInstallPS1OneLinerRunsAndLeavesTheSessionClean(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	e := newPSEnv(t)
	oneLiner := "irm " + registry.URL + "/install.ps1 | iex"

	res := e.exec(t, "-Command", oneLiner)
	if res.exitCode == 0 {
		t.Fatalf("a failed install must end `powershell -c` with a non-zero exit:\n%s", res.output())
	}
	if !res.contains("install.ps1 installs the Putnami CLI on Windows only.") || !res.contains("curl -fsSL https://putnami.dev/install.sh | bash") {
		t.Fatalf("the served script did not run to its platform refusal:\n%s", res.output())
	}

	probe := "try { " + oneLiner + " } catch { }; " +
		"'function=' + [bool](Get-Command Invoke-PutnamiInstaller -ErrorAction SilentlyContinue); " +
		"'variable=' + [bool](Get-Variable BinaryName -ErrorAction SilentlyContinue); " +
		"'preference=' + $ErrorActionPreference; " +
		"'strict=' + ($null -eq $variableNobodyDefined)"
	res = e.exec(t, "-Command", probe)
	for _, want := range []string{"function=False", "variable=False", "preference=Continue", "strict=True"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("the caller's session changed; stdout lacks %q:\n%s", want, res.output())
		}
	}
}

// `irm | iex` runs the script with no arguments, whatever the caller's own
// arguments are, so flags reach it through the script block form the header
// and --help document.
func TestInstallPS1TakesFlagsThroughTheScriptBlockForm(t *testing.T) {
	t.Parallel()
	registry, _, _ := defaultPS1Registry(t)
	e := newPSEnv(t)
	scriptBlock := "& ([scriptblock]::Create((irm " + registry.URL + "/install.ps1)))"

	res := e.exec(t, "-Command", scriptBlock+" --help")
	if res.exitCode != 0 || !res.contains("Usage: irm https://putnami.dev/install.ps1 | iex") {
		t.Fatalf("--help through the script block form exited %d:\n%s", res.exitCode, res.output())
	}
	res = e.exec(t, "-Command", scriptBlock+" --bogus")
	if res.exitCode == 0 || !res.contains("Unknown option: --bogus") {
		t.Fatalf("an unknown flag through the script block form exited %d:\n%s", res.exitCode, res.output())
	}

	res = e.exec(t, "-Command", "function Install-FromSetup { irm "+registry.URL+"/install.ps1 | iex }; Install-FromSetup --bogus")
	if res.contains("Unknown option") || !res.contains("install.ps1 installs the Putnami CLI on Windows only.") {
		t.Fatalf("the one-liner took the arguments of the function that ran it:\n%s", res.output())
	}
}

func TestInstallPS1HelpDocumentsTheContract(t *testing.T) {
	t.Parallel()
	e := newPSEnv(t)
	res := e.exec(t, "-File", installPS1Path(t), "--help")
	if res.exitCode != 0 {
		t.Fatalf("--help exited %d:\n%s", res.exitCode, res.output())
	}
	for _, want := range []string{
		"irm https://putnami.dev/install.ps1 | iex",
		"$env:PUTNAMI_VERSION = 'canary'; irm https://putnami.dev/install.ps1 | iex",
		"PUTNAMI_UNSAFE_INSTALL=1",
		"PUTNAMI_ALLOW_INSECURE_REGISTRY=1",
		"never escalates privileges",
		"Windows 10 version 1803 or later and Windows 11, on amd64",
		"curl -fsSL https://putnami.dev/install.sh | bash",
	} {
		if !res.contains(want) {
			t.Fatalf("--help does not mention %q:\n%s", want, res.output())
		}
	}
}
