//go:build !windows

package rust

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/stacks/stackstest"
)

func TestACrateWithABinaryTargetIsAnApplication(t *testing.T) {
	spectest.Proves(t, feature, "crate-kind-is-detected", "a-crate-with-a-binary-target-is-an-application")
	root := fixture(t)
	stackstest.WriteFile(t, root, "crates/tool/Cargo.toml", "[package]\nname = \"tool\"\nversion = \"0.1.0\"\n\n[[bin]]\nname = \"tool\"\npath = \"cli.rs\"\n")
	stackstest.WriteFile(t, root, "crates/multi/Cargo.toml", "[package]\nname = \"multi\"\nversion = \"0.1.0\"\n")
	stackstest.WriteFile(t, root, "crates/multi/src/bin/one.rs", "fn main() {}\n")
	stackstest.WriteFile(t, root, "crates/off/Cargo.toml", "[package]\nname = \"off\"\nversion = \"0.1.0\"\nautobins = false\n")
	stackstest.WriteFile(t, root, "crates/off/src/main.rs", "fn main() {}\n")
	types := map[string]string{}
	for _, p := range probe(t, root).Projects {
		types[p.Path] = p.Type
	}
	for path, want := range map[string]string{
		"tools/gen": "application", "crates/api": "library", "crates/pricing": "library",
		"crates/tool": "application", "crates/multi": "application", "crates/off": "library",
	} {
		if got := types[path]; got != want {
			t.Errorf("%s is %q, want %q", path, got, want)
		}
	}
}

func rawParams(t *testing.T, values map[string]any) pctx.Params {
	t.Helper()
	params := pctx.Params{}
	for key, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		params[key] = data
	}
	return params
}

func TestRunForwardsTheExitCodeOfTheProgram(t *testing.T) {
	spectest.Proves(t, feature, "run-forwards-the-exit-code", "run-forwards-the-exit-code-of-the-program")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	t.Setenv("STUB_RUN_EXIT", "7")
	status, data, _, _, err := runJobParams(t, root, "tools/gen", RunJobName, rawParams(t, map[string]any{"args": "--flag value", "port": 4000}))
	if err != nil || status != "FAILED" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	if data["exit-code"] != 7 {
		t.Errorf("exit-code = %v, want 7", data["exit-code"])
	}
	logged, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cargo run --package gen --locked -- --flag value", "PORT=4000"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the log misses %q:\n%s", want, logged)
		}
	}

	// --entrypoint names one binary of the crate.
	t.Setenv("STUB_RUN_EXIT", "0")
	if status, _, _, _, err := runJobParams(t, root, "tools/gen", RunJobName, rawParams(t, map[string]any{"entrypoint": "gen-admin"})); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	if logged, _ := os.ReadFile(log); !strings.Contains(string(logged), "cargo run --package gen --locked --bin gen-admin --") { //nolint:gosec // the test's own log
		t.Errorf("the log misses the binary:\n%s", logged)
	}
}

func TestServeRunsTheCrateWithPortSet(t *testing.T) {
	spectest.Proves(t, feature, "serve-sets-the-port", "serve-runs-the-crate-with-port-set")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	for _, params := range []pctx.Params{nil, rawParams(t, map[string]any{"port": 8081, "args": "--verbose"})} {
		if status, _, _, _, err := runJobParams(t, root, "tools/gen", ServeJobName, params); status != "OK" || err != nil {
			t.Fatalf("serve: %s %v", status, err)
		}
	}
	logged, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cargo run --package gen --locked --\n", "PORT=3000", "cargo run --package gen --locked -- --verbose", "PORT=8081"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the log misses %q:\n%s", want, logged)
		}
	}
}

func TestServeOnPortZeroPicksAFreePort(t *testing.T) {
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	if status, _, _, _, err := runJobParams(t, root, "tools/gen", ServeJobName, rawParams(t, map[string]any{"port": 0})); status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	logged, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`PORT=(\d+)`).FindStringSubmatch(string(logged))
	if match == nil {
		t.Fatalf("no PORT in the log:\n%s", logged)
	}
	if port, _ := strconv.Atoi(match[1]); port == 0 {
		t.Errorf("PORT = %s", match[1])
	}
}

func TestServeFailsWithTheExitCodeOfAProgramThatStops(t *testing.T) {
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	t.Setenv("STUB_RUN_EXIT", "101")
	status, data, _, _, err := runJobParams(t, root, "tools/gen", ServeJobName, nil)
	if status != "FAILED" || err != nil || data["exit-code"] != 101 {
		t.Errorf("serve: %s %v %v", status, data, err)
	}
}
