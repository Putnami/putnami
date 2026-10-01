package ci

import (
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	distributionproto "go.putnami.dev/protocol/distribution"
)

func TestValidateMirrorDestinations(t *testing.T) {
	for _, target := range []string{
		"http://registry.example", "https://user:secret@registry.example",
		"user:secret@registry.example", "https://registry.example?token=secret",
		"https://registry.example/#fragment", "https://registry.example/\n",
		strings.Repeat("a", distributionproto.MaxMirrorTargetBytes+1),
	} {
		t.Run(target[:min(len(target), 50)], func(t *testing.T) {
			document := mustParse(t, readFixture(t, "valid", "workspace.json"))
			registry := document.Distribution.Registries["npm"]
			registry.Mirror.To = target
			document.Distribution.Registries["npm"] = registry
			findings := Validate(document)
			if !diag.HasErrors(findings) {
				t.Fatal("invalid mirror destination accepted")
			}
			for _, finding := range findings {
				if strings.Contains(finding.Message, "secret") && strings.Contains(finding.Message, target) {
					t.Fatal("mirror destination leaked in diagnostic")
				}
			}
		})
	}
	for _, target := range []string{"https://registry.npmjs.org", "ghcr.io/org", strings.Repeat("a", distributionproto.MaxMirrorTargetBytes)} {
		document := mustParse(t, readFixture(t, "valid", "workspace.json"))
		registry := document.Distribution.Registries["npm"]
		registry.Mirror.To = target
		document.Distribution.Registries["npm"] = registry
		if findings := Validate(document); diag.HasErrors(findings) {
			t.Fatalf("valid destination rejected: %v", findings)
		}
	}
}
