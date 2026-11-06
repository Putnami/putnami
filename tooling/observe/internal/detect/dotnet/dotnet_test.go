package dotnet

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-dotnet"

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

// sln is a solution as dotnet new sln and dotnet sln add write it, with a
// byte order mark, CRLF line ends and solution folders.
const sln = "\xef\xbb\xbf\r\nMicrosoft Visual Studio Solution File, Format Version 12.00\r\n" +
	"# Visual Studio Version 17\r\n" +
	`Project("{2150E333-8FDC-42A3-9474-1A3956D46DE8}") = "src", "src", "{A9408F26-B093-416F-85D4-E8E5FE52F1F8}"` + "\r\nEndProject\r\n" +
	`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Pricing", "src\Pricing\Pricing.csproj", "{B2F797C7-2016-4B69-917D-114309AF047B}"` + "\r\nEndProject\r\n" +
	`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Api", "src\Api\Api.csproj", "{07D2D3BD-60A9-4733-899B-9054A2707FC4}"` + "\r\nEndProject\r\n" +
	`Project("{9A19103F-16F7-4668-BE54-9A1E7A4F7556}") = "Pricing.Tests", "tests\Pricing.Tests\Pricing.Tests.csproj", "{33A27E1E-B1F2-4215-9EEB-EBA57D2B14F6}"` + "\r\nEndProject\r\n" +
	"Global\r\nEndGlobal\r\n"

const pricing = "\xef\xbb\xbf" + `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
  </PropertyGroup>
</Project>
`

func projectFile(references ...string) string {
	var b strings.Builder
	b.WriteString("<Project Sdk=\"Microsoft.NET.Sdk\">\n  <ItemGroup>\n")
	for _, r := range references {
		b.WriteString(`    <ProjectReference Include="` + r + "\" />\n")
	}
	b.WriteString("  </ItemGroup>\n</Project>\n")
	return b.String()
}

func byID(d Detection) map[string]Component {
	out := map[string]Component{}
	for _, c := range d.Components {
		out[c.ID] = c
	}
	return out
}

func noDiagnostics(t *testing.T, d Detection) {
	t.Helper()
	if len(d.Diagnostics) != 0 {
		t.Errorf("diagnostics %v", d.Diagnostics)
	}
}

func TestSolutionProjectsAreWorkspaceComponents(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-projects-are-components", "solution-projects-are-workspace-components")
	root := writeTree(t, map[string]string{
		"Shop.sln":                                   sln,
		"src/Pricing/Pricing.csproj":                 pricing,
		"src/Api/Api.csproj":                         projectFile(`..\Pricing\Pricing.csproj`),
		"tests/Pricing.Tests/Pricing.Tests.csproj":   projectFile(`..\..\src\Pricing\Pricing.csproj`),
		"src/Api/bin/Debug/net8.0/Api.csproj":        pricing,
		"src/Api/obj/project.assets.json":            "{}",
		"tests/Pricing.Tests/TestResults/x/a.csproj": pricing,
	})
	d := Detect(root)
	noDiagnostics(t, d)
	want := []Component{
		{ID: "src/Api", Name: "Api", Source: evidence.SourceWorkspace, DependsOn: []string{"src/Pricing"}},
		{ID: "src/Pricing", Name: "Pricing", Source: evidence.SourceWorkspace},
		{ID: "tests/Pricing.Tests", Name: "Pricing.Tests", Source: evidence.SourceWorkspace, DependsOn: []string{"src/Pricing"}},
	}
	if !reflect.DeepEqual(d.Components, want) {
		t.Errorf("components\n got %+v\nwant %+v", d.Components, want)
	}

	slnx := writeTree(t, map[string]string{
		"Shop.slnx": `<Solution>
  <Folder Name="/src/">
    <Project Path="src/Pricing/Pricing.fsproj" />
  </Folder>
  <Project Path="src\Legacy\Legacy.vbproj" Type="Classic" />
</Solution>
`,
		"src/Pricing/Pricing.fsproj": pricing,
		"src/Legacy/Legacy.vbproj":   projectFile(`../Pricing/Pricing.fsproj`),
	})
	d = Detect(slnx)
	noDiagnostics(t, d)
	if got := byID(d); got["src/Pricing"].Source != evidence.SourceWorkspace || got["src/Legacy"].Source != evidence.SourceWorkspace || !reflect.DeepEqual(got["src/Legacy"].DependsOn, []string{"src/Pricing"}) {
		t.Errorf("slnx components %+v", d.Components)
	}
}

func TestAProjectOutsideTheSolutionsIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-projects-are-components", "a-project-outside-the-solutions-is-a-manifest-component")
	root := writeTree(t, map[string]string{
		"Shop.sln":                        sln,
		"src/Pricing/Pricing.csproj":      pricing,
		"src/Api/Api.csproj":              projectFile(),
		"tests/Pricing.Tests/T.csproj":    projectFile(),
		"tools/Migrate/Migrate.csproj":    projectFile(`../../src/Pricing/Pricing.csproj`),
		"tools/Migrate/Migrate.zz.csproj": projectFile(`../../src/Api/Api.csproj`),
		"Root.fsproj":                     pricing,
		".github/Hidden.csproj":           pricing,
		"node_modules/x/X.csproj":         pricing,
		"packages/Pkg/Pkg.csproj":         pricing,
	})
	got := byID(Detect(root))
	if want := (Component{ID: "tools/Migrate", Name: "Migrate", Source: evidence.SourceManifest, DependsOn: []string{"src/Api", "src/Pricing"}}); !reflect.DeepEqual(got["tools/Migrate"], want) {
		t.Errorf("tools/Migrate = %+v, want %+v", got["tools/Migrate"], want)
	}
	if want := (Component{ID: ".", Name: "Root", Source: evidence.SourceManifest}); !reflect.DeepEqual(got["."], want) {
		t.Errorf(". = %+v, want %+v", got["."], want)
	}
	if got["tests/Pricing.Tests"].Source != evidence.SourceManifest {
		t.Errorf("a project the solution names under another file name is %+v", got["tests/Pricing.Tests"])
	}
	if len(got) != 5 {
		t.Errorf("components %+v, want five", got)
	}
	if d := Detect(writeTree(t, map[string]string{"README.md": "no project"})); len(d.Components) != 0 || len(d.Diagnostics) != 0 {
		t.Errorf("a repository without a project file: %+v", d)
	}
}

func TestAMalformedSolutionIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-projects-are-components", "a-malformed-solution-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"A.sln":                      "not a solution",
		"B.slnx":                     "<Solution><Project Path=",
		"C.slnx":                     "<Project/>",
		"D.sln":                      sln,
		"src/Pricing/Pricing.csproj": "<Project><ItemGroup>",
	})
	d := Detect(root)
	codes := map[string]int{}
	var messages []string
	for _, x := range d.Diagnostics {
		if x.Severity != diag.Warning {
			t.Errorf("diagnostic %+v is not a warning", x)
		}
		codes[x.Code]++
		messages = append(messages, x.Message)
	}
	// A, B and C do not parse; D lists Api and Pricing.Tests, which are missing.
	if codes[CodeSolutionInvalid] != 5 || codes[CodeProjectInvalid] != 1 {
		t.Errorf("diagnostics %v", messages)
	}
	if want := []Component{{ID: "src/Pricing", Name: "Pricing", Source: evidence.SourceWorkspace}}; !reflect.DeepEqual(d.Components, want) {
		t.Errorf("components %+v, want the project that does not parse, with no edge", d.Components)
	}
}

func TestAProjectReferenceIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-project-references", "a-project-reference-is-an-edge")
	root := writeTree(t, map[string]string{
		"src/Pricing/Pricing.csproj": pricing,
		"src/Tax/Tax.fsproj":         pricing,
		"src/Api/Api.csproj":         projectFile(`..\Pricing\Pricing.csproj`, `../Tax/Tax.fsproj`, `..\Pricing\Pricing.csproj`),
		"src/Worker/Worker.vbproj":   projectFile(` ..\Api\..\Pricing\Pricing.csproj `),
	})
	got := byID(Detect(root))
	if want := []string{"src/Pricing", "src/Tax"}; !reflect.DeepEqual(got["src/Api"].DependsOn, want) {
		t.Errorf("Api depends on %v, want %v", got["src/Api"].DependsOn, want)
	}
	if want := []string{"src/Pricing"}; !reflect.DeepEqual(got["src/Worker"].DependsOn, want) {
		t.Errorf("Worker depends on %v, want %v", got["src/Worker"].DependsOn, want)
	}
}

func TestAnUnresolvedReferenceIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-project-references", "an-unresolved-reference-is-no-edge")
	root := writeTree(t, map[string]string{
		"src/Pricing/Pricing.csproj":           pricing,
		"src/Pricing/Pricing.Generated.csproj": projectFile(),
		"src/Api/Api.csproj": projectFile(
			`$(SolutionDir)src\Pricing\Pricing.csproj`,
			`..\..\..\outside\Lib.csproj`,
			`C:\src\Pricing\Pricing.csproj`,
			`/abs/Pricing.csproj`,
			`..\Missing\Missing.csproj`,
			`Api.csproj`,
			`..\Pricing\Other.csproj`,
		),
	})
	d := Detect(root)
	noDiagnostics(t, d)
	got := byID(d)
	if len(got["src/Api"].DependsOn) != 0 {
		t.Errorf("Api depends on %v, want nothing", got["src/Api"].DependsOn)
	}
	if got["src/Pricing"].Name != "Pricing.Generated" {
		t.Errorf("the first project file in name order names the directory: %+v", got["src/Pricing"])
	}
}

func TestATestAssemblyNamesItsProject(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-test-assemblies", "a-case-goes-to-the-project-of-its-test-assembly")
	root := writeTree(t, map[string]string{
		"src/Pricing/Pricing.csproj":               pricing,
		"tests/Pricing.Tests/Pricing.Tests.csproj": projectFile(),
		"tests/Named/Named.csproj": `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <AssemblyName> Shop.Acceptance </AssemblyName>
  </PropertyGroup>
</Project>`,
		"tests/Computed/Computed.csproj": `<Project><PropertyGroup><AssemblyName>$(Prefix).Computed</AssemblyName></PropertyGroup></Project>`,
	})
	d := Detect(root)
	for name, want := range map[string]string{
		"Pricing.Tests.dll": "tests/Pricing.Tests",
		"/home/runner/work/shop/tests/Pricing.Tests/bin/Debug/net8.0/Pricing.Tests.dll": "tests/Pricing.Tests",
		`C:\a\shop\tests\Pricing.Tests\bin\Release\net8.0\pricing.tests.DLL`:            "tests/Pricing.Tests",
		"Shop.Acceptance.dll":                "tests/Named",
		"Computed.exe":                       "tests/Computed",
		"Named.dll":                          "",
		"Pricing.Tests":                      "",
		"Pricing.Tests.PriceCalculatorTests": "",
		"Pricing":                            "",
		"Unknown.dll":                        "",
	} {
		file, component, ok := d.Locate(name)
		if component != want || ok != (want != "") || file != "" {
			t.Errorf("Locate(%q) = %q, %q, %v, want %q", name, file, component, ok, want)
		}
	}
}

func TestCentralPackagesAreReadOnlyWithoutLockFiles(t *testing.T) {
	spectest.Proves(t, feature, "dotnet-lockfiles", "central-package-versions-are-read-without-lock-files")
	files := map[string]string{
		"Directory.Packages.props":   "<Project/>",
		"src/Pricing/Pricing.csproj": pricing,
	}
	d := Detect(writeTree(t, files))
	if d.LockFiles || !d.IsCentralPackages("Directory.Packages.props") || !d.IsCentralPackages("src/Directory.Packages.props") || d.IsCentralPackages("Directory.Build.props") {
		t.Errorf("without lock files: %+v", d)
	}
	files["src/Pricing/packages.lock.json"] = "{}"
	if d := Detect(writeTree(t, files)); !d.LockFiles || d.IsCentralPackages("Directory.Packages.props") {
		t.Errorf("with lock files: %+v", d)
	}
	if d := Detect(writeTree(t, map[string]string{"Directory.Packages.props": "<Project/>"})); d.IsCentralPackages("Directory.Packages.props") {
		t.Error("a repository without a project reads its Directory.Packages.props")
	}
}
