package pkg

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/typescript/extension/internal/errs"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

func writeNodeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheBaseFollowsTheDeclaredNodeMajor(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-docker-base", "the-base-follows-the-declared-node-major")
	cases := []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"engines", map[string]string{"app/package.json": `{"engines":{"node":">=20.11"}}`, "app/.nvmrc": "24"}, 20},
		{"project nvmrc", map[string]string{"app/package.json": `{}`, "app/.nvmrc": "v24.1.0\n", ".nvmrc": "20"}, 24},
		{"workspace node-version", map[string]string{"app/package.json": `{}`, ".node-version": "22.11.0"}, 22},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeNodeTree(t, root, c.files)
			got, err := NodeBaseImage(filepath.Join(root, "app"), root)
			if err != nil {
				t.Fatal(err)
			}
			if got != nodeBaseImages[c.want] {
				t.Fatalf("base = %s, want the Node %d pin %s", got, c.want, nodeBaseImages[c.want])
			}
		})
	}
	for major, ref := range nodeBaseImages {
		if !strings.Contains(ref, "/nodejs"+strconv.Itoa(major)+"-") || !strings.Contains(ref, ":nonroot@sha256:") {
			t.Errorf("the Node %d base %s is not a digest-pinned nonroot nodejs%d image", major, ref, major)
		}
	}
}

func TestNoDeclaredMajorFailsNamingTheFields(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-docker-base", "no-declared-major-fails-naming-the-fields")
	root := t.TempDir()
	writeNodeTree(t, root, map[string]string{"app/package.json": `{"engines":{}}`})
	_, err := NodeBaseImage(filepath.Join(root, "app"), root)
	if err == nil {
		t.Fatal("a project that declares no Node version got a base")
	}
	if errs.CodeOf(err) != errs.CodeNodeVersionUndeclared {
		t.Errorf("code = %q", errs.CodeOf(err))
	}
	for _, field := range []string{"engines.node", ".nvmrc", ".node-version"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name %s", err, field)
		}
	}
}

func TestAnUnpinnedMajorFailsNamingThePinnedMajors(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-docker-base", "an-unpinned-major-fails-naming-the-pinned-majors")
	root := t.TempDir()
	writeNodeTree(t, root, map[string]string{"app/package.json": `{"engines":{"node":"^18"}}`})
	_, err := NodeBaseImage(filepath.Join(root, "app"), root)
	if err == nil {
		t.Fatal("Node 18 got a base")
	}
	if errs.CodeOf(err) != errs.CodeNodeVersionUndeclared || !strings.Contains(err.Error(), "Node 20, 22 or 24") {
		t.Fatalf("error = %q (%s), want one naming Node 20, 22 or 24", err, errs.CodeOf(err))
	}

	writeNodeTree(t, root, map[string]string{"app/package.json": `{}`, "app/.nvmrc": "lts/iron"})
	if _, err := NodeBaseImage(filepath.Join(root, "app"), root); err == nil || !strings.Contains(err.Error(), "names no major") {
		t.Fatalf("an alias without a major: %v", err)
	}
}

