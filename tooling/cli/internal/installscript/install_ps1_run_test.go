package installscript

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The run form of install.ps1, behavior for behavior with install.sh
// (run_command_test.go): `irm "https://putnami.dev/install.ps1?run=<command>" | iex`,
// `& ([scriptblock]::Create((irm https://putnami.dev/install.ps1))) --run <command>`,
// or `powershell -File install.ps1 --run <command>`. The script checks the
// command, resolves which extension provides it from the command map,
// installs the CLI exactly as without a command, pins that extension for the
// user through the installed CLI, and runs `putnami <command>` in the caller's
// directory, ending with its exit code.
//
// The static checks below read the script; the behavior tests run it through
// the pwsh harness of install_ps1_test.go against one local server that plays
// the site and the registry. install_ps1_windows_test.go runs the real script
// on Windows.

// ps1RunPlaceholderLine is the one line of install.ps1 the site rewrites for
// ?run=<command>. sites/putnami.dev pins the same literal.
const ps1RunPlaceholderLine = `$RunCommandDefault = ''`

// ps1FileDetection is how install.ps1 knows it runs from a file: it asks the
// file of its own script block, which text run by iex does not have, even when
// a caller's script runs that text.
const ps1FileDetection = `$RunsFromFile = [bool]$MyInvocation.MyCommand.ScriptBlock.File`

// The Windows run forms doc/22-installing-the-cli.md gives, with <url> for the
// site and <command> for the command: typed in a PowerShell window, as a
// script block with --run, and from cmd.exe as a download under %TEMP% run
// with -File. install_ps1_windows_test.go runs each against a local site.
const (
	ps1RunOneLinerForm    = `irm "<url>/install.ps1?run=<command>" | iex`
	ps1RunScriptBlockForm = `& ([scriptblock]::Create((irm <url>/install.ps1))) --run <command>`
	ps1RunFromCmdForm     = `curl.exe -fsSLo "%TEMP%\install.ps1" <url>/install.ps1 && powershell -NoProfile -ExecutionPolicy Bypass -File "%TEMP%\install.ps1" --run <command>`
)

// ps1RunForm fills in the site URL and the command of a run form.
func ps1RunForm(form, url, command string) string {
	return strings.NewReplacer("<url>", url, "<command>", command).Replace(form)
}

// bakePS1RunCommand does to install.ps1 what the site does for
// ?run=<command>: it sets the value of the one placeholder line and changes
// nothing else. The command is one the site accepted, which holds no quote.
func bakePS1RunCommand(script []byte, command string) ([]byte, error) {
	lines := strings.Split(string(script), "\n")
	found := -1
	for i, line := range lines {
		if line != ps1RunPlaceholderLine {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("install.ps1 carries the run placeholder on lines %d and %d; the site substitutes exactly one", found+1, i+1)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("install.ps1 carries no %q line for the site to substitute", ps1RunPlaceholderLine)
	}
	lines[found] = `$RunCommandDefault = '` + command + `'`
	return []byte(strings.Join(lines, "\n")), nil
}

// servePS1InstallScript answers /install.ps1 the way the site does: the script
// as is without ?run=, the script with the command baked in for one valid run=
// value, and 400 for anything else. The site's static server has no type for
// .ps1, so both are application/octet-stream.
func servePS1InstallScript(w http.ResponseWriter, r *http.Request, installer []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	runs, asked := r.URL.Query()["run"]
	if !asked {
		_, _ = w.Write(installer)
		return
	}
	if len(runs) != 1 || !siteRunCommandPattern.MatchString(runs[0]) {
		http.Error(w, "invalid run command", http.StatusBadRequest)
		return
	}
	baked, err := bakePS1RunCommand(installer, runs[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(baked)
}

// countWord counts the occurrences of word in s that containsWord would
// report: delimited by bytes that are not part of a word.
func countWord(s, word string) int {
	count := 0
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] == word && containsWord(s[max(0, i-1):min(len(s), i+len(word)+1)], word) {
			count++
		}
	}
	return count
}

// --- Static evidence --------------------------------------------------------

// The install documentation gives the Windows run forms the Windows tests run,
// each in a command block, and none of them passes the one-liner to
// powershell on a command line, which Microsoft Defender blocks.
func TestInstallDocsGiveTheTestedWindowsRunForms(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-docs-give-the-tested-windows-run-forms")
	data, err := os.ReadFile(filepath.Join(filepath.Dir(installScriptPath(t)), "..", "doc", "22-installing-the-cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := fencedLines(string(data))
	for _, form := range []string{ps1RunOneLinerForm, ps1RunScriptBlockForm, ps1RunFromCmdForm} {
		want := ps1RunForm(form, "https://putnami.dev", "<command>")
		found := false
		for _, line := range lines {
			if strings.TrimSpace(line) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("22-installing-the-cli.md has no command line %q", want)
		}
		if oneLinerOnACommandLine.MatchString(want) {
			t.Errorf("the run form %q passes the one-liner on a command line", want)
		}
	}
	src := string(readFile(t, installPS1Path(t)))
	if want := "#   " + ps1RunForm(ps1RunOneLinerForm, "https://putnami.dev", "<command>") + "\n"; !strings.Contains(src, want) {
		t.Errorf("install.ps1's header does not give the run form %q", strings.TrimSpace(want))
	}
}

func TestInstallPS1CarriesExactlyOneRunPlaceholder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-site-bakes-one-line-of-install-ps1")
	script := []byte(readInstallPS1(t))
	baked, err := bakePS1RunCommand(script, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Split(string(script), "\n")
	after := strings.Split(string(baked), "\n")
	if len(before) != len(after) {
		t.Fatalf("baking a command changed the line count from %d to %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
			if after[i] != `$RunCommandDefault = 'deploy'` {
				t.Fatalf("baking a command changed line %d to %q", i+1, after[i])
			}
		}
	}
	if changed != 1 {
		t.Fatalf("baking a command changed %d lines, want exactly the placeholder", changed)
	}
	// The placeholder is a statement of the script block, where the entry point
	// reads it, not text inside a comment or a function.
	code, err := psCode(string(script))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, "\n$RunCommandDefault = ''\n") {
		t.Fatal("the run placeholder is not a top-level statement of the script block")
	}
	main, err := psFunctionBody(string(script), "Invoke-PutnamiInstaller")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(main, "\n    if (-not $runCommand) { $runCommand = $RunCommandDefault }\n") {
		t.Fatal("the entry point does not fall back to the baked command after PUTNAMI_RUN")
	}
}

// checkPS1ExitsOnlyFromAFile reports how src could call exit where it would
// close the window of a user who piped it into iex, or nil when it cannot. The
// one exit ends run mode with the command's status, and only when the script
// block was read from a file.
func checkPS1ExitsOnlyFromAFile(src string) error {
	code, err := psCode(src)
	if err != nil {
		return err
	}
	if n := countWord(strings.ToLower(code), "exit"); n != 1 {
		return fmt.Errorf("install.ps1 calls exit %d times; the only exit is the one of run mode from a file", n)
	}
	body, err := psFunctionBody(src, "Exit-RunMode")
	if err != nil {
		return err
	}
	if want := "\n    $global:LASTEXITCODE = $Status\n    if ($RunsFromFile) { exit $Status }\n"; body != want {
		return fmt.Errorf("Exit-RunMode must set $LASTEXITCODE and exit only from a file; its body is %q", body)
	}
	assignments := regexp.MustCompile(`(?m)^[ \t]*\$RunsFromFile[ \t]*=.*$`).FindAllString(src, -1)
	if len(assignments) != 1 || assignments[0] != ps1FileDetection {
		return fmt.Errorf("$RunsFromFile must be set once, at the top of the block, as %q; found %q", ps1FileDetection, assignments)
	}
	if regexp.MustCompile(`(?i)\$[a-z]+:RunsFromFile|Set-Variable[^\n]*RunsFromFile`).MatchString(src) {
		return errors.New("$RunsFromFile is set outside its one assignment")
	}
	return nil
}

// A script piped into iex runs in the caller's session, where exit ends the
// session and closes the window. Run mode must still end with the command's
// exit code for cmd.exe and CI, which run the script as a file.
func TestInstallPS1ExitsOnlyFromAFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-exits-only-from-a-file")
	src := readInstallPS1(t)
	if err := checkPS1ExitsOnlyFromAFile(src); err != nil {
		t.Fatal(err)
	}
	mutations := []struct{ name, old, new string }{
		{"a failure exits", "    throw 'Putnami CLI installation failed.'\n", "    exit 1\n"},
		{"run mode exits from text", "    if ($RunsFromFile) { exit $Status }\n", "    exit $Status\n"},
		{"the file test is forced", ps1FileDetection + "\n", "$RunsFromFile = $true\n"},
		{"the file test reads the caller's script", ps1FileDetection + "\n", "$RunsFromFile = [bool]$PSCommandPath\n"},
		{"the entry point overrides the file test", "    $tag = ConvertTo-PutnamiTag $version\n", "    $tag = ConvertTo-PutnamiTag $version\n    $RunsFromFile = $true\n"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: install.ps1 does not contain exactly one %q", m.old)
			}
			if err := checkPS1ExitsOnlyFromAFile(strings.Replace(src, m.old, m.new, 1)); err == nil {
				t.Fatal("the check accepted a script that exits where it must not")
			}
		})
	}
}

