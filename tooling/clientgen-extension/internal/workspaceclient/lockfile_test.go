package workspaceclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// managerLock is a workspace whose package manager pins @putnami/client at
// 0.5.0-2 in lockfile.
type managerLock struct {
	name     string
	files    map[string]string
	lockfile string
}

func managerLocks() []managerLock {
	return []managerLock{
		{name: "npm lockfile version 3", lockfile: "package-lock.json", files: map[string]string{
			"package-lock.json": `{"lockfileVersion":3,"packages":{"":{"name":"ws"},` +
				`"node_modules/@putnami/client":{"version":"0.5.0-2"},` +
				`"node_modules/@example/items":{"resolved":"clients/items","link":true}}}`,
		}},
		{name: "npm lockfile version 1", lockfile: "package-lock.json", files: map[string]string{
			"package-lock.json": `{"lockfileVersion":1,"dependencies":{"other":{"version":"1.0.0",` +
				`"dependencies":{"@putnami/client":{"version":"0.5.0-2"}}}}}`,
		}},
		{name: "npm shrinkwrap over package-lock", lockfile: "npm-shrinkwrap.json", files: map[string]string{
			"package.json":        `{"packageManager":"npm@10.9.0"}`,
			"npm-shrinkwrap.json": `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.5.0-2"}}}`,
			"package-lock.json":   `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.4.0"}}}`,
		}},
		{name: "npm shrinkwrap over package-lock without a declared manager", lockfile: "npm-shrinkwrap.json", files: map[string]string{
			"npm-shrinkwrap.json": `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.5.0-2"}}}`,
			"package-lock.json":   `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.4.0"}}}`,
		}},
		{name: "pnpm lockfile version 9", lockfile: "pnpm-lock.yaml", files: map[string]string{
			"pnpm-lock.yaml": "lockfileVersion: '9.0'\n\nimporters:\n\n  .:\n    devDependencies:\n      '@putnami/client':\n" +
				"        specifier: ^0.5.0-0\n        version: 0.5.0-2\n\npackages:\n\n  '@putnami/client@0.5.0-2':\n" +
				"    resolution: {integrity: sha512-x}\n\n  other@1.0.0:\n    resolution: {integrity: sha512-y}\n",
		}},
		{name: "pnpm lockfile version 6", lockfile: "pnpm-lock.yaml", files: map[string]string{
			"pnpm-lock.yaml": "lockfileVersion: '6.0'\n\npackages:\n\n  /@putnami/client@0.5.0-2(typescript@6.0.2):\n" +
				"    resolution: {integrity: sha512-x}\n",
		}},
		{name: "pnpm lockfile version 5", lockfile: "pnpm-lock.yaml", files: map[string]string{
			"pnpm-lock.yaml": "lockfileVersion: 5.4\n\npackages:\n\n  /@putnami/client/0.5.0-2_typescript@6.0.2:\n" +
				"    resolution: {integrity: sha512-x}\n",
		}},
		{name: "Yarn classic", lockfile: "yarn.lock", files: map[string]string{
			"yarn.lock": "# yarn lockfile v1\n\n\"@putnami/client@^0.5.0-0\", \"@putnami/client@0.5.0-2\":\n" +
				"  version \"0.5.0-2\"\n  resolved \"https://registry.npmjs.org/x.tgz\"\n\nother@^1.0.0:\n  version \"1.0.0\"\n",
		}},
		{name: "Yarn Berry", lockfile: "yarn.lock", files: map[string]string{
			"yarn.lock": "__metadata:\n  version: 8\n\n\"@putnami/client@npm:^0.5.0-0\":\n  version: 0.5.0-2\n" +
				"  dependencies:\n    other: \"npm:^1.0.0\"\n\n\"other@npm:^1.0.0\":\n  version: 1.0.0\n",
		}},
		{name: "Rush with pnpm", lockfile: "common/config/rush/pnpm-lock.yaml", files: map[string]string{
			"rush.json":                         `{"pnpmVersion":"9.0.0"}`,
			"common/config/rush/pnpm-lock.yaml": "lockfileVersion: '9.0'\n\npackages:\n\n  '@putnami/client@0.5.0-2':\n    resolution: {integrity: sha512-x}\n",
		}},
		{name: "a declared manager over a stray bun.lock", lockfile: "pnpm-lock.yaml", files: map[string]string{
			"package.json":   `{"packageManager":"pnpm@9.15.0+sha512.x"}`,
			"bun.lock":       "{\n  \"packages\": {\n    \"@putnami/client\": [\"@putnami/client@0.4.0\", \"\", {}, \"sha512-x\"],\n  }\n}\n",
			"pnpm-lock.yaml": "lockfileVersion: '9.0'\n\npackages:\n\n  '@putnami/client@0.5.0-2':\n    resolution: {integrity: sha512-x}\n",
		}},
	}
}

