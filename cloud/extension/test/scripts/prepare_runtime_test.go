package scripts

import (
	"bytes"
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// compiledRuntime is the runtime path prepare-runtime writes, relative to its
// output directory.
const compiledRuntime = "compiled/putnami-cloud"

// hostileGoEnvironment holds Go controls that change the compiled bytes when
// they reach the build.
var hostileGoEnvironment = map[string]string{
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

// pinnedGoEnvironment is the value prepare-runtime gives each Go control it
// sets. It unsets every other control of hostileGoEnvironment.
var pinnedGoEnvironment = map[string]string{
	"GOENV":       "off",
	"GOFLAGS":     "-mod=readonly -buildvcs=false -trimpath",
	"GO111MODULE": "on",
	"GOWORK":      "off",
	"GOTOOLCHAIN": "local",
	"CGO_ENABLED": "0",
}

// prepare-runtime pins the Go controls that change the compiled bytes, unsets
// the ambient ones, keeps the caller's cache, proxy and toolchain locations,
// and builds with $GOROOT/bin/go when GOROOT names a Go toolchain.
func TestPrepareRuntimePinsTheGoEnvironment(t *testing.T) {
	requirePOSIXShell(t)

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
printf '%s\n' "$*" > "$PREPARE_CAPTURE.args"
`
	if err := os.WriteFile(fakeGo, []byte(fakeSource), 0o755); err != nil {
		t.Fatal(err)
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
	for name, value := range hostileGoEnvironment {
		t.Setenv(name, value)
	}
	for name, value := range preserved {
		t.Setenv(name, value)
	}

	output := t.TempDir()
	capture := filepath.Join(t.TempDir(), "environment")
	cmd := exec.Command("/bin/sh", prepareRuntimeScript(t), "--output", output)
	cmd.Env = append(os.Environ(), "PREPARE_CAPTURE="+capture)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare-runtime failed: %v\n%s", err, combined)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	captured := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid captured environment line %q", line)
		}
		captured[name] = value
	}
	for name, want := range pinnedGoEnvironment {
		if got := captured[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for name := range hostileGoEnvironment {
		if _, pinned := pinnedGoEnvironment[name]; pinned {
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

	args, err := os.ReadFile(capture + ".args")
	if err != nil {
		t.Fatal(err)
	}
	want := "build -o " + filepath.Join(output, filepath.FromSlash(compiledRuntime)) + " ./cmd/putnami-cloud"
	if got := strings.TrimSpace(string(args)); got != want {
		t.Errorf("go received %q, want %q", got, want)
	}
}

// Two prepares build the same bytes, even when the caller's Go controls differ.
// The runtime embeds no source path and no version control state, and targets
// the host without cgo.
func TestPrepareRuntimeBuildsTheSameBytesTwice(t *testing.T) {
	requirePOSIXShell(t)

	script := prepareRuntimeScript(t)
	extensionRoot := filepath.Dir(filepath.Dir(script))
	builds := make([][]byte, 0, 2)
	for _, environment := range []map[string]string{
		{},
		{
			"GOFLAGS":     "-buildvcs=true -ldflags=-X=main.prepareRuntimeHostile=1",
			"CGO_ENABLED": "1",
			"GOWORK":      filepath.Join(extensionRoot, "..", "..", "go.work"),
		},
	} {
		output := t.TempDir()
		cmd := exec.Command("/bin/sh", script, "--output", output)
		cmd.Env = os.Environ()
		for name, value := range environment {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare-runtime failed: %v\n%s", err, combined)
		}
		compiled := filepath.Join(output, filepath.FromSlash(compiledRuntime))
		data, err := os.ReadFile(compiled)
		if err != nil {
			t.Fatal(err)
		}
		builds = append(builds, data)

		info, err := buildinfo.ReadFile(compiled)
		if err != nil {
			t.Fatalf("read the runtime build information: %v", err)
		}
		settings := make(map[string]string)
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
			if strings.HasPrefix(setting.Key, "vcs") {
				t.Errorf("the runtime embeds version control state %s=%s", setting.Key, setting.Value)
			}
		}
		for key, want := range map[string]string{
			"-trimpath":   "true",
			"CGO_ENABLED": "0",
			"GOOS":        runtime.GOOS,
			"GOARCH":      runtime.GOARCH,
		} {
			if got := settings[key]; got != want {
				t.Errorf("runtime build setting %s = %q, want %q", key, got, want)
			}
		}
		if bytes.Contains(data, []byte(extensionRoot)) {
			t.Errorf("the runtime embeds the source path %s", extensionRoot)
		}
	}
	if !bytes.Equal(builds[0], builds[1]) {
		t.Error("two prepares of one source tree built different runtimes")
	}
}

func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("prepare-runtime needs a POSIX shell")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Fatalf("no POSIX shell at /bin/sh: %v", err)
	}
}

func prepareRuntimeScript(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the prepare-runtime test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "bin", "prepare-runtime"))
}
