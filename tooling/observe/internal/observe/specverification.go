package observe

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

// Diagnostic codes of the spec verification record.
const (
	CodeSpecVerificationMissing  = "observe.spec_verification_missing"
	CodeSpecVerificationUnread   = "observe.spec_verification_unread"
	specVerificationFile         = "spec-verification.json"
	specVerificationSessionsGlob = ".putnami/sessions/*/" + specVerificationFile
)

// readSpecVerification returns the spec verification record of the
// repository's own Putnami workspace: the newest record a session of the
// engine wrote under .putnami/sessions, which `putnami test` writes when it
// verifies the specs. A repository that is no Putnami workspace has none and
// gets no diagnostic. A workspace whose CI verified no spec before observe
// ran gets a warning that names the step to add.
func readSpecVerification(root string) (json.RawMessage, []diag.Diagnostic) {
	if _, err := os.Stat(filepath.Join(root, analyze.WorkspaceFile)); err != nil {
		return nil, nil
	}
	matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(specVerificationSessionsGlob)))
	var newest json.RawMessage
	var newestAt time.Time
	var diags []diag.Diagnostic
	for _, file := range matches {
		if filepath.Base(filepath.Dir(file)) == "latest" {
			// latest links to one of the sessions listed beside it.
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			diags = append(diags, diag.Warningf(CodeSpecVerificationUnread, "specVerification", "the spec verification record %s cannot be read: %v", file, err))
			continue
		}
		if len(data) > evidence.MaxSpecVerificationBytes {
			diags = append(diags, diag.Warningf(CodeSpecVerificationUnread, "specVerification", "the spec verification record %s is over %d bytes, so it is left out", file, evidence.MaxSpecVerificationBytes))
			continue
		}
		var record struct {
			GeneratedAt string `json:"generatedAt"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			diags = append(diags, diag.Warningf(CodeSpecVerificationUnread, "specVerification", "the spec verification record %s is not a JSON object: %v", file, err))
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, record.GeneratedAt)
		if newest == nil || at.After(newestAt) {
			newest, newestAt = compact(data), at
		}
	}
	if newest == nil && len(diags) == 0 {
		diags = append(diags, diag.Warningf(CodeSpecVerificationMissing, "specVerification",
			"the repository is a Putnami workspace, but no spec verification record was found: run `putnami test` before `putnami observe` so the evidence carries which requirements are proven"))
	}
	return newest, diags
}

// compact returns a JSON document without its insignificant whitespace.
func compact(data []byte) json.RawMessage {
	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		return json.RawMessage(data)
	}
	return json.RawMessage(out.Bytes())
}
