package infra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// PerProjectSchemaURL is the JSON Schema reference stamped into generated
// per-project manifests so editors validate them against the published
// per-project schema.
const PerProjectSchemaURL = "https://putnami.dev/schemas/putnami-infra.json"

// validSlug constrains sidecar filenames so they are filesystem-safe and
// produce a clean contributor identity when echoed back as "framework:<slug>".
var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SidecarPath returns the path to a producer's ephemeral generator scratch
// fragment under projectRoot. Each framework producer owns one file at
// <projectRoot>/.gen/infra/<slug>.json — the slug identifies the producer
// (e.g. "database", "storage", "events"). Multiple producers never share a
// file, so writes need no read-modify-write merge. Language generators fold
// these fragments into committed infra/requirements.json via
// SyncGeneratedRequirements; deploy aggregation does not read them directly.
//
// This is the form the language generator sync uses when it has the project
// root and globs <projectRoot>/.gen/infra/*.json. A describer running in the
// framework's describe phase instead receives the ".gen" directory itself
// (as app.DescribeContext.OutputDir) and must use SidecarPathIn.
func SidecarPath(projectRoot, slug string) string {
	return SidecarPathIn(filepath.Join(projectRoot, AggregatedManifestDir), slug)
}

// SidecarPathIn returns a producer's per-project sidecar path inside genDir —
// the workload's generated-artifact directory (<projectRoot>/.gen) that the
// build's describe phase hands producers as app.DescribeContext.OutputDir.
// genDir already includes the AggregatedManifestDir (".gen") segment, so only
// the per-project infra subdir and the slug are appended:
//
//	SidecarPathIn(filepath.Join(root, AggregatedManifestDir), slug) == SidecarPath(root, slug)
//
// Describers must use this with OutputDir; passing OutputDir to SidecarPath
// (or WriteSidecar) nests the file one ".gen" too deep, where the sync glob
// never sees it.
func SidecarPathIn(genDir, slug string) string {
	return filepath.Join(genDir, PerProjectManifestDir, slug+".json")
}

// WriteSidecar atomically writes manifest to <projectRoot>/.gen/infra/<slug>.json.
// When manifest declares no resources, any existing sidecar for slug is removed
// so a project that dropped its last requirement stops contributing stale data
// to the next SyncGeneratedRequirements run.
//
// Each framework producer owns a single slug — generate/describe hooks pass
// the producer's stable slug (e.g. "database", "storage") and the manifest
// fragment they declare. No merging happens here; if two producers in the
// same project both contribute the same resource kind they use different
// slugs and the language generator unions their contributions into the
// committed per-project manifest.
//
// projectRoot is the project root; the ".gen/infra" segments are appended. A
// describer holding app.DescribeContext.OutputDir (which already points at
// ".gen") must call WriteSidecarIn instead.
func WriteSidecar(projectRoot, slug string, manifest PerProjectManifest) error {
	return WriteSidecarIn(filepath.Join(projectRoot, AggregatedManifestDir), slug, manifest)
}

// WriteSidecarIn is WriteSidecar relative to genDir — the workload's
// generated-artifact directory (<projectRoot>/.gen), handed to producers as
// app.DescribeContext.OutputDir. It writes the sidecar at
// SidecarPathIn(genDir, slug).
func WriteSidecarIn(genDir, slug string, manifest PerProjectManifest) error {
	if !validSlug.MatchString(slug) {
		return fmt.Errorf("infra: sidecar slug %q must match %s", slug, validSlug.String())
	}
	finalPath := SidecarPathIn(genDir, slug)

	if !manifest.hasResources() {
		if err := removeFile(finalPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	manifest.Schema = PerProjectSchemaURL
	manifest.ProtocolVersion = ProtocolVersion

	manifestDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(manifestDir, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := renameFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of the temp file
		return err
	}
	return nil
}

// RemoveSidecar deletes <projectRoot>/.gen/infra/<slug>.json. A missing file
// is not an error — callers use this to clear a stale sidecar when a producer
// has nothing to declare in this build. A describer holding
// app.DescribeContext.OutputDir must call RemoveSidecarIn instead.
func RemoveSidecar(projectRoot, slug string) error {
	return RemoveSidecarIn(filepath.Join(projectRoot, AggregatedManifestDir), slug)
}

// RemoveSidecarIn is RemoveSidecar relative to genDir — the ".gen" directory
// handed to producers as app.DescribeContext.OutputDir.
func RemoveSidecarIn(genDir, slug string) error {
	if !validSlug.MatchString(slug) {
		return fmt.Errorf("infra: sidecar slug %q must match %s", slug, validSlug.String())
	}
	path := SidecarPathIn(genDir, slug)
	if err := removeFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// hasResources reports whether m declares at least one infrastructure
// requirement. Schema/ProtocolVersion alone don't count — an otherwise empty
// manifest carries no requirement and its sidecar should not exist.
func (m PerProjectManifest) hasResources() bool {
	if m.Events != nil && (len(m.Events.Publishes) > 0 || len(m.Events.Subscribes) > 0) {
		return true
	}
	return len(m.Databases) > 0 || len(m.Storage) > 0 || len(m.Secrets) > 0 || len(m.ScheduledJobs) > 0
}
