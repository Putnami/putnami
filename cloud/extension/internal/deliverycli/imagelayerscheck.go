package deliverycli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// putnami cloud image-layers --check — the packaging precondition.
//
// WHY IT EXISTS. A `type: "image"` project's content key is a SHA-256 over the
// whole spec INCLUDING the layer bytes, so `putnami package` faithfully
// publishes whatever sits under `.gen/layers` — including a set produced before
// the workspace repinned Go, bun, node or the lint tools. Nothing in the
// packaging step can notice: stale layers hash to a perfectly valid content key
// for an image that no longer describes this checkout.
//
// The framework's image packaging step belongs to @putnami/go and declares no
// dependency this extension can hook, so the producer cannot be ordered before
// it inside one `package` pipeline. What CAN be guaranteed is that packaging
// never SUCCEEDS against stale layers: this check runs as a step of the same
// `package` command and reads the same static tree, so a produced set that no
// longer matches the workspace's pins fails the command that would have shipped
// it. The repair is one line, and the diagnostic prints it.
//
// It is deliberately execution-free and network-free: every fact it needs is
// either in the layer's own record sidecar or in the workspace file that
// declares the pin, so it runs on any host, in any worktree, in well under a
// second per layer.
//
// The worker's `dockerBaseProject` graph edge packages this image before the
// consumer. That makes this check the single freshness gate for both the runner
// artifact and every workload package that consumes it.

// imageLayersCheckParam is the flag that selects this mode on the image-layers
// verb. It is a mode rather than a separate verb because it shares the manifest
// reader, the output layout and the version derivation with the producer — two
// verbs would be two places for that agreement to drift.
const imageLayersCheckParam = "check"

// imageLayersLayerState is one declared layer that IS current: the identity it
// was produced under and where the image installs it.
type imageLayersLayerState struct {
	Name    string
	Version string
	Path    string
}

// imageLayersInspection is what one read of a produced layer set found: the
// layers that are current, and one sentence per layer that is not.
type imageLayersInspection struct {
	OutDir   string
	Layers   []imageLayersLayerState
	Problems []string
}

// imageLayersInspect answers "was this project's layer set produced from what
// the checkout declares TODAY" for one image project. It is the single
// freshness derivation behind `--check`, the packaging precondition. Keeping the
// inspection separate from rendering lets tests exercise the exact facts the
// package step trusts without reproducing the derivation.
//
// A non-nil error means the project could not be read at all (no manifest, bad
// JSON); a stale or missing layer set is Problems, not an error, so each caller
// can phrase the refusal in its own vocabulary.
func imageLayersInspect(dir, workspaceRoot string) (imageLayersInspection, error) {
	manifest, err := imageLayersReadManifest(dir)
	if err != nil {
		return imageLayersInspection{}, err
	}
	inspection := imageLayersInspection{
		OutDir:   imageLayersOutputDir(dir),
		Layers:   make([]imageLayersLayerState, 0, len(manifest.Layers)),
		Problems: make([]string, 0, len(manifest.Layers)),
	}
	for _, spec := range manifest.Layers {
		version, problem := imageLayersVerifyLayer(spec, workspaceRoot, inspection.OutDir)
		if problem != "" {
			inspection.Problems = append(inspection.Problems, problem)
			continue
		}
		inspection.Layers = append(inspection.Layers, imageLayersLayerState{Name: spec.Name, Version: version, Path: spec.Path})
	}
	return inspection, nil
}

// imageLayersVerify reports whether every layer the project declares has been
// produced from the versions the workspace declares TODAY, and that the
// produced bytes are still the bytes the producer recorded.
func imageLayersVerify(params map[string]any, workspaceRoot string, ioctx clicore.IO) error {
	dir, err := imageProjectRoot(params, workspaceRoot, "project", "context")
	if err != nil {
		return clicore.NewError("cloud image-layers --check: resolve project: "+err.Error(), clicore.ExitUsage)
	}
	inspection, err := imageLayersInspect(dir, workspaceRoot)
	if err != nil {
		return err
	}
	if len(inspection.Problems) > 0 {
		return clicore.NewError(fmt.Sprintf(
			"cloud image-layers --check: %s does not carry the layers this workspace declares:\n  - %s\n"+
				"Produce them before packaging:\n  putnami cloud image-layers --project %s\n"+
				"(the image's content key is a hash of the layer BYTES, so packaging a stale set publishes a "+
				"valid content tag for an image that no longer describes this checkout)",
			inspection.OutDir, strings.Join(inspection.Problems, "\n  - "),
			imageLayersProjectLabel(workspaceRoot, dir)), clicore.ExitFailure)
	}

	layers := make([]map[string]any, 0, len(inspection.Layers))
	names := make([]string, 0, len(inspection.Layers))
	for _, layer := range inspection.Layers {
		layers = append(layers, map[string]any{"name": layer.Name, "version": layer.Version, "path": layer.Path})
		names = append(names, fmt.Sprintf("%s (%s)", layer.Name, layer.Version))
	}
	out := map[string]any{"status": "current", "project": dir, "outputDir": inspection.OutDir, "layers": layers}
	clicore.WriteResult(out, params, ioctx,
		fmt.Sprintf("All %d declared layer(s) in %s were produced from this workspace's pins: %s.",
			len(layers), inspection.OutDir, strings.Join(names, ", ")))
	return nil
}