// shellAssignments returns the values of the NAME="value" and NAME=digits
// assignments of install.sh, with ${NAME} references expanded and \$
// unescaped. BLANKS, the one assignment built from $'\t', is checked and set
// by hand.
func shellAssignments(t *testing.T, shell string) map[string]string {
	t.Helper()
	if !strings.Contains(shell, "\nBLANKS=\" \"$'\\t'\n") {
		t.Fatal(`install.sh no longer sets BLANKS=" "$'\t'; update shellAssignments`)
	}
	values := map[string]string{"BLANKS": " \t"}
	reference := regexp.MustCompile(`\$\{([A-Z_]+)\}`)
	for _, m := range regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)="([^"]*)"$`).FindAllStringSubmatch(shell, -1) {
		value := reference.ReplaceAllStringFunc(m[2], func(ref string) string {
			name := reference.FindStringSubmatch(ref)[1]
			known, ok := values[name]
			if !ok {
				t.Fatalf("install.sh %s references %s before it is set", m[1], name)
			}
			return known
		})
		values[m[1]] = strings.ReplaceAll(value, `\$`, `$`)
	}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=([0-9]+)$`).FindAllStringSubmatch(shell, -1) {
		values[m[1]] = m[2]
	}
	return values
}

// ps1Assignments returns the values of the top-level $Name = 'value' and
// $Name = [long]N assignments of install.ps1.
func ps1Assignments(src string) map[string]string {
	values := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\$([A-Za-z0-9]+) = '((?:[^']|'')*)'$`).FindAllStringSubmatch(src, -1) {
		values[m[1]] = strings.ReplaceAll(m[2], "''", "'")
	}
	for _, m := range regexp.MustCompile(`(?m)^\$([A-Za-z0-9]+) = \[long\]([0-9]+)$`).FindAllStringSubmatch(src, -1) {
		values[m[1]] = m[2]
	}
	return values
}

// ps1RunRuleCorpus holds commands, extension references and map lines on
// both sides of every rule: lengths at the limit, case, blanks, control
// characters, shell and PowerShell syntax, and non-ASCII look-alikes.
var ps1RunRuleCorpus = []string{
	"", "a", "deploy", "lint-docs", "a-", "a1", "1a", "-a", "Deploy", "deploY", "de ploy",
	"deploy\n", "deploy\r", "\ndeploy", "deploy\t", "a_b", "a.b", "deploy;id", "$(id)", "`id`", "deploy'",
	"d\u00e9ploy", "\uff44eploy", "\u212aelvin", "k\u0131", "deploy\u00a0",
	"a" + strings.Repeat("b", 63), "a" + strings.Repeat("b", 64),
	"@acme/deploy", "@acme/deploy@^1.2.0", "@acme/deploy@~1", "@acme/deploy@1.2.3-beta.1+build", "@a/b",
	"@acme/deploy@latest", "@acme/deploy@LATEST", "acme/deploy", "/tmp/x", "@acme/deploy@$(id)",
	"@acme/deploy\r", "@acme/deploy\n", "@Acme/deploy", "@acme/deploy@", "@acme/deploy@^", "@acme/deploy@^^1",
	"@acme/de ploy", "@acme//deploy", "@.acme/deploy", "@acme/.deploy", "@acme/deploy@1\u00e9",
	"@" + strings.Repeat("a", 128) + "/b", "@" + strings.Repeat("a", 129) + "/b",
	"@a/" + strings.Repeat("b", 128), "@a/" + strings.Repeat("b", 129),
	"@a/b@" + strings.Repeat("1", 64), "@a/b@" + strings.Repeat("1", 65),
	"deploy @acme/deploy", "deploy\t@acme/deploy", "deploy  \t @acme/deploy", " deploy @acme/deploy",
	"deploy @acme/deploy ", "deploy @a extra", "deploy @acme/deploy\r", "\r", "deploy\v@x", "deploy\u00a0@x",
	"deploy\f@x", "deploy\n@x",
}

