package java

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // the Gradle wrapper names its distribution directory by this digest
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/observe/detect/maven"
	"go.putnami.dev/tooling/stacks"
)

// WorkspaceInstallJobName is the job that prepares each stack root.
const WorkspaceInstallJobName = "workspace-install"

// Diagnostic codes of the install.
const (
	// CodeInstallFailed is the diagnostic of a stack root whose build tool
	// could not be installed or started.
	CodeInstallFailed = "java.install_failed"
	// CodeDependenciesIncomplete is the diagnostic of dependencies the
	// install could not put in the build tool's cache.
	CodeDependenciesIncomplete = "java.dependencies_incomplete"
)

// mavenGoOffline is the goal that fills Maven's local repository with the
// dependencies and plugins a build's POMs declare.
const mavenGoOffline = "org.apache.maven.plugins:maven-dependency-plugin:3.11.0:go-offline"

// gradleResolveTask is the task the resolve init script gives every project.
const gradleResolveTask = "putnamiResolveDependencies"

// unresolvedMarker starts a line the resolve init script prints for a
// configuration it could not resolve whole.
const unresolvedMarker = "PUTNAMI-UNRESOLVED "

// maxUnresolved bounds the warnings an install reports for one stack root.
const maxUnresolved = 20

// gradleResolveScript gives every project a task that resolves each of its
// resolvable configurations, leniently: a dependency that cannot be resolved
// is printed after unresolvedMarker and fails nothing. The configurations
// Gradle deprecates for resolution are left out.
const gradleResolveScript = `// Written by @putnami/java for putnami install.
allprojects { p ->
    def projectPath = p.path
    def configurations = p.configurations
    p.tasks.register('` + gradleResolveTask + `') {
        doLast {
            configurations.findAll { it.canBeResolved && !(it.name in ['archives', 'default']) }.each { c ->
                try {
                    def artifacts = c.incoming.artifactView { lenient = true }.artifacts
                    artifacts.artifactFiles.files
                    artifacts.failures.each { f ->
                        println "` + unresolvedMarker + `${projectPath} ${c.name}: ${String.valueOf(f.message).readLines().find() ?: f.class.name}"
                    }
                } catch (Exception e) {
                    println "` + unresolvedMarker + `${projectPath} ${c.name}: ${String.valueOf(e.message).readLines().find() ?: e.class.name}"
                }
            }
        }
    }
}
`

// WorkspaceInstallJob prepares each stack root of the workspace's Gradle and
// Maven builds: the JDK .java-version states, which the toolchain check
// installs, the JDKs the build's Gradle toolchains ask for, the build tool,
// and the dependencies of the build. The force flag refreshes the
// dependencies. Offline, it installs nothing and verifies what the machine
// holds.
func WorkspaceInstallJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		force := ctx.Params.Bool("force", false)
		install := func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			switch {
			case settingsFile(root.Dir, wsproto.ProbeRootPath) != "":
				return installGradleRoot(run, root, emit, force)
			case isFile(root.Dir, maven.POM):
				return installMavenRoot(run, root, emit, force)
			}
			emit.Info(root.Dir + " holds no Gradle settings and no pom.xml: nothing to install")
			return nil
		}
		return stack.WorkspaceRootJob(stacks.WarnOnMissingToolchain, install)(ctx, emit, args)
	}
}

// installGradleRoot installs the JDKs the Gradle build at the root asks for
// and its Gradle, the wrapper's distribution through gradlew --version or the
// pinned release, then resolves every resolvable configuration of every
// project. A configuration that cannot be resolved whole is a warning.
func installGradleRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	if offline() && hasWrapper(root.Dir) {
		if dist, installed := wrapperDistribution(root.Dir, root.Env); !installed {
			emit.DiagnosticWithCode("warning", fmt.Sprintf("%s is left out: its Gradle wrapper's distribution %s is not installed, and an offline install downloads nothing", root.Dir, dist), "", 0, 0, CodeInstallFailed)
			return nil
		}
	}
	base := []string{filepath.Join(root.Dir, "gradlew"), "--console=plain", "--version"}
	version, err := Step(ctx, WorkspaceInstallJobName, stacks.Metadata{}, root, base, emit.Info)
	if err != nil {
		return installFailed(emit, offline(), fmt.Errorf("%s: %w", root.Dir, err))
	}
	// The adaptation names the program and appends its arguments, such as
	// the homes of the toolchain JDKs.
	program, added := version[0], version[len(base):]
	emit.Info("$ " + strings.Join(version, " "))
	if err := runAtRoot(ctx, root, version, nil); err != nil {
		return installFailed(emit, false, err)
	}
	scripts, err := os.MkdirTemp("", "putnami-java-install-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scripts) }()
	script := filepath.Join(scripts, "putnami-resolve.gradle")
	if err := os.WriteFile(script, []byte(gradleResolveScript), 0o600); err != nil {
		return err
	}
	resolve := append([]string{program, "--console=plain", "--init-script", script, "-Dorg.gradle.configuration-cache=false", gradleResolveTask}, added...)
	if force {
		resolve = append(resolve, "--refresh-dependencies")
	}
	if offline() {
		resolve = append(resolve, "--offline")
	}
	emit.Info("$ " + strings.Join(resolve, " "))
	var unresolved []string
	err = runAtRoot(ctx, root, resolve, func(line string) {
		if rest, ok := strings.CutPrefix(line, unresolvedMarker); ok {
			unresolved = append(unresolved, rest)
		}
	})
	for i, u := range unresolved {
		if i == maxUnresolved {
			emit.DiagnosticWithCode("warning", fmt.Sprintf("%s: %d more configurations are not resolved whole", root.Dir, len(unresolved)-maxUnresolved), "", 0, 0, CodeDependenciesIncomplete)
			break
		}
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s: the configuration %s is not resolved whole; the first task that needs it resolves it again", root.Dir, u), "", 0, 0, CodeDependenciesIncomplete)
	}
	if err != nil {
		return installFailed(emit, false, err)
	}
	return nil
}