// Every package manager @putnami/typescript installs with: the check reads the
// lockfile the workspace's manager writes, fails an install that lags it
// before the emitter runs, and generates with an install that matches it.
func TestTheCheckReadsTheLockfileOfTheWorkspacePackageManager(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"the-check-reads-the-lockfile-of-the-workspace-package-manager")
	for _, manager := range managerLocks() {
		t.Run(manager.name, func(t *testing.T) {
			root := t.TempDir()
			client := installEmitter(t, root, writesClient)
			for rel, body := range manager.files {
				writeWorkspaceFile(t, root, rel, body)
			}
			modules := filepath.Join(root, "node_modules")
			generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}

			writeInstalledClient(t, modules, "0.5.0-1")
			err := GenerateTypeScript(root, generation)
			if err == nil {
				t.Fatal("an install that lags the lock generated")
			}
			for _, want := range []string{"0.5.0-1", manager.lockfile + " pins 0.5.0-2", "putnami deps install"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if _, err := os.Stat(filepath.Join(client, "ran")); !os.IsNotExist(err) {
				t.Fatalf("the emitter ran: %v", err)
			}

			writeInstalledClient(t, modules, "0.5.0-2")
			if err := GenerateTypeScript(root, generation); err != nil {
				t.Fatalf("an install that matches the lock failed: %v", err)
			}
		})
	}
}

// A lockfile pin that is a source, not a registry version, leaves nothing to
// compare, whatever the manager.
func TestALockThatPinsNoRegistryVersionGeneratesWithAnyManager(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-matches-the-lock-or-a-lock-with-no-registry-pin-generates")
	locks := map[string]string{
		"package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"resolved":"vendor/client","link":true},` +
			`"node_modules/other/node_modules/@putnami/client":{"version":"file:../client.tgz"}}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n\npackages:\n\n  '@putnami/client@file:vendor/client.tgz':\n    resolution: {tarball: file:vendor/client.tgz}\n",
		"yarn.lock":      "__metadata:\n  version: 8\n\n\"@putnami/client@workspace:vendor/client\":\n  version: 0.0.0-use.local\n",
	}
	for lockfile, body := range locks {
		t.Run(lockfile, func(t *testing.T) {
			root := t.TempDir()
			client := installEmitter(t, root, writesClient)
			writeWorkspaceFile(t, root, lockfile, body)
			writeInstalledClient(t, filepath.Join(root, "node_modules"), "0.4.0")
			generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
			if err := GenerateTypeScript(root, generation); err != nil {
				t.Fatalf("a %s that pins no registry version failed: %v", lockfile, err)
			}
		})
	}
}

// An @putnami/client the manager links from the workspace is keyed by the
// task's emitter input, so no lockfile version describes it, even one the
// check cannot read.
func TestAClientLinkedFromTheWorkspaceIsComparedWithNoLock(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-matches-the-lock-or-a-lock-with-no-registry-pin-generates")
	root := t.TempDir()
	client := installEmitter(t, root, writesClient)
	source := filepath.Join(root, "typescript", "framework", "client")
	writeWorkspaceFile(t, root, "typescript/framework/client/package.json", `{"name":"@putnami/client","version":"0.4.0"}`)
	scope := filepath.Join(root, "node_modules", "@putnami")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(scope, "client")); err != nil {
		t.Fatal(err)
	}
	generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
	for _, lockfile := range []string{"package-lock.json", "bun.lockb"} {
		writeWorkspaceFile(t, root, lockfile, `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.5.0-2"}}}`)
		if err := GenerateTypeScript(root, generation); err != nil {
			t.Fatalf("a workspace-linked client under %s failed: %v", lockfile, err)
		}
		if err := os.Remove(filepath.Join(root, lockfile)); err != nil {
			t.Fatal(err)
		}
	}
}

