package doctor

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
)

// fakeWorkstation is a probe whose every answer is fixed: a clean Windows host
// with the LF policy, no CRLF file, long paths on in Windows and in Git, and
// the Visual C++ runtime installed.
// Each test changes the one answer it is about.
func fakeWorkstation() workstationProbe {
	config := map[string]string{"core.longpaths": "true"}
	return workstationProbe{
		goos:      "windows",
		crlfFiles: func() ([]string, error) { return nil, nil },
		gitConfig: func(key string) (string, bool, error) {
			value, ok := config[key]
			return value, ok, nil
		},
		lfPolicy:           func() (bool, error) { return true, nil },
		longPathsEnabled:   func() (bool, error) { return true, nil },
		vcRuntimeInstalled: func() (bool, error) { return true, nil },
	}
}

// withGitConfig returns probe answering key with value.
func withGitConfig(probe workstationProbe, key, value string) workstationProbe {
	next := probe.gitConfig
	probe.gitConfig = func(k string) (string, bool, error) {
		if k == key {
			return value, true, nil
		}
		return next(k)
	}
	return probe
}

func TestCheckWorkstation_CleanHostHasNoFinding(t *testing.T) {
	if findings := checkWorkstation(doctor.ProfileDev, fakeWorkstation()); len(findings) != 0 {
		t.Fatalf("clean workstation findings = %#v, want none", findings)
	}
}

// TestCheckWorkstation_CRLFFilesAreNamedWithTheFix: tracked files checked out
// with CRLF produce one workspace-scoped finding that counts them, names the
// first ones, and carries the baked LF-policy remediation.
func TestCheckWorkstation_CRLFFilesAreNamedWithTheFix(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "lf-checkout-policy", "doctor-names-crlf-files-with-the-fix")
	probe := fakeWorkstation()
	probe.crlfFiles = func() ([]string, error) {
		return []string{"app/a.go", "app/b.go", "app/c.go", "app/d.go"}, nil
	}
	f := findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout)
	if f.Project != doctorWorkspaceScope || f.Evidence[0].Path != "app/a.go" {
		t.Fatalf("finding scope = %q evidence = %#v, want the workspace root and the first file", f.Project, f.Evidence)
	}
	if !strings.Contains(f.Message, "4 tracked text file(s)") || !strings.Contains(f.Message, "app/a.go, app/b.go, app/c.go, … +1 more") {
		t.Fatalf("message = %q, want the count and the first file names", f.Message)
	}
	if !strings.Contains(f.Remediation, "* text=auto eol=lf") {
		t.Fatalf("remediation = %q, want the LF policy rule", f.Remediation)
	}
}

// TestCheckWorkstation_AutoCRLFNeedsTheLFPolicy: core.autocrlf=true is reported
// only when no .gitattributes rule checks the workspace out with LF.
func TestCheckWorkstation_AutoCRLFNeedsTheLFPolicy(t *testing.T) {
	probe := withGitConfig(fakeWorkstation(), "core.autocrlf", "true")
	if n := countCode(checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout); n != 0 {
		t.Fatalf("autocrlf with the LF policy: %d finding(s), want none", n)
	}

	probe.lfPolicy = func() (bool, error) { return false, nil }
	f := findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout)
	if f.Evidence[0].Path != ".gitattributes" || f.Evidence[0].Field != "core.autocrlf" {
		t.Fatalf("evidence = %#v, want .gitattributes / core.autocrlf", f.Evidence)
	}

	probe = withGitConfig(probe, "core.autocrlf", "input")
	if n := countCode(checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout); n != 0 {
		t.Fatalf("autocrlf=input converts only on commit: %d finding(s), want none", n)
	}
}

func TestCheckWorkstation_OutsideAGitCheckoutSkipsLineEndings(t *testing.T) {
	probe := withGitConfig(fakeWorkstation(), "core.autocrlf", "true")
	probe.lfPolicy = func() (bool, error) { return false, nil }
	probe.crlfFiles = func() ([]string, error) { return nil, errors.New("not a git repository") }
	if n := countCode(checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout); n != 0 {
		t.Fatalf("outside a checkout: %d line-ending finding(s), want none", n)
	}
}

