package ruby

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-ruby"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// rails is a Rails modular monolith: the application at the root, two
// packwerk packs, a local engine gem and a gem the engine depends on.
var rails = map[string]string{
	"Gemfile": `source "https://rubygems.org"

gem "rails", "~> 7.2.0"
gem "admin", path: "engines/admin" # the back office

group :development, :test do
  gem "rspec-rails"
end
`,
	"Gemfile.lock":    "GEM\n",
	"package.yml":     "enforce_dependencies: true\n",
	"packwerk.yml":    "include:\n  - \"{app,packs}/**/*.rb\"\n",
	"config/app.rb":   "",
	"app/models/a.rb": "",
	"packs/pricing/package.yml": `enforce_dependencies: true
enforce_privacy: true
`,
	"packs/checkout/package.yml": `# Checkout prices the cart.
enforce_dependencies: true
dependencies:
  - packs/pricing # prices
  - "."
metadata:
  owner: payments
`,
	"engines/admin/admin.gemspec": `Gem::Specification.new do |spec|
  spec.name        = "admin"
  spec.version     = "0.1.0"
  spec.files       = Dir["lib/**/*"]
  spec.add_dependency "rails", ">= 7.2"
  spec.add_dependency("audit_log")
end
`,
	"gems/audit_log/audit_log.gemspec": `Gem::Specification.new do |s|
  s.name = 'audit_log'
  s.version = '1.0.0'
  # s.add_dependency "admin"
end
`,
	"vendor/bundle/ruby/3.3.0/gems/rake-13.2.1/rake.gemspec": "Gem::Specification.new do |s|\n  s.name = \"rake\"\nend\n",
	"tmp/cache/Gemfile":      "",
	".bundle/Gemfile":        "",
	"node_modules/x/Gemfile": "",
	"coverage/package.yml":   "",
	"log/package.yml":        "",
}

func components(t *testing.T, files map[string]string) map[string]Component {
	t.Helper()
	detection := Detect(writeTree(t, files))
	out := map[string]Component{}
	for _, c := range detection.Components {
		out[c.ID] = c
	}
	for _, d := range detection.Diagnostics {
		t.Logf("diagnostic %s: %s", d.Code, d.Message)
	}
	return out
}

func TestPacksGemsAndTheApplicationAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "ruby-components", "packs-gems-and-the-application-are-components")
	got := components(t, rails)
	want := map[string]Component{
		".":              {ID: ".", Name: ".", Source: evidence.SourceWorkspace, DependsOn: []string{"engines/admin"}},
		"packs/pricing":  {ID: "packs/pricing", Name: "packs/pricing", Source: evidence.SourceWorkspace},
		"packs/checkout": {ID: "packs/checkout", Name: "packs/checkout", Source: evidence.SourceWorkspace, DependsOn: []string{".", "packs/pricing"}},
		"engines/admin":  {ID: "engines/admin", Name: "admin", Source: evidence.SourceManifest, DependsOn: []string{"gems/audit_log"}},
		"gems/audit_log": {ID: "gems/audit_log", Name: "audit_log", Source: evidence.SourceManifest},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("components\n got %+v\nwant %+v", got, want)
	}
}

func TestAnApplicationWithoutPacksIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "ruby-components", "packs-gems-and-the-application-are-components")
	got := components(t, map[string]string{"Gemfile": "source \"https://rubygems.org\"\ngem \"rails\"\n"})
	want := map[string]Component{".": {ID: ".", Source: evidence.SourceManifest}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("components %+v", got)
	}
	if detection := Detect(writeTree(t, map[string]string{"README.md": "# no Ruby\n"})); len(detection.Components) != 0 || len(detection.Diagnostics) != 0 {
		t.Errorf("a repository without Ruby files: %+v", detection)
	}
}

func TestAPackageYMLDependencyIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "ruby-edges", "a-package-yml-dependency-is-an-edge")
	for name, packageYML := range map[string]string{
		"block":            "dependencies:\n  - packs/pricing\n",
		"block at the key": "dependencies:\n- 'packs/pricing'\nenforce_privacy: true\n",
		"flow":             "enforce_dependencies: true\ndependencies: [\"packs/pricing\", packs/missing]\n",
		"document start":   "---\ndependencies:\n  # the prices\n  - packs/pricing\n",
		"outside":          "dependencies:\n  - packs/pricing\n  - ../elsewhere\n  - /abs\n  - packs/checkout\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := components(t, map[string]string{
				"packs/pricing/package.yml":  "",
				"packs/checkout/package.yml": packageYML,
			})
			if deps := got["packs/checkout"].DependsOn; !reflect.DeepEqual(deps, []string{"packs/pricing"}) {
				t.Errorf("checkout depends on %v", deps)
			}
		})
	}
}

