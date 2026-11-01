package observe

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// Diagnostic codes of the evidence document.
const (
	CodeEvidenceInvalid      = "observe.evidence_invalid"
	CodeRunURLDropped        = "observe.run_url_dropped"
	CodeEvidenceUnwritable   = "observe.evidence_unwritable"
	CodeTooManyDiagnostics   = "observe.too_many_diagnostics"
	fallbackEcosystem        = "unknown"
	unstampedProducerVersion = "dev"
)

// Evidence returns the change evidence document of an observation made at
// now, with the components and the analyses the observation carries.
func Evidence(o Observation, now time.Time) *evidence.Document {
	version := o.Producer.Version
	if version == "" {
		version = unstampedProducerVersion
	}
	doc := &evidence.Document{
		ProtocolVersion: evidence.ProtocolVersion,
		Subject: evidence.Subject{
			Provider:   o.Subject.Provider,
			Host:       o.Subject.Host,
			ProjectID:  o.Subject.ProjectID,
			Repository: truncate(o.Subject.Repository),
			SHA:        o.Subject.SHA,
			BaseSHA:    o.Subject.BaseSHA,
			Branch:     truncate(o.Subject.Branch),
		},
		CI: evidence.CI{
			System:  o.CI.System,
			RunID:   truncate(o.CI.RunID),
			JobID:   truncate(o.CI.JobID),
			Attempt: o.CI.Attempt,
		},
		Producer:     evidence.Producer{Name: o.Producer.Name, Version: version},
		GeneratedAt:  now.UTC().Format(time.RFC3339),
		ChangedFiles: make([]evidence.ChangedFile, 0, len(o.ChangedFiles)),
	}
	if o.Subject.ChangeRequest != nil {
		doc.Subject.ChangeRequest = &evidence.ChangeRequest{Number: o.Subject.ChangeRequest.Number}
	}
	for _, file := range o.ChangedFiles {
		doc.ChangedFiles = append(doc.ChangedFiles, evidence.ChangedFile{
			Path: file.Path, Status: file.Status, PreviousPath: file.PreviousPath, Component: o.Analysis.Owners[file.Path],
		})
	}
	doc.Components = o.Analysis.Components
	if len(doc.Components) == 0 {
		doc.Components = fallbackComponents(len(doc.ChangedFiles) > 0)
		for i := range doc.ChangedFiles {
			doc.ChangedFiles[i].Component = evidence.RootComponent
		}
	}
	doc.Tests = o.Analysis.Tests
	doc.Coverage = o.Analysis.Coverage
	doc.ChangedLineCoverage = o.Analysis.ChangedLineCoverage
	doc.DependencyChanges = o.Analysis.DependencyChanges
	diags := o.Diagnostics
	if httpsURL(o.CI.URL) {
		doc.CI.URL = o.CI.URL
	} else if o.CI.URL != "" {
		diags = append(append([]diag.Diagnostic{}, diags...), diag.Warningf(CodeRunURLDropped, "ci.url",
			"the run page is left out of the evidence document, which takes only an https URL without credentials"))
	}
	doc.Diagnostics = boundedDiagnostics(diags)
	return doc
}

// fallbackComponents is the repository as one component, for an
// observation that carries no analysis.
func fallbackComponents(changed bool) []evidence.Component {
	impact := evidence.ImpactUnaffected
	if changed {
		impact = evidence.ImpactChanged
	}
	return []evidence.Component{{ID: evidence.RootComponent, Ecosystem: fallbackEcosystem, Source: evidence.SourceFallback, Impact: impact}}
}

// httpsURL reports whether raw is an https URL without credentials, the only
// run page the document takes.
func httpsURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

// WriteEvidence writes the evidence document of o to path when it validates,
// and returns the diagnostics of a document that does not, or of a path that
// cannot be written. The file is replaced in one rename.
func WriteEvidence(path string, o Observation, now time.Time) []diag.Diagnostic {
	doc := Evidence(o, now)
	if problems := evidence.ValidateDocument(doc); diag.HasErrors(problems) {
		return []diag.Diagnostic{diag.Warningf(CodeEvidenceInvalid, "evidence",
			"the evidence document was not written to %s: %s", path, truncate(diag.ErrorText(problems)))}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err == nil {
		err = writeFile(path, append(data, '\n'))
	}
	if err != nil {
		return []diag.Diagnostic{diag.Warningf(CodeEvidenceUnwritable, "evidence", "the evidence document cannot be written to %s: %v", path, err)}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// boundedDiagnostics keeps the diagnostics within the document's bounds: at
// most MaxDiagnostics, each message at most MaxStringBytes.
func boundedDiagnostics(list []diag.Diagnostic) []diag.Diagnostic {
	if len(list) == 0 {
		return nil
	}
	out := make([]diag.Diagnostic, 0, min(len(list), evidence.MaxDiagnostics))
	for _, d := range list {
		if len(out) == evidence.MaxDiagnostics-1 && len(list) > evidence.MaxDiagnostics {
			out = append(out, diag.Warningf(CodeTooManyDiagnostics, "", "%d more diagnostics are left out", len(list)-len(out)))
			break
		}
		d.Message = truncate(d.Message)
		d.Field = truncate(d.Field)
		out = append(out, d)
	}
	return out
}

// truncate cuts s to the document's string bound, on a rune boundary.
func truncate(s string) string {
	if len(s) <= evidence.MaxStringBytes {
		return s
	}
	cut := evidence.MaxStringBytes - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...", s[:cut])
}