// checkPS1RunRules reports how the run rules of ps1 differ from those of
// install.sh, or nil when they agree on the whole corpus. code is install.ps1
// as psCode returns it, where every use of a pattern must be case-sensitive.
func checkPS1RunRules(ps1, shell map[string]string, code string) error {
	pairs := []struct{ ps1, shell string }{
		{"RunCommandPattern", "RUN_COMMAND_PATTERN"},
		{"ExtensionRefPattern", "EXTENSION_REF_PATTERN"},
		{"CommandMapEntryPattern", "COMMAND_MAP_ENTRY_PATTERN"},
	}
	for _, pair := range pairs {
		ours, theirs := ps1[pair.ps1], shell[pair.shell]
		if ours == "" || theirs == "" {
			return fmt.Errorf("$%s or %s is missing", pair.ps1, pair.shell)
		}
		// .NET's $ also matches before a final newline; \z does not.
		if strings.Contains(ours, "$") || !strings.HasSuffix(ours, `\z`) {
			return fmt.Errorf("$%s must end with \\z and hold no $, which .NET matches before a final newline: %q", pair.ps1, ours)
		}
		// Go's $ without (?m) and bash's =~ both anchor at the very end, as \z does.
		ourRule, err := regexp.Compile(ours)
		if err != nil {
			return fmt.Errorf("$%s: %w", pair.ps1, err)
		}
		theirRule := regexp.MustCompile(theirs)
		for _, sample := range ps1RunRuleCorpus {
			if ourMatch, theirMatch := ourRule.FindStringSubmatch(sample), theirRule.FindStringSubmatch(sample); fmt.Sprint(ourMatch) != fmt.Sprint(theirMatch) {
				return fmt.Errorf("$%s and %s disagree on %q: %q and %q", pair.ps1, pair.shell, sample, ourMatch, theirMatch)
			}
		}
		// A PowerShell -match ignores case, and so would admit Deploy.
		uses := regexp.MustCompile(`(-cmatch|-cnotmatch|\[regex\]::Match\(\$line,|=) \$`+pair.ps1+`\b|\$`+pair.ps1+`\b`).FindAllString(code, -1)
		cased := 0
		for _, use := range uses {
			if strings.HasPrefix(use, "-cmatch ") || strings.HasPrefix(use, "-cnotmatch ") || strings.HasPrefix(use, "[regex]::Match(") {
				cased++
			}
		}
		if cased == 0 || cased != len(uses)-1 {
			return fmt.Errorf("$%s must be used with -cmatch, -cnotmatch or [regex]::Match only; its uses are %q", pair.ps1, uses)
		}
	}
	// install.sh compares bytes. PowerShell's -eq and -ceq compare strings by
	// culture, where a byte order mark or another ignorable character vanishes.
	resolve, err := psFunctionBody(code, "Get-RunExtension")
	if err != nil {
		return err
	}
	if regexp.MustCompile(`(?i)-[ci]?(eq|ne)\s+\$(CommandMapHeader|Command|entryCommand)\b|\$(CommandMapHeader|Command|entryCommand)\s+-[ci]?(eq|ne)\b`).MatchString(resolve) {
		return errors.New("Get-RunExtension compares the header or a command with -eq or -ne, which compare by culture")
	}
	for _, want := range []string{
		"[string]::Equals($lines[0], $CommandMapHeader, [StringComparison]::Ordinal)",
		"[string]::Equals($entryCommand, $Command, [StringComparison]::Ordinal)",
	} {
		if !strings.Contains(resolve, want) {
			return fmt.Errorf("Get-RunExtension no longer compares ordinally: %s", want)
		}
	}
	for ours, theirs := range map[string]string{
		"CommandMapHeader":     "COMMAND_MAP_HEADER",
		"CommandMapMaxBytes":   "COMMAND_MAP_MAX_BYTES",
		"DefaultCommandMapUrl": "DEFAULT_COMMAND_MAP_URL",
		"CommandMapUrlEnv":     "COMMAND_MAP_URL_ENV",
		"RunEnv":               "RUN_ENV",
	} {
		if ps1[ours] == "" || ps1[ours] != shell[theirs] {
			return fmt.Errorf("$%s is %q, but install.sh sets %s to %q", ours, ps1[ours], theirs, shell[theirs])
		}
	}
	return nil
}

// The command, the map entries and the extension references follow the rules
// of install.sh to the byte, so a command or a map runs on Windows exactly when
// it runs on macOS and Linux.
func TestInstallPS1HoldsTheRunRulesOfInstallSh(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-holds-the-rules-of-install-sh")
	shellBytes, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := readInstallPS1(t)
	code, err := psCode(src)
	if err != nil {
		t.Fatal(err)
	}
	shell := shellAssignments(t, string(shellBytes))
	if err := checkPS1RunRules(ps1Assignments(src), shell, code); err != nil {
		t.Fatal(err)
	}
	valueMutations := []struct{ name, key, old, new string }{
		{"a longer command", "RunCommandPattern", "{0,63}", "{0,64}"},
		{"a trailing newline accepted", "RunCommandPattern", `\z`, "$"},
		{"a carriage return as a blank", "CommandMapEntryPattern", `\t`, `\t\r`},
		{"uppercase scopes", "ExtensionRefPattern", "^@[a-z0-9]", "^@[A-Za-z0-9]"},
		{"another header", "CommandMapHeader", "v1", "v2"},
		{"a larger map", "CommandMapMaxBytes", "1048576", "2097152"},
		{"another map", "DefaultCommandMapUrl", "putnami.dev", "putnami.example"},
	}
	for _, m := range valueMutations {
		t.Run(m.name, func(t *testing.T) {
			values := ps1Assignments(src)
			if !strings.Contains(values[m.key], m.old) {
				t.Fatalf("the mutation no longer applies: $%s is %q", m.key, values[m.key])
			}
			values[m.key] = strings.ReplaceAll(values[m.key], m.old, m.new)
			if err := checkPS1RunRules(values, shell, code); err == nil {
				t.Fatal("the check accepted run rules that differ from install.sh")
			}
		})
	}
	codeMutations := []struct{ name, old, new string }{
		{"a case-insensitive command check", "$runCommand -cnotmatch $RunCommandPattern", "$runCommand -notmatch $RunCommandPattern"},
		{"a case-insensitive map entry check", "$entryRef -cnotmatch $ExtensionRefPattern", "$entryRef -notmatch $ExtensionRefPattern"},
		{"a header compared by culture", "(-not [string]::Equals($lines[0], $CommandMapHeader, [StringComparison]::Ordinal))", "($lines[0] -cne $CommandMapHeader)"},
		{"a command compared by culture", "([string]::Equals($entryCommand, $Command, [StringComparison]::Ordinal))", "($entryCommand -ceq $Command)"},
	}
	for _, m := range codeMutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(code, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: %q", m.old)
			}
			if err := checkPS1RunRules(ps1Assignments(src), shell, strings.Replace(code, m.old, m.new, 1)); err == nil {
				t.Fatal("the check accepted a comparison unlike install.sh's")
			}
		})
	}
}

