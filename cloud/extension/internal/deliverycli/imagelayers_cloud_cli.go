package deliverycli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// imageLayersCloudCLIExtension is the extension whose artifact carries the
// Cloud CLI binary the runner image bakes as its helper.
const imageLayersCloudCLIExtension = "@putnami/cloud"

// imageLayersCloudCLIBinary is where the @putnami/cloud archive ships its
// binary for the platform it was built for.
const imageLayersCloudCLIBinary = "compiled/putnami-cloud"

// imageLayersCloudCLIEntrypoint is the main package every build of the Cloud
// CLI names, whichever repository and module publish it.
const imageLayersCloudCLIEntrypoint = "/cmd/putnami-cloud"

// imageLayersCloudCLISettings are the build settings the helper must carry:
// the image is linux/amd64 and has no C runtime the binary could link.
var imageLayersCloudCLISettings = []debug.BuildSetting{
	{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"},
	{Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"},
}

// imageLayersCloudCLIArtifact resolves the lock-pinned @putnami/cloud artifact.
// Produce and check both read it, so the layer identity comes from committed
// files and `--check` materializes nothing.
func imageLayersCloudCLIArtifact(workspaceRoot string) (imageLayersWarmArtifact, error) {
	lockPath, lock, err := imageLayersWarmReadLock(workspaceRoot)
	if err != nil {
		return imageLayersWarmArtifact{}, err
	}
	return imageLayersWarmResolveOne(lockPath, lock, imageLayersCloudCLIExtension)
}

// imageLayersCloudCLILabel is the layer identity: the extension version and
// the linux/amd64 integrity the store is content-addressed by. A republished
// artifact under an unchanged version reads as stale.
func imageLayersCloudCLILabel(artifact imageLayersWarmArtifact) string {
	return imageLayersWarmLabel([]imageLayersWarmArtifact{artifact})
}

// imageLayersVerifyCloudCLI checks the restored binary without executing it.
// The lock integrity already proves which artifact supplied the bytes; this
// proves the artifact shipped a linux/amd64, CGO-free, trimmed build of the
// Cloud CLI entrypoint. The module path and the Go version belong to the
// build that publishes the extension, so they are not checked.
func imageLayersVerifyCloudCLI(info *debug.BuildInfo) error {
	if info == nil || info.Main.Path == "" || info.Path != info.Main.Path+imageLayersCloudCLIEntrypoint {
		return fmt.Errorf("the %s artifact's %s is not a build of a %s entrypoint",
			imageLayersCloudCLIExtension, imageLayersCloudCLIBinary, imageLayersCloudCLIEntrypoint)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for _, want := range imageLayersCloudCLISettings {
		if settings[want.Key] != want.Value {
			return fmt.Errorf("the %s artifact's %s requires %s=%s, got %q",
				imageLayersCloudCLIExtension, imageLayersCloudCLIBinary, want.Key, want.Value, settings[want.Key])
		}
	}
	return nil
}

// imageLayersProduceCloudCLI restores the Cloud CLI binary from the
// lock-pinned @putnami/cloud artifact. It reads the same verified store the
// warm layer bakes, so the image downloads that artifact once, and the helper
// is the CLI the workspace pins rather than one this checkout built.
func imageLayersProduceCloudCLI(ctx context.Context, spec imageLayerSpec, _, outDir string, warm *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}
	store, artifacts, err := warm.open(ctx, ioctx)
	if err != nil {
		return imageLayerRecord{}, err
	}
	var source imageLayersWarmArtifact
	for _, artifact := range artifacts {
		if artifact.name == imageLayersCloudCLIExtension {
			source = artifact
		}
	}
	if source.name == "" {
		return imageLayerRecord{}, fmt.Errorf("no warmed extension is %s — putnami.lock.json must pin it for the %s layer",
			imageLayersCloudCLIExtension, spec.Name)
	}
	restored := filepath.Join(imageLayersWarmArtifactDir(store, source), filepath.FromSlash(imageLayersCloudCLIBinary))
	ioctx.Stdout(fmt.Sprintf("Restoring %s for %s from %s %s …", spec.Name, imageLayersWarmPlatform, source.name, source.version))

	info, err := imageLayersReadBuildInfo(restored)
	if errors.Is(err, os.ErrNotExist) {
		return imageLayerRecord{}, fmt.Errorf("the materialized %s %s carries no %s", source.name, source.version, imageLayersCloudCLIBinary)
	}
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("read the build info of the restored %s binary: %w", spec.Name, err)
	}
	if err := imageLayersVerifyCloudCLI(info); err != nil {
		return imageLayerRecord{}, err
	}

	// The store is lent, not owned: copy into this producer's own scratch so
	// the warm layer still carries the artifact intact.
	work, err := os.MkdirTemp(outDir, ".work-")
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("create a scratch directory under %s: %w", outDir, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	staged := filepath.Join(work, spec.Name)
	if err := imageLayersCopyFile(restored, staged); err != nil {
		return imageLayerRecord{}, fmt.Errorf("stage the restored %s binary: %w", spec.Name, err)
	}
	sum, size, err := imageLayersInstallFile(staged, outDir, spec.Name)
	if err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name: spec.Name, Producer: spec.Producer, Path: "/" + prefix,
		File: imageLayersBinDir + "/" + spec.Name, SHA256: sum, Size: size,
		Version: imageLayersCloudCLILabel(source),
		Source: imageLayerSource{
			Package: info.Path, Module: info.Main.Path, Toolchain: info.GoVersion,
			Extensions: []imageLayerExtension{{Name: source.name, Version: source.version, SHA256: source.sha256}},
		},
	}, nil
}
