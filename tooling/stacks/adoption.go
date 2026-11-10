package stacks

import (
	"fmt"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The adoption modes of a monorepo tool integration (D-064): import hands its
// projects to Putnami's runner and cache, delegate keeps the tool running them.
const (
	AdoptionImport   = "import"
	AdoptionDelegate = "delegate"
)

// CodeAdoptionModeInvalid is the diagnostic of a workspace that records an
// adoption mode that names none.
const CodeAdoptionModeInvalid = "stacks.adoption_mode_invalid"

// AdoptionMode is the adoption mode the workspace at root records for
// extension, in options[extension].mode of its putnami.workspace.json: import
// when it records none, as putnami init --from does without a terminal
// (D-064), and an error for a value that names no mode.
func AdoptionMode(root, extension string) (string, error) {
	cfg := wsproto.Load(root)
	if cfg == nil {
		return AdoptionImport, nil
	}
	recorded, present := cfg.Options[extension]["mode"]
	if !present || recorded == "" {
		return AdoptionImport, nil
	}
	return CheckAdoptionMode(recorded)
}

// CheckAdoptionMode answers mode when it names an adoption mode, and an error
// that names the two otherwise.
func CheckAdoptionMode(mode any) (string, error) {
	if s, ok := mode.(string); ok && (s == AdoptionImport || s == AdoptionDelegate) {
		return s, nil
	}
	return "", fmt.Errorf("unknown adoption mode %v: choose %s or %s", mode, AdoptionImport, AdoptionDelegate)
}