// checkPS1ResolvesTheRunBeforeInstalling reports how src could run a command
// it should refuse, install before the command is resolved, or run the command
// before the install is complete, or nil when it cannot.
func checkPS1ResolvesTheRunBeforeInstalling(src string) error {
	main, err := psFunctionBody(src, "Invoke-PutnamiInstaller")
	if err != nil {
		return err
	}
	at := func(s string) (int, error) {
		if strings.Count(main, s) != 1 {
			return -1, fmt.Errorf("Invoke-PutnamiInstaller does not run %q exactly once", strings.TrimSpace(s))
		}
		return strings.Index(main, s), nil
	}
	steps := []string{
		"\n    if ($runCommand) {\n        $Progress.ToStderr = $true\n        if ($runCommand -cnotmatch $RunCommandPattern) {\n",
		"\n    Assert-SupportedPlatform (Get-PutnamiPlatform)\n",
		"\n    if ($valid -and $runCommand) {\n        $valid = Test-RegistryUrl $commandMapUrl 'command map URL'\n    }\n    if (-not $valid) { Stop-Install }\n",
		"\n    if ($runCommand) {\n        $runExtension = Resolve-RunExtension $commandMapUrl $runCommand\n    }\n",
		"\n    Assert-WritableInstallDir $installDir\n",
		"$download = Save-PutnamiDownload ",
		"\n        Complete-PutnamiInstall (Join-Path $installDir $BinaryName) $noAgentHosts\n",
		"\n        if (-not $runCommand) { Write-Footer }\n",
		"\n        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue\n",
		"\n    if ($runCommand) {\n        Invoke-RequestedCommand (Join-Path $installDir $BinaryName) $runCommand $runExtension\n    }\n",
	}
	previous := -1
	for i, step := range steps {
		position, err := at(step)
		if err != nil {
			return err
		}
		if position < previous {
			return fmt.Errorf("%q runs before %q", strings.TrimSpace(step), strings.TrimSpace(steps[i-1]))
		}
		previous = position
	}
	if !strings.HasSuffix(main, steps[len(steps)-1]) {
		return errors.New("the command is not the last thing the installer does")
	}
	gate := strings.Index(main, "if ($runCommand -cnotmatch $RunCommandPattern) {")
	refusal, err := psBlockAt(main, gate)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(refusal), "Stop-Install") {
		return errors.New("an invalid command does not stop the run")
	}
	for _, name := range []string{"Write-Footer", "Resolve-RunExtension", "Invoke-RequestedCommand"} {
		if n := strings.Count(main, name); n != 1 {
			return fmt.Errorf("Invoke-PutnamiInstaller calls %s %d times", name, n)
		}
	}

	resolve, err := psFunctionBody(src, "Resolve-RunExtension")
	if err != nil {
		return err
	}
	for _, want := range []string{
		"\n        $download = Save-PutnamiDownload $MapUrl $mapFile $CommandMapMaxBytes\n        if ($download.Error) {\n",
		"\n            Stop-Install\n        }\n",
		"\n    } finally {\n        Remove-Item -LiteralPath $mapFile -Force -ErrorAction SilentlyContinue\n    }\n    $extension = Get-RunExtension $mapText $Command $mapLabel\n",
	} {
		if !strings.Contains(resolve, want) {
			return fmt.Errorf("Resolve-RunExtension no longer runs %q", strings.TrimSpace(want))
		}
	}
	return nil
}

// A command is checked before any request and resolved before anything is
// written, so a typo installs nothing; the command runs only once the CLI is
// installed and the scratch directory is gone, and prints no install footer.
func TestInstallPS1ResolvesTheRunBeforeInstalling(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-resolves-before-installing")
	src := readInstallPS1(t)
	if err := checkPS1ResolvesTheRunBeforeInstalling(src); err != nil {
		t.Fatal(err)
	}
	platform := "    Assert-SupportedPlatform (Get-PutnamiPlatform)\n"
	writable := "    Assert-WritableInstallDir $installDir\n"
	invoke := "    if ($runCommand) {\n        Invoke-RequestedCommand (Join-Path $installDir $BinaryName) $runCommand $runExtension\n    }\n"
	complete := "        Complete-PutnamiInstall (Join-Path $installDir $BinaryName) $noAgentHosts\n"
	mutations := []struct {
		name  string
		apply func(string) string
	}{
		{"progress stays on stdout", func(s string) string {
			return strings.Replace(s, "        $Progress.ToStderr = $true\n", "", 1)
		}},
		{"the command is not checked", func(s string) string {
			return strings.Replace(s, "        if ($runCommand -cnotmatch $RunCommandPattern) {\n", "        if ($false) {\n", 1)
		}},
		{"an invalid command is warned about", func(s string) string {
			gate := strings.Index(s, "if ($runCommand -cnotmatch $RunCommandPattern) {")
			stop := strings.Index(s[gate:], "Stop-Install") + gate
			return s[:stop] + "Write-Info 'invalid'" + s[stop+len("Stop-Install"):]
		}},
		{"the platform is checked before the command", func(s string) string {
			s = strings.Replace(s, platform, "", 1)
			return strings.Replace(s, "    # Run mode: stdout belongs", platform+"    # Run mode: stdout belongs", 1)
		}},
		{"the map URL is not checked", func(s string) string {
			return strings.Replace(s, "        $valid = Test-RegistryUrl $commandMapUrl 'command map URL'\n", "        $valid = $true\n", 1)
		}},
		{"the install directory is written before the map is read", func(s string) string {
			s = strings.Replace(s, writable, "", 1)
			return strings.Replace(s, "    # A command the map does not list", writable+"    # A command the map does not list", 1)
		}},
		{"the footer prints in run mode", func(s string) string {
			return strings.Replace(s, "        if (-not $runCommand) { Write-Footer }\n", "        Write-Footer\n", 1)
		}},
		{"the command runs before the install completes", func(s string) string {
			s = strings.Replace(s, invoke, "", 1)
			return strings.Replace(s, complete, "    "+strings.ReplaceAll(invoke, "\n    ", "\n        ")+complete, 1)
		}},
		{"the map download is not capped", func(s string) string {
			return strings.Replace(s, "Save-PutnamiDownload $MapUrl $mapFile $CommandMapMaxBytes", "Save-PutnamiDownload $MapUrl $mapFile $MaxDownloadBytes", 1)
		}},
		{"the map scratch file is kept", func(s string) string {
			return strings.Replace(s, "        Remove-Item -LiteralPath $mapFile -Force -ErrorAction SilentlyContinue\n", "", 1)
		}},
		{"a failed map download is read", func(s string) string {
			return strings.Replace(s, "            Write-Hint \"download: $($download.Error)\"\n            Stop-Install\n        }\n        $mapText", "            Write-Hint \"download: $($download.Error)\"\n        }\n        $mapText", 1)
		}},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := m.apply(src)
			if mutated == src {
				t.Fatal("the mutation no longer applies")
			}
			if err := checkPS1ResolvesTheRunBeforeInstalling(mutated); err == nil {
				t.Fatal("the check accepted a script that installs before resolving the run, or runs too early")
			}
		})
	}
}

// checkPS1PinsThenRuns reports how src could run the command without pinning
// its extension the way install.sh does, run it after a failed pin, or hide
// its output or status, or nil when it cannot.
func checkPS1PinsThenRuns(src, shell string) error {
	body, err := psFunctionBody(src, "Invoke-RequestedCommand")
	if err != nil {
		return err
	}
	shellPin := regexp.MustCompile(`"\$binary" ((?:[a-z-]+ )+)"\$extension_ref"`).FindStringSubmatch(shell)
	if shellPin == nil {
		return errors.New(`install.sh no longer pins with "$binary" <words> "$extension_ref"`)
	}
	pin := "\n            & $Binary " + shellPin[1] + "$ExtensionRef 2>&1 | ForEach-Object { [Console]::Error.WriteLine([string]$_) }\n            $status = $LASTEXITCODE\n"
	run := "\n            & $Binary $Command\n            $status = $LASTEXITCODE\n"
	failed := "        if ($status -ne 0) {\n"
	for _, want := range []string{
		"\n    $ErrorActionPreference = 'Continue'\n    $PSNativeCommandUseErrorActionPreference = $false\n",
		pin, failed, run,
	} {
		if strings.Count(body, want) != 1 {
			return fmt.Errorf("Invoke-RequestedCommand does not run %q exactly once", strings.TrimSpace(want))
		}
	}
	if !(strings.Index(body, pin) < strings.Index(body, failed) && strings.Index(body, failed) < strings.Index(body, run)) {
		return errors.New("Invoke-RequestedCommand must pin, check the pin's status, then run the command")
	}
	refusal, err := psBlockAt(body, strings.Index(body, failed))
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(refusal), "Exit-RunMode $status\n            return") {
		return errors.New("a failed pin does not end run mode with the pin's status")
	}
	runAt, finallyAt := strings.Index(body, run), strings.Index(body, "\n    } finally {\n")
	if finallyAt < runAt || !strings.HasSuffix(strings.TrimSpace(body[runAt:finallyAt]), "Exit-RunMode $status") {
		return errors.New("run mode does not end with the command's status")
	}
	if strings.Count(body, "& $Binary") != 2 {
		return errors.New("Invoke-RequestedCommand runs the CLI more than twice")
	}
	return nil
}

