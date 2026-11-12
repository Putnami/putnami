package php

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/dockertest"
)

// TestMain keeps the image tests from the developer's Docker configuration:
// the default keychain would run the credential helper it names, which can
// block until the job timeout.
func TestMain(m *testing.M) {
	restore := dockertest.IsolateConfig()
	code := m.Run()
	restore()
	os.Exit(code)
}
