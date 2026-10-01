// Package installscript holds executable evidence for scripts/install.sh, the
// public curl-pipe installer served at https://putnami.dev/install.sh.
//
// The installer is bash, so the evidence runs bash: every test drives the real
// script against an httptest registry that streams a stub "binary" (a tiny
// shell script with a parseable --version line). What is pinned here is the
// install trust model specified in doc/22-installing-the-cli.md and decided in
// doc/adr/0012-install-trust-and-release-smoke.md: a download is verified
// against the digest the registry advertises for it, an unverifiable download is
// refused, the installed binary must be the version the channel resolved,
// unsupported platforms are refused rather than warned about, and the installer
// never escalates privileges.
package installscript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const stubVersion = "9.9.9-testbuild"

// installScriptPath returns the installer under test. It is the same file the
// site publishes at /install.sh (sites/putnami.dev/putnami.json copies it), so
// these tests exercise the exact bytes users pipe into bash.
func installScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "install.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("install.sh not found: %v", err)
	}
	return script
}

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh does not install on Windows; install_ps1_windows_test.go covers install.ps1 there")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
}

// stubCLI is a stand-in for the published binary: the installer only ever runs
// it with --version, and only to check the stamp the registry resolved.
func stubCLI(version string) []byte {
	return []byte("#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo \"Putnami   " + version + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type registryOptions struct {
	body       []byte
	integrity  string // X-Integrity value, verbatim; empty omits the header
	digest     string // RFC 9530 Digest value, verbatim; empty omits the header
	resolved   string // X-Resolved-Version; empty omits the header
	statusCode int    // defaults to 200
	commandMap string // body of /install-commands.txt; empty serves 404
}

func newRegistry(t *testing.T, opts registryOptions) *httptest.Server {
	t.Helper()
	installer, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatalf("read install script: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/install.sh" {
			serveInstallScript(w, r, installer)
			return
		}
		if r.URL.Path == "/install-commands.txt" && opts.commandMap != "" {
			_, _ = w.Write([]byte(opts.commandMap))
			return
		}
		if r.URL.Path != "/putnami/cli/download" {
			http.NotFound(w, r)
			return
		}
		// plain=1 serves the same bytes with no registry metadata at all: the
		// shape of an arbitrary URL handed to --download-url, where the digest
		// can only come from the caller.
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
	t.Cleanup(server.Close)
	return server
}

// defaultRegistry serves the stub binary with a matching digest and stamp — the
// shape of a healthy release.
func defaultRegistry(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	body := stubCLI(stubVersion)
	return newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	}), body
}

// escalators are every privilege-escalation entry point the installer must
// never reach for. The textual scan in TestInstallScriptContainsNoPrivilege-
// Escalation covers all four; faking all four here makes the runtime assertion
// cover them too, instead of proving something about sudo alone.
var escalators = []string{"sudo", "doas", "pkexec", "run0"}

type env struct {
	home             string
	installDir       string
	escalationMarker string
	pathDirs         []string
	vars             map[string]string
}

// newEnv builds a hermetic environment: a temp HOME, a PATH that contains only
// system directories the script's tools live in (never /usr/local/bin, so a
// stray symlink cannot escape the test), and a fake sudo that records any
// attempt to escalate and fails.
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	fakeBin := filepath.Join(root, "fakebin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "escalation-was-attempted")
	for _, escalator := range escalators {
		fake := "#!/bin/sh\nprintf '" + escalator + " %s\\n' \"$*\" >> " + shellQuote(marker) + "\nexit 1\n"
		if err := os.WriteFile(filepath.Join(fakeBin, escalator), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	return &env{
		home:             home,
		installDir:       filepath.Join(root, "install"),
		escalationMarker: marker,
		pathDirs:         []string{fakeBin, "/usr/bin", "/bin", "/usr/sbin", "/sbin"},
		vars:             map[string]string{},
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (e *env) set(key, value string) *env {
	e.vars[key] = value
	return e
}

func (e *env) prependPath(dir string) *env {
	e.pathDirs = append([]string{dir}, e.pathDirs...)
	return e
}

func (e *env) materialize() []string {
	out := []string{
		"PATH=" + strings.Join(e.pathDirs, string(os.PathListSeparator)),
		"HOME=" + e.home,
		// An unknown shell keeps completion generation and profile edits out of
		// the way of the behavior under test.
		"SHELL=/bin/sh",
	}
	for k, v := range e.vars {
		out = append(out, k+"="+v)
	}
	return out
}

type result struct {
	exitCode int
	output   string
}

func (r result) contains(needle string) bool {
	return strings.Contains(r.output, needle)
}

func (e *env) run(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command("bash", append([]string{installScriptPath(t)}, args...)...)
	cmd.Dir = e.home
	cmd.Env = e.materialize()
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run install.sh: %v\n%s", err, out)
		}
	}
	return result{exitCode: code, output: string(out)}
}

// assertNeverEscalated is the invariant every test shares: a curl-pipe installer
// must not prompt for a password, so it must invoke no escalator at all. Every
// name in escalators writes to the one marker, so this covers all four.
func (e *env) assertNeverEscalated(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(e.escalationMarker); err == nil {
		t.Fatalf("installer escalated privileges: %q", strings.TrimSpace(string(data)))
	}
}

func (e *env) assertNothingInstalled(t *testing.T) {
	t.Helper()
	for _, candidate := range []string{
		filepath.Join(e.installDir, "putnami"),
		filepath.Join(e.home, ".putnami", "bin", "putnami"),
	} {
		if _, err := os.Lstat(candidate); err == nil {
			t.Fatalf("a refused install left %s behind", candidate)
		}
	}
}

func TestInstallVerifiesAdvertisedDigestAndVersionStamp(t *testing.T) {
	requireBash(t)
	server, body := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains("Integrity verified (sha256:" + sha256Hex(body)) {
		t.Fatalf("installer did not report the verified digest:\n%s", res.output)
	}
	if !res.contains("Version stamp verified (" + stubVersion + ")") {
		t.Fatalf("installer did not report the verified version stamp:\n%s", res.output)
	}

	installed := filepath.Join(e.installDir, "putnami")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("binary was not installed: %v\n%s", err, res.output)
	}
	if string(got) != string(body) {
		t.Fatal("installed bytes differ from the bytes the registry served")
	}
	e.assertNeverEscalated(t)
}

func TestInstallAcceptsRFC9530DigestHeader(t *testing.T) {
	requireBash(t)
	body := stubCLI(stubVersion)
	server := newRegistry(t, registryOptions{
		body:     body,
		digest:   "sha-512=ignored, sha-256=" + sha256Hex(body),
		resolved: stubVersion,
	})
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains("Integrity verified") {
		t.Fatalf("Digest header was not used as the integrity source:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallRefusesDigestMismatch(t *testing.T) {
	requireBash(t)
	body := stubCLI(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + strings.Repeat("a", 64),
		resolved:  stubVersion,
	})
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode == 0 {
		t.Fatalf("a mismatched digest installed anyway:\n%s", res.output)
	}
	if !res.contains("Integrity check failed") {
		t.Fatalf("mismatch was not named:\n%s", res.output)
	}
	if res.contains("Integrity verified") {
		t.Fatalf("installer claimed verification for a mismatched download:\n%s", res.output)
	}
	e.assertNothingInstalled(t)
	e.assertNeverEscalated(t)
}

func TestInstallRefusesWhenNoIntegrityIsAdvertised(t *testing.T) {
	requireBash(t)
	body := stubCLI(stubVersion)
	server := newRegistry(t, registryOptions{body: body, resolved: stubVersion})
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode == 0 {
		t.Fatalf("an unverifiable download installed anyway:\n%s", res.output)
	}
	if !res.contains("did not advertise an integrity hash") || !res.contains("PUTNAMI_UNSAFE_INSTALL=1") {
		t.Fatalf("refusal did not name the cause and the override:\n%s", res.output)
	}
	e.assertNothingInstalled(t)
	e.assertNeverEscalated(t)
}

func TestInstallWithoutIntegrityRequiresExplicitOptIn(t *testing.T) {
	requireBash(t)
	body := stubCLI(stubVersion)
	server := newRegistry(t, registryOptions{body: body, resolved: stubVersion})
	e := newEnv(t).
		set("PUTNAMI_REGISTRY_URL", server.URL).
		set("PUTNAMI_UNSAFE_INSTALL", "1")

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("opt-in install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains("without integrity verification") {
		t.Fatalf("the unverified install was not announced loudly:\n%s", res.output)
	}
	if res.contains("Integrity verified") {
		t.Fatalf("installer claimed verification it never performed:\n%s", res.output)
	}
	if _, err := os.Stat(filepath.Join(e.installDir, "putnami")); err != nil {
		t.Fatalf("opt-in install did not install the binary: %v", err)
	}
	e.assertNeverEscalated(t)
}

func TestInstallRefusesStaleVersionStamp(t *testing.T) {
	requireBash(t)
	body := stubCLI(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  "1.0.0-freshtag",
	})
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode == 0 {
		t.Fatalf("a stale build installed under a fresh tag:\n%s", res.output)
	}
	if !res.contains("1.0.0-freshtag") || !res.contains(stubVersion) {
		t.Fatalf("refusal did not name both versions:\n%s", res.output)
	}
	e.assertNothingInstalled(t)
	e.assertNeverEscalated(t)
}

func TestInstallRefusesUnwritableInstallDirectoryBeforeDownloading(t *testing.T) {
	requireBash(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write to any directory, so the refusal cannot be exercised")
	}
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(locked, "bin")

	res := e.run(t, "--install-dir", target)
	if res.exitCode == 0 {
		t.Fatalf("install succeeded into an unwritable location:\n%s", res.output)
	}
	if !res.contains(target) {
		t.Fatalf("error did not name the directory:\n%s", res.output)
	}
	if !res.contains("--install-dir") || !res.contains("PUTNAMI_INSTALL_DIR") {
		t.Fatalf("error did not name the remedy:\n%s", res.output)
	}
	// The check runs before the download, so the registry is never contacted.
	if res.contains("Downloaded") {
		t.Fatalf("installer downloaded before validating the install location:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallRefusesUnsupportedPlatforms(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-shells-get-the-powershell-installer", "install-sh-sends-a-windows-shell-to-install-ps1")
	requireBash(t)
	// A Windows shell gets the two forms that install there: the one-liner
	// typed in a PowerShell window, and for cmd.exe or this shell a download
	// run with -File. Never the one-liner passed to powershell on a command
	// line, which Microsoft Defender blocks as Trojan:Win32/Commando.A!ml.
	windowsRedirect := []string{
		"install.sh does not install on Windows",
		"In a PowerShell window: irm https://putnami.dev/install.ps1 | iex",
		"curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1 && powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1",
	}
	tests := []struct {
		name    string
		osName  string
		machine string
		wants   []string
		never   []string
	}{
		{"windows git bash", "MINGW64_NT-10.0-19045", "x86_64", windowsRedirect, []string{`-c "irm`}},
		{"windows msys2", "MSYS_NT-10.0-22631", "x86_64", windowsRedirect, []string{`-c "irm`}},
		{"windows cygwin", "CYGWIN_NT-10.0-19045", "x86_64", windowsRedirect, []string{`-c "irm`}},
		{"unknown os", "Plan9", "x86_64", []string{"Unsupported operating system", "darwin (macOS) and linux"}, nil},
		{"unknown arch", "Linux", "riscv64", []string{"Unsupported architecture: riscv64", "amd64 or arm64"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := defaultRegistry(t)
			e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

			fake := filepath.Join(t.TempDir(), "unamebin")
			if err := os.MkdirAll(fake, 0o755); err != nil {
				t.Fatal(err)
			}
			uname := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  -s) echo %s ;;\n  -m) echo %s ;;\n  *) echo unknown ;;\nesac\n",
				shellQuote(tc.osName), shellQuote(tc.machine))
			if err := os.WriteFile(filepath.Join(fake, "uname"), []byte(uname), 0o755); err != nil {
				t.Fatal(err)
			}
			e.prependPath(fake)

			res := e.run(t, "--install-dir", e.installDir)
			if res.exitCode == 0 {
				t.Fatalf("unsupported platform installed anyway:\n%s", res.output)
			}
			for _, want := range tc.wants {
				if !res.contains(want) {
					t.Fatalf("error did not mention %q:\n%s", want, res.output)
				}
			}
			for _, unwanted := range tc.never {
				if res.contains(unwanted) {
					t.Fatalf("error mentions %q:\n%s", unwanted, res.output)
				}
			}
			e.assertNothingInstalled(t)
			e.assertNeverEscalated(t)
		})
	}
}

