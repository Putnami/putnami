package deliverycli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const imageLayersCloudCLILayer = `{"name":"putnami-ci-next","producer":"cloud-cli","path":"/usr/local/libexec/putnami-ci-next"}`

var warmCloudDigest = strings.Repeat("3c", 32)

// imageLayersCloudCLIWorkspace pins @putnami/go and @putnami/cloud, and stages
// a store whose @putnami/cloud artifact ships the given binary.
func imageLayersCloudCLIWorkspace(t *testing.T, binary string) (string, *fakeWarmInstall) {
	t.Helper()
	installGoBuild(t, &fakeGoBuild{})
	ws := imageLayersToolWorkspaceBase(t)
	imageLayersWarmLock(t, ws, map[string]map[string]string{
		"@putnami/go":    {"version": "0.1.0-8885222db", "linux/amd64": warmGoDigest},
		"@putnami/cloud": {"version": "0.0.0-20261012-cloud", "linux/amd64": warmCloudDigest},
	})
	tree := warmFixtureStore()
	delete(tree, warmTSDigest)
	tree[warmCloudDigest] = map[string]string{"putnami.extension.json": `{"name":"@putnami/cloud"}`}
	if binary != "" {
		tree[warmCloudDigest][imageLayersCloudCLIBinary] = binary
	}
	fake := &fakeWarmInstall{tree: tree}
	installWarmRun(t, fake)
	return ws, fake
}

func cloudCLIBinary(module string, settings ...string) string {
	return fakeToolBinary("go1.26.9", module, "(devel)", module+"/cmd/putnami-cloud", settings...)
}

// The helper comes from the pinned @putnami/cloud artifact, out of the one
// store the warm layer bakes, whichever repository built that artifact.
func TestImageLayersCloudCLIRestoresThePinnedExtensionBinary(t *testing.T) {
	for _, module := range []string{"go.putnami.dev/cloud/extension", "go.putnami.cloud/cloud/workloads/putnami-cloud"} {
		t.Run(module, func(t *testing.T) {
			binary := cloudCLIBinary(module)
			ws, fake := imageLayersCloudCLIWorkspace(t, binary)
			project := imageLayersProject(t, `{"layers":[`+imageLayersCloudCLILayer+`,`+imageLayersWarmLayer+`]}`)
			if err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string))); err != nil {
				t.Fatalf("cloud image-layers: %v", err)
			}
			if len(fake.stores) != 1 {
				t.Fatalf("the extension set was materialized %d time(s), want once", len(fake.stores))
			}
			installed := filepath.Join(project, ".gen", "layers", "bin", "putnami-ci-next")
			data, err := os.ReadFile(installed) //nolint:gosec // G304: a path this test just produced
			if err != nil || string(data) != binary {
				t.Fatalf("helper = %q (%v), want the artifact's %s", data, err, imageLayersCloudCLIBinary)
			}
			if info, err := os.Stat(installed); err != nil || info.Mode().Perm() != imageLayersToolFileMode {
				t.Fatalf("helper mode = %v (%v), want %v", info, err, imageLayersToolFileMode)
			}
			// The store is lent, not consumed: the warm layer still carries the
			// artifact the helper was copied from.
			tar, err := os.ReadFile(filepath.Join(project, ".gen", "layers", "putnami-warm.tar")) //nolint:gosec // G304: a path this test just produced
			if err != nil {
				t.Fatalf("read the warm layer: %v", err)
			}
			kept := "root/.putnami/artifacts/sha256/" + warmCloudDigest[:2] + "/" + warmCloudDigest + "/" + imageLayersCloudCLIBinary
			if !slices.Contains(headerNames(readTarEntries(t, tar)), kept) {
				t.Fatalf("the warm layer lost %s to the restore", kept)
			}
			record := imageLayersReadRecord(t, project, "putnami-ci-next")
			want := "@putnami/cloud 0.0.0-20261012-cloud " + warmCloudDigest
			if record.Version != want || record.Source.Module != module || record.SHA256 != sha256Hex(data) {
				t.Fatalf("record = %+v, want version %q from module %s", record, want, module)
			}
			if len(record.Source.Extensions) != 1 || record.Source.Extensions[0].SHA256 != warmCloudDigest {
				t.Fatalf("record names %+v, want the @putnami/cloud artifact", record.Source.Extensions)
			}

			spec := imageLayerSpec{Name: "putnami-ci-next", Producer: "cloud-cli", Path: "/usr/local/libexec/putnami-ci-next"}
			out := filepath.Join(project, ".gen", "layers")
			if _, problem := imageLayersVerifyLayer(spec, ws, out); problem != "" {
				t.Fatal(problem)
			}
			imageLayersWarmLock(t, ws, map[string]map[string]string{
				"@putnami/go":    {"version": "0.1.0-8885222db", "linux/amd64": warmGoDigest},
				"@putnami/cloud": {"version": "0.0.0-20261012-cloud", "linux/amd64": strings.Repeat("4d", 32)},
			})
			if _, problem := imageLayersVerifyLayer(spec, ws, out); !strings.Contains(problem, "workspace now declares") {
				t.Fatalf("packaging accepted a helper from a republished artifact: %q", problem)
			}
		})
	}
}

func TestImageLayersCloudCLIRefusesAWrongArtifact(t *testing.T) {
	module := "go.putnami.dev/cloud/extension"
	cases := map[string]struct {
		binary string
		want   string
	}{
		"missing":     {"", "carries no compiled/putnami-cloud"},
		"entrypoint":  {fakeToolBinary("go1.26.9", module, "(devel)", module+"/cmd/other"), "is not a build of a /cmd/putnami-cloud entrypoint"},
		"GOOS":        {cloudCLIBinary(module, "GOOS", "darwin"), "GOOS=linux"},
		"GOARCH":      {cloudCLIBinary(module, "GOARCH", "arm64"), "GOARCH=amd64"},
		"CGO_ENABLED": {cloudCLIBinary(module, "CGO_ENABLED", "1"), "CGO_ENABLED=0"},
		"-trimpath":   {cloudCLIBinary(module, "-trimpath", "false"), "-trimpath=true"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws, _ := imageLayersCloudCLIWorkspace(t, tc.binary)
			project := imageLayersProject(t, `{"layers":[`+imageLayersCloudCLILayer+`]}`)
			err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(project, ".gen", "layers", "bin", "putnami-ci-next")); !os.IsNotExist(err) {
				t.Fatalf("a refused artifact became a layer: %v", err)
			}
		})
	}
}

// A lock that does not pin @putnami/cloud has no helper to restore, and
// `--check` says so without materializing anything.
func TestImageLayersCloudCLIRequiresTheLockPin(t *testing.T) {
	ws := imageLayersWarmWorkspaceFixture(t)
	fake := &fakeWarmInstall{tree: warmFixtureStore()}
	installWarmRun(t, fake)
	project := imageLayersProject(t, `{"layers":[`+imageLayersCloudCLILayer+`]}`)
	err := ImageLayers(map[string]any{"project": project}, nil, ws, nil, imageLayersIO(new([]string)))
	if err == nil || !strings.Contains(err.Error(), "no warmed extension is @putnami/cloud") {
		t.Fatalf("error = %v, want a refusal naming the missing pin", err)
	}
	spec := imageLayerSpec{Name: "putnami-ci-next", Producer: "cloud-cli", Path: "/usr/local/libexec/putnami-ci-next"}
	if _, err := imageLayersDeclaredVersion(spec, ws); err == nil || !strings.Contains(err.Error(), `pins no extensions["@putnami/cloud"].version`) {
		t.Fatalf("check error = %v, want the missing lock pin", err)
	}
}
