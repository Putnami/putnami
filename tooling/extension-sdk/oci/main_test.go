package oci

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/scratch"
)

// TestMain points DOCKER_CONFIG at a directory holding an empty config.json,
// so the default keychain never runs the developer's credential helper.
func TestMain(m *testing.M) {
	// The default keychain loads the docker config and runs the credential
	// helper it names. A developer's helper can block on its desktop app,
	// which hangs these tests until the job timeout. An empty config.json in
	// DOCKER_CONFIG names no helper, and its presence keeps the keychain from
	// falling back to Podman's auth files.
	config, err := scratch.New("putnami-docker-config-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(config.Path(), "config.json"), []byte("{}\n"), 0o600); err != nil {
		panic(err)
	}
	if err := os.Setenv("DOCKER_CONFIG", config.Path()); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = config.Remove()
	os.Exit(code)
}
