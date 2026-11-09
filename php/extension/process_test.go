package php

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// runProjectJob runs a job for the component at rel with the job arguments.
func runProjectJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params, args ...string) (string, map[string]any, error) {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
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

func TestServeRunsThePhpBuiltInServerOnAnApplication(t *testing.T) {
	spectest.Proves(t, feature, "serves-an-application", "serve-runs-the-php-built-in-server-on-an-application")
	log, _ := prepareComposer(t)
	root := fixture(t)
	writeFile(t, root, frontController, "<?php\n")

	status, _, err := runProjectJob(t, root, ".", ServeJob(stack(t)), nil)
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	status, _, err = runProjectJob(t, root, ".", ServeJob(stack(t)), job.Params{"port": json.RawMessage(`8081`)})
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 2 || !strings.Contains(lines[0], "-S 0.0.0.0:3000 -t public ") || !strings.Contains(lines[1], "-S 0.0.0.0:8081 -t public ") || !strings.HasSuffix(lines[0], "router.php") {
		t.Errorf("php ran with %q", lines)
	}
}

func TestServeNeedsAFrontController(t *testing.T) {
	prepareComposer(t)
	root := fixture(t)
	status, _, err := runProjectJob(t, root, ".", ServeJob(stack(t)), nil)
	if status != "FAILED" || err == nil || err.Error() != CodeNoFrontController {
		t.Errorf("serve: %s %v", status, err)
	}
}

func TestServeFailsWhenTheServerExitsOnItsOwn(t *testing.T) {
	prepareComposer(t)
	root := fixture(t)
	writeFile(t, root, frontController, "<?php\n")
	t.Setenv("STUB_EXIT", "5")
	status, data, err := runProjectJob(t, root, ".", ServeJob(stack(t)), nil)
	if status != "FAILED" || err != nil || data[exitCodeKey] != 5 {
		t.Errorf("serve: %s %v %v", status, data, err)
	}
}

func TestRunForwardsTheExitCodeOfTheEntrypoint(t *testing.T) {
	spectest.Proves(t, feature, "runs-an-application-once", "run-forwards-the-exit-code-of-the-entrypoint")
	log, _ := prepareComposer(t)
	root := fixture(t)
	t.Setenv("STUB_EXIT", "7")

	status, data, err := runProjectJob(t, root, ".", RunJob(stack(t)), job.Params{"args": json.RawMessage(`"cache:clear --env=prod"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 7 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	if lines := readLines(t, log); len(lines) != 1 || !strings.HasSuffix(lines[0], ": bin/console cache:clear --env=prod") {
		t.Errorf("php ran with %q", lines)
	}
	t.Setenv("STUB_EXIT", "")
	status, data, err = runProjectJob(t, root, ".", RunJob(stack(t)), job.Params{"entrypoint": json.RawMessage(`"bin/job.php"`)})
	if status != "OK" || err != nil || data[exitCodeKey] != 0 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	if lines := readLines(t, log); !strings.HasSuffix(lines[1], ": bin/job.php") {
		t.Errorf("php ran with %q", lines)
	}
}

func TestDepsUpgradeRunsComposerUpdate(t *testing.T) {
	spectest.Proves(t, feature, "upgrades-the-dependencies", "deps-upgrade-runs-composer-update")
	log, _ := prepareComposer(t)
	root := fixture(t)
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), phpProject(root, ".", `{}`), phpProject(root, "packages/pricing", `{}`)); status != "OK" || err != nil {
		t.Fatalf("upgrade: %s %v", status, err)
	}
	if lines := readLines(t, log); len(lines) != 1 || !strings.HasSuffix(lines[0], "composer.phar update --no-interaction --no-progress") {
		t.Errorf("composer ran with %q", lines)
	}
}

func TestDepsUpgradeFailsWithoutPHP(t *testing.T) {
	root := fixture(t)
	t.Setenv("PATH", t.TempDir())
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), phpProject(root, ".", `{}`)); status != "FAILED" || err == nil {
		t.Errorf("upgrade: %s %v", status, err)
	}
}

func TestAFailedUpgradeNamesTheComposerFailure(t *testing.T) {
	prepareComposer(t)
	root := fixture(t)
	t.Setenv("STUB_COMPOSER_FAILS", "1")
	status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), phpProject(root, ".", `{}`))
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeComposerUpdateFailed) {
		t.Errorf("upgrade: %s %v", status, err)
	}
}

func libraryContext(root string) *pctx.Context {
	return &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "lib", Path: "lib", FullPath: filepath.Join(root, "lib"), Type: "library"}}
}

// runPublish dry-runs the shared publisher: a real push is the registry's.
func runPublish(t *testing.T, root string) (string, map[string]any, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop")}}
	return PublishDockerJob()(ctx, jsonl.New(), []string{"--docker-registry", "registry.test/acme", "--dry-run"})
}

func TestServeAnnouncesAServerThatAcceptsConnections(t *testing.T) {
	port, err := stacks.FreePort()
	if err != nil || port == 0 {
		t.Fatalf("FreePort: %d %v", port, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = stdout }()
	stacks.AnnounceReady(context.Background(), jsonl.New(), port)
	write.Close()
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"port":`+strconv.Itoa(port)) || !strings.Contains(string(output), "ready") {
		t.Errorf("output = %s", output)
	}
}

func TestTheRouterSendsAPathWithADotToTheFrontController(t *testing.T) {
	for _, want := range []string{"is_file($root . $path)", "return false;", "require $root . '/index.php';"} {
		if !strings.Contains(routerScript, want) {
			t.Errorf("the router has no %q", want)
		}
	}
}