func TestInstallRefusesPlaintextNonLoopbackRegistry(t *testing.T) {
	requireBash(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", "http://registry.invalid")

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode == 0 {
		t.Fatalf("plaintext registry was accepted:\n%s", res.output)
	}
	if !res.contains("plaintext http://") {
		t.Fatalf("refusal did not cite the plaintext rule:\n%s", res.output)
	}
	e.assertNothingInstalled(t)
	e.assertNeverEscalated(t)
}

func TestInstallAcceptsPlaintextRegistryWithExplicitOptIn(t *testing.T) {
	requireBash(t)
	e := newEnv(t).
		set("PUTNAMI_REGISTRY_URL", "http://registry.invalid").
		set("PUTNAMI_ALLOW_INSECURE_REGISTRY", "1")

	res := e.run(t, "--install-dir", e.installDir)
	// The host does not exist, so the run still fails — but it must fail on the
	// download, not on the scheme.
	if res.contains("plaintext http://") {
		t.Fatalf("opt-in did not disable the scheme check:\n%s", res.output)
	}
	if !res.contains("No compatible release artifact found") {
		t.Fatalf("expected a download failure after the opt-in:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallAcceptsLoopbackHTTPRegistryWithoutOptIn(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	// httptest binds 127.0.0.1 over plain http; loopback is the routine dev case
	// and needs no override.
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("loopback http registry was refused (exit %d):\n%s", res.exitCode, res.output)
	}
	e.assertNeverEscalated(t)
}

func TestDownloadURLRequiresAnExpectedDigest(t *testing.T) {
	requireBash(t)
	server, body := defaultRegistry(t)
	// An arbitrary asset URL carries no registry metadata, so the digest has to
	// come from the caller.
	plainURL := server.URL + "/putnami/cli/download?channel=latest&plain=1"
	metadataURL := server.URL + "/putnami/cli/download?channel=latest"

	t.Run("refused without --sha256", func(t *testing.T) {
		e := newEnv(t)
		res := e.run(t, "--install-dir", e.installDir, "--download-url", plainURL)
		if res.exitCode == 0 {
			t.Fatalf("direct download installed without a digest:\n%s", res.output)
		}
		if !res.contains("did not advertise an integrity hash") || !res.contains("--sha256") {
			t.Fatalf("refusal did not name the cause and --sha256:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
		e.assertNeverEscalated(t)
	})

	t.Run("installs with a matching --sha256", func(t *testing.T) {
		e := newEnv(t)
		res := e.run(t, "--install-dir", e.installDir, "--download-url", plainURL, "--sha256", sha256Hex(body))
		if res.exitCode != 0 {
			t.Fatalf("direct download with a digest failed (exit %d):\n%s", res.exitCode, res.output)
		}
		if !res.contains("Integrity verified (sha256:" + sha256Hex(body) + ")") {
			t.Fatalf("caller-supplied digest was not verified:\n%s", res.output)
		}
		e.assertNeverEscalated(t)
	})

	t.Run("refused when --sha256 does not match the bytes", func(t *testing.T) {
		e := newEnv(t)
		res := e.run(t, "--install-dir", e.installDir, "--download-url", plainURL, "--sha256", strings.Repeat("b", 64))
		if res.exitCode == 0 {
			t.Fatalf("a mismatched caller digest installed anyway:\n%s", res.output)
		}
		if !res.contains("Integrity check failed") {
			t.Fatalf("mismatch was not named:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
		e.assertNeverEscalated(t)
	})

	t.Run("refused when --sha256 disagrees with an advertised digest", func(t *testing.T) {
		e := newEnv(t)
		res := e.run(t, "--install-dir", e.installDir, "--download-url", metadataURL, "--sha256", strings.Repeat("b", 64))
		if res.exitCode == 0 {
			t.Fatalf("conflicting digests installed anyway:\n%s", res.output)
		}
		if !res.contains("does not match the one the download URL advertised") {
			t.Fatalf("refusal did not name the conflict:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
		e.assertNeverEscalated(t)
	})

	t.Run("--sha256 must be a hex SHA-256", func(t *testing.T) {
		e := newEnv(t)
		res := e.run(t, "--install-dir", e.installDir, "--download-url", plainURL, "--sha256", "not-a-digest")
		if res.exitCode == 0 {
			t.Fatalf("a malformed digest was accepted:\n%s", res.output)
		}
		if !res.contains("expected 64-character hex SHA-256") {
			t.Fatalf("refusal did not explain the expected form:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
		e.assertNeverEscalated(t)
	})
}

func TestInstallLinksIntoAWritablePathDirectoryWithoutSudo(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	localBin := filepath.Join(e.home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	e.prependPath(localBin)

	// No --install-dir: the default versioned layout under ~/.putnami/bin is not
	// on PATH, so the installer must make the command reachable another way.
	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}

	link := filepath.Join(localBin, "putnami")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("installer did not link into the writable PATH directory: %v\n%s", err, res.output)
	}
	if want := filepath.Join(e.home, ".putnami", "bin", "putnami"); target != want {
		t.Fatalf("link target = %q, want %q", target, want)
	}
	if !res.contains("Linked " + link) {
		t.Fatalf("installer did not report the link:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallCreatesMissingUserPathDirectoryForSameShellDiscovery(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	// A fresh HOME has no ~/.local/bin yet, even when the login environment
	// already includes it in PATH. The exact next command in the golden path has
	// no shell restart or export step, so the installer must create this known
	// user-owned directory and link into it.
	localBin := filepath.Join(e.home, ".local", "bin")
	e.prependPath(localBin)

	res := e.run(t)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	link := filepath.Join(localBin, "putnami")
	if _, err := os.Readlink(link); err != nil {
		t.Fatalf("installer did not create the missing user PATH directory and link: %v\n%s", err, res.output)
	}
	if !res.contains("Linked "+link) || !res.contains("putnami now runs "+link) {
		t.Fatalf("installer did not prove immediate same-shell discovery:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallPrintsAndRecordsThePathLineWhenNothingIsLinkable(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}

	exportLine := fmt.Sprintf("export PATH=\"%s:$PATH\"", e.installDir)
	if !res.contains(exportLine) {
		t.Fatalf("installer did not print the exact PATH line to run:\n%s", res.output)
	}
	if res.contains("is already on your PATH") {
		t.Fatalf("installer claimed the command was reachable when it was not:\n%s", res.output)
	}

	profile := filepath.Join(e.home, ".profile")
	first, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("installer did not record the PATH line for future shells: %v", err)
	}
	if !strings.Contains(string(first), exportLine) {
		t.Fatalf("profile does not contain %q:\n%s", exportLine, first)
	}

	// Re-running must not append the same line twice.
	if res := e.run(t, "--install-dir", e.installDir); res.exitCode != 0 {
		t.Fatalf("second install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	second, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(second), exportLine); got != 1 {
		t.Fatalf("PATH line appears %d times after two installs, want 1:\n%s", got, second)
	}
	e.assertNeverEscalated(t)
}

// nextLines returns the commands the installer prints under "Next:", in order.
func nextLines(output string) []string {
	_, after, found := strings.Cut(output, "\nNext:\n")
	if !found {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(after, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

// A shell that cannot reach the command reads the PATH line as its first next
// step; a shell that already reaches it reads no PATH line at all.
func TestInstallFooterStartsWithThePathLineOnlyWhenTheCommandIsNotReachable(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)

	unreachable := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	res := unreachable.run(t, "--install-dir", unreachable.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	exportLine := fmt.Sprintf("export PATH=\"%s:$PATH\"", unreachable.installDir)
	next := nextLines(res.output)
	if len(next) < 2 || next[0] != exportLine || next[1] != "putnami --help" {
		t.Fatalf("Next: lines = %q, want %q then %q first:\n%s", next, exportLine, "putnami --help", res.output)
	}

	reachable := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	if err := os.MkdirAll(reachable.installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reachable.prependPath(reachable.installDir)
	res = reachable.run(t, "--install-dir", reachable.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	next = nextLines(res.output)
	if len(next) == 0 || next[0] != "putnami --help" {
		t.Fatalf("Next: lines = %q, want %q first:\n%s", next, "putnami --help", res.output)
	}
	for _, line := range next {
		if strings.Contains(line, "PATH") {
			t.Fatalf("Next: gives a PATH line to a shell that already reaches the command: %q", line)
		}
	}
}

func TestInstallReportsAnInstallDirectoryAlreadyOnPath(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	if err := os.MkdirAll(e.installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.prependPath(e.installDir)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains(e.installDir + " is already on your PATH") {
		t.Fatalf("installer did not report the already-reachable directory:\n%s", res.output)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".profile")); err == nil {
		t.Fatal("installer edited a shell profile it did not need to edit")
	}
	e.assertNeverEscalated(t)
}

// The published entry point is `curl ... | bash -s -- <args>`, where the script
// arrives on stdin. A guard that assumed a file path would turn every public
// install into a silent no-op.
func TestInstallRunsWhenPipedIntoBash(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-s", "--", "--install-dir", e.installDir)
	cmd.Dir = e.home
	cmd.Env = e.materialize()
	cmd.Stdin = strings.NewReader(string(script))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("piped install failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Integrity verified") {
		t.Fatalf("piped install did not verify the download:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.installDir, "putnami")); err != nil {
		t.Fatalf("piped install did not install the binary: %v\n%s", err, out)
	}
	e.assertNeverEscalated(t)
}

// The installer's only privilege model is "write where the user already can".
// A grep is the cheapest way to keep a `sudo` from reappearing in a branch no
// test happens to reach.
//
// Comments and quoted strings are excluded: the script tells the user in prose
// that it never escalates, and a scan that cannot tell prose from a command
// would force that promise out of the output.
func TestInstallScriptContainsNoPrivilegeEscalation(t *testing.T) {
	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(script), "\n")
	code := 0
	for i, line := range lines {
		bare := stripCommentsAndStrings(line)
		if strings.TrimSpace(bare) != "" {
			code++
		}
		for _, banned := range []string{"sudo", "doas", "pkexec", "run0"} {
			if containsWord(bare, banned) {
				t.Fatalf("install.sh:%d escalates privileges: %s", i+1, strings.TrimSpace(line))
			}
		}
	}
	// Non-vacuity: a scan that saw no executable lines proves nothing.
	if code < 100 {
		t.Fatalf("only %d executable lines survived stripping; the scan is vacuous", code)
	}
}

// stripCommentsAndStrings removes single-quoted spans, double-quoted spans, and
// a trailing comment from one shell line, leaving the part bash would execute as
// words. It is deliberately simple: the installer contains no quoted string that
// spans lines except heredoc-free literal blocks, which carry no commands.
func stripCommentsAndStrings(line string) string {
	var out strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			out.WriteByte(' ')
		case r == '#':
			return out.String()
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// containsWord reports whether word appears in s delimited by non-word bytes, so
// "pseudo-terminal" does not read as "sudo".
func containsWord(s, word string) bool {
	isWord := func(b byte) bool {
		return b == '_' || b == '-' || b == '.' ||
			(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		if i > 0 && isWord(s[i-1]) {
			continue
		}
		if end := i + len(word); end < len(s) && isWord(s[end]) {
			continue
		}
		return true
	}
	return false
}

// The fake escalators the environment installs are the only thing standing
// behind every assertNeverEscalated call. If the marker mechanism stopped
// working, every one of those assertions would pass while proving nothing. Each
// of the four is checked, so a fake that was never written stays visible.
func TestFakeEscalatorsRecordAnEscalationAttempt(t *testing.T) {
	requireBash(t)

	for _, escalator := range escalators {
		t.Run(escalator, func(t *testing.T) {
			e := newEnv(t)

			cmd := exec.Command("bash", "-c", escalator+" ln -s /nowhere /nowhere-else")
			cmd.Env = e.materialize()
			if err := cmd.Run(); err == nil {
				t.Fatalf("the fake %s succeeded; it must fail so a real escalation cannot be mistaken for one", escalator)
			}

			data, err := os.ReadFile(e.escalationMarker)
			if err != nil {
				t.Fatalf("the fake %s did not record the attempt: %v", escalator, err)
			}
			if !strings.Contains(string(data), escalator) {
				t.Fatalf("marker does not name the escalator: %q", data)
			}
			if !strings.Contains(string(data), "ln -s") {
				t.Fatalf("marker does not record the arguments: %q", data)
			}
		})
	}
}

// Every reference in the tree points at putnami.dev/install.sh; put.putnami.dev
// is the artifact registry and does not serve the script.
func TestInstallScriptAdvertisesTheServedURL(t *testing.T) {
	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "put.putnami.dev/install.sh") {
		t.Fatal("install.sh advertises put.putnami.dev/install.sh, which serves no script")
	}
	if !strings.Contains(string(script), "https://putnami.dev/install.sh") {
		t.Fatal("install.sh does not advertise https://putnami.dev/install.sh")
	}
}

// Backward compatibility: --variant and a semver --version keep producing the
// versioned ~/.putnami/bin layout `putnami version use` and `upgrade` manage.
func TestInstallHonorsVariantAndExactVersion(t *testing.T) {
	requireBash(t)
	body := stubCLI("1.2.3")
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  "1.2.3",
	})
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--variant", "ts", "--version", "1.2.3")
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}

	binDir := filepath.Join(e.home, ".putnami", "bin")
	// normalize_tag v-prefixes a bare semver, matching `putnami version use`.
	if _, err := os.Stat(filepath.Join(binDir, "putnami-ts-v1.2.3")); err != nil {
		t.Fatalf("versioned binary was not installed: %v\n%s", err, res.output)
	}
	target, err := os.Readlink(filepath.Join(binDir, "putnami"))
	if err != nil {
		t.Fatalf("active symlink was not created: %v\n%s", err, res.output)
	}
	if target != "putnami-ts-v1.2.3" {
		t.Fatalf("active symlink = %q, want a relative putnami-ts-v1.2.3", target)
	}
	e.assertNeverEscalated(t)
}

// Without a SHA-256 tool the installer cannot compare anything, so it refuses
// rather than printing an unearned success.
func TestInstallRefusesWithoutASHA256Tool(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)

	// A PATH holding only what the script needs before the check: uname, curl,
	// and tar. Neither sha256sum nor shasum is reachable.
	toolDir := filepath.Join(t.TempDir(), "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"uname", "curl", "tar"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
		if err := os.Symlink(real, filepath.Join(toolDir, tool)); err != nil {
			t.Fatal(err)
		}
	}

	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	e.pathDirs = []string{toolDir}

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode == 0 {
		t.Fatalf("install succeeded with no way to verify the download:\n%s", res.output)
	}
	if !res.contains("sha256sum or shasum is required") {
		t.Fatalf("refusal did not name the missing tool:\n%s", res.output)
	}
	if !res.contains("PUTNAMI_UNSAFE_INSTALL=1") {
		t.Fatalf("refusal did not name the documented override:\n%s", res.output)
	}
	if res.contains("Downloaded") {
		t.Fatalf("installer downloaded before checking it could verify:\n%s", res.output)
	}
	e.assertNothingInstalled(t)
}

// The release smoke and CI logs read this output with grep. An escape sequence in
// the middle of "Integrity verified" makes a correct installer look broken.
func TestInstallOutputCarriesNoEscapeSequencesWhenPiped(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if strings.Contains(res.output, "\x1b[") {
		t.Fatalf("piped output contains ANSI escapes:\n%q", res.output)
	}
}

// The release smoke greps the install log for the installer's own words. Those
// two files are edited independently, so the literals are pinned against a real
// run rather than trusted.
func TestReleaseSmokeGrepsStringsTheInstallerPrints(t *testing.T) {
	requireBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	smoke, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "smoke-check-release.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// Collect every literal the smoke greps out of the installer's log.
	var literals []string
	for _, line := range strings.Split(string(smoke), "\n") {
		if !strings.Contains(line, `"$install_log"`) || !strings.Contains(line, "grep") {
			continue
		}
		start := strings.Index(line, "'")
		end := strings.LastIndex(line, "'")
		if start < 0 || end <= start {
			continue
		}
		literals = append(literals, line[start+1:end])
	}
	if len(literals) < 2 {
		t.Fatalf("found %d install-log greps in the smoke; the drift guard is vacuous", len(literals))
	}

	body := stubCLI(stubVersion)
	verified := newEnv(t).set("PUTNAMI_REGISTRY_URL", newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	}).URL)
	unverified := newEnv(t).
		set("PUTNAMI_REGISTRY_URL", newRegistry(t, registryOptions{body: body, resolved: stubVersion}).URL).
		set("PUTNAMI_UNSAFE_INSTALL", "1")

	outputs := []string{
		verified.run(t, "--install-dir", verified.installDir).output,
		unverified.run(t, "--install-dir", unverified.installDir).output,
	}
	for _, literal := range literals {
		found := false
		for _, out := range outputs {
			if strings.Contains(out, literal) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the smoke greps %q, which the installer never prints", literal)
		}
	}
}

// --help is the only documentation a curl-pipe user sees before running the
// script, so it names the served URL, the matrix, and the privilege promise.
func TestHelpDocumentsTheContract(t *testing.T) {
	requireBash(t)
	res := newEnv(t).run(t, "--help")
	if res.exitCode != 0 {
		t.Fatalf("--help exited %d:\n%s", res.exitCode, res.output)
	}
	for _, want := range []string{
		"https://putnami.dev/install.sh",
		"--install-dir",
		"--variant",
		"--download-url",
		"--sha256",
		"PUTNAMI_UNSAFE_INSTALL=1",
		"PUTNAMI_ALLOW_INSECURE_REGISTRY=1",
		"never escalates privileges",
		"darwin (macOS) and linux, on amd64 or arm64",
		"install.sh?run=<command>",
		"--run <command>",
		"PUTNAMI_RUN",
		"PUTNAMI_COMMAND_MAP_URL",
		"https://putnami.dev/install-commands.txt",
		"irm https://putnami.dev/install.ps1 | iex",
	} {
		if !res.contains(want) {
			t.Fatalf("--help does not mention %q:\n%s", want, res.output)
		}
	}
}

// The download endpoint is shared with `putnami upgrade`, which sends
// runtime.GOARCH. One client speaking a private dialect is how a platform
// silently stops being covered.
func TestInstallScriptSpeaksGOARCHVocabulary(t *testing.T) {
	requireBash(t)
	script := installScriptPath(t)
	for machine, want := range map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"arm64":   "arm64",
		"aarch64": "arm64",
		"riscv64": "unknown",
	} {
		fake := filepath.Join(t.TempDir(), "unamebin")
		if err := os.MkdirAll(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		uname := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  -m) echo %s ;;\n  *) echo Linux ;;\nesac\n", shellQuote(machine))
		if err := os.WriteFile(filepath.Join(fake, "uname"), []byte(uname), 0o755); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("bash", "-c",
			`eval "$(sed -n '/^detect_arch()/,/^}/p' "$1")"; detect_arch`, "bash", script)
		cmd.Env = []string{"PATH=" + fake + string(os.PathListSeparator) + "/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("detect_arch for %s failed: %v\n%s", machine, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("detect_arch(%s) = %q, want %q", machine, got, want)
		}
	}
}

func assertDirEmpty(t *testing.T, dir string, context string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	t.Fatalf("installer leaked %d temporary entr(ies) in %s: %v\n%s", len(entries), dir, names, context)
}

// A successful install must leave no scratch directory behind. The EXIT trap
// expands $TEMP_DIR when the shell exits, which on success is after main has
// returned — so declaring it `local` made the trap run `rm -rf ""` and leak the
// downloaded binary, the response headers and curl's stderr on every install.
// Refusals cleaned up correctly, because `exit` fires while main is still on
// the stack, which is why only the success path needed this test.
func TestInstallLeavesNoTemporaryDirectoryBehind(t *testing.T) {
	requireBash(t)

	t.Run("on success", func(t *testing.T) {
		server, _ := defaultRegistry(t)
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
		scratch := t.TempDir()
		e.set("TMPDIR", scratch)

		res := e.run(t, "--install-dir", e.installDir)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
		}
		assertDirEmpty(t, scratch, res.output)
	})

	t.Run("on refusal", func(t *testing.T) {
		body := stubCLI(stubVersion)
		server := newRegistry(t, registryOptions{
			body:      body,
			integrity: "sha256:" + strings.Repeat("a", 64),
			resolved:  stubVersion,
		})
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
		scratch := t.TempDir()
		e.set("TMPDIR", scratch)

		res := e.run(t, "--install-dir", e.installDir)
		if res.exitCode == 0 {
			t.Fatalf("a digest mismatch must refuse:\n%s", res.output)
		}
		assertDirEmpty(t, scratch, res.output)
	})
}

// PUTNAMI_REGISTRY_URL is allowed to carry credentials, and the release smoke
// tails this script's log straight into CI output. No line may echo the
// userinfo back, on the success path or on any refusal.
func TestInstallRedactsRegistryCredentials(t *testing.T) {
	requireBash(t)
	const secret = "s3cr3t-token"

	t.Run("on the download path", func(t *testing.T) {
		server, _ := defaultRegistry(t)
		withCredentials := strings.Replace(server.URL, "http://", "http://user:"+secret+"@", 1)
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", withCredentials)

		res := e.run(t, "--install-dir", e.installDir)
		if res.exitCode != 0 {
			t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
		}
		if strings.Contains(res.output, secret) {
			t.Fatalf("registry credentials reached stdout:\n%s", res.output)
		}
		if !res.contains("***@") {
			t.Fatalf("userinfo was dropped rather than redacted to ***@:\n%s", res.output)
		}
	})

	t.Run("on a rejected scheme", func(t *testing.T) {
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", "ftp://user:"+secret+"@example.com")

		res := e.run(t, "--install-dir", e.installDir)
		if res.exitCode == 0 {
			t.Fatalf("an unsupported scheme must be refused:\n%s", res.output)
		}
		if strings.Contains(res.output, secret) {
			t.Fatalf("registry credentials reached stdout on the error path:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
	})
}

// The variant becomes part of the installed filename (putnami-<variant>-<tag>),
// so an unchecked value installs a binary under a name neither the symlink nor
// `putnami version use` resolves. The flag and the environment variable are held
// to the same rule.
func TestInstallRejectsUnknownVariant(t *testing.T) {
	requireBash(t)

	t.Run("via the flag", func(t *testing.T) {
		server, _ := defaultRegistry(t)
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)

		res := e.run(t, "--install-dir", e.installDir, "--variant", "nope")
		if res.exitCode == 0 {
			t.Fatalf("an unknown --variant must be refused:\n%s", res.output)
		}
		if !res.contains("expected go or ts") {
			t.Fatalf("the refusal does not name the accepted values:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
	})

	t.Run("via the environment", func(t *testing.T) {
		server, _ := defaultRegistry(t)
		e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL).set("PUTNAMI_VARIANT", "nope")

		res := e.run(t, "--install-dir", e.installDir)
		if res.exitCode == 0 {
			t.Fatalf("an unknown PUTNAMI_VARIANT must be refused:\n%s", res.output)
		}
		e.assertNothingInstalled(t)
	})
}

// Completions are regenerated on every install, including re-installs. Writing
// the generator's stdout straight into the completion file truncates it before
// the binary runs, so a generator that fails leaves the user with nothing where
// a working file used to be.
func TestInstallKeepsWorkingCompletionsWhenRegenerationFails(t *testing.T) {
	requireBash(t)

	body := []byte("#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo \"Putnami   " + stubVersion + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"completion\" ]; then\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n")
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL).set("SHELL", "/bin/fish")
	completions := filepath.Join(e.home, ".config", "fish", "completions", "putnami.fish")
	if err := os.MkdirAll(filepath.Dir(completions), 0o755); err != nil {
		t.Fatal(err)
	}
	const working = "# completions that already work\n"
	if err := os.WriteFile(completions, []byte(working), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}

	got, err := os.ReadFile(completions)
	if err != nil {
		t.Fatalf("a failed regeneration removed completions that worked: %v\n%s", err, res.output)
	}
	if string(got) != working {
		t.Fatalf("a failed regeneration replaced working completions with %q\n%s", got, res.output)
	}
}