// TestCheckWorkstation_LongPathPrerequisitesAreNamedWithTheirFix: on Windows,
// each missing long-path setting is its own finding whose remediation says how
// to enable it.
func TestCheckWorkstation_LongPathPrerequisitesAreNamedWithTheirFix(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-prerequisites-are-named", "doctor-names-each-long-path-setting-with-its-fix")
	probe := fakeWorkstation()
	probe.longPathsEnabled = func() (bool, error) { return false, nil }
	probe.gitConfig = func(string) (string, bool, error) { return "", false, nil }
	findings := checkWorkstation(doctor.ProfileDev, probe)

	win := findingByCode(t, findings, doctor.CheckLongPathsDisabled)
	if win.Evidence[0].Field != longPathsRegistryField || !strings.Contains(win.Remediation, "LongPathsEnabled -Value 1") {
		t.Fatalf("long paths finding = %#v, want the registry value and how to set it", win)
	}
	gitFinding := findingByCode(t, findings, doctor.CheckGitLongPathsDisabled)
	if gitFinding.Evidence[0].Field != "core.longpaths" || !strings.Contains(gitFinding.Remediation, "git config --global core.longpaths true") {
		t.Fatalf("git long paths finding = %#v, want core.longpaths and how to set it", gitFinding)
	}
}

// TestCheckWorkstation_UnreadableLongPathSettingsAreReported: a setting the
// check cannot read is reported, never assumed on.
func TestCheckWorkstation_UnreadableLongPathSettingsAreReported(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-prerequisites-are-named", "doctor-names-each-long-path-setting-with-its-fix")
	probe := fakeWorkstation()
	probe.longPathsEnabled = func() (bool, error) { return false, errors.New("access denied") }
	probe.gitConfig = func(string) (string, bool, error) { return "", false, exec.ErrNotFound }
	findings := checkWorkstation(doctor.ProfileDev, probe)
	if f := findingByCode(t, findings, doctor.CheckLongPathsDisabled); !strings.Contains(f.Message, "access denied") {
		t.Fatalf("message = %q, want the read error", f.Message)
	}
	if f := findingByCode(t, findings, doctor.CheckGitLongPathsDisabled); !strings.Contains(f.Message, "Git for Windows") {
		t.Fatalf("message = %q, want Git for Windows named", f.Message)
	}

	probe.longPathsEnabled = nil
	findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckLongPathsDisabled)
}

// TestCheckWorkstation_MissingVisualCppRuntimeIsNamedWithItsFix: on Windows, a
// host without vcruntime140.dll gets one finding that names the file, says
// Biome does not start, and carries the install command. A probe that cannot
// look, or a build without one, is reported, never assumed installed.
func TestCheckWorkstation_MissingVisualCppRuntimeIsNamedWithItsFix(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-prerequisites-are-named", "doctor-names-a-missing-visual-cpp-runtime-with-its-fix")
	probe := fakeWorkstation()
	probe.vcRuntimeInstalled = func() (bool, error) { return false, nil }
	findings := checkWorkstation(doctor.ProfileDev, probe)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want only the Visual C++ runtime", findings)
	}
	f := findingByCode(t, findings, doctor.CheckVCRuntimeMissing)
	if f.Project != doctorWorkspaceScope || f.Evidence[0].Field != `%SystemRoot%\System32\vcruntime140.dll` || f.Evidence[0].Path != "" {
		t.Fatalf("finding scope = %q evidence = %#v, want the workspace root and the system DLL", f.Project, f.Evidence)
	}
	if !strings.Contains(f.Message, "Biome") {
		t.Fatalf("message = %q, want Biome named", f.Message)
	}
	if !strings.Contains(f.Remediation, "winget install --id Microsoft.VCRedist.2015+.x64") || !strings.Contains(f.Remediation, "vc_redist.x64.exe") {
		t.Fatalf("remediation = %q, want the winget command and the installer", f.Remediation)
	}

	probe.vcRuntimeInstalled = func() (bool, error) { return false, errors.New("access denied") }
	if f := findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckVCRuntimeMissing); !strings.Contains(f.Message, "access denied") {
		t.Fatalf("message = %q, want the probe error", f.Message)
	}
	probe.vcRuntimeInstalled = nil
	findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckVCRuntimeMissing)
}