// nodeFixture is an installed project: express hoisted with its debug, a
// second debug nested under express where the hoisted one is another version,
// a peer, an absent optional dependency, a workspace link whose own
// dependency ships, and a devDependency.
func nodeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeNodeTree(t, root, map[string]string{
		"app/package.json":                                     `{"dependencies":{"express":"4","debug":"4","@acme/lib":"workspace:*"},"optionalDependencies":{"fsevents":"2"},"devDependencies":{"typescript":"5"}}`,
		"node_modules/express/package.json":                    `{"dependencies":{"debug":"2","ms":"2"},"peerDependencies":{"absent-peer":"1"}}`,
		"node_modules/express/index.js":                        "module.exports = 1\n",
		"node_modules/express/node_modules/debug/package.json": `{"version":"2.6.9","dependencies":{"ms":"2.0.0"}}`,
		"node_modules/express/node_modules/ms/package.json":    `{"version":"2.0.0"}`,
		"node_modules/debug/package.json":                      `{"version":"4.3.4","dependencies":{"ms":"2.1.2"}}`,
		"node_modules/ms/package.json":                         `{"version":"2.1.2"}`,
		"node_modules/typescript/package.json":                 `{"version":"5"}`,
		"libs/lib/package.json":                                `{"name":"@acme/lib","dependencies":{"pg":"8"}}`,
		"node_modules/pg/package.json":                         `{"version":"8.13.0"}`,
	})
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "libs", "lib"), filepath.Join(root, "node_modules", "@acme", "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestTheImageShipsTheBundleAndTheProductionDependencies(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-docker-image", "the-image-ships-the-bundle-and-the-production-dependencies")
	root := nodeFixture(t)
	closure, err := nodeDependencyClosure(filepath.Join(root, "app"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range closure {
		paths = append(paths, p.Path)
	}
	want := []string{
		"node_modules/debug",
		"node_modules/express",
		"node_modules/express/node_modules/debug",
		"node_modules/express/node_modules/ms",
		"node_modules/ms",
		"node_modules/pg",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("closure = %v, want %v", paths, want)
	}

	compile := filepath.Join(root, "out", "compile")
	writeNodeTree(t, compile, map[string]string{
		"src/main.js":       "import express from \"express\";\n",
		"src/main.js.map":   "{}",
		"package.json":      `{"type":"module"}`,
		"putnami-node.json": `{"entrypoint":"src/main.js"}`,
		contentStampFile:    `{}`,
	})
	spec, err := nodeDockerSpec(nodeBaseImages[22], "linux/amd64", filepath.Join(root, "app"), compile, filepath.Join(compile, contentStampFile), 8080, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spec.Entrypoint, " "); got != "/nodejs/bin/node --enable-source-maps /app/src/main.js" {
		t.Errorf("entrypoint = %q", got)
	}
	if spec.BaseRef != nodeBaseImages[22] || spec.WorkingDir != "/app" || !reflect.DeepEqual(spec.ExposedPorts, []string{"8080/tcp"}) {
		t.Errorf("spec = %+v", spec)
	}
	var shipped []string
	for _, layer := range spec.Layers {
		for _, file := range layer.Files {
			shipped = append(shipped, file.Path)
		}
	}
	sort.Strings(shipped)
	wantFiles := []string{
		"/app/.gen/version.json",
		"/app/node_modules/debug/package.json",
		"/app/node_modules/express/index.js",
		"/app/node_modules/express/node_modules/debug/package.json",
		"/app/node_modules/express/node_modules/ms/package.json",
		"/app/node_modules/express/package.json",
		"/app/node_modules/ms/package.json",
		"/app/node_modules/pg/package.json",
		"/app/package.json",
		"/app/putnami-node.json",
		"/app/src/main.js",
		"/app/src/main.js.map",
	}
	if !reflect.DeepEqual(shipped, wantFiles) {
		t.Fatalf("image files =\n%s\nwant\n%s", strings.Join(shipped, "\n"), strings.Join(wantFiles, "\n"))
	}
}

func TestAMissingDependencyFailsNamingPutnamiInstall(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-docker-image", "a-missing-dependency-fails-naming-putnami-install")
	root := nodeFixture(t)
	if err := os.RemoveAll(filepath.Join(root, "node_modules", "ms")); err != nil {
		t.Fatal(err)
	}
	_, err := nodeDependencyClosure(filepath.Join(root, "app"))
	if err == nil {
		t.Fatal("a closure with a dependency that is not installed succeeded")
	}
	if errs.CodeOf(err) != errs.CodeDependencyNotInstalled || !strings.Contains(err.Error(), "ms is not installed") || !strings.Contains(err.Error(), "putnami install") {
		t.Fatalf("error = %q (%s)", err, errs.CodeOf(err))
	}
}
