package configextract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocfg "go.putnami.dev/protocol/config"
)

const mergeFixtureConfig = `package app

import "go.putnami.dev/config"

type ServerOptions struct {
	Host string ` + "`json:\"host\"`" + `
}

var ServerConfig = config.Config[ServerOptions]("server")
`

func TestMergeDependencyBlocks_UnionsWorkloadAndDependency(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.25\n",
		"config.go": mergeFixtureConfig,
	})

	depBlocks := []protocfg.Block{{
		Path: "core",
		Fields: []protocfg.FieldSchema{
			{Name: "clientSecret", Type: protocfg.FieldTypeString, Sensitive: true, Env: "CORE_CLIENT_SECRET"},
		},
	}}

	res, ok, err := MergeDependencyBlocks(dir, "app", "0.0.0", DefaultOutputPath, depBlocks)
	if err != nil {
		t.Fatalf("MergeDependencyBlocks: %v", err)
	}
	if !ok || res == nil {
		t.Fatalf("expected ok result, got ok=%v res=%v", ok, res)
	}

	var manifest protocfg.SchemaManifest
	data, err := os.ReadFile(filepath.Join(dir, DefaultOutputPath))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	paths := map[string]bool{}
	for _, b := range manifest.Configs {
		paths[b.Path] = true
	}
	if !paths["server"] || !paths["core"] {
		t.Fatalf("expected both server and core blocks, got %v", paths)
	}

	// The library-owned secret lands in the secrets sidecar, so a deploy can
	// actually publish core.auth-style secrets the workload never declares.
	m := readSidecar(t, dir)
	found := false
	for _, s := range m.Secrets {
		if s == "core_client_secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected core_client_secret in secrets sidecar, got %v", m.Secrets)
	}
}

func TestMergeDependencyBlocks_OnlyDependencyBlocks(t *testing.T) {
	// A workload with no config of its own still publishes its dependencies'.
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	depBlocks := []protocfg.Block{{
		Path:   "core",
		Fields: []protocfg.FieldSchema{{Name: "host", Type: protocfg.FieldTypeString}},
	}}

	_, ok, err := MergeDependencyBlocks(dir, "app", "0.0.0", DefaultOutputPath, depBlocks)
	if err != nil {
		t.Fatalf("MergeDependencyBlocks: %v", err)
	}
	if !ok {
		t.Fatal("expected a schema to be written for dependency-only config")
	}
	var manifest protocfg.SchemaManifest
	data, err := os.ReadFile(filepath.Join(dir, DefaultOutputPath))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if len(manifest.Configs) != 1 || manifest.Configs[0].Path != "core" {
		t.Fatalf("expected single core block, got %+v", manifest.Configs)
	}
}

func TestMergeDependencyBlocks_ConflictingPathErrors(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.25\n",
		"config.go": mergeFixtureConfig,
	})

	depBlocks := []protocfg.Block{{
		Path:   "server", // collides with the workload's own block
		Fields: []protocfg.FieldSchema{{Name: "host", Type: protocfg.FieldTypeString}},
	}}

	_, _, err := MergeDependencyBlocks(dir, "app", "0.0.0", DefaultOutputPath, depBlocks)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("error should name the conflicting path: %v", err)
	}
}
