package observe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

const feature = "tooling/observe"

var now = time.Date(2026, 11, 2, 9, 30, 0, 0, time.FixedZone("CET", 3600))

func pullRequest() Observation {
	attempt := 1
	return Observation{
		Subject: ci.Subject{
			Provider: ci.ProviderGitHub, Host: "github.com", ProjectID: "123456", Repository: "acme/shop",
			SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "feature/cart",
			ChangeRequest: &ci.ChangeRequest{Number: 42},
		},
		CI:       ci.Run{System: ci.SystemGitHubActions, RunID: "987", JobID: "test", Attempt: &attempt, URL: "https://github.com/acme/shop/actions/runs/987"},
		Producer: Producer{Name: "@putnami/observe", Version: "1.2.3"},
		Base:     &ci.Base{Ref: "main", Source: ci.BaseFromPullRequest},
		ChangedFiles: []gitbase.ChangedFile{
			{Path: "packages/web/index.ts", Status: gitbase.StatusModified},
			{Path: "packages/web/new.ts", Status: gitbase.StatusRenamed, PreviousPath: "packages/web/old.ts"},
		},
		Diagnostics: []diag.Diagnostic{diag.Warningf("observe.report_missing", "coverage", "no coverage report was found")},
	}
}

func readEvidence(t *testing.T, path string) *evidence.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, problems := evidence.ParseAndValidate(data)
	if diag.HasErrors(problems) {
		t.Fatalf("the written document does not validate: %v\n%s", problems, data)
	}
	return doc
}

func TestTheEvidenceDocumentValidates(t *testing.T) {
	spectest.Proves(t, feature, "evidence-document", "the-evidence-document-validates")
	path := filepath.Join(t.TempDir(), "evidence.json")
	if diags := WriteEvidence(path, pullRequest(), now); len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	doc := readEvidence(t, path)
	if doc.GeneratedAt != "2026-11-02T08:30:00Z" || doc.Producer.Version != "1.2.3" {
		t.Fatalf("generatedAt %s producer %+v", doc.GeneratedAt, doc.Producer)
	}
	if doc.Subject.ProjectID != "123456" || doc.Subject.ChangeRequest == nil || doc.Subject.ChangeRequest.Number != 42 {
		t.Fatalf("subject %+v", doc.Subject)
	}
	want := []evidence.Component{{ID: ".", Ecosystem: "unknown", Source: evidence.SourceFallback, Impact: evidence.ImpactChanged}}
	if !reflect.DeepEqual(doc.Components, want) {
		t.Fatalf("components %+v", doc.Components)
	}
	if len(doc.ChangedFiles) != 2 || doc.ChangedFiles[1].PreviousPath != "packages/web/old.ts" || doc.ChangedFiles[0].Component != "." {
		t.Fatalf("changed files %+v", doc.ChangedFiles)
	}
	if len(doc.Diagnostics) != 1 || doc.Diagnostics[0].Code != "observe.report_missing" {
		t.Fatalf("diagnostics %+v", doc.Diagnostics)
	}
}

func TestAnUnchangedOrNoisyObservationStaysWithinTheDocumentBounds(t *testing.T) {
	spectest.Proves(t, feature, "evidence-document", "the-evidence-document-validates")
	o := pullRequest()
	o.ChangedFiles = nil
	o.Producer.Version = ""
	o.Diagnostics = nil
	for range evidence.MaxDiagnostics + 20 {
		o.Diagnostics = append(o.Diagnostics, diag.Warningf("observe.report_unrecognized", "junit", "%s", strings.Repeat("é", evidence.MaxStringBytes)))
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if diags := WriteEvidence(path, o, now); len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	doc := readEvidence(t, path)
	if doc.Components[0].Impact != evidence.ImpactUnaffected || doc.Producer.Version != "dev" {
		t.Fatalf("components %+v producer %+v", doc.Components, doc.Producer)
	}
	if len(doc.Diagnostics) != evidence.MaxDiagnostics || doc.Diagnostics[len(doc.Diagnostics)-1].Code != CodeTooManyDiagnostics {
		t.Fatalf("%d diagnostics, last %+v", len(doc.Diagnostics), doc.Diagnostics[len(doc.Diagnostics)-1])
	}
	if !strings.Contains(doc.Diagnostics[len(doc.Diagnostics)-1].Message, "21 more") {
		t.Fatalf("the bound diagnostic says %q", doc.Diagnostics[len(doc.Diagnostics)-1].Message)
	}
}

func TestAnInvalidEvidenceDocumentIsNotWritten(t *testing.T) {
	spectest.Proves(t, feature, "evidence-document", "an-invalid-evidence-document-is-not-written")
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		observation func() Observation
		path        string
		code        string
	}{
		"no commit": {
			observation: func() Observation { o := pullRequest(); o.Subject.SHA = ""; return o },
			path:        filepath.Join(dir, "no-commit.json"),
			code:        CodeEvidenceInvalid,
		},
		"a GitHub job without its project id": {
			observation: func() Observation { o := pullRequest(); o.Subject.ProjectID = ""; return o },
			path:        filepath.Join(dir, "no-id.json"),
			code:        CodeEvidenceInvalid,
		},
		"a directory that does not exist": {
			observation: pullRequest,
			path:        filepath.Join(dir, "missing", "evidence.json"),
			code:        CodeEvidenceUnwritable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			diags := WriteEvidence(tc.path, tc.observation(), now)
			if len(diags) != 1 || diags[0].Code != tc.code || diags[0].Severity != diag.Warning {
				t.Fatalf("diagnostics %v, want one %s warning", diags, tc.code)
			}
			if _, err := os.Stat(tc.path); !os.IsNotExist(err) {
				t.Fatalf("%s was written", tc.path)
			}
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the directory holds %v (%v)", entries, err)
	}
}
