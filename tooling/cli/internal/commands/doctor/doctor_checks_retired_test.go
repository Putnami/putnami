package doctor

import (
	"os"
	"path/filepath"
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const retiredRequirement = "a-retired-extension-is-named"

// retiredNotice is the notice of the retired Intelligence extension.
const retiredNotice = "This checkout binds @putnami/intelligence, which is retired; rebase on the default branch or bind @putnami/cloud."

// writeWorkspace writes a workspace manifest into a new directory.
func writeWorkspace(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCheckWorkstation_ARetiredExtensionTheWorkspaceBindsIsNamed(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, retiredRequirement, "doctor-names-a-retired-extension-the-workspace-binds")
	root := writeWorkspace(t, `{"extensions":["@putnami/intelligence"],"options":{"collaboration":{"missions":{"provider":"@putnami/intelligence","version":1}}}}`)
	probe := fakeWorkstation()
	probe.retiredBindings = systemWorkstationProbe(root).retiredBindings
	findings := checkWorkstation(doctor.ProfileProduction, probe)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want one", findings)
	}
	finding := findings[0]
	if finding.Code != doctor.CheckRetiredExtensionBound || finding.Severity != doctor.SeverityWarning || finding.Project != doctorWorkspaceScope {
		t.Fatalf("finding = %#v", finding)
	}
	if finding.Message != retiredNotice {
		t.Fatalf("message = %q, want %q", finding.Message, retiredNotice)
	}
	if len(finding.Evidence) != 1 || finding.Evidence[0].Path != wsproto.WorkspaceConfigFilename {
		t.Fatalf("evidence = %#v", finding.Evidence)
	}
	if finding.Remediation == "" {
		t.Fatal("the finding has no remediation")
	}
}

func TestCheckWorkstation_NoRetiredExtensionIsQuiet(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, retiredRequirement, "doctor-is-quiet-without-a-retired-extension")
	root := writeWorkspace(t, `{"extensions":["@putnami/cloud"],"options":{"collaboration":{"missions":{"provider":"@putnami/cloud","version":2}}}}`)
	probe := fakeWorkstation()
	probe.retiredBindings = systemWorkstationProbe(root).retiredBindings
	if findings := checkWorkstation(doctor.ProfileProduction, probe); len(findings) != 0 {
		t.Fatalf("findings = %#v, want none", findings)
	}
}
