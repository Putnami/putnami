package java

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// appFixture writes a nested Gradle build whose app applies the application
// plugin and whose boot project applies Spring Boot, and a nested Maven build
// whose app is a Spring Boot module, with stub build tools that log what they
// run and write what each build writes.
func appFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bin := t.TempDir()
	log := filepath.Join(bin, "run.log")
	writeFile(t, root, "g/settings.gradle.kts", "include(\":app\", \":boot\", \":lib\")\n")
	writeFile(t, root, "g/app/build.gradle.kts", "plugins {\n    application\n}\n")
	writeFile(t, root, "g/app/src/main/java/App.java", "class App {}\n")
	writeFile(t, root, "g/boot/build.gradle.kts", "plugins {\n    id(\"org.springframework.boot\") version \"3.5.16\"\n    java\n}\n")
	writeFile(t, root, "g/lib/build.gradle.kts", "plugins {\n    `java-library`\n}\n")
	writeFile(t, root, "g/gradlew", `#!/bin/sh
echo "$PWD: gradlew $*" >> "`+log+`"
for a in "$@"; do
  case "$a" in
    *:installDist)
      mkdir -p build/install/app/bin build/install/app/lib
      printf '#!/bin/sh\necho "$PWD: app $* PORT=$PORT SERVER_PORT=$SERVER_PORT" >> "`+log+`"\nexit ${STUB_EXIT:-0}\n' > build/install/app/bin/app
      echo bat > build/install/app/bin/app.bat
      chmod +x build/install/app/bin/app
      echo jar > build/install/app/lib/app.jar;;
    *:bootJar)
      mkdir -p build/libs; echo boot > build/libs/boot.jar; echo plain > build/libs/boot-plain.jar;;
  esac
done
`)
	writeFile(t, root, "m/pom.xml", `<project><groupId>com.acme</groupId><artifactId>shop</artifactId><packaging>pom</packaging><modules><module>app</module></modules></project>`)
	writeFile(t, root, "m/app/pom.xml", `<project><parent><groupId>com.acme</groupId></parent><artifactId>app</artifactId><build><plugins><plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build></project>`)
	writeFile(t, bin, "javac", "#!/bin/sh\necho 'javac 21.0.13'\n")
	writeFile(t, bin, "java", "#!/bin/sh\necho \"$PWD: java $* PORT=$PORT\" >> \""+log+"\"\nexit ${STUB_EXIT:-0}\n")
	writeFile(t, bin, "mvn", "#!/bin/sh\necho \"$PWD: mvn $*\" >> \""+log+"\"\nmkdir -p target && echo boot > target/app-1.0.jar && echo src > target/app-1.0-sources.jar\n")
	for _, f := range []string{filepath.Join(root, "g/gradlew"), filepath.Join(bin, "javac"), filepath.Join(bin, "java"), filepath.Join(bin, "mvn")} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return root, log
}

// runProjectJob runs a job for the component at rel with the job parameters.
func runProjectJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params, args ...string) (string, map[string]any, error) {
	t.Helper()
	p := probe(t, root, rel, "g", "m")[rel]
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel), Type: p.Type,
		Metadata: map[string]json.RawMessage{ExtensionName: p.Metadata},
	}}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	return jobFunc(ctx, jsonl.New(), args)
}

func TestServeRunsTheApplicationItsBuildProduces(t *testing.T) {
	spectest.Proves(t, feature, "serve-runs-the-application", "serve-runs-the-application-until-stopped-and-reports-it-ready")
	root, log := appFixture(t)
	s := stack(t)
	for _, c := range []struct {
		rel  string
		port string
		want []string
	}{
		{"g/app", "8081", []string{
			filepath.Join(root, "g/app") + ": gradlew --console=plain :app:installDist",
			filepath.Join(root, "g/app") + ": app  PORT=8081 SERVER_PORT=8081",
		}},
		{"g/boot", "3000", []string{
			filepath.Join(root, "g/boot") + ": gradlew --console=plain :boot:bootJar",
			filepath.Join(root, "g/boot") + ": java -jar " + filepath.Join(root, "g/boot/build/libs/boot.jar") + " PORT=3000",
		}},
		{"m/app", "3000", []string{
			filepath.Join(root, "m/app") + ": mvn --batch-mode --file " + filepath.Join(root, "m/pom.xml") + " --projects com.acme:app --also-make package -DskipTests",
			filepath.Join(root, "m/app") + ": java -jar " + filepath.Join(root, "m/app/target/app-1.0.jar") + " PORT=3000",
		}},
	} {
		var params job.Params
		if c.port != "3000" {
			params = job.Params{"port": json.RawMessage(c.port)}
		}
		if status, _, err := runProjectJob(t, root, c.rel, ServeJob(s), params); status != "OK" || err != nil {
			t.Fatalf("%s serve: %s %v\n%s", c.rel, status, err, ran(t, log))
		}
		for _, want := range c.want {
			if !strings.Contains(ran(t, log), want+"\n") {
				t.Errorf("%s ran\n%s\nwant %q", c.rel, ran(t, log), want)
			}
		}
	}

	t.Setenv("STUB_EXIT", "5")
	if status, data, err := runProjectJob(t, root, "g/app", ServeJob(s), nil); status != "FAILED" || err != nil || data[exitCodeKey] != 5 {
		t.Errorf("a server that exits on its own fails: %s %v %v", status, data, err)
	}
	if status, _, err := runProjectJob(t, root, "g/lib", ServeJob(s), nil); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeNotRunnable) {
		t.Errorf("a library does not serve: %s %v", status, err)
	}
}