// The pin is the invocation install.sh makes, and its output joins the
// installer's on stderr; the command's own output and status are its own.
func TestInstallPS1PinsTheExtensionLikeInstallSh(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-pins-like-install-sh")
	shell, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := readInstallPS1(t)
	if err := checkPS1PinsThenRuns(src, string(shell)); err != nil {
		t.Fatal(err)
	}
	mutations := []struct{ name, old, new string }{
		{"the pin keeps an older release", "extensions install --user --latest $ExtensionRef 2>&1", "extensions install --user $ExtensionRef 2>&1"},
		{"the pin writes to stdout", "$ExtensionRef 2>&1 | ForEach-Object { [Console]::Error.WriteLine([string]$_) }", "$ExtensionRef"},
		{"a failed pin runs the command", "            Exit-RunMode $status\n            return\n", "            Write-Info 'continuing'\n"},
		{"the command's output is dropped", "            & $Binary $Command\n", "            $null = & $Binary $Command\n"},
		{"a non-zero exit throws under the caller's preference", "    $PSNativeCommandUseErrorActionPreference = $false\n", ""},
		{"the status is not the command's", "            Write-InstallError \"Could not run ${Binary}: $($_.Exception.Message)\"\n        }\n        Exit-RunMode $status\n", "            Write-InstallError \"Could not run ${Binary}: $($_.Exception.Message)\"\n        }\n        Exit-RunMode 0\n"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(src, m.old) != 1 {
				t.Fatalf("the mutation no longer applies: install.ps1 does not contain exactly one %q", m.old)
			}
			if err := checkPS1PinsThenRuns(strings.Replace(src, m.old, m.new, 1), string(shell)); err == nil {
				t.Fatal("the check accepted a script that pins or runs unlike install.sh")
			}
		})
	}
}

// --- Behavior under PowerShell ----------------------------------------------

// ps1RunEnv is a hermetic harness run from a caller's directory: a fresh
// `git init` directory outside the profile, so git itself can prove nothing
// was written to it, and a CLI log that lives outside it.
type ps1RunEnv struct {
	*psEnv
	registry  *ps1Registry
	callerDir string
	cliLog    string
}

// newPS1RunEnv serves runStubCLI as the Windows release and commandMap as the
// command map. mapStatus, when set, answers the map with that status.
func newPS1RunEnv(t *testing.T, commandMap string, mapStatus int) *ps1RunEnv {
	t.Helper()
	e := newPSEnv(t)
	requireGit(t)
	archive := windowsArchive(t, "putnami.exe", runStubCLI())
	registry := newPS1Registry(t, ps1RegistryOptions{
		registryOptions: registryOptions{
			body:       archive,
			integrity:  "sha256:" + sha256Hex(archive),
			resolved:   stubVersion,
			commandMap: commandMap,
		},
		mapStatus: mapStatus,
	})
	callerDir := filepath.Join(e.root, "caller")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, callerDir, "init", "-q")
	e.workDir = callerDir
	cliLog := filepath.Join(e.root, "cli-calls.log")
	e.set("PUTNAMI_REGISTRY_URL", registry.URL).
		set("PUTNAMI_COMMAND_MAP_URL", registry.URL+"/install-commands.txt").
		set("PUTNAMI_TEST_RUN_LOG", cliLog)
	return &ps1RunEnv{psEnv: e, registry: registry, callerDir: callerDir, cliLog: cliLog}
}

func (r *ps1RunEnv) cliCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.cliLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (r *ps1RunEnv) requests(path string) int {
	count := 0
	for _, request := range r.registry.recorded() {
		if request.path == path {
			count++
		}
	}
	return count
}

// status returns the $LASTEXITCODE the script left behind, and whether it
// returned to its caller rather than exiting.
func (r *ps1RunEnv) status(t *testing.T) (string, bool) {
	t.Helper()
	return strings.TrimSpace(r.readState(t, "status.log")), r.readState(t, "returned.log") != ""
}

