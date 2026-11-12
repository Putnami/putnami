// Package dockertest isolates a test binary from the Docker configuration of
// the machine that runs it.
//
// The default keychain of go-containerregistry loads the Docker configuration
// and runs the credential helper it names, even for a registry the test serves
// on 127.0.0.1. A developer's helper can block on its desktop application,
// which hangs the test until the job timeout.
//
// A test binary that resolves registry credentials through the default
// keychain calls IsolateConfig in its TestMain, and its registry fixture calls
// RequireIsolated. Only tests and the helper packages that only tests import
// use this package; no product package imports it.
package dockertest

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/scratch"
)

// configEnv names the directory the Docker clients read config.json from.
const configEnv = "DOCKER_CONFIG"

// isolated is the directory IsolateConfig last pointed DOCKER_CONFIG at. The
// variable keeps the directory reachable, and so owned, until restore runs.
var isolated *scratch.Dir

// IsolateConfig points DOCKER_CONFIG at a new directory that holds an empty
// config.json, and answers a function that removes the directory and restores
// the previous value. It panics when it cannot write the directory.
//
// An empty config.json names no credential helper, and its presence keeps the
// default keychain from falling back to Podman's auth files. Call it from
// TestMain, before m.Run: it changes the environment of the process, which a
// parallel test cannot do with t.Setenv.
func IsolateConfig() (restore func()) {
	config, err := scratch.New("putnami-docker-config-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(config.Path(), "config.json"), []byte("{}\n"), 0o600); err != nil {
		panic(err)
	}
	previous, wasSet := os.LookupEnv(configEnv)
	if err := os.Setenv(configEnv, config.Path()); err != nil {
		panic(err)
	}
	before := isolated
	isolated = config
	return func() {
		isolated = before
		if wasSet {
			_ = os.Setenv(configEnv, previous)
		} else {
			_ = os.Unsetenv(configEnv)
		}
		_ = config.Remove()
	}
}

// ConfigIsolated reports whether DOCKER_CONFIG names the directory
// IsolateConfig created.
func ConfigIsolated() bool {
	return isolated != nil && os.Getenv(configEnv) == isolated.Path()
}

// RequireIsolated fails the test when its binary reads the developer's Docker
// configuration. A fixture that serves a registry calls it first.
func RequireIsolated(t testing.TB) {
	t.Helper()
	if !ConfigIsolated() {
		t.Fatal("this test binary reads the developer's Docker configuration: call dockertest.IsolateConfig (go.putnami.dev/sdk/extension/dockertest) in TestMain, and leave DOCKER_CONFIG as it sets it")
	}
}
