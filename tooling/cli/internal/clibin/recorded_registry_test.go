package clibin

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
)

// putRegistryRecording loads a response the production Put registry sent,
// recorded in tooling/cli/testdata/recorded/put-registry. The README there says
// which request produced it.
func putRegistryRecording(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("..", "..", "testdata", "recorded", "put-registry", name))
}
