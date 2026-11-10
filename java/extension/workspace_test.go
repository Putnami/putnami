package java

import (
	"bufio"
	"crypto/md5" //nolint:gosec // the Gradle wrapper's directory name
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// installWorkspace is a workspace with a Gradle build in shop, whose api
// project asks for a Java 17 toolchain, a Maven build in billing, a Gradle
// build in samples that the e2e tag disables, and a project of another
// extension. Its stub javac prints 21, its gradlew and mvn log their
// arguments to the returned log, and the resolve task of gradlew prints one
// configuration it cannot resolve.
func installWorkspace(t *testing.T) (string, string, []job.ProjectRef) {
	t.Helper()
	root := t.TempDir()
	bin := t.TempDir()
	log := filepath.Join(bin, "install.log")
	stackstest.WriteFile(t, root, "putnami.workspace.json", `{"disable": {"tags": ["e2e"]}}`)
	stackstest.WriteFile(t, root, ".java-version", "21\n")
	gradlew := "#!/bin/sh\necho \"$PWD: gradlew $*\" >> \"" + log + "\"\ncase \"$*\" in *putnamiResolveDependencies*) echo 'PUTNAMI-UNRESOLVED :api compileClasspath: Could not find com.acme:gone:1.0.';; esac\n"
	for _, dir := range []string{"shop", "samples/gradle"} {
		stackstest.WriteFile(t, root, dir+"/settings.gradle.kts", "include(\":api\")\n")
		stackstest.WriteFile(t, root, dir+"/api/build.gradle.kts", "plugins { `java-library` }\njava { toolchain { languageVersion = JavaLanguageVersion.of(17) } }\n")
		stackstest.WriteFile(t, root, dir+"/api/src/main/java/A.java", "class A {}\n")
		stackstest.WriteFile(t, root, dir+"/gradlew", gradlew)
		if err := os.Chmod(filepath.Join(root, dir, "gradlew"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stackstest.WriteFile(t, root, "billing/pom.xml", `<project><groupId>com.acme</groupId><artifactId>billing</artifactId><packaging>pom</packaging><modules><module>invoices</module></modules></project>`)
	stackstest.WriteFile(t, root, "billing/invoices/pom.xml", `<project><parent><groupId>com.acme</groupId></parent><artifactId>invoices</artifactId></project>`)
	stackstest.WriteFile(t, root, "other/build.gradle.kts", "\n")
	stackstest.WriteFile(t, bin, "javac", "#!/bin/sh\necho 'javac 21.0.4'\n")
	stackstest.WriteFile(t, bin, "mvn", "#!/bin/sh\necho \"$PWD: mvn $*\" >> \""+log+"\"\n")
	for _, f := range []string{"javac", "mvn"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("GRADLE_USER_HOME", t.TempDir())
	ref := func(path, config string, extensions ...string) job.ProjectRef {
		if extensions == nil {
			extensions = []string{ExtensionName}
		}
		return job.ProjectRef{Name: path, Path: path, FullPath: filepath.Join(root, path), Extensions: extensions, Config: []byte(config)}
	}
	refs := []job.ProjectRef{
		ref("shop", `{}`), ref("shop/api", `{}`),
		ref("billing", `{}`), ref("billing/invoices", `{}`),
		ref("samples/gradle", `{"tags": ["e2e"]}`), ref("samples/gradle/api", `{"tags": ["e2e"]}`),
		ref("other", `{}`, "@putnami/kotlin"),
	}
	return root, log, refs
}

// runInstall runs workspace-install with the force flag and answers its
// status and the diagnostics it emitted, as "severity code: message".
func runInstall(t *testing.T, root string, force bool, refs []job.ProjectRef) (string, []string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "emit")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	params := job.Params{}
	if force {
		params["force"] = json.RawMessage("true")
	}
	jobFunc := WorkspaceInstallJob(stack(t))
	status, _, _ := jobFunc(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}, jsonl.New(), nil)
	os.Stdout = stdout
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var diags []string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var event struct {
			Type, Severity, Code, Message string
		}
		if json.Unmarshal(sc.Bytes(), &event) == nil && event.Type == "diagnostic" {
			diags = append(diags, event.Severity+" "+event.Code+": "+event.Message)
		}
	}
	_ = out.Close()
	return status, diags
}

func TestWorkspaceInstallPreparesEachStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-prepares-each-build", "workspace-install-installs-the-jdks-the-build-tool-and-the-dependencies")
	root, log, refs := installWorkspace(t)
	serveJDKs(t)
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	status, diags := runInstall(t, root, false, refs)
	if status != "OK" {
		t.Fatalf("install: %s %v", status, diags)
	}
	jdk17 := "-Porg.gradle.java.installations.paths=" + filepath.Join(home, "toolchains/java/java-17.0.21/jdk-17")
	lines := strings.Split(strings.TrimSpace(ran(t, log)), "\n")
	if len(lines) != 3 {
		t.Fatalf("one Gradle version, one Gradle resolution and one go-offline: %q", lines)
	}
	shop, billing := filepath.Join(root, "shop"), filepath.Join(root, "billing")
	// The stack roots run in path order.
	if lines[0] != billing+": mvn --batch-mode --file "+filepath.Join(billing, "pom.xml")+" "+mavenGoOffline {
		t.Errorf("Maven runs go-offline at the root: %q", lines[0])
	}
	if lines[1] != shop+": gradlew --console=plain --version "+jdk17 {
		t.Errorf("the wrapper downloads its distribution, with the toolchain JDKs: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], shop+": gradlew --console=plain --init-script ") || !strings.HasSuffix(lines[2], " -Dorg.gradle.configuration-cache=false putnamiResolveDependencies "+jdk17) {
		t.Errorf("the init script resolves every configuration: %q", lines[2])
	}
	joined := strings.Join(diags, "\n")
	if !strings.Contains(joined, "warning "+CodeDependenciesIncomplete+": "+shop+": the configuration :api compileClasspath: Could not find com.acme:gone:1.0.") {
		t.Errorf("an unresolved configuration is a warning: %v", diags)
	}
	if !strings.Contains(joined, "warning "+CodeDependenciesIncomplete+": "+billing+": dependency:go-offline downloads the dependencies and plugins the POMs declare, not the artifacts a plugin resolves only when it runs") {
		t.Errorf("the go-offline warning names what it misses: %v", diags)
	}
	if strings.Contains(ran(t, log), "samples") {
		t.Errorf("a stack root disable.tags excludes is left out: %s", ran(t, log))
	}

	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if status, diags := runInstall(t, root, true, refs); status != "OK" {
		t.Fatalf("forced install: %s %v", status, diags)
	}
	got := ran(t, log)
	if !strings.Contains(got, "putnamiResolveDependencies "+jdk17+" --refresh-dependencies\n") || !strings.Contains(got, mavenGoOffline+" --update-snapshots\n") {
		t.Errorf("force refreshes the dependencies: %s", got)
	}
}

func TestWorkspaceInstallReportsWhatItCannotPrepare(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-prepares-each-build", "workspace-install-is-best-effort-for-maven-and-fails-on-a-gradle-build-that-does-not-resolve")
	root, log, refs := installWorkspace(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	stackstest.WriteFile(t, root, "shop/api/build.gradle.kts", "plugins { `java-library` }\n")
	mvn := filepath.Join(filepath.Dir(log), "mvn")
	stackstest.WriteFile(t, filepath.Dir(log), "mvn", "#!/bin/sh\necho \"$PWD: mvn $*\" >> \""+log+"\"\nexit 1\n")
	if err := os.Chmod(mvn, 0o755); err != nil {
		t.Fatal(err)
	}
	status, diags := runInstall(t, root, false, refs)
	if status != "OK" || !strings.Contains(strings.Join(diags, "\n"), "warning "+CodeDependenciesIncomplete+": "+filepath.Join(root, "billing")+": dependency:go-offline did not complete") {
		t.Errorf("a go-offline that fails is a warning: %s %v", status, diags)
	}
	stackstest.WriteFile(t, root, "shop/gradlew", "#!/bin/sh\ncase \"$*\" in *putnamiResolveDependencies*) exit 1;; esac\n")
	if status, diags := runInstall(t, root, false, refs); status == "OK" || !strings.Contains(strings.Join(diags, "\n"), "error "+CodeInstallFailed+": ") {
		t.Errorf("a Gradle build that does not resolve fails the install: %s %v", status, diags)
	}
}

func TestAnOfflineWorkspaceInstallInstallsNothing(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-offline-installs-nothing", "an-offline-workspace-install-verifies-and-installs-nothing")
	root, log, refs := installWorkspace(t)
	downloads := serveJDKs(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	stackstest.WriteFile(t, root, "shop/gradle/wrapper/gradle-wrapper.properties", "distributionUrl=https\\://services.gradle.org/distributions/gradle-8.14.3-bin.zip\n")

	status, diags := runInstall(t, root, false, refs)
	joined := strings.Join(diags, "\n")
	if status != "OK" || !strings.Contains(joined, "warning "+CodeInstallFailed+": "+filepath.Join(root, "shop")+" is left out: its Gradle wrapper's distribution gradle-8.14.3-bin.zip is not installed") {
		t.Errorf("a Gradle root whose distribution is missing is left out: %s %v", status, diags)
	}
	if got := ran(t, log); strings.Contains(got, "gradlew") || !strings.Contains(got, mavenGoOffline+" --offline\n") {
		t.Errorf("Maven verifies offline, and Gradle does not run: %s", got)
	}

	// The wrapper's distribution, once in GRADLE_USER_HOME, is verified offline.
	url := "https://services.gradle.org/distributions/gradle-8.14.3-bin.zip"
	sum := md5.Sum([]byte(url)) //nolint:gosec // the wrapper's directory name
	stackstest.WriteFile(t, os.Getenv("GRADLE_USER_HOME"), "wrapper/dists/gradle-8.14.3-bin/"+new(big.Int).SetBytes(sum[:]).Text(36)+"/gradle-8.14.3-bin.zip.ok", "")
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	status, diags = runInstall(t, root, false, refs)
	got := ran(t, log)
	if status != "OK" || !strings.Contains(got, "putnamiResolveDependencies --offline\n") || strings.Contains(got, gradleInstallationsProperty) || len(downloads) != 0 {
		t.Errorf("Gradle resolves offline, without the JDK it would install: %s %v %v", got, diags, downloads)
	}

	// A Maven root with no Maven to run is left out.
	if err := os.Rename(filepath.Join(filepath.Dir(log), "mvn"), filepath.Join(filepath.Dir(log), "mvn-gone")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(log))
	status, diags = runInstall(t, root, false, refs)
	if status != "OK" || !strings.Contains(strings.Join(diags, "\n"), "warning "+CodeInstallFailed+": "+filepath.Join(root, "billing")+": the Apache Maven "+mavenVersion+" is not installed: an offline run installs nothing: the stack root is left out") {
		t.Errorf("a Maven root with no Maven is left out offline: %s %v", status, diags)
	}
}
