package workspaceclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// firstPartyContract declares one first-party operation and one an external
// authority owns.
const firstPartyContract = `{"openapi":"3.1.0","x-putnami-client":{},"paths":{` +
	`"/items":{"get":{"operationId":"listItems","x-putnami-client":{}}},` +
	`"/oauth/token":{"post":{"operationId":"token","x-putnami-external-contract":"RFC 6749"}}}}`

// externalOnlyContract serves only an operation an external authority owns.
const externalOnlyContract = `{"openapi":"3.1.0","x-putnami-client":{},"paths":{` +
	`"/oauth/token":{"post":{"operationId":"token","x-putnami-external-contract":"RFC 6749"}}}}`

// installEmitter installs a TypeScript emitter shim at the workspace root that
// runs body, and returns the client project directory.
func installEmitter(t *testing.T, root, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	if err := os.Unsetenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "putnami-client-generate"), []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test-owned executable fixture
		t.Fatal(err)
	}
	client := filepath.Join(root, "clients", "items")
	if err := os.MkdirAll(client, 0o755); err != nil {
		t.Fatal(err)
	}
	return client
}

func writeContract(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "openapi.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An install that predates the --contract and --output arguments runs an
// emitter that ignores them, exits 0 and writes nothing. The generation fails
// and names the package to upgrade, so the task never succeeds with an empty
// client and no cache entry records the client's src empty.
func TestAGenerationThatLeavesNoClientForAContractWithOperationsFails(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-emitter-that-writes-no-client-fails",
		"a-generation-that-leaves-no-client-for-a-contract-with-operations-fails")
	root := t.TempDir()
	client := installEmitter(t, root, "exit 0\n")
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	err := GenerateTypeScript(root, generation)
	if err == nil {
		t.Fatal("an emitter that wrote no client succeeded")
	}
	for _, want := range []string{"wrote no src/index.ts", "1 first-party operation", "predates the --contract", "upgrade @putnami/client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

// The emitter's own output satisfies the check, and a contract with no
// first-party operation needs no client: the emitter removes src for it.
func TestAGenerationOfAContractWithNoFirstPartyOperationNeedsNoClient(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-emitter-that-writes-no-client-fails",
		"a-generation-of-a-contract-with-no-first-party-operation-needs-no-client")
	root := t.TempDir()
	client := installEmitter(t, root, "mkdir -p src && printf 'export {};\\n' > src/index.ts\n")
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err != nil {
		t.Fatalf("an emitter that wrote the client failed: %v", err)
	}

	root = t.TempDir()
	client = installEmitter(t, root, "exit 0\n")
	generation = TypeScriptGeneration{Contract: writeContract(t, root, externalOnlyContract), Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err != nil {
		t.Fatalf("a contract with no first-party operation failed: %v", err)
	}
}

// A contract whose path items carry members besides operations, such as
// parameters or a summary, still counts only its operations.
func TestAGenerationCountsOnlyTheOperationsOfAPathItem(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-emitter-that-writes-no-client-fails",
		"a-generation-of-a-contract-with-no-first-party-operation-needs-no-client")
	root := t.TempDir()
	client := installEmitter(t, root, "exit 0\n")
	contract := `{"openapi":"3.1.0","x-putnami-client":{},"paths":{"/oauth/token":{` +
		`"summary":"Token","parameters":[{"name":"x","in":"query"}],` +
		`"post":{"operationId":"token","x-putnami-external-contract":"RFC 6749"}}}}`
	generation := TypeScriptGeneration{Contract: writeContract(t, root, contract), Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err != nil {
		t.Fatalf("a path item with parameters failed: %v", err)
	}

	generation.Contract = writeContract(t, root, `{"paths":`)
	if err := GenerateTypeScript(root, generation); err == nil || !strings.Contains(err.Error(), "read the provider's contract") {
		t.Fatalf("an unparsable contract error = %v", err)
	}
}

// writeLock writes the workspace's bun.lock, pinning @putnami/client at pin
// under key.
func writeLock(t *testing.T, root, key, pin string) {
	t.Helper()
	lock := "{\n  \"lockfileVersion\": 1,\n  \"packages\": {\n" +
		"    \"" + key + "\": [\"@putnami/client@" + pin + "\", \"\", {}, \"sha512-x\"],\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(root, "bun.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeInstalledClient writes the manifest of the @putnami/client installed
// beside the emitter shim under modules.
func writeInstalledClient(t *testing.T, modules, version string) {
	t.Helper()
	dir := filepath.Join(modules, "@putnami", "client")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"@putnami/client","version":"`+version+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writesClient is an emitter body that writes the client and leaves a mark
// that it ran.
const writesClient = "touch ran && mkdir -p src && printf 'export {};\\n' > src/index.ts\n"

// The task's key folds bun.lock, never node_modules: an install that lags the
// lock would store an older client under the lock's key. The generation fails
// before the emitter runs.
func TestAnInstallThatLagsTheLockFailsBeforeTheEmitterRuns(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-lags-the-lock-fails-before-the-emitter-runs")
	root := t.TempDir()
	client := installEmitter(t, root, writesClient)
	writeLock(t, root, "@putnami/client", "0.5.0-2")
	writeInstalledClient(t, filepath.Join(root, "node_modules"), "0.5.0-1")
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	err := GenerateTypeScript(root, generation)
	if err == nil {
		t.Fatal("an install that lags the lock generated")
	}
	for _, want := range []string{"0.5.0-1", "bun.lock pins 0.5.0-2", "putnami deps install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(client, "ran")); !os.IsNotExist(err) {
		t.Fatalf("the emitter ran: %v", err)
	}

	// A lock that pins the package but an install that lost its manifest.
	if err := os.RemoveAll(filepath.Join(root, "node_modules", "@putnami")); err != nil {
		t.Fatal(err)
	}
	if err := GenerateTypeScript(root, generation); err == nil || !strings.Contains(err.Error(), "putnami deps install") {
		t.Fatalf("an install with no manifest error = %v", err)
	}
}

func TestAnInstallThatMatchesTheLockGenerates(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-matches-the-lock-or-a-lock-with-no-registry-pin-generates")
	root := t.TempDir()
	client := installEmitter(t, root, writesClient)
	writeLock(t, root, "@putnami/client", "0.5.0-2")
	writeInstalledClient(t, filepath.Join(root, "node_modules"), "0.5.0-2")
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err != nil {
		t.Fatalf("an install that matches the lock failed: %v", err)
	}

	// A workspace package, or a file: tarball that bun writes as a path or a
	// bare file name, pins no registry version, and has nothing to compare.
	if err := os.RemoveAll(filepath.Join(root, "node_modules", "@putnami")); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"workspace:typescript/framework/client", "../vendor/client.tgz", "0.5.0-2.tgz"} {
		writeLock(t, root, "@putnami/client", pin)
		if err := GenerateTypeScript(root, generation); err != nil {
			t.Fatalf("a lock that pins %s failed: %v", pin, err)
		}
	}
}

// The client project's own install is keyed in bun.lock under the project's
// name.
func TestTheClientProjectsOwnInstallIsComparedWithItsOwnPin(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-lags-the-lock-fails-before-the-emitter-runs")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	root := t.TempDir()
	client := installEmitter(t, root, writesClient)
	modules := filepath.Join(client, "node_modules")
	writeExecutableFixture(t, filepath.Join(modules, ".bin"), "putnami-client-generate", "ts")
	t.Setenv("CLIENTGEN_CAPTURE", filepath.Join(root, "calls"))
	writeInstalledClient(t, modules, "0.4.0")
	writeLock(t, root, "@example/items/@putnami/client", "0.5.0-2")
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	if err := GenerateTypeScript(root, generation); err == nil || !strings.Contains(err.Error(), "bun.lock pins 0.5.0-2") {
		t.Fatalf("a project install that lags its own pin error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
		t.Fatalf("the project's emitter ran: %v", err)
	}
}
