package doctor

import (
	"fmt"
	"runtime"

	doctor "go.putnami.dev/protocol/doctor"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
)

// Workstation prerequisites (decisions D-W4 and D-W8). Unlike the
// committed-artifact checks, these read the checkout and the host: the line
// endings Git wrote, and on Windows the long-path settings and the Visual C++
// runtime Biome needs. They run only in
// `putnami doctor` itself, never in the production build gate, and they are
// advisory: they concern the machine a workspace is checked out on, not the
// workload it deploys.

// longPathsRegistryField names the registry value the long-path check reads.
const longPathsRegistryField = `HKLM\SYSTEM\CurrentControlSet\Control\FileSystem\LongPathsEnabled`

// vcRuntimeDLL is the Visual C++ runtime library Biome imports on Windows.
const vcRuntimeDLL = "vcruntime140.dll"

// vcRuntimeField names the file the Visual C++ runtime check looks for.
const vcRuntimeField = `%SystemRoot%\System32\` + vcRuntimeDLL

// workstationProbe answers the host questions the workstation checks ask.
// DoctorCommand wires the real host (systemWorkstationProbe); tests inject
// fixed answers.
type workstationProbe struct {
	// goos is the host operating system. The long-path and Visual C++ runtime
	// checks apply only on windows.
	goos string
	// crlfFiles lists the tracked text files whose working-tree copy has CRLF
	// endings while the index has LF. An error means the workspace is not a
	// Git checkout Git can read, and the line-ending check does not apply.
	crlfFiles func() ([]string, error)
	// gitConfig returns a Git configuration value and whether it is set.
	gitConfig func(key string) (string, bool, error)
	// lfPolicy reports whether .gitattributes checks the workspace out with LF
	// endings.
	lfPolicy func() (bool, error)
	// longPathsEnabled reads the Windows LongPathsEnabled registry value.
	longPathsEnabled func() (bool, error)
	// vcRuntimeInstalled reports whether the Visual C++ runtime is in the
	// Windows system directory.
	vcRuntimeInstalled func() (bool, error)
}

// systemWorkstationProbe returns the probe of the running host for the
// workspace at wsRoot.
func systemWorkstationProbe(wsRoot string) workstationProbe {
	return workstationProbe{
		goos:      runtime.GOOS,
		crlfFiles: func() ([]string, error) { return git.CRLFCheckouts(wsRoot) },
		gitConfig: func(key string) (string, bool, error) { return git.ConfigValue(wsRoot, key) },
		lfPolicy: func() (bool, error) {
			return git.HasLFPolicy(wsRoot, wsproto.WorkspaceConfigFilename)
		},
		longPathsEnabled:   readLongPathsEnabled,
		vcRuntimeInstalled: vcRuntimeInstalled,
	}
}

// checkWorkstation runs every workstation check and returns its findings, all
// scoped to the workspace root.
func checkWorkstation(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	var findings []doctor.Finding
	findings = append(findings, checkCRLFCheckout(profile, probe)...)
	if probe.goos == "windows" {
		findings = append(findings, checkLongPaths(profile, probe)...)
		findings = append(findings, checkGitLongPaths(profile, probe)...)
		findings = append(findings, checkVCRuntime(profile, probe)...)
	}
	return findings
}

// checkCRLFCheckout reports a checkout whose line endings differ from the
// committed bytes. Tracked files already rewritten with CRLF are named; when
// none is, core.autocrlf=true without the LF policy is reported as the
// conversion that will rewrite the next checkout. A workspace outside a Git
// checkout has no committed bytes to compare with and is not checked.
func checkCRLFCheckout(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	files, err := probe.crlfFiles()
	if err != nil {
		return nil
	}
	if len(files) > 0 {
		return []doctor.Finding{workstationFinding(doctor.CheckCRLFCheckout, profile, files[0], "",
			fmt.Sprintf("%d tracked text file(s) are checked out with CRLF line endings while the commit has LF (%s); every content digest of this checkout differs from the same commit elsewhere",
				len(files), summarizeNames(files)))}
	}
	value, set, err := probe.gitConfig("core.autocrlf")
	if err != nil || !set || !git.ConfigBool(value) {
		return nil
	}
	if policy, err := probe.lfPolicy(); err == nil && policy {
		return nil
	}
	return []doctor.Finding{workstationFinding(doctor.CheckCRLFCheckout, profile, ".gitattributes", "core.autocrlf",
		"core.autocrlf=true and no .gitattributes rule sets eol=lf for this workspace, so Git checks text files out with CRLF line endings")}
}

// checkLongPaths reports a Windows host where Win32 long paths are off. The
// store already holds paths longer than 260 characters, and a child process
// without its own long-path handling fails on them.
func checkLongPaths(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	if probe.longPathsEnabled == nil {
		return []doctor.Finding{workstationFinding(doctor.CheckLongPathsDisabled, profile, "", longPathsRegistryField,
			"this build cannot read LongPathsEnabled")}
	}
	enabled, err := probe.longPathsEnabled()
	switch {
	case err != nil:
		return []doctor.Finding{workstationFinding(doctor.CheckLongPathsDisabled, profile, "", longPathsRegistryField,
			fmt.Sprintf("cannot read LongPathsEnabled, so long paths may be off: %v", err))}
	case !enabled:
		return []doctor.Finding{workstationFinding(doctor.CheckLongPathsDisabled, profile, "", longPathsRegistryField,
			"Win32 long paths are off (LongPathsEnabled is not 1): programs fail on paths longer than 260 characters")}
	}
	return nil
}

// checkGitLongPaths reports a Windows host where Git does not set
// core.longpaths. Without it, Git for Windows cannot check out or read a path
// longer than 260 characters, even when Win32 long paths are on.
func checkGitLongPaths(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	value, set, err := probe.gitConfig("core.longpaths")
	switch {
	case err != nil:
		return []doctor.Finding{workstationFinding(doctor.CheckGitLongPathsDisabled, profile, "", "core.longpaths",
			fmt.Sprintf("cannot read core.longpaths; install Git for Windows and put git on PATH: %v", err))}
	case !set || !git.ConfigBool(value):
		return []doctor.Finding{workstationFinding(doctor.CheckGitLongPathsDisabled, profile, "", "core.longpaths",
			"Git does not set core.longpaths=true: git fails on paths longer than 260 characters")}
	}
	return nil
}

// checkVCRuntime reports a Windows host without the Visual C++ runtime. The
// Windows build of Biome, which @putnami/typescript runs to lint TypeScript and
// to format generated clients, imports vcruntime140.dll, and Windows refuses
// to start it without the DLL (exit status 0xC0000135).
func checkVCRuntime(profile doctor.Profile, probe workstationProbe) []doctor.Finding {
	if probe.vcRuntimeInstalled == nil {
		return []doctor.Finding{workstationFinding(doctor.CheckVCRuntimeMissing, profile, "", vcRuntimeField,
			"this build cannot look for the Visual C++ runtime")}
	}
	installed, err := probe.vcRuntimeInstalled()
	switch {
	case err != nil:
		return []doctor.Finding{workstationFinding(doctor.CheckVCRuntimeMissing, profile, "", vcRuntimeField,
			fmt.Sprintf("cannot look for the Visual C++ runtime, so Biome may not start: %v", err))}
	case !installed:
		return []doctor.Finding{workstationFinding(doctor.CheckVCRuntimeMissing, profile, "", vcRuntimeField,
			"the Visual C++ runtime (vcruntime140.dll) is not installed: Biome, which @putnami/typescript runs to lint and format TypeScript, does not start")}
	}
	return nil
}

// workstationFinding builds a workspace-scoped finding with the baked
// remediation of code, graded like every advisory code.
func workstationFinding(code doctor.CheckCode, profile doctor.Profile, path, field, message string) doctor.Finding {
	return doctor.Finding{
		Code:        code,
		Severity:    severityFor(code, profile, false),
		Profile:     profile,
		Project:     doctorWorkspaceScope,
		Message:     message,
		Evidence:    []doctor.Evidence{{Path: path, Field: field}},
		Remediation: doctor.Remediation(code),
	}
}
