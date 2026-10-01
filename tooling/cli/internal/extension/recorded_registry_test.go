package extension

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
)

// putRegistryRecording loads a response the production Put registry sent,
// recorded in tooling/cli/testdata/recorded/put-registry. The README there says
// which request produced it. A status-code path at the registry boundary
// answers with one of these, never with a status and body a test wrote.
func putRegistryRecording(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("..", "..", "testdata", "recorded", "put-registry", name))
}