// TestCheckWorkstation_LongPathsApplyOnlyOnWindows: elsewhere the long-path
// and Visual C++ runtime checks do not run, whatever the probes would answer.
func TestCheckWorkstation_LongPathsApplyOnlyOnWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		probe := fakeWorkstation()
		probe.goos = goos
		probe.longPathsEnabled = func() (bool, error) { return false, nil }
		probe.vcRuntimeInstalled = func() (bool, error) { return false, nil }
		probe.gitConfig = func(string) (string, bool, error) { return "", false, nil }
		if findings := checkWorkstation(doctor.ProfileDev, probe); len(findings) != 0 {
			t.Fatalf("%s: findings = %#v, want none", goos, findings)
		}
	}
}

// TestDoctorRun_WorkstationFindingsAreAdvisoryAndWaivable: workstation findings
// never block, even under production, pass the frozen report validation, and
// a workspace-wide waiver suppresses them like any other finding.
func TestDoctorRun_WorkstationFindingsAreAdvisoryAndWaivable(t *testing.T) {
	probe := fakeWorkstation()
	probe.longPathsEnabled = func() (bool, error) { return false, nil }
	probe.vcRuntimeInstalled = func() (bool, error) { return false, nil }
	probe.crlfFiles = func() ([]string, error) { return []string{"a.txt"}, nil }

	root := t.TempDir()
	report, err := doctorRun(root, nil, doctor.ProfileProduction, doctorTestClock,
		checkWorkstation(doctor.ProfileProduction, probe))
	if err != nil {
		t.Fatalf("workstation findings must not block production, got: %v", err)
	}
	assertValidReport(t, report)
	if report.Summary.Warning != 3 || report.Summary.Findings != 3 {
		t.Fatalf("summary = %#v, want three warnings", report.Summary)
	}

	waivers := `{
  "protocolVersion": 1,
  "waivers": [
    {"code": "doctor.crlf_checkout", "owner": "team", "reason": "legacy checkout", "expires": "2099-01-01"}
  ]
}
`
	if err := os.WriteFile(filepath.Join(root, doctor.WaiverFilename), []byte(waivers), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = doctorRun(root, nil, doctor.ProfileDev, doctorTestClock,
		checkWorkstation(doctor.ProfileDev, probe))
	if err != nil {
		t.Fatalf("doctorRun with a waiver: %v", err)
	}
	assertValidReport(t, report)
	if f := findingByCode(t, report.Findings, doctor.CheckCRLFCheckout); f.WaivedBy == nil {
		t.Fatal("a workspace-wide waiver must suppress the CRLF finding")
	}
}

// TestPrintDoctorHuman_PrintsTheFix: the human report carries each finding's
// remediation, so a missing prerequisite is printed with how to enable it.
func TestPrintDoctorHuman_PrintsTheFix(t *testing.T) {
	probe := fakeWorkstation()
	probe.gitConfig = func(string) (string, bool, error) { return "", false, nil }
	report, _ := doctorRun(t.TempDir(), nil, doctor.ProfileDev, doctorTestClock,
		checkWorkstation(doctor.ProfileDev, probe))
	var buf bytes.Buffer
	printDoctorHuman(&buf, report)
	if !strings.Contains(buf.String(), "fix: Run `git config --global core.longpaths true`") {
		t.Fatalf("human output lacks the fix:\n%s", buf.String())
	}
}

// TestSystemWorkstationProbe_ReportsACRLFCheckout is the end-to-end line-ending
// check against a real Git checkout: a committed LF file rewritten with CRLF
// in the working tree is reported by the probe doctor wires for the host.
func TestSystemWorkstationProbe_ReportsACRLFCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitIn("init", "-q")
	gitIn("config", "user.email", "test@test.com")
	gitIn("config", "user.name", "Test")
	gitIn("config", "commit.gpgsign", "false")
	gitIn("config", "core.autocrlf", "false")
	file := filepath.Join(root, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "initial")

	probe := systemWorkstationProbe(root)
	if n := countCode(checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout); n != 0 {
		t.Fatalf("LF checkout: %d finding(s), want none", n)
	}
	if err := os.WriteFile(file, []byte("package main\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := findingByCode(t, checkWorkstation(doctor.ProfileDev, probe), doctor.CheckCRLFCheckout)
	if f.Evidence[0].Path != "main.go" {
		t.Fatalf("evidence = %#v, want main.go", f.Evidence)
	}
}