// assertRefusedBeforeInstalling proves a refusal happened before the CLI was
// downloaded and before anything was written: no binary, no .putnami, no CLI
// invocation, no scratch file, and nothing on stdout, which belongs to the
// command.
func (r *ps1RunEnv) assertRefusedBeforeInstalling(t *testing.T, res psResult) {
	t.Helper()
	if res.exitCode == 0 {
		t.Fatalf("the run was not refused:\n%s", res.output())
	}
	if got := r.requests("/putnami/cli/download"); got != 0 {
		t.Fatalf("the CLI was downloaded %d time(s) before the refusal:\n%s", got, res.output())
	}
	r.assertNothingInstalled(t)
	if _, err := os.Stat(filepath.Join(r.home, ".putnami")); err == nil {
		t.Fatalf("a refused run created %s:\n%s", filepath.Join(r.home, ".putnami"), res.output())
	}
	if calls := r.cliCalls(t); len(calls) != 0 {
		t.Fatalf("a refused run invoked the CLI: %q", calls)
	}
	if res.stdout != "" {
		t.Fatalf("a refused run wrote to stdout, which belongs to the command:\n%s", res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
	r.assertNoScratchLeft(t, res.output())
}

// runningIn reports whether stderr names dir, as the logical or the physical
// path, as the directory the command runs in.
func runningIn(t *testing.T, stderr, command, dir string) bool {
	t.Helper()
	return strings.Contains(stderr, "Running putnami "+command+" in "+dir+"...") ||
		strings.Contains(stderr, "Running putnami "+command+" in "+physicalPath(t, dir)+"...")
}

func TestInstallPS1RunResolvesPinsAndRunsTheCommandInTheCallerDirectory(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-runs-the-command-in-the-callers-directory")
	r := newPS1RunEnv(t, commandMap("lint-docs @acme/docs", "deploy @acme/deploy@^1.2.0"), 0)

	res := r.run(t, "--run", "deploy")
	if res.exitCode != 0 {
		t.Fatalf("run failed (exit %d):\n%s", res.exitCode, res.output())
	}
	want := []string{
		"pin argv=extensions install --user --latest @acme/deploy@^1.2.0",
		"run argv=deploy",
		"run cwd=" + physicalPath(t, r.callerDir),
		"run stdin=eof",
	}
	if got := r.cliCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CLI calls = %q, want %q\n%s", got, want, res.output())
	}
	// stdout belongs to the command; everything the installer says is on stderr.
	if res.stdout != "output of putnami deploy\n" {
		t.Fatalf("stdout carries more than the command's output:\n%s", res.output())
	}
	archive := windowsArchive(t, "putnami.exe", runStubCLI())
	for _, line := range []string{
		"putnami deploy is provided by @acme/deploy@^1.2.0",
		"Integrity verified (sha256:" + sha256Hex(archive) + ")",
		"Version stamp verified (" + stubVersion + ")",
		"Pinned @acme/deploy@^1.2.0",
	} {
		if !strings.Contains(res.stderr, line) {
			t.Fatalf("stderr does not report %q:\n%s", line, res.output())
		}
	}
	if !runningIn(t, res.stderr, "deploy", r.callerDir) {
		t.Fatalf("stderr does not name the caller's directory %s:\n%s", r.callerDir, res.output())
	}
	if strings.Contains(res.output(), "Next:") {
		t.Fatalf("run mode printed the install footer:\n%s", res.output())
	}
	if strings.Contains(res.output(), "\x1b[") {
		t.Fatalf("redirected run output contains ANSI escapes:\n%q", res.stderr)
	}
	if status, returned := r.status(t); status != "0" || !returned {
		t.Fatalf("run from text left $LASTEXITCODE %q (returned %v), want 0 and a return", status, returned)
	}
	if _, err := os.Stat(filepath.Join(r.binDir(), "putnami.exe")); err != nil {
		t.Fatalf("the CLI was not installed: %v\n%s", err, res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
	r.assertNoScratchLeft(t, res.output())
}

// From a file the script exits with the command's status; from text it leaves
// the status in $LASTEXITCODE and returns, since exit would end the caller's
// session.
func TestInstallPS1RunEndsWithTheCommandsExitStatus(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-ends-with-the-commands-status")
	for _, fromFile := range []bool{true, false} {
		t.Run(fmt.Sprintf("from file %v", fromFile), func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
			r.set("PUTNAMI_TEST_COMMAND_EXIT", "7")
			if fromFile {
				r.set("PUTNAMI_HARNESS_FROM_FILE", "1")
			}
			res := r.run(t, "--run", "deploy")
			status, returned := r.status(t)
			if status != "7" {
				t.Fatalf("$LASTEXITCODE = %q, want the command's 7:\n%s", status, res.output())
			}
			if fromFile && (res.exitCode != 7 || returned) {
				t.Fatalf("from a file: exit %d, returned %v; want exit 7 without returning:\n%s", res.exitCode, returned, res.output())
			}
			if !fromFile && !returned {
				t.Fatalf("from text the script exited instead of returning:\n%s", res.output())
			}
			if res.stdout != "output of putnami deploy\n" {
				t.Fatalf("the failing command's output did not reach stdout:\n%s", res.output())
			}
			assertGitDirUntouched(t, r.callerDir)
		})
	}
}

func TestInstallPS1RunDoesNotRunTheCommandWhenThePinFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "a-failed-pin-does-not-run-the-command")
	r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
	r.set("PUTNAMI_TEST_PIN_EXIT", "5").set("PUTNAMI_HARNESS_FROM_FILE", "1")

	res := r.run(t, "--run", "deploy")
	if res.exitCode != 5 {
		t.Fatalf("exit = %d, want the pin's 5:\n%s", res.exitCode, res.output())
	}
	if got := r.cliCalls(t); len(got) != 1 || got[0] != "pin argv=extensions install --user --latest @acme/deploy" {
		t.Fatalf("the command ran after a failed pin: %q", got)
	}
	if !strings.Contains(res.stderr, "putnami extensions install --user --latest @acme/deploy failed (exit 5); putnami deploy was not run") {
		t.Fatalf("the failed pin was not named:\n%s", res.output())
	}
	if !strings.Contains(res.stderr, "The CLI itself is installed: ") {
		t.Fatalf("the failed pin does not say the CLI is installed:\n%s", res.output())
	}
	if res.stdout != "" {
		t.Fatalf("a failed pin wrote to stdout:\n%s", res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
}

// /install.ps1?run=<command> serves the script with its placeholder set, and
// `irm | iex` then runs it with no argument at all.
func TestInstallPS1RunUsesTheCommandTheSiteBakedIn(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-site-bakes-one-line-of-install-ps1")
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantRun string
	}{
		{name: "baked value alone", wantRun: "deploy"},
		{name: "--run wins over the baked value", args: []string{"--run", "preview"}, wantRun: "preview"},
		{name: "PUTNAMI_RUN wins over the baked value", env: map[string]string{"PUTNAMI_RUN": "preview"}, wantRun: "preview"},
		{name: "--run wins over PUTNAMI_RUN", args: []string{"--run", "deploy"}, env: map[string]string{"PUTNAMI_RUN": "preview"}, wantRun: "deploy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, commandMap("deploy @acme/deploy", "preview @acme/preview"), 0)
			res, err := http.Get(r.registry.URL + "/install.ps1?run=deploy")
			if err != nil {
				t.Fatal(err)
			}
			served, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("GET /install.ps1?run=deploy: %d %v", res.StatusCode, err)
			}
			r.script = filepath.Join(r.root, "served-install.ps1")
			if err := os.WriteFile(r.script, served, 0o644); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.env {
				r.set(k, v)
			}
			out := r.run(t, tc.args...)
			if out.exitCode != 0 {
				t.Fatalf("baked run failed (exit %d):\n%s", out.exitCode, out.output())
			}
			calls := r.cliCalls(t)
			if len(calls) < 2 || calls[1] != "run argv="+tc.wantRun {
				t.Fatalf("CLI calls = %q, want putnami %s", calls, tc.wantRun)
			}
			if out.stdout != "output of putnami "+tc.wantRun+"\n" {
				t.Fatalf("stdout = %q, want the output of putnami %s", out.stdout, tc.wantRun)
			}
			assertGitDirUntouched(t, r.callerDir)
		})
	}
}

func TestInstallPS1RunRefusesACommandTheMapDoesNotList(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-refuses-before-installing")
	r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)

	res := r.run(t, "--run", "depoly")
	r.assertRefusedBeforeInstalling(t, res)
	if !strings.Contains(res.stderr, "putnami depoly is not a command the installer can run: "+r.registry.URL+"/install-commands.txt does not list it") {
		t.Fatalf("the refusal does not name the command and the map:\n%s", res.output())
	}
	if !strings.Contains(res.stderr, "Nothing was installed") {
		t.Fatalf("the refusal does not say nothing was installed:\n%s", res.output())
	}
}

