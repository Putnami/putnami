package deliverycli

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func imageLayersCloudCLIFixture(t *testing.T) (string, string, *debug.BuildInfo) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.work"), []byte("go 1.26.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(workspace, ".putnami/out/cloud/extension/build/bin/linux-x64/putnami-cloud")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("workspace CLI prerequisite bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := &debug.BuildInfo{
		GoVersion: "go1.26.1", Path: imageLayersCloudCLIModule + "/cmd/putnami-cloud",
		Main:     debug.Module{Path: imageLayersCloudCLIModule, Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"}, {Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"}},
	}
	previous := imageLayersReadBuildInfo
	t.Cleanup(func() { imageLayersReadBuildInfo = previous })
	imageLayersReadBuildInfo = func(path string) (*debug.BuildInfo, error) {
		if path != binary {
			t.Fatalf("producer selected wrong prerequisite: %s", path)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		return info, nil
	}
	return workspace, binary, info
}

func TestImageLayersCloudCLIUsesVerifiedPrerequisiteWithoutMovingIt(t *testing.T) {
	workspace, binary, _ := imageLayersCloudCLIFixture(t)
	spec := imageLayerSpec{Name: "putnami-ci-next", Producer: "cloud-cli", Path: "/usr/local/libexec/putnami-ci-next"}
	out := filepath.Join(workspace, "layers")
	record, err := imageLayersProduceCloudCLI(context.Background(), spec, workspace, out, nil, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("producer moved or removed the prerequisite: %v", err)
	}
	installed := filepath.Join(out, record.File)
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != string(original) || record.Version != "workspace:"+record.SHA256 {
		t.Fatalf("layer identity/content mismatch: %+v err=%v", record, err)
	}
	if info, err := os.Stat(installed); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("helper is not executable: %v, %v", info, err)
	}
	if err := imageLayersWriteRecord(out, record); err != nil {
		t.Fatal(err)
	}
	if _, problem := imageLayersVerifyLayer(spec, workspace, out); problem != "" {
		t.Fatal(problem)
	}
	if err := os.WriteFile(binary, []byte("newly built CLI"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, problem := imageLayersVerifyLayer(spec, workspace, out); !strings.Contains(problem, "workspace now declares") {
		t.Fatalf("packaging accepted a helper from the prior prerequisite build: %s", problem)
	}
}

func TestImageLayersCloudCLIRefusesWrongOrMissingPrerequisite(t *testing.T) {
	for _, mutation := range []string{"missing", "toolchain", "module", "entrypoint", "GOOS", "GOARCH", "CGO_ENABLED", "-trimpath"} {
		t.Run(mutation, func(t *testing.T) {
			workspace, binary, info := imageLayersCloudCLIFixture(t)
			switch mutation {
			case "missing":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
			case "toolchain":
				info.GoVersion = "go1.25.0"
			case "module":
				info.Main.Path = "other/module"
			case "entrypoint":
				info.Path = imageLayersCloudCLIModule + "/cmd/other"
			default:
				for i := range info.Settings {
					if info.Settings[i].Key == mutation {
						info.Settings[i].Value = "wrong"
					}
				}
			}
			if _, _, _, err := imageLayersCloudCLI(workspace); err == nil {
				t.Fatalf("accepted %s prerequisite", mutation)
			}
		})
	}
}
