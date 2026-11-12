// Package dockertest isolates a test binary from the Docker configuration of
// the machine that runs it.
//
// The default keychain of go-containerregistry loads the Docker configuration
// and runs the credential helper it names, even for a registry the test serves
// on 127.0.0.1. A developer's helper can block on its desktop application,
// which hangs the test until the job timeout.
//
// Every test binary that reaches a registry calls IsolateConfig in its
// TestMain. Only tests and the helper packages that only tests import use this
// package; no product package imports it.
package dockertest

import (
	"os"
	"path/filepath"

	"go.putnami.dev/sdk/extension/scratch"
)

// configEnv names the directory the Docker clients read config.json from.
const configEnv = "DOCKER_CONFIG"

// isolated is the directory IsolateConfig last pointed DOCKER_CONFIG at.
var isolated string

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
	isolated = config.Path()
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

// ConfigIsolated reports whether DOCKER_CONFIG still names the directory
// IsolateConfig created. A fixture that serves a registry calls it to fail a
// test whose binary would read the developer's Docker configuration.
func ConfigIsolated() bool {
	return isolated != "" && os.Getenv(configEnv) == isolated
}