// The shipped map resolves agent-readiness through its public extension.
func TestInstallPS1RunReadsTheShippedMap(t *testing.T) {
	t.Parallel()
	shipped := readFile(t, filepath.Join(filepath.Dir(installScriptPath(t)), "install-commands.txt"))
	r := newPS1RunEnv(t, string(shipped), 0)
	res := r.run(t, "--run", "agent-readiness")
	if res.exitCode != 0 {
		t.Fatalf("run failed (exit %d):\n%s", res.exitCode, res.output())
	}
	want := []string{
		"pin argv=extensions install --user --latest @putnami/agent-readiness",
		"run argv=agent-readiness",
	}
	if got := r.cliCalls(t); len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("CLI calls = %q, want %q first\n%s", got, want, res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
}

func TestInstallPS1RunIgnoresBlankAndIndentedCommentLines(t *testing.T) {
	t.Parallel()
	r := newPS1RunEnv(t, commandMap("   ", "\t", "  # an indented comment", "\t# a tab-indented comment", "deploy @acme/deploy", " "), 0)
	res := r.run(t, "--run", "deploy")
	if res.exitCode != 0 {
		t.Fatalf("run failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if got := r.cliCalls(t); len(got) == 0 || got[0] != "pin argv=extensions install --user --latest @acme/deploy" {
		t.Fatalf("CLI calls = %q, want the pin of @acme/deploy first\n%s", got, res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
}

func TestInstallPS1RunFailsClosedOnAMalformedCommandMap(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-refuses-before-installing")
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{name: "no header", body: "deploy @acme/deploy\n", want: "is not a command map: its first line is not " + commandMapHeader},
		{name: "another format version", body: "putnami.install-commands.v2\ndeploy @acme/deploy\n", want: "is not a command map"},
		{name: "header with a carriage return", body: commandMapHeader + "\r\ndeploy @acme/deploy\r\n", want: "is not a command map"},
		{name: "header after a byte order mark", body: "\ufeff" + commandMap("deploy @acme/deploy"), want: "is not a command map"},
		{name: "empty file", body: "", status: http.StatusOK, want: "is empty, not a command map"},
		{name: "one field", body: commandMap("deploy"), want: "line 4 is not \"<command> <@scope/name[@constraint]>\""},
		{name: "three fields", body: commandMap("deploy @acme/deploy extra"), want: "line 4 is not"},
		{name: "indented entry", body: commandMap(" deploy @acme/deploy"), want: "line 4 is not"},
		{name: "uppercase command", body: commandMap("Deploy @acme/deploy"), want: "line 4 names a command that is not"},
		{name: "unscoped extension", body: commandMap("deploy acme/deploy"), want: "line 4 names an extension that is not"},
		{name: "local path extension", body: commandMap("deploy C:\\extension"), want: "line 4 names an extension that is not"},
		{name: "substitution in the constraint", body: commandMap("deploy @acme/deploy@$(id)"), want: "line 4 names an extension that is not"},
		{name: "entry with a carriage return", body: commandMap("deploy @acme/deploy\r"), want: "line 4 names an extension that is not"},
		{name: "line holding a carriage return only", body: commandMap("\r", "deploy @acme/deploy"), want: "line 4 is not"},
		{name: "command listed twice", body: commandMap("deploy @acme/deploy", "deploy @other/deploy"), want: "lists deploy twice (line 5)"},
		{name: "malformed line after the match", body: commandMap("deploy @acme/deploy", "broken"), want: "line 5 is not"},
		{name: "map not found", body: "not found", status: http.StatusNotFound, want: "Could not download the command map from "},
		// Well-formed and listing the command, but over the 1 MiB limit: only the
		// limit refuses it.
		{name: "map larger than 1 MiB", body: commandMap(strings.Repeat("#\n", 1<<19) + "deploy @acme/deploy"), want: "Could not download the command map from "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, tc.body, tc.status)
			res := r.run(t, "--run", "deploy")
			r.assertRefusedBeforeInstalling(t, res)
			if !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("the refusal does not say %q:\n%s", tc.want, res.output())
			}
		})
	}
}

// The map decides which extension gets installed, so it is held to the
// registry's transport rule: https, loopback http, or an explicit opt-in.
func TestInstallPS1RunRefusesAnInsecureCommandMapURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, url string
		insecure  bool
		want      string
		forbid    string
	}{
		{name: "plaintext non-loopback", url: "http://map.invalid/install-commands.txt", want: "command map URL \"http://map.invalid/install-commands.txt\" uses plaintext http://"},
		{name: "unsupported scheme", url: "file:///etc/install-commands.txt", want: "has unsupported scheme \"file\""},
		// The host does not exist: the opt-in moves the failure from the scheme
		// check to the download.
		{name: "plaintext with the explicit opt-in", url: "http://map.invalid/install-commands.txt", insecure: true, want: "Could not download the command map", forbid: "plaintext http://"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
			r.set("PUTNAMI_COMMAND_MAP_URL", tc.url)
			if tc.insecure {
				r.set("PUTNAMI_ALLOW_INSECURE_REGISTRY", "1")
			}
			res := r.run(t, "--run", "deploy")
			r.assertRefusedBeforeInstalling(t, res)
			if !strings.Contains(res.stderr, tc.want) || (tc.forbid != "" && strings.Contains(res.stderr, tc.forbid)) {
				t.Fatalf("the refusal does not say %q:\n%s", tc.want, res.output())
			}
		})
	}
}

func TestInstallPS1RunRejectsAnInvalidCommandBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-refuses-before-installing")
	baked := strings.Replace(readInstallPS1(t), ps1RunPlaceholderLine, `$RunCommandDefault = 'Deploy'`, 1)
	tests := []struct {
		name  string
		baked bool
		args  []string
		env   map[string]string
		want  string
	}{
		{name: "uppercase", args: []string{"--run", "Deploy"}},
		{name: "leading dash", args: []string{"--run", "-deploy"}},
		{name: "command separator", args: []string{"--run", "deploy;id"}},
		{name: "subexpression", args: []string{"--run", "$(id)"}},
		{name: "backquotes", args: []string{"--run", "`id`"}},
		{name: "quote", args: []string{"--run", "deploy'"}},
		{name: "trailing newline", args: []string{"--run", "deploy\n"}},
		{name: "newline", args: []string{"--run", "deploy\nid"}},
		{name: "space", env: map[string]string{"PUTNAMI_RUN": "deploy now"}},
		{name: "Kelvin sign", args: []string{"--run", "\u212aelvin"}},
		{name: "too long", args: []string{"--run", strings.Repeat("a", 65)}},
		{name: "PowerShell spelling", args: []string{"-Run:Deploy"}},
		{name: "baked value the site would refuse", baked: true},
		{name: "missing value", args: []string{"--run", ""}, want: "--run requires a command"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
			if tc.baked {
				r.script = filepath.Join(r.root, "baked-install.ps1")
				if err := os.WriteFile(r.script, []byte(baked), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for k, v := range tc.env {
				r.set(k, v)
			}
			res := r.run(t, tc.args...)
			r.assertRefusedBeforeInstalling(t, res)
			want := tc.want
			if want == "" {
				want = "must match ^[a-z][a-z0-9-]{0,63}$. Nothing was installed."
			}
			if !strings.Contains(res.stderr, want) {
				t.Fatalf("the refusal does not say %q:\n%s", want, res.output())
			}
			if got := len(r.registry.recorded()); got != 0 {
				t.Fatalf("an invalid command made %d request(s):\n%s", got, res.output())
			}
		})
	}
}