func TestRunForwardsTheExitCode(t *testing.T) {
	spectest.Proves(t, feature, "run-runs-the-application-once", "run-forwards-the-exit-code")
	root, log := appFixture(t)
	t.Setenv("STUB_EXIT", "7")
	status, data, err := runProjectJob(t, root, "g/app", RunJob(stack(t)), job.Params{"args": json.RawMessage(`"import --dry-run"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 7 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	if !strings.Contains(ran(t, log), ": app import --dry-run PORT=") {
		t.Errorf("ran %s", ran(t, log))
	}
	t.Setenv("STUB_EXIT", "")
	if status, data, err := runProjectJob(t, root, "m/app", RunJob(stack(t)), nil); status != "OK" || err != nil || data[exitCodeKey] != 0 {
		t.Errorf("run: %s %v %v", status, data, err)
	}
}

func TestPackageAssemblesTheImageOfAnApplication(t *testing.T) {
	spectest.Proves(t, feature, "package-assembles-an-image", "package-assembles-the-image-of-an-application")
	root, _ := appFixture(t)
	s := stack(t)
	if status, _, err := runProjectJob(t, root, "g/lib", PackageDockerJob(s), nil); status != "SKIP" || err != nil {
		t.Errorf("a library packages nothing: %s %v", status, err)
	}
	if status, _, err := runProjectJob(t, root, "g/app", PackageDockerJob(s), nil, "--dry-run"); status != "OK" || err != nil {
		t.Fatalf("dry run: %s %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/g/app/package/docker")); err != nil {
		t.Errorf("a dry run writes the channel record: %v", err)
	}
	if status, _, err := runProjectJob(t, root, "g/app", PackageDockerJob(s), nil, "--docker-base-image", "eclipse-temurin:21-jre"); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeBaseNotPinned) {
		t.Errorf("a base with no digest fails: %s %v", status, err)
	}

	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Path: "g/app", FullPath: filepath.Join(root, "g/app"), Metadata: map[string]json.RawMessage{ExtensionName: probe(t, root, "g", "g/app")["g/app"].Metadata}}}
	rootStep, app, err := application(ctx, s, jsonl.New())
	if err != nil {
		t.Fatal(err)
	}
	base, err := baseImage(t.Context(), rootStep, "")
	if err != nil || base != jreBases["21"] {
		t.Fatalf("the base follows the selected Java major: %s %v", base, err)
	}
	spec, err := imageSpec(app, base, defaultPlatform, defaultImagePort)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range spec.Layers[0].Files {
		paths = append(paths, f.Path)
		if f.Path == "/app/bin/app" && f.Mode != 0o755 {
			t.Errorf("the launcher stays executable: %o", f.Mode)
		}
	}
	if strings.Join(paths, ",") != "/app/bin/app,/app/bin/app.bat,/app/lib/app.jar" || strings.Join(spec.Entrypoint, " ") != "/app/bin/app" || spec.User != imageUser || strings.Join(spec.Env, ",") != "PORT=8080,SERVER_PORT=8080" {
		t.Errorf("installDist image = %+v %v", spec, paths)
	}
	boot, err := imageSpec(runnable{kind: bootJar, path: "/x/boot.jar", command: []string{"java", "-jar", "/x/boot.jar"}}, base, defaultPlatform, defaultImagePort)
	if err != nil || strings.Join(boot.Entrypoint, " ") != imageJava+" -jar /app/app.jar" || boot.Layers[0].Files[0].Path != "/app/app.jar" {
		t.Errorf("boot jar image = %+v %v", boot, err)
	}
	for major, ref := range jreBases {
		if !strings.Contains(ref, "eclipse-temurin:"+major+".") || !strings.Contains(ref, "-jre@sha256:") {
			t.Errorf("Java %s base %s", major, ref)
		}
	}
}

func TestAnApplicationThatPublishesAnImageIsAReleaseMember(t *testing.T) {
	spectest.Proves(t, feature, "publish-pushes-the-image", "an-application-that-publishes-an-image-is-a-release-member")
	root, _ := appFixture(t)
	writeFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "ghcr.io/acme"}}}`)
	writeFile(t, root, "g/app/putnami.json", `{"name": "shop-app", "publish": ["docker"]}`)
	writeFile(t, root, "g/lib/putnami.json", `{"publish": ["docker"]}`)
	got := probe(t, root, "g", "g/app", "g/lib")
	var meta struct {
		ReleaseSet struct {
			Ecosystems []struct {
				Ecosystem, Coordinate string
			} `json:"ecosystems"`
		} `json:"releaseSet"`
	}
	if err := json.Unmarshal(got["g/app"].Metadata, &meta); err != nil || len(meta.ReleaseSet.Ecosystems) != 1 || meta.ReleaseSet.Ecosystems[0].Coordinate != "acme/shop-app" || meta.ReleaseSet.Ecosystems[0].Ecosystem != "oci" {
		t.Errorf("app release set = %s", got["g/app"].Metadata)
	}
	if strings.Contains(string(got["g/lib"].Metadata), "releaseSet") {
		t.Errorf("a library publishes no image: %s", got["g/lib"].Metadata)
	}
}
