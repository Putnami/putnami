package maven

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAPOMListsItsDeclaredVersions(t *testing.T) {
	spectest.Proves(t, feature, "maven-dependency-changes", "a-pom-lists-its-declared-versions")
	// The POMs of this repository's Maven sample: a Spring Boot parent, and
	// a module whose dependencies take their versions from it.
	module := func(coordinates string) bool {
		return coordinates == "dev.putnami.samples:java-maven-sample" || coordinates == "dev.putnami.samples:library" || coordinates == "dev.putnami.samples:application"
	}
	for name, want := range map[string]map[string][]string{
		"spring/pom.xml": {"org.springframework.boot:spring-boot-starter-parent": {"3.5.16"}},
		// The parent and the library are modules; the other dependencies
		// state no version.
		"spring/application/pom.xml": {},
	} {
		got, err := ReadDeclaredVersions(readTestdata(t, name), module)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	// Without the modules, the parent and the library are packages too.
	got, err := ReadDeclaredVersions(readTestdata(t, "spring/application/pom.xml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"dev.putnami.samples:java-maven-sample": {"1.0.0"}, "dev.putnami.samples:library": {"1.0.0"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("without modules: %v, want %v", got, want)
	}

	got, err = ReadDeclaredVersions([]byte(`<project>
  <parent><groupId>com.acme</groupId><artifactId>shop</artifactId><version>2.0.0</version></parent>
  <artifactId>pricing</artifactId>
  <version>${revision}</version>
  <properties>
    <revision>2.1.0</revision>
    <jackson.version>2.18.2</jackson.version>
    <jackson.bom>${jackson.version}</jackson.bom>
    <loop>${loop}</loop>
  </properties>
  <dependencyManagement>
    <dependencies>
      <dependency><groupId>com.fasterxml.jackson</groupId><artifactId>jackson-bom</artifactId><version>${jackson.bom}</version><type>pom</type><scope>import</scope></dependency>
    </dependencies>
  </dependencyManagement>
  <dependencies>
    <dependency><groupId>${project.groupId}</groupId><artifactId>pricing-api</artifactId><version>${project.version}</version></dependency>
    <dependency><groupId>${project.parent.groupId}</groupId><artifactId>tools</artifactId><version>${project.parent.version}</version></dependency>
    <dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>33.4.0-jre</version></dependency>
    <dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>33.4.0-jre</version><classifier>sources</classifier></dependency>
    <dependency><groupId>org.slf4j</groupId><artifactId>slf4j-api</artifactId><version>${slf4j.version}</version></dependency>
    <dependency><groupId>org.example</groupId><artifactId>looping</artifactId><version>${loop}</version></dependency>
    <dependency><groupId>org.example</groupId><artifactId>ranged</artifactId><version>[1.0,2.0)</version></dependency>
    <dependency><groupId>org.junit.jupiter</groupId><artifactId>junit-jupiter</artifactId><scope>test</scope></dependency>
  </dependencies>
  <profiles>
    <profile>
      <dependencies>
        <dependency><groupId>org.postgresql</groupId><artifactId>postgresql</artifactId><version>42.7.4</version></dependency>
      </dependencies>
      <dependencyManagement>
        <dependencies>
          <dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>32.1.3-jre</version></dependency>
        </dependencies>
      </dependencyManagement>
    </profile>
  </profiles>
</project>`), func(coordinates string) bool { return coordinates == "com.acme:shop" })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"com.acme:pricing-api":              {"2.1.0"},
		"com.acme:tools":                    {"2.0.0"},
		"com.fasterxml.jackson:jackson-bom": {"2.18.2"},
		"com.google.guava:guava":            {"33.4.0-jre", "32.1.3-jre"},
		"org.example:ranged":                {"[1.0,2.0)"},
		"org.postgresql:postgresql":         {"42.7.4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("declared versions\n got %v\nwant %v", got, want)
	}
	for name, content := range map[string]string{
		"not XML":       `<project>`,
		"not a project": `<settings><artifactId>a</artifactId></settings>`,
		"no artifactId": `<project><groupId>a</groupId></project>`,
	} {
		if _, err := ReadDeclaredVersions([]byte(content), nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAParentPropertyBumpIsAChangeOfTheParentPOM(t *testing.T) {
	spectest.Proves(t, feature, "maven-dependency-changes", "a-pom-lists-its-declared-versions")
	parent := func(guava string) string {
		return `<project><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version><packaging>pom</packaging>
  <modules><module>platform</module></modules>
  <properties><guava.version>` + guava + `</guava.version></properties>
  <profiles><profile><id>pg</id><properties><postgresql.version>42.7.4</postgresql.version></properties></profile></profiles>
</project>`
	}
	root := writeTree(t, map[string]string{
		"pom.xml": parent("33.3.1-jre"),
		"platform/pom.xml": `<project><parent><groupId>com.acme</groupId><artifactId>shop</artifactId><version>1.0.0</version></parent>
  <artifactId>platform</artifactId><packaging>pom</packaging>
  <modules><module>../pricing</module></modules>
  <properties><slf4j.version>2.0.16</slf4j.version></properties>
</project>`,
		"pricing/pom.xml": `<project><parent><groupId>com.acme</groupId><artifactId>platform</artifactId><version>1.0.0</version></parent>
  <artifactId>pricing</artifactId>
  <properties><own.version>1.2.3</own.version></properties>
  <dependencies>
    <dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>${guava.version}</version></dependency>
    <dependency><groupId>org.slf4j</groupId><artifactId>slf4j-api</artifactId><version>${slf4j.version}</version></dependency>
    <dependency><groupId>org.postgresql</groupId><artifactId>postgresql</artifactId><version>${postgresql.version}</version></dependency>
    <dependency><groupId>org.example</groupId><artifactId>own</artifactId><version>${own.version}</version></dependency>
    <dependency><groupId>org.example</groupId><artifactId>literal</artifactId><version>4.0.0</version></dependency>
    <dependency><groupId>${project.groupId}</groupId><artifactId>platform</artifactId><version>${project.version}</version></dependency>
  </dependencies>
</project>`,
	})
	d := Detect(root)
	if len(d.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", d.Diagnostics)
	}
	for file, want := range map[string]map[string][]string{
		// The bumped parent lists what its modules take from its properties.
		"pom.xml": {"com.google.guava:guava": {"33.4.0-jre"}, "org.postgresql:postgresql": {"42.7.4"}},
		// An intermediate parent lists what resolves through it and above.
		"platform/pom.xml": {"com.google.guava:guava": {"33.3.1-jre"}, "org.postgresql:postgresql": {"42.7.4"}, "org.slf4j:slf4j-api": {"2.0.16"}},
		// The module resolves its own references through its parents.
		"pricing/pom.xml": {
			"com.google.guava:guava": {"33.3.1-jre"}, "org.postgresql:postgresql": {"42.7.4"}, "org.slf4j:slf4j-api": {"2.0.16"},
			"org.example:own": {"1.2.3"}, "org.example:literal": {"4.0.0"},
		},
	} {
		data := readFile(t, root, file)
		if file == "pom.xml" {
			data = []byte(parent("33.4.0-jre"))
		}
		got, err := d.ReadDeclared(file, data)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", file, got, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "src/it/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src/it/app/pom.xml"), []byte("<project/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !d.IsDeclaredVersions("gone/pom.xml") || !d.IsDeclaredVersions("pricing/pom.xml") || d.IsDeclaredVersions("pricing/build.xml") || d.IsDeclaredVersions("src/it/app/pom.xml") {
		t.Error("IsDeclaredVersions")
	}
	if (Detection{}).IsDeclaredVersions("pom.xml") {
		t.Error("a repository without a Maven build reads its pom.xml")
	}
}

func readFile(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