// Declining agent-host registration is independent of running a command.
func TestInstallPS1RunHonorsNoAgentHosts(t *testing.T) {
	t.Parallel()
	for _, decline := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--no-agent-hosts", "--run", "deploy"}},
		{name: "environment", args: []string{"--run", "deploy"}, env: map[string]string{"PUTNAMI_NO_AGENT_HOSTS": "1"}},
	} {
		t.Run(decline.name, func(t *testing.T) {
			t.Parallel()
			r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
			for k, v := range decline.env {
				r.set(k, v)
			}
			claude := writeFakeAgentHost(t, r.state, r.fakeBin, filepath.Join(r.binDir(), "putnami.exe"), "claude", false)
			res := r.run(t, decline.args...)
			if res.exitCode != 0 {
				t.Fatalf("run failed (exit %d):\n%s", res.exitCode, res.output())
			}
			if !strings.Contains(res.stderr, "Skipping agent-host registration") {
				t.Fatalf("the declined registration was not reported on stderr:\n%s", res.output())
			}
			if data, err := os.ReadFile(claude.log); err == nil && len(data) != 0 {
				t.Fatalf("a declined run still called claude: %s", data)
			}
			if res.stdout != "output of putnami deploy\n" {
				t.Fatalf("the command did not run:\n%s", res.output())
			}
			assertGitDirUntouched(t, r.callerDir)
		})
	}
}

// The file test of install.ps1, run by real PowerShell: from a file the block
// exits, which returns to whoever ran the file; from text it returns, even
// when the text runs inside a caller's script, whose own run must go on.
func TestInstallPS1FileTestHoldsUnderPowerShell(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-exits-only-from-a-file")
	e := newPSEnv(t)
	src := readInstallPS1(t)
	exitBody, err := psFunctionBody(src, "Exit-RunMode")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "\n"+ps1FileDetection+"\n") {
		t.Fatal("install.ps1 no longer detects a file run with " + ps1FileDetection)
	}
	probe := filepath.Join(e.root, "probe.ps1")
	caller := filepath.Join(e.root, "caller.ps1")
	for path, body := range map[string]string{
		probe: "& {\nSet-StrictMode -Version 3.0\n$ErrorActionPreference = 'Stop'\n" + ps1FileDetection + "\n" +
			"function Exit-RunMode([int]$Status) {" + exitBody + "}\n" +
			"Exit-RunMode 7\n[Console]::Out.WriteLine('returned')\n} @args\n",
		caller: "Get-Content -Raw -LiteralPath '" + probe + "' | Invoke-Expression\n" +
			"[Console]::Out.WriteLine('caller went on after ' + $LASTEXITCODE)\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		args     []string
		exitCode int
		want     []string
		forbid   string
	}{
		{name: "powershell -File", args: []string{"-File", probe}, exitCode: 7, forbid: "returned"},
		{name: "& from a session", args: []string{"-Command", "& '" + probe + "'; [Console]::Out.WriteLine('session went on after ' + $LASTEXITCODE)"}, want: []string{"session went on after 7"}, forbid: "returned"},
		{name: "iex from a session", args: []string{"-Command", "Get-Content -Raw -LiteralPath '" + probe + "' | Invoke-Expression; [Console]::Out.WriteLine('session went on after ' + $LASTEXITCODE)"}, want: []string{"returned", "session went on after 7"}},
		{name: "iex from a caller's script", args: []string{"-File", caller}, want: []string{"returned", "caller went on after 7"}},
	}
	for _, tc := range tests {
		res := e.exec(t, tc.args...)
		if res.exitCode != tc.exitCode {
			t.Errorf("%s: exit %d, want %d:\n%s", tc.name, res.exitCode, tc.exitCode, res.output())
		}
		for _, want := range tc.want {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("%s: stdout lacks %q:\n%s", tc.name, want, res.output())
			}
		}
		if tc.forbid != "" && strings.Contains(res.stdout, tc.forbid) {
			t.Errorf("%s: the block went on after exit:\n%s", tc.name, res.output())
		}
	}
}

// The baked one-liner runs under real PowerShell: the served script parses,
// takes the baked command, and, on a host that is not Windows, refuses before
// it fetches the map, with nothing on stdout.
func TestInstallPS1BakedOneLinerRunsUnderPowerShell(t *testing.T) {
	t.Parallel()
	r := newPS1RunEnv(t, commandMap("deploy @acme/deploy"), 0)
	res := r.exec(t, "-Command", "irm \""+r.registry.URL+"/install.ps1?run=deploy\" | iex")
	if res.exitCode == 0 || !strings.Contains(res.stderr, "install.ps1 installs the Putnami CLI on Windows only.") {
		t.Fatalf("the baked script did not run to its platform refusal (exit %d):\n%s", res.exitCode, res.output())
	}
	if res.stdout != "" {
		t.Fatalf("the baked run wrote to stdout, which belongs to the command:\n%s", res.output())
	}
	if got := r.requests("/install-commands.txt"); got != 0 {
		t.Fatalf("the map was fetched %d time(s) before the platform check", got)
	}
	res = r.exec(t, "-Command", "irm \""+r.registry.URL+"/install.ps1?run=Deploy\" | iex")
	if res.exitCode == 0 || !strings.Contains(res.output(), "invalid run command") {
		t.Fatalf("the site served a command it must refuse (exit %d):\n%s", res.exitCode, res.output())
	}
	assertGitDirUntouched(t, r.callerDir)
}

// The behavioral run tests above need PowerShell. This static check holds the
// same two rules on every host: a failed pin ends the run before the command
// starts, and the command runs where the caller stands, with nothing written.
func TestInstallPS1RunOrderAndLocationHoldInTheScript(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "a-failed-pin-does-not-run-the-command")
	spectest.Proves(t, "cli/windows-consumers", "powershell-installer-run-form", "the-run-form-runs-the-command-in-the-callers-directory")
	body, err := psFunctionBody(readInstallPS1(t), "Invoke-RequestedCommand")
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Index(body, "& $Binary extensions install --user --latest $ExtensionRef")
	refusal := strings.Index(body, "if ($status -ne 0) {")
	command := strings.Index(body, "& $Binary $Command")
	if pin < 0 || refusal < 0 || command < 0 {
		t.Fatalf("Invoke-RequestedCommand lost its pin, its status check or its command call (pin %d, check %d, command %d)", pin, refusal, command)
	}
	if !(pin < refusal && refusal < command) {
		t.Fatalf("the command must start only after the pin status check: pin %d, check %d, command %d", pin, refusal, command)
	}
	failed, _, found := strings.Cut(body[refusal:command], "\n        }\n")
	if !found || !strings.Contains(failed, "Exit-RunMode $status") || !strings.Contains(failed, "return") {
		t.Fatalf("a failed pin must end the run before the command starts:\n%s", failed)
	}
	for _, mutation := range []string{"Set-Location", "Push-Location", "cd ", "New-Item", "Out-File", "Set-Content", "Add-Content", "Copy-Item", "Move-Item"} {
		if strings.Contains(body, mutation) {
			t.Errorf("Invoke-RequestedCommand must not change or write the caller's directory; it uses %s", mutation)
		}
	}
}
