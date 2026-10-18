package verify

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// TestThePlanSubprocessReceivesNoHostChannel runs the default plan with the
// channel in the process environment: the workspace CLI it starts sees
// neither variable.
func TestThePlanSubprocessReceivesNoHostChannel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the default plan runs the putnami on PATH on Windows, not the fixture script")
	}
	t.Setenv(wire.SocketEnv, "/tmp/host.sock")
	t.Setenv(wire.TokenEnv, "launch-token")
	root := t.TempDir()
	script := "#!/bin/sh\nenv > environment\n" +
		`printf '%s\n' '{"protocolVersion":2,"status":"success","exitCode":0,"plan":{"dryRun":true,"tasks":[{"identity":{"key":"/application:test"}}]}}'` + "\n"
	if err := os.WriteFile(filepath.Join(root, "putnamiw"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	keys, err := workspacePlan(root, []string{"test"}, nil, strings.Repeat("a", 40))
	if err != nil || !slices.Equal(keys, []string{"/application:test"}) {
		t.Fatalf("workspacePlan = %q, %v", keys, err)
	}
	environment, err := os.ReadFile(filepath.Join(root, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(environment), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if name == wire.SocketEnv || name == wire.TokenEnv {
			t.Errorf("the workspace CLI received %s", name)
		}
	}
	if !strings.Contains(string(environment), "PATH=") {
		t.Errorf("the workspace CLI lost the rest of the environment:\n%s", environment)
	}
}
