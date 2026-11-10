package pnpmworkspace

import (
	"reflect"
	"strings"
	"testing"
)

func TestPackagesReadsTheBlockAndFlowForms(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"block", "packages:\n  - apps/*\n  - 'packages/*' # libraries\n  - \"tools/cli\"\n", []string{"apps/*", "packages/*", "tools/cli"}},
		{"unindented block", "packages:\n- apps/api\n- packages/pricing\nonlyBuiltDependencies:\n  - esbuild\n", []string{"apps/api", "packages/pricing"}},
		{"flow", "packages: ['apps/*', \"packages/*\"] # members\n", []string{"apps/*", "packages/*"}},
		{"empty flow", "packages: []\n", []string{}},
		{"comments between items", "# workspace\npackages:\n  # apps\n  - apps/api\n\n  # libs\n  - packages/pricing\n", []string{"apps/api", "packages/pricing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := Packages(tc.text)
			if err != nil || !found {
				t.Fatalf("Packages = %v, %v, %v", got, found, err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Packages = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestPackagesReportsAMissingKeyAndRefusesAnotherShape(t *testing.T) {
	if _, found, err := Packages("catalog:\n  react: ^19\n"); found || err != nil {
		t.Fatalf("a file without packages: found=%v err=%v", found, err)
	}
	for _, text := range []string{"packages: apps/*\n", "packages:\n  apps: api\n", "packages: [a, [b]]\n"} {
		if _, _, err := Packages(text); err == nil {
			t.Errorf("Packages(%q) succeeded; want an error", text)
		}
	}
}

func TestSetPackagesReplacesOnlyTheList(t *testing.T) {
	text := "# the workspace\npackages:\n    - 'apps/*'\n    - packages/* # libs\n\n# settings\nonlyBuiltDependencies:\n  - esbuild\ncatalog:\n  react: ^19.0.0\n"
	got := SetPackages(text, []string{"apps/api", "packages/pricing"})
	want := "# the workspace\npackages:\n    - apps/api\n    - packages/pricing\n\n# settings\nonlyBuiltDependencies:\n  - esbuild\ncatalog:\n  react: ^19.0.0\n"
	if got != want {
		t.Fatalf("SetPackages =\n%s\nwant\n%s", got, want)
	}
	if list, _, err := Packages(got); err != nil || !reflect.DeepEqual(list, []string{"apps/api", "packages/pricing"}) {
		t.Fatalf("read back %v, %v", list, err)
	}
}

func TestSetPackagesWritesAFlowListAsABlockAndAddsAMissingKey(t *testing.T) {
	if got, want := SetPackages("packages: ['apps/*'] # members\nlinkWorkspacePackages: true\n", []string{"apps/api"}),
		"packages:\n  - apps/api\nlinkWorkspacePackages: true\n"; got != want {
		t.Fatalf("flow list:\n%s\nwant\n%s", got, want)
	}
	if got, want := SetPackages("catalog:\n  react: ^19\n", []string{"apps/api", "@scope/odd dir"}),
		"packages:\n  - apps/api\n  - '@scope/odd dir'\ncatalog:\n  react: ^19\n"; got != want {
		t.Fatalf("missing key:\n%s\nwant\n%s", got, want)
	}
	if got, want := SetPackages("", []string{"apps/api"}), "packages:\n  - apps/api\n"; got != want {
		t.Fatalf("empty file:\n%q\nwant\n%q", got, want)
	}
	if got, want := SetPackages("packages:\n  - apps/api\n", nil), "packages: []\n"; got != want {
		t.Fatalf("no member:\n%q\nwant\n%q", got, want)
	}
}

func TestSetPackagesKeepsCRLFLineEndings(t *testing.T) {
	got := SetPackages("packages:\r\n  - a\r\nx: 1\r\n", []string{"b"})
	if want := "packages:\r\n  - b\r\nx: 1\r\n"; got != want {
		t.Fatalf("SetPackages = %q, want %q", got, want)
	}
}

func TestReadCatalogsReadsTheDefaultAndNamedCatalogs(t *testing.T) {
	text := "packages:\n  - apps/*\ncatalog:\n  react: ^19.0.0 # ui\n  '@putnami/web': 1.2.3\ncatalogs:\n  framework:\n    \"@putnami/application\": latest\n  legacy: {react: ^18.0.0, '@types/node': 20.0.0}\n"
	got, err := ReadCatalogs(text)
	if err != nil {
		t.Fatal(err)
	}
	want := Catalogs{
		Default: map[string]string{"react": "^19.0.0", "@putnami/web": "1.2.3"},
		Named: map[string]map[string]string{
			"framework": {"@putnami/application": "latest"},
			"legacy":    {"react": "^18.0.0", "@types/node": "20.0.0"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadCatalogs = %#v, want %#v", got, want)
	}
	if got, err := ReadCatalogs("packages: []\n"); err != nil || got.Default != nil || got.Named != nil {
		t.Fatalf("no catalog: %#v, %v", got, err)
	}
}

func TestAddCatalogEntriesAddsAfterTheLastEntryAndKeepsTheRest(t *testing.T) {
	text := "packages:\n  - apps/*\ncatalog:\n    react: ^19.0.0\n    '@putnami/web': 1.2.3 # pinned\n\n# overrides\noverrides:\n  foo: 1.0.0\n"
	got, err := AddCatalogEntries(text, []Seed{
		{Name: "@putnami/application", Version: "latest"},
		{Name: "@putnami/web", Version: "latest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "packages:\n  - apps/*\ncatalog:\n    react: ^19.0.0\n    '@putnami/web': 1.2.3 # pinned\n    '@putnami/application': latest\n\n# overrides\noverrides:\n  foo: 1.0.0\n"
	if got != want {
		t.Fatalf("AddCatalogEntries =\n%s\nwant\n%s", got, want)
	}
}

func TestAddCatalogEntriesCreatesMissingCatalogs(t *testing.T) {
	got, err := AddCatalogEntries("packages:\n  - apps/*", []Seed{
		{Name: "@putnami/web", Version: "latest"},
		{Catalog: "framework", Name: "@putnami/application", Version: "1.0.0"},
		{Catalog: "framework", Name: "@putnami/runtime", Version: "1.0.0"},
		{Catalog: "tools", Name: "@putnami/cli", Version: "latest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "packages:\n  - apps/*\ncatalog:\n  '@putnami/web': latest\ncatalogs:\n  framework:\n    '@putnami/application': 1.0.0\n    '@putnami/runtime': 1.0.0\n  tools:\n    '@putnami/cli': latest\n"
	if got != want {
		t.Fatalf("AddCatalogEntries =\n%s\nwant\n%s", got, want)
	}
	cats, err := ReadCatalogs(got)
	if err != nil {
		t.Fatal(err)
	}
	if cats.Named["framework"]["@putnami/runtime"] != "1.0.0" || cats.Default["@putnami/web"] != "latest" {
		t.Fatalf("read back %#v", cats)
	}
}

func TestAddCatalogEntriesFillsAnEmptyFlowMappingAndRefusesAFullOne(t *testing.T) {
	got, err := AddCatalogEntries("catalog: {}\ncatalogs: {}\n", []Seed{{Name: "@putnami/web", Version: "latest"}, {Catalog: "f", Name: "x", Version: "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "catalog:\n  '@putnami/web': latest\ncatalogs:\n  f:\n    x: 1.0.0\n"; got != want {
		t.Fatalf("AddCatalogEntries =\n%s\nwant\n%s", got, want)
	}
	if _, err := AddCatalogEntries("catalog: {react: ^19}\n", []Seed{{Name: "@putnami/web", Version: "latest"}}); err == nil || !strings.Contains(err.Error(), "block mapping") {
		t.Fatalf("a full flow mapping: err = %v", err)
	}
}

func TestRenderScalarQuotesWhatYAMLWouldReadOtherwise(t *testing.T) {
	for in, want := range map[string]string{
		"apps/api":     "apps/api",
		"latest":       "latest",
		"^1.2.3":       "^1.2.3",
		"1.2.3":        "1.2.3",
		"1.2":          "'1.2'",
		"true":         "'true'",
		">=1.0.0":      "'>=1.0.0'",
		"@putnami/web": "'@putnami/web'",
		"it's":         "'it''s'",
		"a: b":         "'a: b'",
	} {
		if got := renderScalar(in); got != want {
			t.Errorf("renderScalar(%q) = %s, want %s", in, got, want)
		}
	}
}
