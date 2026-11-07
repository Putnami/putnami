package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `architecture import` end to end, on the Structurizr "Big Bank plc" example
// adapted to a fixture repository (ADR 0005).
//
// The code graph of the fixture:
//
//	/api                   -> /api/mainframe-facade, /api/email-component  (one domain)
//	/api/mainframe-facade  -> /mainframe   the model relates them
//	/api/email-component   -> /email       the model relates them
//	/web-application       -> /spa         the model relates them
//
// and, with unmodeled set, /mainframe -> /email, which no relationship allows.
func bigBankWorkspace(t *testing.T, unmodeled bool) (string, []*wsview.Project) {
	t.Helper()
	root := t.TempDir()
	model, err := os.ReadFile(filepath.Join("..", "..", "internal", "structurizr", "testdata", "big-bank.dsl"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "model", "workspace.dsl"), string(model))
	paths := []string{"api", "api/mainframe-facade", "api/email-component", "mainframe", "email", "spa", "web-application"}
	writeFixture(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"big-bank","includes":["`+strings.Join(paths, `","`)+`"]}`)
	dependencies := map[string][]string{
		"api":                  {"/api/email-component", "/api/mainframe-facade"},
		"api/mainframe-facade": {"/mainframe"},
		"api/email-component":  {"/email"},
		"web-application":      {"/spa"},
	}
	if unmodeled {
		dependencies["mainframe"] = []string{"/email"}
	}
	projects := make([]*wsview.Project, 0, len(paths))
	for _, path := range paths {
		name := strings.ReplaceAll(path, "/", "-")
		writeFixture(t, filepath.Join(root, filepath.FromSlash(path), "putnami.json"), `{"name":"`+name+`"}`)
		projects = append(projects, &wsview.Project{
			ID: "/" + path, Name: name, SourceName: name, Path: path, Dependencies: dependencies[path],
		})
	}
	return root, projects
}

func runImport(t *testing.T, root string, projects []*wsview.Project, params map[string]any) (sdd.ArchitectureImportReport, int, string) {
	t.Helper()
	all := map[string]any{"output": "json", "from": "structurizr"}
	for key, value := range params {
		all[key] = value
	}
	ctx := wireContext(root, projects, nil, all)
	stdout, _, code := runSubcommand(t, ctx, "architecture", "import", filepath.Join(root, "model", "workspace.dsl"))
	_, report := decodeEnvelope[sdd.ArchitectureImportReport](t, stdout)
	return report, code, stdout
}

func runValidate(t *testing.T, root string, projects []*wsview.Project) (int, string) {
	t.Helper()
	ctx := wireContext(root, projects, nil, map[string]any{"output": "json"})
	stdout, _, code := runSubcommand(t, ctx, "architecture", "validate")
	return code, stdout
}

func readManifest(t *testing.T, root, domain string) *archproto.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "architecture", filepath.FromSlash(domain), archproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diagnostics := archproto.ParseAndValidateManifest(data)
	if manifest == nil || len(diagnostics) > 0 {
		t.Fatalf("%s does not validate: %v\n%s", domain, diagnostics, data)
	}
	return manifest
}

// TestArchitectureImportWritesManifestsValidateAccepts is the done-when of
// issue 948: the import writes manifests the gate accepts.
func TestArchitectureImportWritesManifestsValidateAccepts(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "structurizr-models-import-as-manifests",
		"import-writes-manifests-validate-accepts")
	root, projects := bigBankWorkspace(t, false)

	report, code, stdout := runImport(t, root, projects, map[string]any{"apply": true})
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if report.Summary.Domains != 8 || report.Summary.Created != 8 || report.Summary.Written != 8 {
		t.Fatalf("summary = %+v, want 8 domains created and written", report.Summary)
	}
	api := readManifest(t, root, "internet-banking-system/api-application")
	if strings.Join(api.Projects, ",") != "/api,/api/email-component,/api/mainframe-facade" {
		t.Errorf("api projects = %v", api.Projects)
	}
	imports := map[string]archproto.Import{}
	for _, imported := range api.Imports {
		imports[imported.From.Domain] = imported
	}
	mainframe, ok := imports["mainframe-banking-system"]
	if !ok || mainframe.Mode != archproto.ModeReference || mainframe.From.Export != "mainframe-banking-system.interface.v1" {
		t.Fatalf("api imports = %+v", api.Imports)
	}
	if len(mainframe.Bindings) != 1 || mainframe.Bindings[0].ConsumerProject != "/api/mainframe-facade" {
		t.Errorf("mainframe bindings = %+v, want the observed facade edge", mainframe.Bindings)
	}
	if !strings.Contains(mainframe.Justification, "Makes API calls to [XML/HTTPS]") {
		t.Errorf("justification = %q", mainframe.Justification)
	}
	if _, ok := imports["internet-banking-system/database"]; !ok {
		t.Errorf("api imports no database: %+v", api.Imports)
	}
	if exports := readManifest(t, root, "mainframe-banking-system").Exports; len(exports) != 1 {
		t.Errorf("mainframe exports = %+v", exports)
	}

	if code, stdout := runValidate(t, root, projects); code != protocolcli.ExitSuccess {
		t.Fatalf("architecture validate exit = %d on the imported manifests\n%s", code, stdout)
	}
}

// TestArchitectureImportLeavesAnUnmodeledEdgeAViolation: a code dependency the
// model does not relate is not bound, and validate reports it.
func TestArchitectureImportLeavesAnUnmodeledEdgeAViolation(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "structurizr-models-import-as-manifests",
		"import-leaves-an-unmodeled-edge-a-violation")
	root, projects := bigBankWorkspace(t, true)

	report, code, stdout := runImport(t, root, projects, map[string]any{"apply": true})
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	for _, manifest := range report.Manifests {
		for _, binding := range manifest.AddedBindings {
			if binding.ConsumerProject == "/mainframe" {
				t.Errorf("the unmodeled edge was bound: %+v", binding)
			}
		}
	}
	code, stdout = runValidate(t, root, projects)
	if code == protocolcli.ExitSuccess {
		t.Fatalf("architecture validate passed with an unmodeled edge\n%s", stdout)
	}
	if !strings.Contains(stdout, archproto.ErrorCodeUndeclaredProjectDependency) || !strings.Contains(stdout, "/mainframe") {
		t.Errorf("validate does not name the unmodeled edge:\n%s", stdout)
	}
}

// TestArchitectureImportReportsWhatItCannotMap: each skipped element names its
// reason, and a run without --apply writes nothing.
func TestArchitectureImportReportsWhatItCannotMap(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "structurizr-models-import-as-manifests",
		"import-reports-what-it-cannot-map")
	root, projects := bigBankWorkspace(t, false)

	report, code, stdout := runImport(t, root, projects, nil)
	if code != protocolcli.ExitSuccess {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if report.Summary.Written != 0 || report.Summary.Changed != 8 {
		t.Errorf("summary = %+v, want 8 changes and nothing written", report.Summary)
	}
	if _, err := os.Stat(filepath.Join(root, "architecture")); !os.IsNotExist(err) {
		t.Errorf("a run without --apply wrote %s", filepath.Join(root, "architecture"))
	}
	reasons := map[string]string{}
	for _, unmapped := range report.Unmapped {
		reasons[unmapped.Element] = unmapped.Reason
	}
	for element, want := range map[string]string{
		"Personal Banking Customer":                                  "a person is an actor outside the code",
		"Internet Banking System/API Application/Sign In Controller": `a component maps to a project only through a "path" property`,
		"Internet Banking System -> Mainframe Banking System":        "spans several domains",
		"Internet Banking System/API Application/Sign In Controller -> Internet Banking System/API Application/Security Component": "stays inside domain internet-banking-system/api-application",
	} {
		if !strings.Contains(reasons[element], want) {
			t.Errorf("unmapped %q = %q, want it to say %q", element, reasons[element], want)
		}
	}
	diff := ""
	for _, manifest := range report.Manifests {
		diff += manifest.Diff
	}
	if !strings.Contains(diff, "+++ b/architecture/atm/putnami.architecture.json") || !strings.Contains(diff, `+  "domain": "atm",`) {
		t.Errorf("the diff does not show the new manifests:\n%s", diff)
	}
}

// TestArchitectureImportRerunKeepsHandEdits: a rerun only adds, so what a
// person changed in an imported manifest survives, and an import the model no
// longer has is reported, not removed.
func TestArchitectureImportRerunKeepsHandEdits(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "structurizr-models-import-as-manifests",
		"import-rerun-keeps-hand-edits")
	root, projects := bigBankWorkspace(t, false)
	if _, code, stdout := runImport(t, root, projects, map[string]any{"apply": true}); code != protocolcli.ExitSuccess {
		t.Fatalf("first import exit = %d\n%s", code, stdout)
	}

	// A person refines the owner and the justification, and the model drops the
	// ATM's relationship to the mainframe.
	atmPath := filepath.Join(root, "architecture", "atm", archproto.ManifestFilename)
	edited, err := os.ReadFile(atmPath)
	if err != nil {
		t.Fatal(err)
	}
	handEdited := strings.Replace(string(edited), `"owner": "atm"`, `"owner": "branch-network"`, 1)
	handEdited = strings.Replace(handEdited, `"justification": "From the Structurizr model:`, `"justification": "Reviewed: the ATM reads balances. From the Structurizr model:`, 1)
	writeFixture(t, atmPath, handEdited)
	modelPath := filepath.Join(root, "model", "workspace.dsl")
	model, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, modelPath, strings.Replace(string(model), `atm -> mainframe "Uses"`, "", 1))

	report, code, stdout := runImport(t, root, projects, map[string]any{"apply": true})
	if code != protocolcli.ExitSuccess {
		t.Fatalf("rerun exit = %d\n%s", code, stdout)
	}
	if report.Summary.Changed != 0 || report.Summary.Written != 0 {
		t.Errorf("rerun summary = %+v, want no change", report.Summary)
	}
	if report.Summary.NotInModel != 1 {
		t.Errorf("not in model = %d, want the dropped ATM import", report.Summary.NotInModel)
	}
	after, err := os.ReadFile(atmPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != handEdited {
		t.Errorf("the rerun rewrote the hand-edited manifest:\n%s", after)
	}
}