func TestAMalformedPackageYMLIsAWarning(t *testing.T) {
	spectest.Proves(t, feature, "ruby-components", "a-malformed-package-yml-is-a-warning")
	for name, packageYML := range map[string]string{
		"scalar":  "dependencies: packs/pricing\n",
		"mapping": "dependencies:\n  pricing: packs/pricing\n",
		"item":    "dependencies:\n  - packs/pricing\n  - {name: packs/pricing}\n",
	} {
		t.Run(name, func(t *testing.T) {
			detection := Detect(writeTree(t, map[string]string{
				"packs/pricing/package.yml":  "",
				"packs/checkout/package.yml": packageYML,
			}))
			if len(detection.Components) != 2 {
				t.Fatalf("components %+v", detection.Components)
			}
			if deps := detection.Components[0].DependsOn; len(deps) != 0 {
				t.Errorf("checkout depends on %v", deps)
			}
			if len(detection.Diagnostics) != 1 || detection.Diagnostics[0].Code != CodePackageInvalid || detection.Diagnostics[0].Field != "packs/checkout/package.yml" {
				t.Errorf("diagnostics %+v", detection.Diagnostics)
			}
		})
	}
}

func TestAGemfilePathGemIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "ruby-edges", "a-gemfile-path-gem-is-an-edge")
	got := components(t, map[string]string{
		"app/Gemfile": `source "https://rubygems.org"
gem 'billing', :path => '../engines/billing'
gem("search", path: "../engines/search", require: false)
gem "missing", path: "../engines/missing"
gem "outside", path: "../../elsewhere"
gem "remote", git: "https://github.com/acme/remote.git"

if ENV["WITH_REPORTS"]
  gem "reports", path: "../engines/reports"
end

path "../gems" do
  gem "audit_log"
  gem "admin" # a gem of another path
end

group :test do
  gem "admin"
end
`,
		"engines/billing/billing.gemspec":  "Gem::Specification.new { |s| s.name = 'billing' }\n",
		"engines/search/search.gemspec":    "Gem::Specification.new { |s| s.name = 'search' }\n",
		"engines/reports/reports.gemspec":  "Gem::Specification.new { |s| s.name = 'reports' }\n",
		"engines/admin/admin.gemspec":      "Gem::Specification.new { |s| s.name = 'admin' }\n",
		"gems/audit_log/audit_log.gemspec": "Gem::Specification.new { |s| s.name = 'audit_log' }\n",
	})
	want := []string{"engines/billing", "engines/reports", "engines/search", "gems/audit_log"}
	if deps := got["app"].DependsOn; !reflect.DeepEqual(deps, want) {
		t.Errorf("app depends on %v, want %v", deps, want)
	}
}

func TestAGemspecDependencyOnALocalGemIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "ruby-edges", "a-gemspec-dependency-on-a-local-gem-is-an-edge")
	got := components(t, map[string]string{
		"engines/admin/admin.gemspec": `Gem::Specification.new do |spec|
  spec.name = "admin"
  spec.add_runtime_dependency "pricing", "~> 1.0"
  spec.add_development_dependency 'factories'
  spec.add_dependency "rails"
  spec.add_dependency "admin"
end
`,
		"gems/pricing/pricing.gemspec":     "Gem::Specification.new do |s|\n  s.name = \"pricing\"\nend\n",
		"gems/factories/factories.gemspec": "Gem::Specification.new do |s|\n  s.name = \"factories\"\nend\n",
	})
	if deps := got["engines/admin"].DependsOn; !reflect.DeepEqual(deps, []string{"gems/factories", "gems/pricing"}) {
		t.Errorf("admin depends on %v", deps)
	}
}

func TestPackageDependenciesReadsOnlyTheTopLevelKey(t *testing.T) {
	deps, err := packageDependencies([]byte("metadata:\n  dependencies:\n    - nope\nignored_dependencies: [x]\n"))
	if err != nil || len(deps) != 0 {
		t.Errorf("deps %v, err %v", deps, err)
	}
	deps, err = packageDependencies([]byte("dependencies: []\n"))
	if err != nil || len(deps) != 0 {
		t.Errorf("deps %v, err %v", deps, err)
	}
	if _, err := packageDependencies([]byte("dependencies: packs/x\n")); err == nil || !strings.Contains(err.Error(), "not a list") {
		t.Errorf("err %v", err)
	}
}
