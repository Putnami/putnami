package components

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-analyses"

type fake struct {
	name  string
	found Found
}

func (f fake) Name() string        { return f.name }
func (f fake) Detect(string) Found { return f.found }
func ids(s Set) []string           { return idsOf(s.Components) }
func idsOf(list []Component) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}

func TestTheDetectorsCombineByPrecedence(t *testing.T) {
	spectest.Proves(t, feature, "components", "the-detectors-combine-by-precedence")
	workspace := fake{"node", Found{
		Components: []Component{
			{ID: "packages/pricing", Name: "@demo/pricing", Ecosystem: "node", Source: evidence.SourceWorkspace},
			{ID: "apps/api", Ecosystem: "node", Source: evidence.SourceWorkspace, DependsOn: []string{"packages/pricing", "packages/gone", "apps/api"}},
		},
		Diagnostics: []diag.Diagnostic{diag.Warningf("observe.node_manifest_invalid", "", "one")},
	}}
	orchestrator := fake{"nx", Found{Components: []Component{
		{ID: "packages/pricing", Name: "pricing", Ecosystem: "nx", Source: evidence.SourceOrchestrator},
	}}}
	manifest := fake{"composer", Found{Components: []Component{
		{ID: "apps/api", Ecosystem: "composer", Source: evidence.SourceManifest},
		{ID: "", Ecosystem: "composer", Source: "declared"},
	}}}
	set := Detect("", []Detector{workspace, orchestrator, manifest})
	if got := ids(set); !slices.Equal(got, []string{".", "apps/api", "packages/pricing"}) {
		t.Fatalf("components %v", got)
	}
	byID := map[string]Component{}
	for _, c := range set.Components {
		byID[c.ID] = c
	}
	if c := byID["packages/pricing"]; c.Source != evidence.SourceOrchestrator || c.Detector != "nx" || c.Name != "pricing" {
		t.Errorf("the orchestrator does not win: %+v", c)
	}
	if c := byID["apps/api"]; c.Source != evidence.SourceWorkspace || !slices.Equal(c.DependsOn, []string{"packages/pricing"}) {
		t.Errorf("the workspace does not win over the manifest, or unknown and self edges stay: %+v", c)
	}
	if c := byID["."]; c.Source != evidence.SourceManifest {
		t.Errorf("a detector cannot claim a hand declaration: %+v", c)
	}
	if len(set.Diagnostics) != 1 {
		t.Errorf("diagnostics %v", set.Diagnostics)
	}
	tie := Detect("", []Detector{
		fake{"first", Found{Components: []Component{{ID: "a", Ecosystem: "x", Source: evidence.SourceWorkspace}}}},
		fake{"second", Found{Components: []Component{{ID: "a", Ecosystem: "y", Source: evidence.SourceWorkspace}}}},
	})
	if tie.Components[0].Detector != "first" {
		t.Errorf("a tie goes to %s", tie.Components[0].Detector)
	}
}

func TestChangedFilesGiveChangedAndDependentComponents(t *testing.T) {
	spectest.Proves(t, feature, "components", "changed-files-give-changed-and-dependent-components")
	set := Detect("", []Detector{fake{"x", Found{Components: []Component{
		{ID: ".", Ecosystem: "x", Source: evidence.SourceManifest},
		{ID: "packages/pricing", Ecosystem: "x", Source: evidence.SourceManifest},
		{ID: "packages/pricing/plugins", Ecosystem: "x", Source: evidence.SourceManifest},
		{ID: "apps/api", Ecosystem: "x", Source: evidence.SourceManifest, DependsOn: []string{"packages/pricing"}},
		{ID: "apps/web", Ecosystem: "x", Source: evidence.SourceManifest, DependsOn: []string{"apps/api"}},
		{ID: "apps/admin", Ecosystem: "x", Source: evidence.SourceManifest},
	}}}})
	for file, want := range map[string]string{
		"packages/pricing/src/index.ts":     "packages/pricing",
		"packages/pricing/plugins/a.ts":     "packages/pricing/plugins",
		"packages/pricingx/a.ts":            ".",
		"README.md":                         ".",
		"apps/api/../../packages/pricing/x": "packages/pricing",
		// A submodule's gitlink is the directory, not a file inside it.
		"packages/pricing/plugins": "packages/pricing",
		"apps/admin":               ".",
	} {
		if got, ok := set.Owner(file); !ok || got != want {
			t.Errorf("owner of %s = %q, want %q", file, got, want)
		}
	}
	got := set.Impacts(map[string]bool{"packages/pricing": true})
	want := map[string]string{
		".": evidence.ImpactUnaffected, "packages/pricing": evidence.ImpactChanged, "packages/pricing/plugins": evidence.ImpactUnaffected,
		"apps/api": evidence.ImpactDependent, "apps/web": evidence.ImpactDependent, "apps/admin": evidence.ImpactUnaffected,
	}
	for id, impact := range want {
		if got[id] != impact {
			t.Errorf("impact of %s = %s, want %s", id, got[id], impact)
		}
	}
	none := Detect("", []Detector{fake{"x", Found{Components: []Component{{ID: "a", Ecosystem: "x", Source: evidence.SourceManifest}}}}})
	if _, ok := none.Owner("b/file"); ok {
		t.Error("a file outside every component has an owner")
	}
}