// installMavenRoot installs the Maven the build at the root runs, then runs
// dependency:go-offline, best effort: it downloads the dependencies and the
// plugins the POMs declare, not the artifacts a plugin resolves only when it
// runs, which the warning names.
func installMavenRoot(ctx context.Context, root stacks.RootStep, emit *jsonl.Emitter, force bool) error {
	mvn, err := mavenProgram(ctx, root.Dir, root.Env, emit.Info)
	if err != nil {
		return installFailed(emit, offline(), fmt.Errorf("%s: %w", root.Dir, err))
	}
	argv := []string{mvn, "--batch-mode", "--file", filepath.Join(root.Dir, maven.POM), mavenGoOffline}
	if force {
		argv = append(argv, "--update-snapshots")
	}
	if offline() {
		argv = append(argv, "--offline")
	}
	emit.Info("$ " + strings.Join(argv, " "))
	if err := runAtRoot(ctx, root, argv, nil); err != nil {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s: dependency:go-offline did not complete, so the first task downloads what it misses: %v", root.Dir, err), "", 0, 0, CodeDependenciesIncomplete)
		return nil
	}
	emit.DiagnosticWithCode("warning", fmt.Sprintf("%s: dependency:go-offline downloads the dependencies and plugins the POMs declare, not the artifacts a plugin resolves only when it runs, such as Surefire's test framework provider: the first task that runs that plugin downloads them", root.Dir), "", 0, 0, CodeDependenciesIncomplete)
	return nil
}

// installFailed reports a stack root the install could not prepare: offline,
// a warning, and the install moves on to the next root; otherwise an error
// that fails it.
func installFailed(emit *jsonl.Emitter, leaveOut bool, err error) error {
	if leaveOut {
		emit.DiagnosticWithCode("warning", err.Error()+": the stack root is left out", "", 0, 0, CodeInstallFailed)
		return nil
	}
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeInstallFailed)
	return fmt.Errorf("%s: %w", CodeInstallFailed, err)
}

// runAtRoot runs argv at the stack root with its environment. Its output goes
// to stderr, since the job's stdout carries its protocol; each line of its
// standard output also reaches line, when set.
func runAtRoot(ctx context.Context, root stacks.RootStep, argv []string, line func(string)) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the stack root's own build tool
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	cmd.Stderr = os.Stderr
	if line == nil {
		cmd.Stdout = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s in %s: %w", strings.Join(argv, " "), root.Dir, err)
		}
		return nil
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s in %s: %w", strings.Join(argv, " "), root.Dir, err)
	}
	sc := bufio.NewScanner(io.TeeReader(out, os.Stderr))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line(sc.Text())
	}
	_, _ = io.Copy(os.Stderr, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s in %s: %w", strings.Join(argv, " "), root.Dir, err)
	}
	return nil
}

// wrapperDistribution answers the distribution the Gradle wrapper of the
// build at root downloads, and whether it already did: the wrapper keeps it
// under <base>/<distributionPath>/<name>/<hash>, base being GRADLE_USER_HOME
// or the build's root, and marks it complete with <archive>.ok.
func wrapperDistribution(root string, env []string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "gradle", "wrapper", "gradle-wrapper.properties")) //nolint:gosec // the build's wrapper properties
	if err != nil {
		return "gradle/wrapper/gradle-wrapper.properties", false
	}
	props := readProperties(data)
	distributionURL := props["distributionUrl"]
	u, err := url.Parse(distributionURL)
	if err != nil || distributionURL == "" {
		return distributionURL, false
	}
	archive := path.Base(u.Path)
	name := strings.TrimSuffix(archive, path.Ext(archive))
	sum := md5.Sum([]byte(distributionURL)) //nolint:gosec // the wrapper's directory name
	hash := new(big.Int).SetBytes(sum[:]).Text(36)
	base := gradleUserHome(env)
	if props["distributionBase"] == "PROJECT" {
		base = root
	}
	distPath := props["distributionPath"]
	if distPath == "" {
		distPath = "wrapper/dists"
	}
	_, err = os.Stat(filepath.Join(base, filepath.FromSlash(distPath), name, hash, archive+".ok"))
	return archive, err == nil
}

// gradleUserHome is GRADLE_USER_HOME of env, else ~/.gradle.
func gradleUserHome(env []string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GRADLE_USER_HOME="); ok && value != "" {
			return value
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gradle")
}
