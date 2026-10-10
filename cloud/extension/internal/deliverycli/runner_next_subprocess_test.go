package deliverycli

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This proof is explicitly enabled after a Putnami build of the extension
// binary (cmd/putnami-cloud). A library test cannot depend on that binary
// without a project cycle. Keep the binary input local to tests and run
// uncached, with no nested compiler or handwritten HTTP client.
func TestRunnerNextBuiltCLIAgainstProviderFake(t *testing.T) {
	binary := os.Getenv("PUTNAMI_RUNNER_NEXT_TEST_BINARY")
	if binary == "" {
		t.Skip("build cmd/putnami-cloud, then set PUTNAMI_RUNNER_NEXT_TEST_BINARY")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("PUTNAMI_RUNNER_NEXT_TEST_BINARY must name an absolute built CLI path")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatalf("read built CLI identity: %v", err)
	}
	if info.Path != imageLayersCloudCLIModule+"/cmd/putnami-cloud" || info.Main.Path != imageLayersCloudCLIModule {
		t.Fatal("subprocess proof requires the actual production entrypoint")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		t.Fatalf("built CLI targets %s/%s, proof host is %s/%s", settings["GOOS"], settings["GOARCH"], runtime.GOOS, runtime.GOARCH)
	}
	isolated := t.TempDir()
	runnerNextProviderProof(t, func(args []string, input []byte) (int, string, string) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, append([]string{"__runner-next"}, args...)...)
		command.Dir = isolated
		// The actual startup dispatcher must reach the helper without a user
		// session, checkout, ambient Cloud token, launcher or external command.
		command.Env = []string{"PATH=", "HOME=" + isolated, "PUTNAMI_HOME=" + filepath.Join(isolated, ".putnami")}
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if ctx.Err() != nil {
			t.Fatalf("built runner helper exceeded its subprocess budget: %v", ctx.Err())
		}
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("launch built runner helper: %v", err)
			}
			code = exit.ExitCode()
		}
		return code, stdout.String(), stderr.String()
	})
}