func TestARepositoryWithoutDetectedComponentsIsOneComponent(t *testing.T) {
	spectest.Proves(t, feature, "components", "a-repository-without-detected-components-is-one-component")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# nothing to detect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Detect(root, Default(Options{}))
	if len(set.Components) != 1 || set.Components[0].ID != evidence.RootComponent || set.Components[0].Source != evidence.SourceFallback || set.Components[0].Ecosystem != FallbackEcosystem {
		t.Fatalf("components %+v", set.Components)
	}
	if owner, ok := set.Owner("README.md"); !ok || owner != "." {
		t.Errorf("owner %q", owner)
	}
	if _, _, ok := set.Locate("example.com/x"); ok {
		t.Error("the fallback locates an import path")
	}
	if _, _, ok := set.Lockfile("go.mod"); ok {
		t.Error("the fallback reads a go.mod")
	}
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheDefaultDetectorsReadEachEcosystem(t *testing.T) {
	spectest.Proves(t, feature, "components", "the-detectors-combine-by-precedence")
	root := t.TempDir()
	write(t, root, map[string]string{
		"package.json":                  `{"name":"root","private":true,"workspaces":["packages/*","apps/*"]}`,
		"packages/pricing/package.json": `{"name":"@demo/pricing","version":"1.0.0"}`,
		"apps/api/package.json":         `{"name":"@demo/api","dependencies":{"@demo/pricing":"*"}}`,
		"go.work":                       "go 1.23\n\nuse ./svc\n",
		"svc/go.mod":                    "module example.com/svc\n\ngo 1.23\n\nrequire golang.org/x/text v0.21.0\n",
		"php/composer.json":             `{"name":"demo/php","autoload":{"psr-4":{"Demo\\":"src/"}}}`,
	})
	set := Detect(root, Default(Options{}))
	byID := map[string]Component{}
	for _, c := range set.Components {
		byID[c.ID] = c
	}
	for id, ecosystem := range map[string]string{"packages/pricing": "node", "apps/api": "node", "svc": "go", "php": "composer"} {
		if byID[id].Ecosystem != ecosystem {
			t.Errorf("%s: %+v, want ecosystem %s (all: %v)", id, byID[id], ecosystem, ids(set))
		}
	}
	if !slices.Equal(byID["apps/api"].DependsOn, []string{"packages/pricing"}) {
		t.Errorf("apps/api edges %v", byID["apps/api"].DependsOn)
	}
	if file, component, ok := set.Locate("example.com/svc/x/y.go"); !ok || file != "svc/x/y.go" || component != "svc" {
		t.Errorf("locate %s %s %v", file, component, ok)
	}
	ecosystem, read, ok := set.Lockfile("svc/go.mod")
	if !ok || ecosystem != "go" {
		t.Fatalf("go.mod reader %v %s", ok, ecosystem)
	}
	locked, err := read([]byte("module example.com/svc\n\nrequire golang.org/x/text v0.22.0 // indirect\n"))
	if err != nil || !slices.Equal(locked["golang.org/x/text"], []string{"v0.22.0"}) {
		t.Errorf("locked %v %v", locked, err)
	}
	jvmRoot := t.TempDir()
	write(t, jvmRoot, map[string]string{
		"settings.gradle.kts":      "include(\":app\", \":pricing\")\n",
		"app/build.gradle.kts":     "dependencies { implementation(projects.pricing) }\n",
		"pricing/build.gradle.kts": "",
		"pom.xml":                  "<project><groupId>com.acme</groupId><artifactId>shop</artifactId><packaging>pom</packaging><modules><module>tools</module></modules></project>",
		"tools/pom.xml":            "<project><groupId>com.acme</groupId><artifactId>tools</artifactId></project>",
	})
	jvmSet := Detect(jvmRoot, Default(Options{}))
	jvm := map[string]Component{}
	for _, c := range jvmSet.Components {
		jvm[c.ID] = c
	}
	if jvm["app"].Ecosystem != "gradle" || jvm["app"].Name != ":app" || !slices.Equal(jvm["app"].DependsOn, []string{"pricing"}) || jvm["tools"].Ecosystem != "maven" || jvm["tools"].Name != "com.acme:tools" || len(jvmSet.Components) != 3 {
		t.Errorf("the Gradle and Maven detectors: %+v", jvmSet.Components)
	}
	nxRoot := t.TempDir()
	write(t, nxRoot, map[string]string{
		"nx.json":             `{}`,
		"package.json":        `{"name":"root","private":true,"workspaces":["libs/*"]}`,
		"libs/a/package.json": `{"name":"a"}`,
		"libs/a/project.json": `{"name":"a"}`,
	})
	nxSet := Detect(nxRoot, Default(Options{}))
	if len(nxSet.Components) != 1 || nxSet.Components[0].Ecosystem != "nx" {
		t.Errorf("an Nx project does not win over its workspace package: %+v", nxSet.Components)
	}
}

func TestAPantsResolveLockfileIsAPyPILockfile(t *testing.T) {
	spectest.Proves(t, "tooling/observe-pants", "pants-resolve-lockfiles", "a-resolve-lockfile-is-a-pypi-lockfile")
	root := t.TempDir()
	config := "[python.resolves]\npython-default = \"3rdparty/python/default.lock\"\n"
	if err := os.WriteFile(filepath.Join(root, "pants.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Detect(root, Default(Options{}))
	ecosystem, read, ok := set.Lockfile("3rdparty/python/default.lock")
	if !ok || ecosystem != "pypi" {
		t.Fatalf("lockfile %q, %v", ecosystem, ok)
	}
	packages, err := read([]byte("// header\n{\"locked_resolves\": [{\"locked_requirements\": [{\"project_name\": \"FastAPI\", \"version\": \"0.115.6\"}]}]}\n"))
	if err != nil || !slices.Equal(packages["fastapi"], []string{"0.115.6"}) {
		t.Errorf("packages %v, %v", packages, err)
	}
	if _, _, ok := set.Lockfile("3rdparty/python/other.lock"); ok {
		t.Error("a lockfile no resolve names is read")
	}
}