// The binary bun.lockb, a lockfile that does not parse, and a manager
// @putnami/typescript does not install with leave the check blind: the
// generation fails before the emitter runs and names the remedy.
func TestALockfileTheCheckCannotReadRefusesTheGeneration(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"a-lockfile-the-check-cannot-read-refuses-the-generation")
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "binary bun.lockb", files: map[string]string{"bun.lockb": "#!/usr/bin/env bun\nbun-lockfile-format-v0\n\x00\x01"},
			want: []string{"bun.lockb", "cannot read", "--save-text-lockfile"}},
		{name: "unparsable package-lock.json", files: map[string]string{"package-lock.json": `{"packages":`},
			want: []string{"parse the workspace's package-lock.json"}},
		{name: "pnpm-lock.yaml with no lockfileVersion", files: map[string]string{"pnpm-lock.yaml": "<<<<<<< HEAD\npackages:\n"},
			want: []string{"parse the workspace's pnpm-lock.yaml"}},
		{name: "yarn.lock in no Yarn format", files: map[string]string{"yarn.lock": "not a lockfile\n"},
			want: []string{"parse the workspace's yarn.lock"}},
		{name: "unknown manager", files: map[string]string{"package.json": `{"packageManager":"deno@2.1.0"}`, "bun.lock": "{}"},
			want: []string{`"deno@2.1.0"`, "does not install with", "bun, npm, pnpm or yarn"}},
		{name: "unparsable package.json", files: map[string]string{"package.json": `{"packageManager":`},
			want: []string{"parse the workspace's package.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			client := installEmitter(t, root, writesClient)
			for rel, body := range tc.files {
				writeWorkspaceFile(t, root, rel, body)
			}
			writeInstalledClient(t, filepath.Join(root, "node_modules"), "0.5.0-2")
			generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
			err := GenerateTypeScript(root, generation)
			if err == nil {
				t.Fatal("the generation ran with a lockfile the check cannot read")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if _, err := os.Stat(filepath.Join(client, "ran")); !os.IsNotExist(err) {
				t.Fatalf("the emitter ran: %v", err)
			}
		})
	}
}

// A workspace whose manager wrote no lockfile yet, such as a pnpm workspace
// before its first install, leaves nothing to compare.
func TestAWorkspaceWithNoLockfileGenerates(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "an-install-that-differs-from-the-lock-fails",
		"an-install-that-matches-the-lock-or-a-lock-with-no-registry-pin-generates")
	for name, files := range map[string]map[string]string{
		"no file":                {},
		"pnpm-workspace.yaml":    {"pnpm-workspace.yaml": "packages:\n  - clients/*\n", "yarn.lock": "\"@putnami/client@^0.5.0\":\n  version \"0.4.0\"\n"},
		"declared with no lock":  {"package.json": `{"packageManager":"yarn@4.5.0"}`},
		"Rush with no lock":      {"rush.json": `{}`, "package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/@putnami/client":{"version":"0.4.0"}}}`},
		"packageManager is null": {"package.json": `{"packageManager":null}`},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			client := installEmitter(t, root, writesClient)
			for rel, body := range files {
				writeWorkspaceFile(t, root, rel, body)
			}
			writeInstalledClient(t, filepath.Join(root, "node_modules"), "0.5.0-2")
			generation := TypeScriptGeneration{Contract: writeContract(t, root, firstPartyContract), Project: client, PackageName: "@example/items"}
			if err := GenerateTypeScript(root, generation); err != nil {
				t.Fatalf("a workspace with no lockfile failed: %v", err)
			}
		})
	}
}