// imageLayersVerifyLayer checks one layer and returns its resolved version, or
// the single sentence that says what is wrong with it. Every check is stated as
// what the reader must DO, because this diagnostic is the whole user interface
// of the precondition.
func imageLayersVerifyLayer(spec imageLayerSpec, workspaceRoot, outDir string) (string, string) {
	record, err := imageLayersLoadRecord(outDir, spec.Name)
	if err != nil {
		return "", fmt.Sprintf("%s: %s", spec.Name, err.Error())
	}
	if record.Path != spec.Path {
		return "", fmt.Sprintf("%s: produced for %s, but the manifest now installs it at %s",
			spec.Name, record.Path, spec.Path)
	}
	artifact := record.artifact()
	if artifact == "" {
		return "", fmt.Sprintf("%s: its record names no produced file", spec.Name)
	}
	path := filepath.Join(outDir, filepath.FromSlash(artifact))
	sum, size, err := imageLayersHashFile(path)
	if err != nil {
		return "", fmt.Sprintf("%s: %s", spec.Name, err.Error())
	}
	if sum != record.SHA256 || size != record.Size {
		return "", fmt.Sprintf("%s: %s hashes to %s (%d bytes) but its record claims %s (%d bytes) — the file changed under the record",
			spec.Name, artifact, sum, size, record.SHA256, record.Size)
	}
	declared, err := imageLayersDeclaredVersion(spec, workspaceRoot)
	if err != nil {
		return "", fmt.Sprintf("%s: %s", spec.Name, err.Error())
	}
	if declared != record.Version {
		return "", fmt.Sprintf("%s: produced from %s, but the workspace now declares %s",
			spec.Name, record.Version, declared)
	}
	return declared, ""
}

// imageLayersDeclaredVersion resolves the IDENTITY a layer WOULD be produced
// under right now, through the same label function the producer records with —
// never a second derivation of its own, or the check and the producer could
// disagree about what "current" means.
//
// "Identity", not "version": a label states every input that SELECTS the
// artifact, because several of them live in image-layers.json and move without
// any version string moving (uv's digest is the pin; a tag template, an image
// repository, a go-tool's package). Comparing versions alone left those edits
// reading as current and packaging reusing the previous bytes.
func imageLayersDeclaredVersion(spec imageLayerSpec, workspaceRoot string) (string, error) {
	switch spec.Producer {
	case "go-toolchain":
		// The whole layer is the toolchain go.work names, so the version IS the
		// identity — there is no second manifest field to fold in.
		version, err := imageBuildGoWorkVersion(workspaceRoot)
		if err != nil {
			return "", err
		}
		if version == "" {
			return "", fmt.Errorf("go.work under %q declares no Go version", workspaceRoot)
		}
		return version, nil
	case "go-tool":
		version, err := imageLayersToolVersion(spec, workspaceRoot)
		if err != nil {
			return "", err
		}
		// A DERIVED tool is restored from the lock-pinned @putnami/go artifact
		// rather than compiled, so the artifact that supplied the
		// bytes is part of what selected them: the framework can republish the
		// same tool pin built with another Go. Both halves come from committed
		// files, so this stays the execution-free, network-free read it was.
		artifact, err := imageLayersGoToolArtifact(spec, workspaceRoot)
		if err != nil {
			return "", err
		}
		return imageLayersGoToolLabel(spec, version, artifact), nil
	case "oci-file":
		version, err := imageLayersOCIVersion(spec, workspaceRoot)
		if err != nil {
			return "", err
		}
		return imageLayersOCILabel(spec, version), nil
	case "putnami-cli":
		version, integrity, err := imageLayersLockCLI(workspaceRoot)
		if err != nil {
			return "", err
		}
		return imageLayersCLILabel(version, integrity), nil
	case "cloud-cli":
		_, _, sum, err := imageLayersCloudCLI(workspaceRoot)
		return "workspace:" + sum, err
	case "putnami-warm":
		// Resolving the whole set — versions AND the integrities they are
		// verified against — is deliberate: a lock that repinned an extension
		// and a lock that lost the integrity entry both make the produced store
		// something this checkout no longer describes.
		artifacts, err := imageLayersWarmResolve(workspaceRoot)
		if err != nil {
			return "", err
		}
		return imageLayersWarmLabel(artifacts), nil
	case "dirs":
		// A directory layer derives from no workspace source at all: the
		// declared path and the fixed mode ARE the bytes, so the label is the
		// whole identity and this case reads nothing else.
		prefix, err := imageLayersPrefix(spec.Path)
		if err != nil {
			return "", err
		}
		return imageLayersDirsLabel(prefix), nil
	default:
		return "", fmt.Errorf("unknown producer %q", spec.Producer)
	}
}

// imageLayersLoadRecord loads the sidecar the producer wrote beside a layer. An
// absent record means the layer was never produced in THIS checkout — the
// common case on a fresh clone or after `.gen` was cleaned — and says so
// instead of reporting a hash mismatch against nothing.
func imageLayersLoadRecord(outDir, name string) (imageLayerRecord, error) {
	path := filepath.Join(outDir, name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a producer-owned output path under the resolved project root
	if os.IsNotExist(err) {
		return imageLayerRecord{}, fmt.Errorf("no layer has been produced (%s is missing)", filepath.Base(path))
	}
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("read %s: %w", path, err)
	}
	var record imageLayerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return imageLayerRecord{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return record, nil
}

// imageLayersProjectLabel renders the project the repair command should name:
// its workspace-relative path when it sits inside the workspace, so the printed
// line can be pasted from the workspace root.
func imageLayersProjectLabel(workspaceRoot, dir string) string {
	if workspaceRoot == "" {
		return dir
	}
	rel, err := filepath.Rel(workspaceRoot, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return dir
	}
	return filepath.ToSlash(rel)
}
