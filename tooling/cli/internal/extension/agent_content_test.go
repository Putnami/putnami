package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	contentExtension = "@acme/contributor"
	contentDigest    = "3f6c9a1d2b7e4f8a0c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3"
)

func packagedContentManifest(version string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "agentContent": {"path": "agent-content", "manifestSha256": %q}
}`, contentExtension, version, protocolcli.AgentContentContract, contentDigest)
}

// installContentExtension writes an installed, lock-pinned extension the way
// the installer leaves one: the manifest under the stable link path and a lock
// entry binding its digest. It returns the manifest digest.
func installContentExtension(t *testing.T, wsRoot, version, manifest string) string {
	t.Helper()
	writeExtensionManifest(t, layout.StableDir(wsRoot, layout.Extensions, contentExtension), manifest)
	digest := lockfile.HashBytes([]byte(manifest))
	lock := lockfile.NewLockFile()
	lock.SetExtension(contentExtension, lockfile.LockEntry{Version: version, ManifestHash: digest})
	if err := lockfile.WriteLockFile(wsRoot, lock); err != nil {
		t.Fatal(err)
	}
	return digest
}

func contentConfig(refs ...string) *wsproto.Config {
	list := make(map[string]string, len(refs))
	for _, ref := range refs {
		list[ref] = ""
	}
	return &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: list}}
}

func TestLocateAgentContent_BindsAnInstalledExtensionToItsPin(t *testing.T) {
	wsRoot := t.TempDir()
	digest := installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))

	source, err := LocateAgentContent(wsRoot, contentConfig(contentExtension), contentExtension)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if source.Local || source.Name != contentExtension || source.Version != "1.2.0" {
		t.Fatalf("source = %+v", source)
	}
	if source.ManifestHash != digest {
		t.Fatalf("manifest hash = %s, want the pinned %s", source.ManifestHash, digest)
	}
	if source.Contribution.Path != "agent-content" || source.Contribution.ManifestSHA256 != contentDigest {
		t.Fatalf("contribution = %+v", source.Contribution)
	}
	if source.Root != layout.StableDir(wsRoot, layout.Extensions, contentExtension) {
		t.Fatalf("root = %s, want the stable link", source.Root)
	}
}

// A run that died between extraction and relink leaves the exact release in
// its artifact directory and no stable link. Discovery loads the extension's
// commands from that directory, so its content is read from there too, under
// the same pin; a manifest there that the pin does not bind is refused.
func TestLocateAgentContent_ReadsTheExactArtifactWhenTheStableLinkIsMissing(t *testing.T) {
	wsRoot := t.TempDir()
	manifest := packagedContentManifest("1.2.0")
	digest := installContentExtension(t, wsRoot, "1.2.0", manifest)
	if err := os.RemoveAll(layout.StableDir(wsRoot, layout.Extensions, contentExtension)); err != nil {
		t.Fatal(err)
	}
	artifact := layout.ArtifactDir(wsRoot, layout.Extensions, contentExtension, "1.2.0")
	writeExtensionManifest(t, artifact, manifest)

	source, err := LocateAgentContent(wsRoot, contentConfig(contentExtension), contentExtension)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if source.Root != artifact || source.ManifestHash != digest || source.Version != "1.2.0" {
		t.Fatalf("source = %+v, want the exact artifact directory %s", source, artifact)
	}
	lock, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	loaded, skipped := tryLoadInstalledExtension(wsRoot, contentExtension, lock)
	if skipped != nil || loaded == nil || loaded.Path != source.Root {
		t.Fatalf("discovery loaded %+v (skipped %+v); the content came from %s", loaded, skipped, source.Root)
	}

	writeExtensionManifest(t, artifact, strings.Replace(manifest, contentDigest, strings.Repeat("0", 64), 1))
	if _, err := LocateAgentContent(wsRoot, contentConfig(contentExtension), contentExtension); err == nil ||
		!strings.Contains(err.Error(), "does not match its pin") {
		t.Fatalf("error = %v, want the drifted artifact refused", err)
	}
}

func TestLocateAgentContent_RefusesWhatThePinDoesNotBind(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, wsRoot string)
		cfg   *wsproto.Config
		want  string
	}{
		"an undeclared extension": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))
			},
			cfg:  contentConfig("@acme/other"),
			want: "declares no extension " + contentExtension,
		},
		"an unpinned extension": {
			setup: func(*testing.T, string) {},
			cfg:   contentConfig(contentExtension),
			want:  "pins no extension " + contentExtension,
		},
		"a pin without a manifest digest": {
			setup: func(t *testing.T, wsRoot string) {
				lock := lockfile.NewLockFile()
				lock.SetExtension(contentExtension, lockfile.LockEntry{Version: "1.2.0"})
				if err := lockfile.WriteLockFile(wsRoot, lock); err != nil {
					t.Fatal(err)
				}
			},
			cfg:  contentConfig(contentExtension),
			want: "without a manifest digest",
		},
		"a pinned but uninstalled extension": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))
				if err := os.RemoveAll(layout.StableDir(wsRoot, layout.Extensions, contentExtension)); err != nil {
					t.Fatal(err)
				}
			},
			cfg:  contentConfig(contentExtension),
			want: "pinned but not installed",
		},
		"a manifest that drifted from its pin": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))
				writeExtensionManifest(t, layout.StableDir(wsRoot, layout.Extensions, contentExtension),
					strings.Replace(packagedContentManifest("1.2.0"), contentDigest, strings.Repeat("0", 64), 1))
			},
			cfg:  contentConfig(contentExtension),
			want: "does not match its pin",
		},
		"a manifest version other than the pin": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("9.9.9"))
			},
			cfg:  contentConfig(contentExtension),
			want: "declares version 9.9.9",
		},
		"an installed extension that ships only its source": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", fmt.Sprintf(
					`{"name":%q,"version":"1.2.0","cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
					contentExtension, protocolcli.AgentContentContract))
			},
			cfg:  contentConfig(contentExtension),
			want: "must be re-packaged",
		},
		"an extension without agent content": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", fmt.Sprintf(
					`{"name":%q,"version":"1.2.0","cliContract":%d,"commands":{"x":{"run":[{"id":"x","task":"t"}]}},"tasks":{"t":{"kind":"command","command":"echo"}}}`,
					contentExtension, protocolcli.CurrentContract))
			},
			cfg:  contentConfig(contentExtension),
			want: "declares no agent content",
		},
		"a content manifest stamped below its vocabulary": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0",
					strings.Replace(packagedContentManifest("1.2.0"), fmt.Sprintf(`"cliContract": %d`, protocolcli.AgentContentContract),
						fmt.Sprintf(`"cliContract": %d`, protocolcli.CurrentContract), 1))
			},
			cfg:  contentConfig(contentExtension),
			want: "for its agent-content contribution",
		},
	} {
		t.Run(name, func(t *testing.T) {
			wsRoot := t.TempDir()
			tc.setup(t, wsRoot)
			source, err := LocateAgentContent(wsRoot, tc.cfg, contentExtension)
			if err == nil || source != nil {
				t.Fatalf("located %+v, want a refusal", source)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestFindDeclaredExtension_ResolvesALocalEntryByItsManifestName(t *testing.T) {
	wsRoot := t.TempDir()
	writeExtensionManifest(t, filepath.Join(wsRoot, "tools", "contributor"), fmt.Sprintf(
		`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		contentExtension, protocolcli.AgentContentContract))

	declared, err := FindDeclaredExtension(wsRoot, contentConfig("/tools/contributor"), contentExtension)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !declared.Local || declared.Ref != "/tools/contributor" || declared.Dir != filepath.Join(wsRoot, "tools", "contributor") {
		t.Fatalf("declared = %+v", declared)
	}
	source, err := LocateAgentContent(wsRoot, contentConfig("/tools/contributor"), contentExtension)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if !source.Local || source.Contribution.Source != "agent-src" {
		t.Fatalf("source = %+v", source)
	}

	// One name reached through two entries is ambiguous, not a silent pick.
	if _, err := FindDeclaredExtension(wsRoot, contentConfig("/tools/contributor", contentExtension), contentExtension); err == nil ||
		!strings.Contains(err.Error(), "declared more than once") {
		t.Fatalf("error = %v, want an ambiguity refusal", err)
	}
	if _, err := FindDeclaredExtension(wsRoot, contentConfig(contentExtension), " "); err == nil {
		t.Fatal("an empty name must be refused")
	}
	if _, err := FindDeclaredExtension(wsRoot, nil, contentExtension); err == nil {
		t.Fatal("a workspace without a config declares nothing")
	}
}

func TestFindDeclaredExtension_ReportsALocalEntryThatCannotLoad(t *testing.T) {
	wsRoot := t.TempDir()
	dir := filepath.Join(wsRoot, "tools", "contributor")
	writeExtensionManifest(t, dir, fmt.Sprintf(
		`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		contentExtension, protocolcli.CurrentContract))
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"`+contentExtension+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := FindDeclaredExtension(wsRoot, contentConfig("/tools/contributor"), contentExtension)
	if err == nil || !strings.Contains(err.Error(), "cannot be loaded") {
		t.Fatalf("error = %v, want the load failure", err)
	}
}

// declarePackageExtension writes a root package.json declaring name in
// devDependencies, the way a workspace declares an npm extension.
func declarePackageExtension(t *testing.T, wsRoot, name, version string) {
	t.Helper()
	data := fmt.Sprintf(`{"name":"ws","private":true,"devDependencies":{%q:%q}}`, name, version)
	if err := os.WriteFile(filepath.Join(wsRoot, "package.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installPackageExtension writes what a package manager leaves in
// node_modules for an npm extension: its package.json and its manifest. It
// returns the package directory.
func installPackageExtension(t *testing.T, wsRoot, packageName, packageVersion, manifest string) string {
	t.Helper()
	dir := filepath.Join(wsRoot, "node_modules", filepath.FromSlash(packageName))
	writeExtensionManifest(t, dir, manifest)
	pkg := fmt.Sprintf(`{"name":%q,"version":%q}`, packageName, packageVersion)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// An npm extension's content is read from the directory discovery loads its
// commands and tools from, and bound to the package the package manager
// installed there.
func TestLocateAgentContent_ReadsAPackageFromWhereItsCommandsLoad(t *testing.T) {
	wsRoot := t.TempDir()
	cfg := contentConfig()
	declarePackageExtension(t, wsRoot, contentExtension, "^1.2.0")
	manifest := packagedContentManifest("1.2.0")
	dir := installPackageExtension(t, wsRoot, contentExtension, "1.2.0", manifest)

	declared, err := FindDeclaredExtension(wsRoot, cfg, contentExtension)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !declared.Package || declared.Local || declared.Dir != dir || !PackageExtensionInstalled(declared) {
		t.Fatalf("declared = %+v", declared)
	}
	source, err := LocateAgentContent(wsRoot, cfg, contentExtension)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if !source.Package || source.Local || source.Version != "1.2.0" || source.Root != dir {
		t.Fatalf("source = %+v", source)
	}
	if source.ManifestHash != lockfile.HashBytes([]byte(manifest)) || source.Contribution.ManifestSHA256 != contentDigest {
		t.Fatalf("source %+v is not bound to the installed manifest", source)
	}

	discovered, err := DiscoverExtensions(wsRoot, cfg, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var commandsRoot string
	for _, ext := range discovered {
		if ext.Name == contentExtension {
			commandsRoot = ext.Path
		}
	}
	if commandsRoot != source.Root {
		t.Fatalf("discovery loads %s from %q, but its content is read from %q", contentExtension, commandsRoot, source.Root)
	}
	if names := DeclaredExtensionNames(wsRoot, cfg); len(names) != 1 || names[0] != contentExtension {
		t.Fatalf("declared names = %v", names)
	}

	// A local entry of the same name shadows the devDependency in discovery,
	// so it is the one source.
	writeExtensionManifest(t, filepath.Join(wsRoot, "tools", "contributor"), fmt.Sprintf(
		`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		contentExtension, protocolcli.AgentContentContract))
	if declared, err := FindDeclaredExtension(wsRoot, contentConfig("/tools/contributor"), contentExtension); err != nil || !declared.Local {
		t.Fatalf("declared = %+v, err %v; want the local entry", declared, err)
	}
}

func TestLocateAgentContent_RefusesAPackageThatDoesNotBindItsContent(t *testing.T) {
	sourceOnly := fmt.Sprintf(`{"name":%q,"version":"1.2.0","cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		contentExtension, protocolcli.AgentContentContract)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, wsRoot string)
		cfg   *wsproto.Config
		want  string
	}{
		"a package the package manager has not installed": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, contentExtension, "1.2.0")
			},
			cfg:  contentConfig(),
			want: "has not installed it",
		},
		"another package installed under the name": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, contentExtension, "npm:@acme/other@1.2.0")
				dir := installPackageExtension(t, wsRoot, contentExtension, "1.2.0", packagedContentManifest("1.2.0"))
				if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"@acme/other","version":"1.2.0"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			cfg:  contentConfig(),
			want: `holds package "@acme/other"`,
		},
		"a manifest version other than the package version": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, contentExtension, "1.3.0")
				installPackageExtension(t, wsRoot, contentExtension, "1.3.0", packagedContentManifest("1.2.0"))
			},
			cfg:  contentConfig(),
			want: "carries its package version",
		},
		"a package that ships only its source": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, contentExtension, "1.2.0")
				installPackageExtension(t, wsRoot, contentExtension, "1.2.0", sourceOnly)
			},
			cfg:  contentConfig(),
			want: "declare its directory",
		},
		"a package that ships another extension's manifest": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, contentExtension, "1.2.0")
				installPackageExtension(t, wsRoot, contentExtension, "1.2.0",
					strings.Replace(packagedContentManifest("1.2.0"), contentExtension, "@acme/other", 1))
			},
			cfg:  contentConfig(),
			want: "published under the extension's own name",
		},
		"a registry entry the package manager also declares": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))
				declarePackageExtension(t, wsRoot, contentExtension, "1.2.0")
			},
			cfg:  contentConfig(contentExtension),
			want: "declared twice",
		},
		"a registry entry node_modules also provides": {
			setup: func(t *testing.T, wsRoot string) {
				installContentExtension(t, wsRoot, "1.2.0", packagedContentManifest("1.2.0"))
				installPackageExtension(t, wsRoot, contentExtension, "1.3.0", packagedContentManifest("1.3.0"))
			},
			cfg:  contentConfig(contentExtension),
			want: "also provides it",
		},
		"a devDependency that is not a package name": {
			setup: func(t *testing.T, wsRoot string) {
				declarePackageExtension(t, wsRoot, "../"+contentExtension, "1.2.0")
			},
			cfg:  contentConfig(),
			want: "declares no extension",
		},
	} {
		t.Run(name, func(t *testing.T) {
			wsRoot := t.TempDir()
			tc.setup(t, wsRoot)
			lookup := contentExtension
			if strings.HasPrefix(name, "a devDependency that is not") {
				lookup = "../" + contentExtension
			}
			source, err := LocateAgentContent(wsRoot, tc.cfg, lookup)
			if err == nil || source != nil {
				t.Fatalf("located %+v, want a refusal", source)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
