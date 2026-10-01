package infra

import (
	"os"
	"path/filepath"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// GeneratedRequirementsContributor is the provenance assigned to a project's
// committed generator-owned infra/requirements.json when it is merged into a
// workload artifact.
const GeneratedRequirementsContributor = ContributorID("framework:requirements")

// legacyGeneratedProtocolVersion is the one generated manifest version whose
// resource shape is wire-compatible with the current protocol. Infra v2 only
// removed workload runtime cost policy, while generated per-project manifests
// have never been allowed to carry a runtime block. Keeping this bridge scoped
// to generator-owned manifests lets a new extension rebuild projects that are
// still linked against a v1 framework without weakening the strict readers for
// authored runtime or deployer-facing aggregated manifests.
const legacyGeneratedProtocolVersion = 1

// ProjectRequirementsPath returns the committed generator-owned infra
// requirements manifest path for projectRoot.
func ProjectRequirementsPath(projectRoot string) string {
	return filepath.Join(projectRoot, PerProjectManifestDir, PerProjectManifestFilename)
}

// LoadGeneratedPerProjectManifest reads a generator-owned per-project
// manifest with the normal strict field and resource validation. It also
// accepts the wire-compatible v1 form, normalizes it to the current version in
// memory, and reports that migration as a warning. The next
// SyncGeneratedRequirements write persists the normalized v2 form.
//
// This compatibility boundary is deliberately narrower than
// LoadPerProjectManifest: callers must use it only for framework-generated
// .gen/infra fragments or generator-owned infra/requirements.json files.
func LoadGeneratedPerProjectManifest(path string) (*PerProjectManifest, []diag.Diagnostic) {
	data, err := readFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"read generated per-project infra manifest %s: %v", path, err)}
	}

	m, diags := ParsePerProjectManifest(data)
	if diag.HasErrors(diags) {
		prefixWithPath(diags, path)
		return nil, diags
	}
	if m.ProtocolVersion == legacyGeneratedProtocolVersion {
		m.ProtocolVersion = ProtocolVersion
		diags = append(diags, diag.Warningf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"generated protocolVersion %d was normalized to %d; re-run `putnami build` to persist the migrated manifest",
			legacyGeneratedProtocolVersion, ProtocolVersion))
	}
	diags = append(diags, ValidatePerProjectManifest(m)...)
	prefixWithPath(diags, path)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, diags
}

// GeneratedSidecarDir returns the ephemeral per-producer scratch directory
// used by language/framework generators before they sync infra/requirements.json.
func GeneratedSidecarDir(projectRoot string) string {
	return filepath.Join(projectRoot, AggregatedManifestDir, PerProjectManifestDir)
}

// ClearGeneratedRequirementSidecars removes the generator scratch fragments
// under <project>/.gen/infra. Generators call this once at the start of a
// build-generate run so producers removed from code cannot leave stale
// requirements behind. The committed infra/requirements.json is reconciled by
// SyncGeneratedRequirements after producers run.
func ClearGeneratedRequirementSidecars(projectRoot string) error {
	matches, err := filepath.Glob(filepath.Join(GeneratedSidecarDir(projectRoot), "*.json"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		if err := removeFile(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// SyncGeneratedRequirements merges all generator scratch fragments at
// <project>/.gen/infra/*.json into the committed, generator-owned
// <project>/infra/requirements.json. The output is deterministic and
// newline-terminated. When no valid fragment declares resources, any stale
// committed requirements file is removed.
//
// The runtime defaults scratch file (<project>/.gen/infra/runtime.json) is
// deliberately ignored: runtime intent belongs in infra/runtime.json and the
// build aggregator adds runtime to the final .gen/requirements.json.
func SyncGeneratedRequirements(projectRoot string) ([]diag.Diagnostic, error) {
	fragments, diags := loadGeneratedFragments(projectRoot)
	manifest, mergeDiags := mergeGeneratedFragments(fragments)
	diags = append(diags, mergeDiags...)

	path := ProjectRequirementsPath(projectRoot)
	if !manifest.hasResources() {
		if err := removeFile(path); err != nil && !os.IsNotExist(err) {
			return diags, err
		}
		return diags, nil
	}

	manifest.Schema = PerProjectSchemaURL
	manifest.ProtocolVersion = ProtocolVersion
	if err := writeProjectRequirements(path, manifest); err != nil {
		return diags, err
	}
	return diags, nil
}

type generatedFragment struct {
	slug     string
	manifest PerProjectManifest
}

func loadGeneratedFragments(projectRoot string) ([]generatedFragment, []diag.Diagnostic) {
	matches, err := filepath.Glob(filepath.Join(GeneratedSidecarDir(projectRoot), "*.json"))
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "glob generated infra fragments: %v", err)}
	}
	sort.Strings(matches)

	fragments := make([]generatedFragment, 0, len(matches))
	var diags []diag.Diagnostic
	for _, path := range matches {
		if filepath.Base(path) == "runtime.json" {
			continue
		}
		m, d := LoadGeneratedPerProjectManifest(path)
		if diag.HasErrors(d) {
			diags = append(diags, d...)
			continue
		}
		diags = append(diags, d...)
		if m == nil || !m.hasResources() {
			continue
		}
		fragments = append(fragments, generatedFragment{
			slug:     sidecarSlugFromPath(path),
			manifest: *m,
		})
	}
	return fragments, diags
}

func sidecarSlugFromPath(path string) string {
	base := filepath.Base(path)
	return base[:len(base)-len(filepath.Ext(base))]
}

func mergeGeneratedFragments(fragments []generatedFragment) (PerProjectManifest, []diag.Diagnostic) {
	contributions := make([]ProjectContribution, 0, len(fragments))
	for _, fragment := range fragments {
		contributions = append(contributions, ProjectContribution{
			Project:     "generated",
			Contributor: FrameworkContributor(fragment.slug),
			Manifest:    fragment.manifest,
		})
	}

	merged, diags := Merge("generated", contributions)
	out := PerProjectManifest{
		Schema:          PerProjectSchemaURL,
		ProtocolVersion: ProtocolVersion,
	}
	for _, db := range merged.Databases {
		out.Databases = append(out.Databases, Database{
			Name:    db.Name,
			Engine:  db.Engine,
			Schemas: db.Schemas,
		})
	}
	if merged.Events != nil {
		events := &Events{}
		for _, topic := range merged.Events.Publishes {
			events.Publishes = append(events.Publishes, topic.Name)
		}
		for _, topic := range merged.Events.Subscribes {
			events.Subscribes = append(events.Subscribes, Subscription{Topic: topic.Name, Delivery: topic.Delivery})
		}
		if len(events.Publishes) > 0 || len(events.Subscribes) > 0 {
			out.Events = events
		}
	}
	for _, bucket := range merged.Storage {
		out.Storage = append(out.Storage, StorageBucket{
			Name:      bucket.Name,
			Access:    bucket.Access,
			Public:    bucket.Public,
			Retention: bucket.Retention,
		})
	}
	for _, secret := range merged.Secrets {
		out.Secrets = append(out.Secrets, secret.Name)
	}
	for _, job := range merged.ScheduledJobs {
		out.ScheduledJobs = append(out.ScheduledJobs, ScheduledJob{
			Name:       job.Name,
			Schedule:   job.Schedule,
			Entrypoint: job.Entrypoint,
		})
	}
	return out, diags
}

func writeProjectRequirements(path string, manifest PerProjectManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := marshalCommitted(manifest)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := renameFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
