package lockdiff

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const dotnetFeature = "tooling/observe-dotnet"

func TestAPackagesLockIsANuGetLockfile(t *testing.T) {
	spectest.Proves(t, dotnetFeature, "dotnet-lockfiles", "a-packages-lock-is-a-nuget-lockfile")
	kind, ok := KindOf("src/Api/packages.lock.json")
	if !ok || kind.Ecosystem != EcosystemNuGet {
		t.Fatalf("KindOf(packages.lock.json) = %+v, %v", kind, ok)
	}
	got, err := kind.Read([]byte("\xef\xbb\xbf" + `{
  "version": 2,
  "dependencies": {
    "net8.0": {
      "pricing": {"type": "Project", "dependencies": {"Newtonsoft.Json": "[13.0.2, )"}},
      "Newtonsoft.Json": {"type": "CentralTransitive", "requested": "[13.0.2, )", "resolved": "13.0.2"},
      "xunit": {"type": "Direct", "requested": "[2.4.2, )", "resolved": "2.4.2"}
    },
    "net8.0/linux-x64": {
      "System.Runtime": {"type": "Transitive", "resolved": "4.3.0"}
    },
    "net6.0": {
      "Newtonsoft.Json": {"type": "Direct", "requested": "[13.0.1, )", "resolved": "13.0.1"}
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "packages.lock.json", got, map[string][]string{"Newtonsoft.Json": {"13.0.1", "13.0.2"}, "System.Runtime": {"4.3.0"}, "xunit": {"2.4.2"}})
	if _, err := ReadNuGetLock([]byte("<Project/>")); err == nil {
		t.Error("an XML packages.lock.json reads without error")
	}
}

func TestCentralPackageVersionsReadAsPackages(t *testing.T) {
	spectest.Proves(t, dotnetFeature, "dotnet-lockfiles", "central-package-versions-are-read-without-lock-files")
	if _, ok := KindOf(CentralPackages); ok {
		t.Error("Directory.Packages.props has a generic reader; the .NET detector decides when it is read")
	}
	got, err := ReadCentralPackages([]byte("\xef\xbb\xbf" + `<?xml version="1.0" encoding="utf-8"?>
<Project>
  <PropertyGroup>
    <ManagePackageVersionsCentrally>true</ManagePackageVersionsCentrally>
    <XunitVersion>2.4.2</XunitVersion>
  </PropertyGroup>
  <ItemGroup>
    <PackageVersion Include="Newtonsoft.Json" Version="13.0.3" />
    <PackageVersion Include="xunit" Version="$(XunitVersion)" />
    <PackageVersion Include="Serilog" Version=" 4.1.0 " Condition="'$(TargetFramework)' == 'net8.0'" />
  </ItemGroup>
</Project>`))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "Directory.Packages.props", got, map[string][]string{"Newtonsoft.Json": {"13.0.3"}, "Serilog": {"4.1.0"}})
	for name, content := range map[string]string{
		"not XML":    `{"Project": {}}`,
		"wrong root": `<Solution/>`,
		"truncated":  `<Project><ItemGroup>`,
	} {
		if _, err := ReadCentralPackages([]byte(content)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
