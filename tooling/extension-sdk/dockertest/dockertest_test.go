package dockertest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// helperEnv makes the test binary a Docker credential helper: TestMain
// records the call in the file the variable names instead of running the
// tests, so the helper runs wherever the tests run, with no shell.
const helperEnv = "PUTNAMI_DOCKERTEST_HELPER_CALLS"

func TestMain(m *testing.M) {
	if calls := os.Getenv(helperEnv); calls != "" {
		if err := os.WriteFile(calls, []byte("called\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(125)
		}
		// The answer of a helper that holds no credential for the registry.
		fmt.Println("credentials not found in native keychain")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// developerConfig makes the process read a Docker configuration that names a
// credential helper, as a developer's machine does, and answers that
// directory and the file the helper writes when it runs.
func developerConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, "docker-credential-putnami-test")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A link starts faster than a copy: macOS checks a new executable on its
	// first run.
	if os.Link(self, helper) != nil && os.Symlink(self, helper) != nil {
		copyFile(t, self, helper)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"credsStore":"putnami-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv(helperEnv, calls)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(configEnv, dir)
	return dir, calls
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	source, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T) authn.Authenticator {
	t.Helper()
	registry, err := name.NewRegistry("127.0.0.1:5000")
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := authn.DefaultKeychain.Resolve(registry)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}

func TestIsolateConfigKeepsTheDefaultKeychainFromTheCredentialHelper(t *testing.T) {
	developer, calls := developerConfig(t)

	// Without isolation the default keychain runs the helper, even for a
	// registry on 127.0.0.1: the fixture reproduces the developer's machine.
	resolve(t)
	if _, err := os.Stat(calls); err != nil {
		t.Fatalf("the default keychain did not run the credential helper: %v", err)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	if ConfigIsolated() {
		t.Fatal("the configuration reads as isolated before IsolateConfig")
	}

	restore := IsolateConfig()
	dir := os.Getenv(configEnv)
	if dir == developer || !ConfigIsolated() {
		t.Fatalf("DOCKER_CONFIG = %s, isolated = %t", dir, ConfigIsolated())
	}
	if content, err := os.ReadFile(filepath.Join(dir, "config.json")); err != nil || string(content) != "{}\n" {
		t.Fatalf("config.json = %q, %v", content, err)
	}
	if authenticator := resolve(t); authenticator != authn.Anonymous {
		t.Errorf("authenticator = %v, want anonymous", authenticator)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Errorf("the default keychain ran the credential helper: %v", err)
	}

	// A test that changes DOCKER_CONFIG leaves the isolation.
	t.Run("another configuration", func(t *testing.T) {
		t.Setenv(configEnv, developer)
		if ConfigIsolated() {
			t.Error("another DOCKER_CONFIG reads as isolated")
		}
	})

	restore()
	if got := os.Getenv(configEnv); got != developer || ConfigIsolated() {
		t.Errorf("DOCKER_CONFIG = %s after restore, isolated = %t", got, ConfigIsolated())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the isolated directory outlives restore: %v", err)
	}
}

func TestIsolateConfigRestoresAnUnsetVariable(t *testing.T) {
	// t.Setenv registers the restoration of the variable; Unsetenv then makes
	// it absent for the test.
	t.Setenv(configEnv, "")
	if err := os.Unsetenv(configEnv); err != nil {
		t.Fatal(err)
	}
	restore := IsolateConfig()
	if !ConfigIsolated() {
		t.Fatal("the configuration is not isolated")
	}
	restore()
	if value, set := os.LookupEnv(configEnv); set {
		t.Errorf("DOCKER_CONFIG = %q after restore, want unset", value)
	}
}

// fatalRecorder records the failure of the helper under test. Fatal ends the
// goroutine, as testing.T does, so the helper runs in one of its own.
type fatalRecorder struct {
	testing.TB
	message string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatal(args ...any) {
	r.message = fmt.Sprint(args...)
	runtime.Goexit()
}

// requireIsolated answers the failure RequireIsolated reports, or "".
func requireIsolated(t *testing.T) string {
	t.Helper()
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RequireIsolated(recorder)
	}()
	<-done
	return recorder.message
}

func TestRequireIsolatedFailsABinaryThatReadsTheDeveloperConfiguration(t *testing.T) {
	t.Setenv(configEnv, t.TempDir())
	if message := requireIsolated(t); !strings.Contains(message, "call dockertest.IsolateConfig") {
		t.Fatalf("message = %q", message)
	}
	restore := IsolateConfig()
	defer restore()
	if message := requireIsolated(t); message != "" {
		t.Errorf("an isolated binary fails: %s", message)
	}
}
