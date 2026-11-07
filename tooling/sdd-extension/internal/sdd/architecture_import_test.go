package sdd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func importWorkspace(t *testing.T, model, name string, projects ...*workspace.Project) (*workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, name)
	if err := os.WriteFile(source, []byte(model), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace.NewWorkspace(root, &wsproto.Config{Name: "import-test"}, projects), source
}

func unmappedReason(report ArchitectureImportReport, element string) string {
	for _, unmapped := range report.Unmapped {
		if unmapped.Element == element {
			return unmapped.Reason
		}
	}
	return ""
}

func TestImportReadsAJSONModelWithDomainOverrides(t *testing.T) {
	ws, source := importWorkspace(t, `{
  "name": "Shop",
  "model": {
    "softwareSystems": [
      {"id": "1", "name": "Shop", "properties": {"putnami.domain": "shop", "putnami.owner": "shop-team"},
       "containers": [
         {"id": "2", "name": "Web", "properties": {"path": "/web"},
          "relationships": [{"sourceId": "2", "destinationId": "4", "description": "Reads stock from"}]},
         {"id": "3", "name": "Worker", "properties": {"path": "worker"},
          "components": [{"id": "5", "name": "Job", "properties": {"path": "nowhere"}}]}
       ]},
      {"id": "4", "name": "9 Stock", "properties": {"path": "missing"}},
      {"id": "6", "name": "Ledger", "properties": {"putnami.domain": "Ledger!"}}
    ]
  }
}`, "workspace.json",
		&workspace.Project{ID: "/web", Name: "web", Path: "web"},
		&workspace.Project{ID: "/worker", Name: "worker", Path: "worker"},
	)
	report, err := BuildArchitectureImportResult(ws, ArchitectureImportOptions{
		Format: ImportFormatStructurizr, Source: source, SourceLabel: "workspace.json", At: "contracts",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(report.Manifests) != 1 || report.Manifests[0].Domain != "shop" {
		t.Fatalf("manifests = %+v, want the one shop domain", report.Manifests)
	}
	shop := report.Manifests[0]
	if shop.Path != "contracts/shop/putnami.architecture.json" {
		t.Errorf("path = %q", shop.Path)
	}
	if strings.Join(shop.AddedProjects, ",") != "/web,/worker" || !strings.Contains(shop.Contents, `"owner": "shop-team"`) {
		t.Errorf("shop = %+v", shop)
	}
	for element, want := range map[string]string{
		"9 Stock":             "is not a valid domain id",
		"Ledger":              "is not a valid domain id",
		"Shop/Worker/Job":     `path "nowhere" is no project of this workspace`,
		"Shop/Web -> 9 Stock": "its destination is not mapped",
	} {
		if !strings.Contains(unmappedReason(report, element), want) {
			t.Errorf("unmapped %q = %q, want %q", element, unmappedReason(report, element), want)
		}
	}
}

func TestImportRefusesWhatItCannotRead(t *testing.T) {
	ws, source := importWorkspace(t, "workspace {\n  model {\n    !include x.dsl\n  }\n}\n", "workspace.dsl")
	cases := map[string]ArchitectureImportOptions{
		"format":  {Format: "c4-plantuml", Source: source},
		"source":  {Format: ImportFormatStructurizr},
		"at":      {Format: ImportFormatStructurizr, Source: source, At: "../outside"},
		"missing": {Format: ImportFormatStructurizr, Source: filepath.Join(ws.Root, "none.dsl")},
		"include": {Format: ImportFormatStructurizr, Source: source},
	}
	for name, options := range cases {
		if _, err := BuildArchitectureImportResult(ws, options); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := BuildArchitectureImportResult(nil, cases["format"]); !errors.Is(err, protocolcli.ErrUsage) {
		t.Errorf("a nil workspace is not a usage error: %v", err)
	}
}

func TestLineDiffShowsHunks(t *testing.T) {
	before := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	after := "a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"
	diff := lineDiff("m.json", before, after)
	for _, want := range []string{"--- a/m.json", "+++ b/m.json", "@@ -1,5 +1,5 @@", "-b", "+B", "+k"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	if created := lineDiff("n.json", "", "x\n"); !strings.Contains(created, "--- /dev/null") || !strings.Contains(created, "@@ -0,0 +1,1 @@") {
		t.Errorf("creation diff:\n%s", created)
	}
}

func TestImportNamingHelpers(t *testing.T) {
	if got := slugImportName("E-mail System (legacy)"); got != "e-mail-system-legacy" {
		t.Errorf("slug = %q", got)
	}
	if got := truncateText(strings.Repeat("é", 10), 9); got != "ééé…" || len(got) > 9 {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateText("short", 9); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := importImportID("a/b", "c", 2); got != "a.b.uses.c.v2" {
		t.Errorf("import id = %q", got)
	}
	if got := importFactName("e-mail-system"); got != "e_mail_system.interface" {
		t.Errorf("fact = %q", got)
	}
}

func TestImportRefusesTwoNamesThatGiveOneDomain(t *testing.T) {
	ws, source := importWorkspace(t, `workspace {
    model {
        a = softwareSystem "Billing"
        b = softwareSystem "Billing!"
        a -> b "Calls"
    }
}`, "workspace.dsl")
	report, err := BuildArchitectureImportResult(ws, ArchitectureImportOptions{Format: ImportFormatStructurizr, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unmappedReason(report, "Billing!"), "which Billing already gives") {
		t.Errorf("unmapped = %+v", report.Unmapped)
	}
	if report.Unmapped == nil || len(report.Manifests) != 1 {
		t.Errorf("manifests = %+v", report.Manifests)
	}
}
