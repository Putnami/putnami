package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPreparedRuntimesPreferExplicitGoRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prepare scripts and executable fixture are Unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	repoRoot := prepareRepoRoot(t)
	goRoot := t.TempDir()
	fakeGo := filepath.Join(goRoot, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSource := `#!/bin/sh
printf '%s\n' "$*" > "$PREPARE_CAPTURE"
previous=""
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then
    printf '#!/bin/sh\nexit 0\n' > "$argument"
    chmod +x "$argument"
    exit 0
  fi
  previous="$argument"
done
exit 2
`
	if err := os.WriteFile(fakeGo, []byte(fakeSource), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		extension  string
		outputName string
	}{
		{name: "go", extension: "go", outputName: "putnami-go"},
		{name: "python", extension: "python", outputName: "putnami-python"},
		{name: "typescript", extension: "typescript", outputName: "putnami-ts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			extensionRoot := filepath.Join(repoRoot, test.extension, "extension")
			prepare := filepath.Join(extensionRoot, "bin", "prepare")
			output := t.TempDir()
			capture := filepath.Join(t.TempDir(), "arguments")
			cmd := exec.Command("bash", prepare, "--output", output)
			cmd.Dir = extensionRoot
			cmd.Env = append(os.Environ(),
				"GOROOT="+goRoot,
				"PREPARE_CAPTURE="+capture,
			)
			if combined, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, combined)
			}
			compiled := filepath.Join(output, "compiled", test.outputName)
			if info, err := os.Stat(compiled); err != nil || info.Mode()&0o111 == 0 {
				t.Fatalf("compiled runtime %q is not executable: %v", compiled, err)
			}
			args, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "build -o ") {
				t.Errorf("fake GOROOT Go received unexpected args %q", args)
			}
		})
	}
}

func TestPreparedRuntimesSanitizeHostileGoEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("prepare scripts and executable fixture are Unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	repoRoot := prepareRepoRoot(t)
	goRoot := t.TempDir()
	fakeGo := filepath.Join(goRoot, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSource := `#!/bin/sh
for name in \
  GOENV GOFLAGS GO111MODULE GOWORK GOTOOLCHAIN CGO_ENABLED \
  GOOS GOARCH GO386 GOAMD64 GOARM GOARM64 GOMIPS GOMIPS64 GOPPC64 GORISCV64 GOWASM \
  GOEXPERIMENT GOFIPS140 GODEBUG GO_EXTLINK_ENABLED GO_LDSO \
  GOROOT PATH GOCACHE GOMODCACHE GOPATH GOPROXY GONOPROXY GONOSUMDB GOSUMDB
do
  eval "value=\${$name-<unset>}"
  printf '%s=%s\n' "$name" "$value"
done > "$PREPARE_CAPTURE"
previous=""
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then
    printf '#!/bin/sh\nexit 0\n' > "$argument"
    chmod +x "$argument"
    exit 0
  fi
  previous="$argument"
done
exit 2
`
	if err := os.WriteFile(fakeGo, []byte(fakeSource), 0o755); err != nil {
		t.Fatal(err)
	}

	hostile := map[string]string{
		"GOENV":              "/hostile/go/env",
		"GOFLAGS":            "-race",
		"GO111MODULE":        "off",
		"GOWORK":             "/hostile/go.work",
		"GOTOOLCHAIN":        "auto",
		"CGO_ENABLED":        "1",
		"GOOS":               "plan9",
		"GOARCH":             "arm64",
		"GO386":              "387",
		"GOAMD64":            "v4",
		"GOARM":              "5",
		"GOARM64":            "v9.5",
		"GOMIPS":             "softfloat",
		"GOMIPS64":           "softfloat",
		"GOPPC64":            "power10",
		"GORISCV64":          "rva23u64",
		"GOWASM":             "satconv,signext",
		"GOEXPERIMENT":       "fieldtrack",
		"GOFIPS140":          "latest",
		"GODEBUG":            "installgoroot=all",
		"GO_EXTLINK_ENABLED": "1",
		"GO_LDSO":            "/hostile/ld.so",
	}
	sanitized := map[string]string{
		"GOENV":       "off",
		"GOFLAGS":     "-mod=mod -buildvcs=false -trimpath",
		"GO111MODULE": "on",
		"GOWORK":      "off",
		"GOTOOLCHAIN": "local",
		"CGO_ENABLED": "0",
	}
	preserved := map[string]string{
		"GOROOT":     goRoot,
		"PATH":       "/intentional/toolchain/bin:/usr/bin:/bin",
		"GOCACHE":    "/intentional/cache/build",
		"GOMODCACHE": "/intentional/cache/modules",
		"GOPATH":     "/intentional/go",
		"GOPROXY":    "https://proxy.example.test,direct",
		"GONOPROXY":  "private.example.test",
		"GONOSUMDB":  "private.example.test",
		"GOSUMDB":    "sum.example.test",
	}

	for _, test := range []struct {
		name       string
		extension  string
		outputName string
	}{
		{name: "go", extension: "go", outputName: "putnami-go"},
		{name: "python", extension: "python", outputName: "putnami-python"},
		{name: "typescript", extension: "typescript", outputName: "putnami-ts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range hostile {
				t.Setenv(name, value)
			}
			for name, value := range preserved {
				t.Setenv(name, value)
			}

			extensionRoot := filepath.Join(repoRoot, test.extension, "extension")
			prepare := filepath.Join(extensionRoot, "bin", "prepare")
			output := t.TempDir()
			capture := filepath.Join(t.TempDir(), "environment")
			cmd := exec.Command("bash", prepare, "--output", output)
			cmd.Dir = extensionRoot
			cmd.Env = append(os.Environ(), "PREPARE_CAPTURE="+capture)
			if combined, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, combined)
			}
			compiled := filepath.Join(output, "compiled", test.outputName)
			if info, err := os.Stat(compiled); err != nil || info.Mode()&0o111 == 0 {
				t.Fatalf("compiled runtime %q is not executable: %v", compiled, err)
			}

			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			captured := capturedPrepareEnvironment(t, data)
			for name, want := range sanitized {
				if got := captured[name]; got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			for name := range hostile {
				if _, overwritten := sanitized[name]; overwritten {
					continue
				}
				if got := captured[name]; got != "<unset>" {
					t.Errorf("%s = %q, want unset", name, got)
				}
			}
			for name, want := range preserved {
				if got := captured[name]; got != want {
					t.Errorf("%s = %q, want preserved value %q", name, got, want)
				}
			}
		})
	}
}

func capturedPrepareEnvironment(t *testing.T, data []byte) map[string]string {
	t.Helper()
	captured := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid captured environment line %q", line)
		}
		captured[name] = value
	}
	return captured
}

func prepareRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate prepare test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}
