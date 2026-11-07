package observe

import (
	"testing"

	"go.putnami.dev/protocol/evidence"
)

func TestTheSummaryNamesEveryCISystem(t *testing.T) {
	seen := map[string]string{}
	for _, system := range evidence.CISystems {
		label := systemLabel(system)
		if label == "" || label == system {
			t.Errorf("system %q has no label, got %q", system, label)
		}
		if other, ok := seen[label]; ok {
			t.Errorf("systems %q and %q share the label %q", other, system, label)
		}
		seen[label] = system
	}
	if got := systemLabel(evidence.CISystemJenkins); got != "Jenkins" {
		t.Errorf("jenkins reads %q, want Jenkins", got)
	}
	if got := systemLabel(""); got != "generic CI" {
		t.Errorf("an empty system reads %q, want generic CI", got)
	}
}
