package maven

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-jvm"

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

func codes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

const parentPOM = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.acme</groupId>
  <artifactId>shop</artifactId>
  <version>1.0.0</version>
  <packaging>pom</packaging>
  <modules>
    <module>pricing</module>
    <module>services</module>
  </modules>
  <profiles>
    <profile>
      <id>it</id>
      <modules><module>integration-tests/pom.xml</module></modules>
    </profile>
  </profiles>
</project>
`

func module(artifact, body string) string {
	return `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <parent><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version></parent>
  <artifactId>` + artifact + `</artifactId>
` + body + `
</project>
`
}

func TestPOMModulesAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "maven-modules-are-components", "pom-modules-are-components")
	root := writeTree(t, map[string]string{
		"pom.xml":         parentPOM,
		"pricing/pom.xml": module("pricing", ""),
		"services/pom.xml": module("services", `<packaging>pom</packaging>
  <subprojects><subproject>api</subproject><subproject>../pricing</subproject></subprojects>`),
		"services/api/pom.xml":      module("api", `<groupId>com.acme.services</groupId><packaging>war</packaging>`),
		"integration-tests/pom.xml": module("integration-tests", ""),
	})
	got := Detect(root)
	if len(got.Diagnostics) != 0 {
		t.Fatalf("diagnostics %+v", got.Diagnostics)
	}
	want := []Component{
		{ID: "integration-tests", Name: "com.acme:integration-tests"},
		{ID: "pricing", Name: "com.acme:pricing"},
		{ID: "services/api", Name: "com.acme.services:api"},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}

	single := writeTree(t, map[string]string{"pom.xml": module("calc", "<groupId>com.example</groupId>")})
	if got := Detect(single); !reflect.DeepEqual(got.Components, []Component{{ID: ".", Name: "com.example:calc"}}) {
		t.Errorf("a single-module build: %+v", got)
	}
	if got := Detect(t.TempDir()); len(got.Components) != 0 || len(got.Diagnostics) != 0 {
		t.Errorf("a directory without pom.xml: %+v", got)
	}
}

func TestAMissingOrInvalidPOMIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "maven-modules-are-components", "a-missing-or-invalid-pom-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"pom.xml": `<project><groupId>com.acme</groupId><artifactId>shop</artifactId><packaging>pom</packaging>
  <modules><module>gone</module><module>broken</module><module>../outside</module><module>ok</module><module>notes</module></modules>
</project>`,
		"broken/pom.xml": "<project><artifactId>broken",
		"notes/pom.xml":  "<settings/>",
		"ok/pom.xml":     module("ok", ""),
	})
	got := Detect(root)
	if want := []Component{{ID: "ok", Name: "com.acme:ok"}}; !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components %+v, want %+v", got.Components, want)
	}
	if want := []string{CodeModuleMissing, CodeModuleMissing, CodePOMInvalid, CodePOMInvalid}; !reflect.DeepEqual(codes(got.Diagnostics), want) {
		t.Errorf("diagnostics %+v, want %v", got.Diagnostics, want)
	}
}

func TestADependencyOnAModuleIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "maven-dependency-edges", "a-dependency-on-a-module-is-an-edge")
	root := writeTree(t, map[string]string{
		"pom.xml": `<project><groupId>com.acme</groupId><artifactId>shop</artifactId><packaging>pom</packaging>
  <modules><module>api</module><module>pricing</module><module>model</module><module>web</module><module>bom</module></modules>
</project>`,
		"pricing/pom.xml": module("pricing", ""),
		"model/pom.xml":   module("model", ""),
		"bom/pom.xml":     module("bom", "<packaging>pom</packaging>"),
		"api/pom.xml": module("api", `<properties><model.artifact>model</model.artifact></properties>
  <dependencies>
    <dependency><groupId>${project.groupId}</groupId><artifactId>pricing</artifactId><version>${project.version}</version></dependency>
    <dependency><groupId>org.junit.jupiter</groupId><artifactId>junit-jupiter</artifactId><scope>test</scope></dependency>
    <dependency><groupId>com.acme</groupId><artifactId>api</artifactId></dependency>
    <dependency><groupId>com.acme</groupId><artifactId>bom</artifactId><type>pom</type></dependency>
  </dependencies>
  <profiles><profile><id>full</id><dependencies>
    <dependency><groupId>${project.parent.groupId}</groupId><artifactId>${model.artifact}</artifactId></dependency>
  </dependencies></profile></profiles>`),
		"web/pom.xml": module("web", `<dependencyManagement><dependencies>
    <dependency><groupId>com.acme</groupId><artifactId>pricing</artifactId></dependency>
  </dependencies></dependencyManagement>
  <build><plugins><plugin><artifactId>p</artifactId><dependencies>
    <dependency><groupId>com.acme</groupId><artifactId>model</artifactId></dependency>
  </dependencies></plugin></plugins></build>`),
	})
	got := Detect(root)
	want := map[string][]string{"api": {"model", "pricing"}, "model": nil, "pricing": nil, "web": nil}
	if len(got.Components) != len(want) {
		t.Fatalf("components %+v", got.Components)
	}
	for _, c := range got.Components {
		if !reflect.DeepEqual(c.DependsOn, want[c.ID]) {
			t.Errorf("%s depends on %v, want %v", c.ID, c.DependsOn, want[c.ID])
		}
	}
}
