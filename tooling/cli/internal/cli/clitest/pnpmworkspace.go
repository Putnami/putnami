package clitest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// NodeImportE2EEnv opts into the tests that install a Node repository's own
// tools with the real pnpm, from the npm registry. Each takes about two
// minutes and needs the network, so the regular gate skips them: set it to 1
// to run them.
const NodeImportE2EEnv = "PUTNAMI_NODE_IMPORT_E2E"

// RequirePnpm skips the test unless NodeImportE2EEnv is set, and where node
// or pnpm 10 or later is not on PATH. It returns the pnpm version.
func RequirePnpm(t *testing.T) string {
	t.Helper()
	if os.Getenv(NodeImportE2EEnv) == "" {
		t.Skipf("set %s=1 to install the repository's tools with the real pnpm, from the npm registry", NodeImportE2EEnv)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("the repository's tools run on node, which is not on PATH: %v", err)
	}
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skipf("the repository installs with pnpm, which is not on PATH: %v", err)
	}
	cmd := exec.Command(pnpm, "--version")
	cmd.Dir = t.TempDir()
	data, err := cmd.Output()
	if err != nil {
		t.Skipf("pnpm --version: %v", err)
	}
	version := strings.TrimSpace(string(data))
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 10 {
		t.Skipf("the repository pins pnpm 10, and PATH has pnpm %s", version)
	}
	return version
}

// WritePnpmWorkspace writes files under root, the root package.json among
// them, and adds to that package.json the packageManager pnpm@pnpmVersion
// and devDependencies.
func WritePnpmWorkspace(t *testing.T, root string, files map[string]string, pnpmVersion string, devDependencies map[string]string) {
	t.Helper()
	for rel, content := range files {
		WriteFile(t, filepath.Join(root, rel), content)
	}
	var pkg map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		t.Fatalf("read the root package.json: %v", err)
	}
	deps := map[string]string{}
	if raw, ok := pkg["devDependencies"]; ok {
		if err := json.Unmarshal(raw, &deps); err != nil {
			t.Fatal(err)
		}
	}
	for name, version := range devDependencies {
		deps[name] = version
	}
	pkg["packageManager"], _ = json.Marshal("pnpm@" + pnpmVersion)
	pkg["devDependencies"], _ = json.Marshal(deps)
	data, _ = json.MarshalIndent(pkg, "", "  ")
	WriteFile(t, filepath.Join(root, "package.json"), string(data)+"\n")
}

// PnpmInstall installs the workspace at root with the real pnpm, the way
// the team installed it before it adopted Putnami: it writes pnpm-lock.yaml
// and node_modules. A CI host makes pnpm install frozen by default; this
// install writes the first lock, so it resolves.
func PnpmInstall(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("pnpm", "install", "--no-frozen-lockfile")
	cmd.Dir = root
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pnpm install: %v\n%s", err, data)
	}
	if _, err := os.Stat(filepath.Join(root, "pnpm-lock.yaml")); err != nil {
		t.Fatalf("pnpm install wrote no pnpm-lock.yaml: %v", err)
	}
}

// CheckPnpmKept fails when the install dropped pnpm-lock.yaml or wrote a
// bun.lock: the workspace keeps installing with pnpm.
func CheckPnpmKept(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "pnpm-lock.yaml")); err != nil {
		t.Errorf("the install dropped pnpm-lock.yaml: %v", err)
	}
	for _, lock := range []string{"bun.lock", "bun.lockb"} {
		if _, err := os.Stat(filepath.Join(root, lock)); !os.IsNotExist(err) {
			t.Errorf("the install wrote %s: %v", lock, err)
		}
	}
}

// CheckNodeToolsRan fails unless, in the workspace WritePnpmWorkspace wrote
// from pnpm-node-tools.txtar, Vitest tested core, Jest tested utils, and tsc
// wrote both packages' dist/index.js.
func CheckNodeToolsRan(t *testing.T, root string) {
	t.Helper()
	for pkg, suites := range map[string]string{"core": `<testsuites name="vitest tests"`, "utils": `<testsuites name="jest"`} {
		report := filepath.Join(root, ".putnami/out/packages", pkg, "~test/results.junit.xml")
		data, err := os.ReadFile(report)
		if err != nil {
			t.Errorf("%s wrote no JUnit report: %v", pkg, err)
			continue
		}
		if !strings.Contains(string(data), suites) || !strings.Contains(string(data), `tests="1" failures="0"`) {
			t.Errorf("%s's JUnit report does not come from its runner (%s):\n%s", pkg, suites, data)
		}
		if _, err := os.Stat(filepath.Join(root, "packages", pkg, "dist/index.js")); err != nil {
			t.Errorf("tsc wrote no packages/%s/dist/index.js: %v", pkg, err)
		}
	}
}
