package installscript

// Evidence for scripts/smoke-check-release.ps1, the windows/amd64 port of the
// release smoke. The static tests read both scripts and run on every host, so
// the port cannot drift from smoke-check-release.sh without a failing test on
// macOS and Linux. smoke_ps1_windows_test.go runs the script unmodified through
// Windows PowerShell 5.1 on a Windows host.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/git"
)

func smokePS1Path(t *testing.T) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(smokeScriptPath(t)), "smoke-check-release.ps1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("smoke-check-release.ps1 not found: %v", err)
	}
	return path
}

func readSmokePS1(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(smokePS1Path(t))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func readSmokeSh(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(smokeScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func smokePS1Body(t *testing.T, name string) string {
	t.Helper()
	body, err := psFunctionBody(readSmokePS1(t), name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// errorLegs returns the sorted set of legs the script names in its
// "::error::<leg> leg:" lines. In the PowerShell script, Stop-Smoke adds the
// ::error:: prefix to the message it is given.
func errorLegs(src string) []string {
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?:::error::|Stop-Smoke ['"]|\$installFailure = ['"])([a-z]+) leg: `).FindAllStringSubmatch(src, -1) {
		seen[m[1]] = true
	}
	legs := make([]string, 0, len(seen))
	for leg := range seen {
		legs = append(legs, leg)
	}
	sort.Strings(legs)
	return legs
}

// Windows PowerShell 5.1 reads a script without a byte order mark in the ANSI
// code page, where a UTF-8 dash or arrow becomes other characters.
func TestSmokePS1IsPlainASCII(t *testing.T) {
	src := readSmokePS1(t)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c > 0x7e || (c < 0x20 && c != '\n' && c != '\r' && c != '\t') {
			line := strings.Count(src[:i], "\n") + 1
			t.Fatalf("smoke-check-release.ps1:%d has byte 0x%02x; the script must be ASCII", line, c)
		}
	}
	if _, err := psCode(src); err != nil {
		t.Fatalf("the static reader cannot parse smoke-check-release.ps1: %v", err)
	}
}

// The Windows smoke runs what a Windows user runs: the documented one-liner,
// then the same init and serve commands as the macOS and Linux smoke, and the
// same machine-readable inspection.
func TestSmokePS1PinsExactPublicCommands(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-runs-the-public-commands")
	src := readSmokePS1(t)
	sh := readSmokeSh(t)
	for _, command := range []string{
		"irm https://putnami.dev/install.ps1 | iex",
		`'irm "https://putnami.dev/install.ps1?run=' + $RunCommand + '" | iex'`,
		"putnami init --project webapp --extension ts",
		"putnami serve webapp",
	} {
		if !strings.Contains(src, command) {
			t.Fatalf("smoke-check-release.ps1 does not run exact public command %q", command)
		}
	}
	inspect := regexp.MustCompile(`(?m)^\s+(putnami [a-z]+ .*--output=jsonl)$`).FindAllStringSubmatch(sh, -1)
	if len(inspect) != 4 {
		t.Fatalf("smoke-check-release.sh has %d inspection commands, want 4", len(inspect))
	}
	for _, m := range inspect {
		if !strings.Contains(src, "'"+m[1]+"'") {
			t.Fatalf("smoke-check-release.ps1 does not run the inspection %q that smoke-check-release.sh runs", m[1])
		}
	}
	if !strings.Contains(src, "$PublicInstallerUrl = 'https://putnami.dev/install.ps1'") {
		t.Fatal("the default installer URL is not https://putnami.dev/install.ps1")
	}
	if strings.Contains(src, "--extension go") || strings.Contains(src, "install.sh") {
		t.Fatal("smoke-check-release.ps1 runs something other than the Windows TypeScript golden path")
	}
}

// Microsoft Defender blocks the install one-liner when a PowerShell command
// line carries it, so the install leg writes the one-liner, as script text,
// into a script and starts that script with -File, the way the install guide
// gives it.
func TestSmokePS1RunsTheOneLinerAsScriptText(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-runs-the-one-liner-as-script-text")
	src := readSmokePS1(t)
	for i, line := range strings.Split(src, "\n") {
		if oneLinerOnACommandLine.MatchString(line) {
			t.Errorf("smoke-check-release.ps1:%d passes the one-liner on a command line, which Defender blocks: %s",
				i+1, strings.TrimSpace(line))
		}
	}
	leg := strings.Index(src, "# -- Leg 2:")
	if leg < 0 {
		t.Fatal("smoke-check-release.ps1 has no install leg")
	}
	install := src[leg:]
	for _, want := range []string{
		"$oneLiner = 'irm https://putnami.dev/install.ps1 | iex'",
		"$oneLiner = \"irm $InstallerUrl | iex\"",
		"[IO.File]::WriteAllText($installScript, $oneLiner + \"`r`n\" + $discovery + \"`r`n\", [Text.Encoding]::ASCII)",
		`$installCommand = 'powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $installScript + '"'`,
		"$installStatus = Invoke-SmokeCommand (Get-SmokePowerShell) $installCommand $Workspace $installLog",
	} {
		if !strings.Contains(install, want) {
			t.Fatalf("the install leg of smoke-check-release.ps1 does not contain %q", want)
		}
	}
	// The run leg runs its one-liner the same way, in the caller's directory,
	// and ends with the status the installer leaves in $LASTEXITCODE.
	run := strings.Index(src, "# -- Optional leg: run one command without a workspace --")
	if run < 0 {
		t.Fatal("smoke-check-release.ps1 has no run leg")
	}
	for _, want := range []string{
		"[IO.File]::WriteAllText($runScript, $runLine + \"`r`n\" + 'exit $LASTEXITCODE' + \"`r`n\", [Text.Encoding]::ASCII)",
		`$runCommandLine = 'powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $runScript + '"'`,
		"$runStatus = Invoke-SmokeCommand (Get-SmokePowerShell) $runCommandLine $runDir $runLog",
		"Set-SmokeEnv 'PATH' \"$fakeBin;$pathBeforeRun\"",
		"if ($runMap) { Set-SmokeEnv 'PUTNAMI_COMMAND_MAP_URL' $runMap }",
	} {
		if !strings.Contains(src[run:], want) {
			t.Fatalf("the run leg of smoke-check-release.ps1 does not contain %q", want)
		}
	}
}

// Both smokes take the same inputs with the same defaults, so a release job
// runs either with one set of variables.
func TestSmokePS1TakesTheInputsOfTheShellSmoke(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-takes-the-inputs-of-the-shell-smoke")
	src := readSmokePS1(t)
	sh := readSmokeSh(t)
	for _, pin := range []struct{ sh, ps1 string }{
		{`channel="${1:-latest}"`, `param([string]$Channel = 'latest')`},
		{`base="${PUTNAMI_REGISTRY_URL:-https://put.putnami.dev}"`, "$Base = [string]$env:PUTNAMI_REGISTRY_URL\nif (-not $Base) { $Base = 'https://put.putnami.dev' }"},
		{`retries="${SMOKE_RETRIES:-5}"`, "$RetriesText = [string]$env:SMOKE_RETRIES\nif (-not $RetriesText) { $RetriesText = '5' }"},
		{`startup_timeout="${SMOKE_STARTUP_TIMEOUT:-180}"`, "$StartupTimeoutText = [string]$env:SMOKE_STARTUP_TIMEOUT\nif (-not $StartupTimeoutText) { $StartupTimeoutText = '180' }"},
		{`installer_url="${SMOKE_INSTALL_URL:-https://putnami.dev/install.sh}"`, "$InstallerUrl = [string]$env:SMOKE_INSTALL_URL\nif (-not $InstallerUrl) { $InstallerUrl = $PublicInstallerUrl }"},
		{`diagnostics_dir="${SMOKE_DIAGNOSTICS_DIR:-}"`, "$DiagnosticsDir = [string]$env:SMOKE_DIAGNOSTICS_DIR"},
		{`diagnostic_byte_limit=262144`, `$DiagnosticByteLimit = 262144`},
		{`*) diagnostics_dir="${launch_dir}/${diagnostics_dir}" ;;`, `$DiagnosticsDir = Join-Path $LaunchDir $DiagnosticsDir`},
		{`echo "::error::SMOKE_STARTUP_TIMEOUT must be a non-negative integer, got '${startup_timeout}'"`, `Write-Smoke "::error::SMOKE_STARTUP_TIMEOUT must be a non-negative integer, got '$StartupTimeoutText'"`},
		{`echo "::error::SMOKE_RETRIES must be a positive integer, got '${retries}'"`, `Write-Smoke "::error::SMOKE_RETRIES must be a positive integer, got '$RetriesText'"`},
		{`run_command="${SMOKE_RUN_COMMAND:-}"`, `$RunCommand = [string]$env:SMOKE_RUN_COMMAND`},
		{`echo "::error::SMOKE_RUN_COMMAND must match ^[a-z][a-z0-9-]{0,63}\$, got '${run_command}'"`, "Write-Smoke \"::error::SMOKE_RUN_COMMAND must match ^[a-z][a-z0-9-]{0,63}`$, got '$RunCommand'\""},
		{`run_command_pattern='^[abcdefghijklmnopqrstuvwxyz][abcdefghijklmnopqrstuvwxyz0123456789-]{0,63}$'`, `if ($RunCommand -and ($RunCommand -cnotmatch '^[a-z][a-z0-9-]{0,63}\z')) {`},
	} {
		if !strings.Contains(sh, pin.sh) {
			t.Fatalf("smoke-check-release.sh no longer contains %q; update the Windows port with it", pin.sh)
		}
		if !strings.Contains(src, pin.ps1) {
			t.Fatalf("smoke-check-release.ps1 does not mirror %q; want %q", pin.sh, pin.ps1)
		}
	}
	for _, name := range []string{"PUTNAMI_REGISTRY_URL", "SMOKE_INSTALL_URL", "SMOKE_RETRIES", "SMOKE_STARTUP_TIMEOUT", "SMOKE_DIAGNOSTICS_DIR", "SMOKE_RUN_COMMAND"} {
		if !strings.Contains(src, "#   "+name+" ") {
			t.Fatalf("the header of smoke-check-release.ps1 does not document %s", name)
		}
	}
}

// Every variable the shell smoke empties, the Windows smoke empties too, and
// the prefixes it clears are the same. Windows environment names ignore case,
// so the lowercase proxy variables are the uppercase ones there.
func TestSmokePS1NeutralizesWhatTheShellSmokeNeutralizes(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-runs-in-a-neutral-environment")
	src := readSmokePS1(t)
	sh := readSmokeSh(t)
	body := smokePS1Body(t, "Invoke-Smoke")
	emptied := regexp.MustCompile(`(?m)^export ([A-Za-z_][A-Za-z0-9_]*)=""$`).FindAllStringSubmatch(sh, -1)
	if len(emptied) < 20 {
		t.Fatalf("found only %d emptied variables in smoke-check-release.sh", len(emptied))
	}
	for _, m := range emptied {
		name := strings.ToUpper(m[1])
		if !strings.Contains(body, "'"+name+"'") {
			t.Fatalf("smoke-check-release.sh empties %s and smoke-check-release.ps1 does not", m[1])
		}
	}
	for _, prefix := range []string{"PUTNAMI_", "BUN_", "NPM_CONFIG_"} {
		if !strings.Contains(sh, `"${!`+prefix+`@}"`) {
			t.Fatalf("smoke-check-release.sh no longer clears %s*", prefix)
		}
	}
	for _, rule := range []string{"if ($name -match '^(BUN_|NPM_CONFIG_)') { Set-SmokeEnv $name '' }", "if ($name -match '^PUTNAMI_') { Set-SmokeEnv $name '' }"} {
		if !strings.Contains(body, rule) {
			t.Fatalf("smoke-check-release.ps1 does not clear a prefix the shell smoke clears; want %q", rule)
		}
	}
	for _, name := range smokeNeutralEnvironment {
		covered := strings.HasPrefix(name, "PUTNAMI_") || strings.HasPrefix(name, "BUN_") || strings.HasPrefix(name, "NPM_CONFIG_") || strings.Contains(body, "'"+name+"'")
		if !covered {
			t.Fatalf("smoke-check-release.ps1 lets %s reach the golden path", name)
		}
	}
	for _, pin := range []string{
		"Set-SmokeEnv 'PUTNAMI_TELEMETRY' 'off'",
		"Set-SmokeEnv 'DO_NOT_TRACK' '1'",
		"Set-SmokeEnv 'PUTNAMI_NO_RELAUNCH' '1'",
		"Set-SmokeEnv 'PUTNAMI_REGISTRY_URL' $Base",
		"Set-SmokeEnv 'PUTNAMI_VERSION' $Channel",
		"Set-SmokeEnv 'GIT_CONFIG_NOSYSTEM' '1'",
		// Decision D-W7: the CLI's home is USERPROFILE on Windows.
		"Set-SmokeEnv 'USERPROFILE' $smokeHome",
		// Decision D-W8: Git's long paths are a prerequisite, set here because
		// the system and global configuration are ignored.
		"Set-SmokeEnv 'GIT_CONFIG_KEY_0' 'core.longpaths'",
	} {
		if !strings.Contains(body, pin) {
			t.Fatalf("smoke-check-release.ps1 does not set %q", pin)
		}
	}
	// The neutral environment is in place before the first request.
	neutral := strings.Index(body, "Set-SmokeEnv 'HTTPS_PROXY'")
	if neutral < 0 {
		neutral = strings.Index(body, "'HTTPS_PROXY'")
	}
	if first := strings.Index(body, "Invoke-SmokeGet"); neutral < 0 || first < 0 || neutral > first {
		t.Fatal("smoke-check-release.ps1 sends a request before the neutral environment is in place")
	}
	if !strings.Contains(src, "$env:PUTNAMI_SMOKE_CHILD -ne '1'") {
		t.Fatal("smoke-check-release.ps1 must run in a child PowerShell, so the caller's session keeps its environment")
	}
}

// A missing prerequisite is named before the release channel is touched, as
// the shell smoke does for Bun, and the Windows ones are named too.
func TestSmokePS1NamesPrerequisitesBeforeTheFirstRequest(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-names-prerequisites-before-any-request")
	body := smokePS1Body(t, "Invoke-Smoke")
	first := strings.Index(body, "Invoke-SmokeGet")
	announce := strings.Index(body, `Write-Smoke "smoke: GET $url"`)
	if first < 0 || announce < 0 || announce > first {
		t.Fatal("smoke-check-release.ps1 must print `smoke: GET <url>` before its first request")
	}
	checks := []string{
		"prerequisites leg: Bun v1.4.0 or later is required for the TypeScript web init/serve golden path",
		"Bun v1.4.0 or later is required\"",
		"install Git for Windows, which provides git and sh",
		"prerequisites leg: LongPathsEnabled is off",
		"prerequisites leg: tar.exe is missing from System32",
	}
	for _, check := range checks {
		at := strings.Index(body, check)
		if at < 0 {
			t.Fatalf("smoke-check-release.ps1 does not name the prerequisite %q", check)
		}
		if at > announce {
			t.Fatalf("smoke-check-release.ps1 checks %q after announcing its first request", check)
		}
	}
}

// The Windows smoke fails for every reason the shell smoke fails, under the
// same leg names, so one reader reads both.
func TestSmokePS1NamesTheLegsOfTheShellSmoke(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-fails-under-the-legs-of-the-shell-smoke")
	src := readSmokePS1(t)
	sh := readSmokeSh(t)
	shLegs := errorLegs(sh)
	ps1Legs := errorLegs(src)
	want := []string{"channel", "cleanup", "discovery", "http", "init", "install", "platform", "prerequisites", "run", "serve", "shutdown", "stamp"}
	if strings.Join(shLegs, ",") != strings.Join(want, ",") {
		t.Fatalf("smoke-check-release.sh names legs %v, want %v; update this test and the Windows port", shLegs, want)
	}
	if strings.Join(ps1Legs, ",") != strings.Join(want, ",") {
		t.Fatalf("smoke-check-release.ps1 names legs %v, want the legs of the shell smoke %v", ps1Legs, want)
	}
	// Each regression the shell smoke names is named by the Windows smoke.
	for _, issue := range regexp.MustCompile(`see #[0-9]+(?:/#[0-9]+)?`).FindAllString(sh, -1) {
		if !strings.Contains(src, issue) {
			t.Fatalf("smoke-check-release.ps1 does not name %q", issue)
		}
	}
	for _, text := range []string{
		"install leg: the installer did not report a verified digest",
		"install leg: the installer took the unverified path (PUTNAMI_UNSAFE_INSTALL)",
		"discovery leg: 'putnami' resolved to $bin, outside the smoke workdir",
		"init leg: public starter unexpectedly depends on private @putnami/cloud state",
		"init leg: machine-readable inspection did not report @putnami/typescript",
		"serve leg: webapp's initial serve session ended before readiness",
		"http leg: webapp returned an empty successful response",
		"shutdown leg: webapp did not stop within the graceful shutdown budget",
		"shutdown leg: HTTP listener still answers after the CLI exited; the starter may be orphaned",
		"run leg: git init failed in $runDir",
		"run leg: putnami $RunCommand through ${InstallerUrl}?run=$RunCommand exited $runStatus",
		"run leg: the installer escalated privileges - a one-line install must never ask for elevation",
		"run leg: git status failed in $runDir",
		"run leg: putnami $RunCommand changed the directory it ran in; the run form must leave the caller's directory untouched",
	} {
		if !strings.Contains(src, text) {
			t.Fatalf("smoke-check-release.ps1 does not fail with %q", text)
		}
	}
	for _, pattern := range []string{`"type":"ready".*"port":`, `"record":"session:end"`, `'Integrity verified'`, `'without integrity verification'`} {
		if !strings.Contains(src, pattern) {
			t.Fatalf("smoke-check-release.ps1 does not look for %s", pattern)
		}
	}
}

// init writes the LF checkout policy (decision D-W4), and the Windows smoke
// requires it next to the files the shell smoke requires.
func TestSmokePS1RequiresTheGeneratedWorkspaceOfTheShellSmoke(t *testing.T) {
	src := readSmokePS1(t)
	sh := readSmokeSh(t)
	required := regexp.MustCompile(`(?m)^require_generated_text (\S+) '([^']+)'$`).FindAllStringSubmatch(sh, -1)
	if len(required) < 9 {
		t.Fatalf("found only %d required texts in smoke-check-release.sh", len(required))
	}
	for _, m := range required {
		if m[1] == ".npmrc" {
			if !strings.Contains(src, "'"+m[2]+"'") {
				t.Fatalf("smoke-check-release.ps1 does not require %s in .npmrc", m[2])
			}
			continue
		}
		if !strings.Contains(src, "@('"+m[1]+"', '"+m[2]+"')") {
			t.Fatalf("smoke-check-release.ps1 does not require %s in %s", m[2], m[1])
		}
	}
	if !strings.Contains(src, "@('.gitattributes', '"+git.LFPolicyAttributes+"')") {
		t.Fatalf("smoke-check-release.ps1 does not require %q in .gitattributes", git.LFPolicyAttributes)
	}
	files := regexp.MustCompile(`for path in \\\n\s+(putnami\.workspace\.json putnami\.lock\.json[^;]*); do\n\s+require_generated_file`).FindStringSubmatch(sh)
	if files == nil {
		t.Fatal("cannot find the required files of smoke-check-release.sh")
	}
	for _, path := range strings.Fields(strings.ReplaceAll(files[1], "\\", "")) {
		if !strings.Contains(src, "'"+path+"'") {
			t.Fatalf("smoke-check-release.ps1 does not require the generated file %s", path)
		}
	}
	if !strings.Contains(src, "if ($key.ToLowerInvariant() -match '(_authtoken|_auth)\\s*$')") {
		t.Fatal("smoke-check-release.ps1 does not reject an authentication directive in .npmrc")
	}
}

// Failure diagnostics copy bounded logs and generated files, and never
// .npmrc: even a rejected auth directive must never reach an artifact.
func TestSmokePS1NeverCopiesNpmrcIntoDiagnostics(t *testing.T) {
	body := smokePS1Body(t, "Save-FailureArtifacts")
	code, err := psCode(body)
	if err != nil {
		t.Fatal(err)
	}
	copied := regexp.MustCompile(`foreach \(\$path in @\(([^)]*)\)\)`).FindStringSubmatch(body)
	if copied == nil {
		t.Fatal("cannot find the files Save-FailureArtifacts copies")
	}
	if strings.Contains(copied[1], "npmrc") {
		t.Fatalf("Save-FailureArtifacts copies .npmrc: %s", copied[1])
	}
	if strings.Contains(code, "Copy-Item") {
		t.Fatal("Save-FailureArtifacts copies files unbounded")
	}
	for _, pin := range []string{"Write-BoundedTail", "Write-BoundedHead", "'workspace-files.txt'", `"$safeChannel-windows-amd64"`} {
		if !strings.Contains(body, pin) {
			t.Fatalf("Save-FailureArtifacts does not use %s", pin)
		}
	}
	for _, writer := range []string{"Write-BoundedTail", "Write-BoundedHead"} {
		if !strings.Contains(smokePS1Body(t, writer), "$bytes.Length -gt $DiagnosticByteLimit") {
			t.Fatalf("%s does not stop at the diagnostic byte limit", writer)
		}
	}
}

// The served starter stops the way Ctrl+C stops it (decision D-W9): the CLI
// runs as the root of its own console process group, receives
// CTRL_BREAK_EVENT, and a stop that needs TerminateProcess fails the release.
func TestSmokePS1StopsTheServerTheWayCtrlCDoes(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-release-smoke", "the-windows-smoke-stops-the-server-with-ctrl-break")
	src := readSmokePS1(t)
	if !strings.Contains(src, "[PutnamiSmoke.ChildProcess]::Start($bin, (Get-CommandLine $bin 'putnami serve webapp'), $Workspace, $serveLog, $true)") {
		t.Fatal("putnami serve does not start in a new process group")
	}
	stop := smokePS1Body(t, "Stop-Server")
	breakAt := strings.Index(stop, "$server.SendCtrlBreak()")
	waitAt := strings.Index(stop, "$server.WaitForExit(20000)")
	killAt := strings.Index(stop, "$server.Kill()")
	if breakAt < 0 || waitAt < breakAt || killAt < waitAt {
		t.Fatal("Stop-Server must send CTRL_BREAK_EVENT, wait 20 seconds, and only then kill")
	}
	if !strings.Contains(stop, "$forced = $true") || !strings.Contains(stop, "return (-not $forced)") {
		t.Fatal("a forced kill must fail the shutdown leg")
	}
	for _, pin := range []string{"CreateNewProcessGroup = 0x00000200", "GenerateConsoleCtrlEvent(CtrlBreakEvent, (uint)Id)", "CtrlBreakEvent = 1"} {
		if !strings.Contains(src, pin) {
			t.Fatalf("the native helper does not contain %q", pin)
		}
	}
	final := `Write-Smoke "smoke: OK - channel '$Channel' passes irm install -> TypeScript init -> serve -> HTTP -> clean stop on windows/amd64"`
	if !strings.Contains(src, final) {
		t.Fatalf("smoke-check-release.ps1 does not end with %q", final)
	}
}

// The install leg writes the user's Path in HKCU\Environment; the smoke puts
// it back whatever the outcome, and removes its workdir.
func TestSmokePS1RestoresTheUserPathAndRemovesItsWorkdir(t *testing.T) {
	src := readSmokePS1(t)
	finally := regexp.MustCompile(`(?s)\n\} finally \{\n(.*)\n\}\nexit \$SmokeStatus\n$`).FindStringSubmatch(src)
	if finally == nil {
		t.Fatal("smoke-check-release.ps1 must end with try { Invoke-Smoke } ... finally { cleanup } and exit $SmokeStatus")
	}
	for _, step := range []string{"Stop-Server", "Save-FailureArtifacts", "Restore-UserPath", "Set-Location -LiteralPath $LaunchDir", "Remove-Workdir"} {
		if !strings.Contains(finally[1], step) {
			t.Fatalf("the final cleanup does not run %s", step)
		}
	}
	install := smokePS1Body(t, "Invoke-Smoke")
	save := strings.Index(install, "$script:SavedUserPath = Get-UserPathValue")
	run := strings.Index(install, "$installStatus = Invoke-SmokeCommand")
	if save < 0 || run < save {
		t.Fatal("the user Path must be saved before the installer runs")
	}
}
