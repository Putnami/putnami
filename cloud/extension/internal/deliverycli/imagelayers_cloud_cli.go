package deliverycli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const imageLayersCloudCLIModule = "go.putnami.dev/cloud/extension"

// The runner image already depends on the Cloud CLI. Consume its ordinary
// prerequisite output rather than compiling a second copy inside the producer.
// build.platforms includes linux/amd64 on every host; archive release selection
// remains independent. Hashing these exact bytes also makes --check reject a
// layer left over from a different prerequisite build.
func imageLayersCloudCLI(workspaceRoot string) (string, *debug.BuildInfo, string, error) {
	binary := filepath.Join(workspaceRoot, ".putnami/out/cloud/extension/build/bin/linux-x64/putnami-cloud")
	info, err := imageLayersReadBuildInfo(binary)
	if err != nil {
		return "", nil, "", fmt.Errorf("read Cloud CLI prerequisite: %w; run putnami build --projects cloud/extension", err)
	}
	goVersion, err := imageBuildGoWorkVersion(workspaceRoot)
	if err != nil {
		return "", nil, "", err
	}
	if info == nil || info.Main.Path != imageLayersCloudCLIModule || info.Path != imageLayersCloudCLIModule+"/cmd/putnami-cloud" || info.GoVersion != "go"+goVersion {
		return "", nil, "", fmt.Errorf("cloud CLI prerequisite must be the workspace entrypoint built with go%s", goVersion)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "-trimpath": "true"} {
		if settings[key] != want {
			return "", nil, "", fmt.Errorf("cloud CLI prerequisite requires %s=%s, got %q", key, want, settings[key])
		}
	}
	sum, _, err := imageLayersHashFile(binary)
	return binary, info, sum, err
}

func imageLayersProduceCloudCLI(_ context.Context, spec imageLayerSpec, workspaceRoot, outDir string, _ *imageLayersWarmStore, _ clicore.IO) (imageLayerRecord, error) {
	binary, info, expected, err := imageLayersCloudCLI(workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return imageLayerRecord{}, err
	}
	source, err := os.Open(binary) //nolint:gosec // G304: fixed prerequisite path under the workspace
	if err != nil {
		return imageLayerRecord{}, err
	}
	defer func() { _ = source.Close() }()
	staged, err := os.CreateTemp(outDir, ".cloud-cli-*")
	if err != nil {
		return imageLayerRecord{}, err
	}
	defer func() { _ = os.Remove(staged.Name()) }()
	_, copyErr := io.Copy(staged, source)
	closeErr := staged.Close()
	if copyErr != nil {
		return imageLayerRecord{}, copyErr
	}
	if closeErr != nil {
		return imageLayerRecord{}, closeErr
	}
	sum, size, err := imageLayersHashFile(staged.Name())
	if err != nil {
		return imageLayerRecord{}, err
	}
	if sum != expected {
		return imageLayerRecord{}, fmt.Errorf("cloud CLI prerequisite changed during layer production")
	}
	if _, _, err := imageLayersInstallFile(staged.Name(), outDir, spec.Name); err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name: spec.Name, Producer: spec.Producer, Path: spec.Path,
		File: imageLayersBinDir + "/" + spec.Name, SHA256: sum, Size: size,
		Version: "workspace:" + sum,
		Source:  imageLayerSource{Package: info.Path, Module: info.Main.Path, Toolchain: info.GoVersion},
	}, nil
}
