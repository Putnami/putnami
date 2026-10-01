package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestDoctorReportsAProjectWithoutAReadme asserts doctor names a project that
// has no README.md with an advisory finding under every profile, and leaves a
// project that has one alone.
func TestDoctorReportsAProjectWithoutAReadme(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "doctor-names-a-missing-readme", "doctor-reports-a-project-without-a-readme")
	root := t.TempDir()
	for _, dir := range []string{"bare", "documented", "miscased"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "documented", "README.md"), []byte("# documented\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// readme.md passes a stat on a case-insensitive disk; it must still be reported.
	if err := os.WriteFile(filepath.Join(root, "miscased", "readme.md"), []byte("# miscased\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, profile := range doctor.ProfileValues {
		report, err := DoctorRun(root, []*workspace.Project{proj("bare"), proj("documented"), proj("miscased")}, profile, doctorTestClock)
		if err != nil {
			t.Fatalf("profile %s: a missing README blocked the run: %v", profile, err)
		}
		found := findingsByCode(report.Findings)[doctor.CheckMissingReadme]
		if len(found) != 2 || found[0].Project != "/bare" || found[1].Project != "/miscased" {
			t.Fatalf("profile %s: missing_readme findings = %#v, want one for /bare and one for /miscased", profile, found)
		}
		if found[0].Severity == doctor.SeverityHigh || found[0].Severity == doctor.SeverityCritical {
			t.Fatalf("profile %s: severity = %s, want advisory", profile, found[0].Severity)
		}
		if len(found[0].Evidence) != 1 || found[0].Evidence[0].Path != "bare/README.md" {
			t.Fatalf("profile %s: evidence = %#v, want bare/README.md", profile, found[0].Evidence)
		}
		if !strings.Contains(found[0].Remediation, "README.md") {
			t.Fatalf("profile %s: remediation = %q", profile, found[0].Remediation)
		}
	}
}

// TestDoctorExemptsATemplateFromTheReadmeCheck asserts a template project,
// which ships README.md.template, and a generated client project are not asked
// for a README.md of their own.
func TestDoctorExemptsATemplateFromTheReadmeCheck(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "doctor-names-a-missing-readme", "templates-and-generated-clients-are-exempt")
	root := t.TempDir()
	for _, dir := range []string{"tpl", "client"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	template := &workspace.Project{ID: "/tpl", Path: "tpl", Type: "template"}
	client := &workspace.Project{ID: "/client", Path: "client", GeneratedClient: &workspace.GeneratedClientBinding{}}
	report, err := DoctorRun(root, []*workspace.Project{template, client}, doctor.ProfileProduction, doctorTestClock)
	if err != nil {
		t.Fatal(err)
	}
	if found := findingsByCode(report.Findings)[doctor.CheckMissingReadme]; len(found) != 0 {
		t.Fatalf("an exempt project got %#v", found)
	}
}

// TestDoctorAcceptsAReadmeThatLinksToAFile asserts a README.md that is a
// symbolic link to a regular file satisfies the check.
func TestDoctorAcceptsAReadmeThatLinksToAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "linked", "doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "linked", "doc", "index.md"), []byte("# linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("doc", "index.md"), filepath.Join(root, "linked", "README.md")); err != nil {
		t.Skipf("this host cannot create a symbolic link: %v", err)
	}
	report, err := DoctorRun(root, []*workspace.Project{proj("linked")}, doctor.ProfileProduction, doctorTestClock)
	if err != nil {
		t.Fatal(err)
	}
	if found := findingsByCode(report.Findings)[doctor.CheckMissingReadme]; len(found) != 0 {
		t.Fatalf("a README.md linking to a file got %#v", found)
	}
}
