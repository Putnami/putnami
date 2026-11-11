package stacks

import (
	"strings"
	"testing"
)

func TestAdoptionModeDefaultsToImportAndRefusesAnUnknownMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if mode, err := AdoptionMode(root, "@putnami/demo"); err != nil || mode != AdoptionImport {
		t.Errorf("no workspace config: %s %v", mode, err)
	}
	for recorded, want := range map[string]string{
		`{}`: AdoptionImport,
		`{"options": {"@putnami/demo": {"mode": "delegate"}}}`:  AdoptionDelegate,
		`{"options": {"@putnami/demo": {"mode": "import"}}}`:    AdoptionImport,
		`{"options": {"@putnami/other": {"mode": "delegate"}}}`: AdoptionImport,
	} {
		writeFile(t, root, "putnami.workspace.json", recorded)
		if mode, err := AdoptionMode(root, "@putnami/demo"); err != nil || mode != want {
			t.Errorf("%s: %s %v, want %s", recorded, mode, err, want)
		}
	}
	writeFile(t, root, "putnami.workspace.json", `{"options": {"@putnami/demo": {"mode": "delgate"}}}`)
	if _, err := AdoptionMode(root, "@putnami/demo"); err == nil || !strings.Contains(err.Error(), "delgate") {
		t.Errorf("an unknown mode: %v", err)
	}
	if _, err := CheckAdoptionMode(3); err == nil {
		t.Error("a mode that is no string is an error")
	}
}
