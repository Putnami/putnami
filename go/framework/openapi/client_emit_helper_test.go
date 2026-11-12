package openapi

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/api"
)

// emitGoClient emits the Go client of a contract the way a client project's
// client-generate does, and returns its source.
func emitGoClient(t *testing.T, contract []byte, packageName, clientName string) ([]byte, error) {
	t.Helper()
	output := t.TempDir()
	if _, err := api.EmitGoClient(contract, output, api.EmitGoClientOptions{PackageName: packageName, ClientName: clientName}); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(output, api.GoClientFile)) //nolint:gosec // G304: test-owned temporary directory
}

// emitDescribedGoClient emits the Go client of the contract a describe run
// wrote below outputDir.
func emitDescribedGoClient(t *testing.T, outputDir, packageName, clientName string) ([]byte, error) {
	t.Helper()
	contract, err := os.ReadFile(filepath.Join(outputDir, DefaultDescribePath)) //nolint:gosec // G304: test-owned temporary directory
	if err != nil {
		return nil, err
	}
	return emitGoClient(t, contract, packageName, clientName)
}
