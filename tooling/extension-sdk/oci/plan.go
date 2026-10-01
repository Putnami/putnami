package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// ImagePlanVersion is intentionally v2: the former implicit v1 identity
	// streamed source bytes into one whole-image hash and could not reuse a
	// trustworthy fingerprint already computed upstream. V2 hashes explicit,
	// versioned source and compression identities instead.
	ImagePlanVersion = 2
	// LayerPlanVersion versions the reusable compressed-layer key contract.
	LayerPlanVersion = 1

	imagePlanDomain       = "putnami/oci/image-plan"
	layerPlanDomain       = "putnami/oci/layer-plan"
	sha256Algorithm       = "sha256"
	compressionAlgorithm  = "gzip"
	compressionImpl       = "putnami/go-gzip/v1"
	legacyDerivedIdentity = "putnami/oci/derived-from-image-plan/v1"
)

// SourceFingerprint is the immutable identity of source bytes. Algorithm and
// size are explicit so fingerprints handed in by an orchestrator cannot be
// confused with task keys or path/metadata hashes.
type SourceFingerprint struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// CompressionIdentity pins the byte-producing compression contract. Bump the
// implementation identity only when deterministic compressed bytes change.
type CompressionIdentity struct {
	Algorithm      string `json:"algorithm"`
	Level          int    `json:"level"`
	Implementation string `json:"implementation"`
}

// FilePlan binds an in-image destination to source bytes. Derived identifies a
// deterministic recipe whose output depends on the image identity itself; its
// concrete fingerprint is bound after that output has been staged.
type FilePlan struct {
	Path        string             `json:"path"`
	Mode        int64              `json:"mode"`
	Fingerprint *SourceFingerprint `json:"fingerprint,omitempty"`
	Derived     string             `json:"derived,omitempty"`
}

// LayerKind identifies how a layer's filesystem bytes are supplied.
type LayerKind string

const (
	// LayerKindFiles is a deterministic tar synthesized from individual files.
	LayerKindFiles LayerKind = "files"
	// LayerKindTarball is a prebuilt filesystem tar stream.
	LayerKindTarball LayerKind = "tarball"
)

// LayerPlan is the canonical, machine-independent identity of one layer.
type LayerPlan struct {
	Version     int                 `json:"version"`
	Kind        LayerKind           `json:"kind"`
	Compression CompressionIdentity `json:"compression"`
	Files       []FilePlan          `json:"files,omitempty"`
	Tarball     *SourceFingerprint  `json:"tarball,omitempty"`
}

// LayerKey returns the content-addressed cache key for the exact compressed
// layer bytes. Every derived file must be bound before a key can be produced.
type LayerKey string

// Key returns the versioned cache identity for the planned layer.
func (p LayerPlan) Key() (LayerKey, error) {
	if p.Version != LayerPlanVersion {
		return "", fmt.Errorf("unsupported layer plan version %d", p.Version)
	}
	if p.Compression != currentCompressionIdentity() {
		return "", fmt.Errorf("unsupported layer compression identity %+v", p.Compression)
	}
	type keyFile struct {
		Path        string            `json:"path"`
		Mode        int64             `json:"mode"`
		Fingerprint SourceFingerprint `json:"fingerprint"`
	}
	files := make([]keyFile, 0, len(p.Files))
	for _, file := range p.Files {
		if file.Fingerprint == nil {
			return "", fmt.Errorf("file %s has no bound source fingerprint", file.Path)
		}
		if err := file.Fingerprint.validate(); err != nil {
			return "", fmt.Errorf("file %s fingerprint: %w", file.Path, err)
		}
		files = append(files, keyFile{Path: file.Path, Mode: file.Mode, Fingerprint: *file.Fingerprint})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i := 1; i < len(files); i++ {
		if files[i-1].Path == files[i].Path {
			return "", fmt.Errorf("layer contains duplicate image path %s", files[i].Path)
		}
	}
	if p.Kind == LayerKindTarball {
		if p.Tarball == nil {
			return "", fmt.Errorf("tarball layer has no source fingerprint")
		}
		if len(files) != 0 {
			return "", fmt.Errorf("tarball layer unexpectedly contains file plans")
		}
		if err := p.Tarball.validate(); err != nil {
			return "", fmt.Errorf("tarball fingerprint: %w", err)
		}
	} else if p.Kind != LayerKindFiles {
		return "", fmt.Errorf("unsupported layer kind %q", p.Kind)
	} else if p.Tarball != nil {
		return "", fmt.Errorf("file layer unexpectedly has a tarball fingerprint")
	}
	material := struct {
		Domain      string              `json:"domain"`
		Version     int                 `json:"version"`
		Kind        LayerKind           `json:"kind"`
		Compression CompressionIdentity `json:"compression"`
		Files       []keyFile           `json:"files,omitempty"`
		Tarball     *SourceFingerprint  `json:"tarball,omitempty"`
	}{
		Domain: layerPlanDomain, Version: p.Version, Kind: p.Kind,
		Compression: p.Compression, Files: files, Tarball: p.Tarball,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return LayerKey(hex.EncodeToString(sum[:])), nil
}

// ImagePlan is a reusable snapshot of source identities and the complete
// content-addressed image identity. The local spec is deliberately excluded
// from JSON: host paths are execution details, never identity material.
type ImagePlan struct {
	Version  int         `json:"version"`
	Identity string      `json:"identity"`
	Layers   []LayerPlan `json:"layers"`

	spec Spec
}

// PlanOptions controls source identity acquisition.
type PlanOptions struct {
	// DerivedFiles maps an in-image destination to a stable recipe identity.
	// The source may be absent until after ImagePlan.Identity is known.
	DerivedFiles map[string]string
	// TrustedFingerprints maps local source paths to raw-byte fingerprints.
	// Callers may use this only when the producer guarantees that the
	// fingerprint names the immutable bytes intended for this invocation.
	// Cache misses still verify the source while building the layer.
	TrustedFingerprints map[string]SourceFingerprint
}

// NewImagePlan fingerprints each non-derived source at most once, then derives
// ordered layer and whole-image identities without rereading source content.
func NewImagePlan(spec Spec, opts PlanOptions) (*ImagePlan, error) {
	spec = cloneSpec(spec)
	plan := &ImagePlan{
		Version: ImagePlanVersion,
		Layers:  make([]LayerPlan, len(spec.Layers)),
		spec:    spec,
	}
	matchedDerived := make(map[string]bool, len(opts.DerivedFiles))
	for layerIndex, layer := range spec.Layers {
		if layer.Tarball != "" && len(layer.Files) > 0 {
			return nil, fmt.Errorf("layer %d declares both files and tarball", layerIndex)
		}
		layerPlan := LayerPlan{
			Version:     LayerPlanVersion,
			Kind:        LayerKindFiles,
			Compression: currentCompressionIdentity(),
		}
		if layer.Tarball != "" {
			layerPlan.Kind = LayerKindTarball
			fingerprint, err := sourceFingerprint(layer.Tarball, opts.TrustedFingerprints)
			if err != nil {
				return nil, fmt.Errorf("fingerprinting %s: %w", layer.Tarball, err)
			}
			layerPlan.Tarball = &fingerprint
			plan.Layers[layerIndex] = layerPlan
			continue
		}

		files := append([]File(nil), layer.Files...)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		layerPlan.Files = make([]FilePlan, 0, len(files))
		for fileIndex, file := range files {
			if fileIndex > 0 && files[fileIndex-1].Path == file.Path {
				return nil, fmt.Errorf("layer %d contains duplicate image path %s", layerIndex, file.Path)
			}
			filePlan := FilePlan{Path: file.Path, Mode: file.Mode}
			if derived, ok := opts.DerivedFiles[file.Path]; ok {
				if derived == "" {
					return nil, fmt.Errorf("derived file %s has an empty recipe identity", file.Path)
				}
				filePlan.Derived = derived
				matchedDerived[file.Path] = true
			} else {
				fingerprint, err := sourceFingerprint(file.Source, opts.TrustedFingerprints)
				if err != nil {
					return nil, fmt.Errorf("fingerprinting %s: %w", file.Source, err)
				}
				filePlan.Fingerprint = &fingerprint
			}
			layerPlan.Files = append(layerPlan.Files, filePlan)
		}
		plan.Layers[layerIndex] = layerPlan
	}
	for path := range opts.DerivedFiles {
		if !matchedDerived[path] {
			return nil, fmt.Errorf("derived file %s is not present in the image spec", path)
		}
	}
	identity, err := imagePlanIdentity(plan)
	if err != nil {
		return nil, err
	}
	plan.Identity = identity
	return plan, nil
}

// BindDerivedFiles fingerprints outputs staged after the image identity was
// computed. Their recipe identity remains in the image hash while their exact
// bytes become part of the layer cache key and are verified during a miss.
func (p *ImagePlan) BindDerivedFiles() error {
	if err := p.validate(false); err != nil {
		return err
	}
	for layerIndex := range p.Layers {
		if p.Layers[layerIndex].Kind != LayerKindFiles {
			continue
		}
		sources := make(map[string]string, len(p.spec.Layers[layerIndex].Files))
		for _, file := range p.spec.Layers[layerIndex].Files {
			sources[file.Path] = file.Source
		}
		for fileIndex := range p.Layers[layerIndex].Files {
			filePlan := &p.Layers[layerIndex].Files[fileIndex]
			if filePlan.Derived == "" || filePlan.Fingerprint != nil {
				continue
			}
			fingerprint, err := fingerprintSourceFile(sources[filePlan.Path])
			if err != nil {
				return fmt.Errorf("fingerprinting derived file %s: %w", filePlan.Path, err)
			}
			filePlan.Fingerprint = &fingerprint
		}
	}
	return p.validate(true)
}

func (p *ImagePlan) validate(requireBound bool) error {
	if p == nil {
		return fmt.Errorf("image plan is nil")
	}
	if p.Version != ImagePlanVersion {
		return fmt.Errorf("unsupported image plan version %d", p.Version)
	}
	if len(p.Layers) != len(p.spec.Layers) {
		return fmt.Errorf("image plan layer count does not match its spec")
	}
	wantIdentity, err := imagePlanIdentity(p)
	if err != nil {
		return err
	}
	if p.Identity != wantIdentity {
		return fmt.Errorf("image plan identity does not match its contents")
	}
	for layerIndex, layerPlan := range p.Layers {
		layer := p.spec.Layers[layerIndex]
		if layerPlan.Version != LayerPlanVersion || layerPlan.Compression != currentCompressionIdentity() {
			return fmt.Errorf("layer %d has an unsupported plan identity", layerIndex)
		}
		if layer.Tarball != "" {
			if layerPlan.Kind != LayerKindTarball || layerPlan.Tarball == nil || len(layerPlan.Files) != 0 {
				return fmt.Errorf("layer %d plan does not match its tarball spec", layerIndex)
			}
			if err := layerPlan.Tarball.validate(); err != nil {
				return fmt.Errorf("layer %d tarball fingerprint: %w", layerIndex, err)
			}
			continue
		}
		files := append([]File(nil), layer.Files...)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		if layerPlan.Kind != LayerKindFiles || len(layerPlan.Files) != len(files) {
			return fmt.Errorf("layer %d plan does not match its file spec", layerIndex)
		}
		for fileIndex, filePlan := range layerPlan.Files {
			if filePlan.Path != files[fileIndex].Path || filePlan.Mode != files[fileIndex].Mode {
				return fmt.Errorf("layer %d file plan does not match its spec", layerIndex)
			}
			if filePlan.Fingerprint == nil {
				if requireBound || filePlan.Derived == "" {
					return fmt.Errorf("file %s has no bound source fingerprint", filePlan.Path)
				}
				continue
			}
			if err := filePlan.Fingerprint.validate(); err != nil {
				return fmt.Errorf("file %s fingerprint: %w", filePlan.Path, err)
			}
		}
	}
	return nil
}

func imagePlanIdentity(plan *ImagePlan) (string, error) {
	type identityFile struct {
		Path        string             `json:"path"`
		Mode        int64              `json:"mode"`
		Fingerprint *SourceFingerprint `json:"fingerprint,omitempty"`
		Derived     string             `json:"derived,omitempty"`
	}
	type identityLayer struct {
		Version     int                 `json:"version"`
		Kind        LayerKind           `json:"kind"`
		Compression CompressionIdentity `json:"compression"`
		Files       []identityFile      `json:"files,omitempty"`
		Tarball     *SourceFingerprint  `json:"tarball,omitempty"`
	}
	layers := make([]identityLayer, len(plan.Layers))
	for i, layer := range plan.Layers {
		layers[i] = identityLayer{
			Version: layer.Version, Kind: layer.Kind,
			Compression: layer.Compression, Tarball: layer.Tarball,
			Files: make([]identityFile, 0, len(layer.Files)),
		}
		for _, file := range layer.Files {
			identity := identityFile{Path: file.Path, Mode: file.Mode, Derived: file.Derived}
			if file.Derived == "" {
				identity.Fingerprint = file.Fingerprint
			}
			layers[i].Files = append(layers[i].Files, identity)
		}
	}
	ports := append([]string(nil), plan.spec.ExposedPorts...)
	sort.Strings(ports)
	material := struct {
		Domain       string          `json:"domain"`
		Version      int             `json:"version"`
		BaseRef      string          `json:"baseRef"`
		Platform     string          `json:"platform"`
		Layers       []identityLayer `json:"layers"`
		Env          []string        `json:"env,omitempty"`
		Entrypoint   []string        `json:"entrypoint,omitempty"`
		WorkingDir   string          `json:"workingDir,omitempty"`
		User         string          `json:"user,omitempty"`
		ExposedPorts []string        `json:"exposedPorts,omitempty"`
	}{
		Domain: imagePlanDomain, Version: plan.Version,
		BaseRef: plan.spec.BaseRef, Platform: plan.spec.Platform, Layers: layers,
		Env: plan.spec.Env, Entrypoint: plan.spec.Entrypoint,
		WorkingDir: plan.spec.WorkingDir, User: plan.spec.User, ExposedPorts: ports,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func currentCompressionIdentity() CompressionIdentity {
	return CompressionIdentity{
		Algorithm:      compressionAlgorithm,
		Level:          layerCompressionLevel,
		Implementation: compressionImpl,
	}
}

func sourceFingerprint(path string, trusted map[string]SourceFingerprint) (SourceFingerprint, error) {
	if fingerprint, ok := trusted[path]; ok {
		if err := fingerprint.validate(); err != nil {
			return SourceFingerprint{}, fmt.Errorf("trusted fingerprint: %w", err)
		}
		return fingerprint, nil
	}
	return fingerprintSourceFile(path)
}

func fingerprintSourceFile(path string) (SourceFingerprint, error) {
	digest, size, err := digestSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{Algorithm: sha256Algorithm, Digest: digest, Size: size}, nil
}

func (f SourceFingerprint) validate() error {
	if f.Algorithm != sha256Algorithm {
		return fmt.Errorf("algorithm %q is not sha256", f.Algorithm)
	}
	if f.Size < 0 {
		return fmt.Errorf("size %d is negative", f.Size)
	}
	if len(f.Digest) != sha256.Size*2 {
		return fmt.Errorf("digest %q is not a full sha256", f.Digest)
	}
	if f.Digest != strings.ToLower(f.Digest) {
		return fmt.Errorf("digest %q is not canonical lowercase hexadecimal", f.Digest)
	}
	if _, err := hex.DecodeString(f.Digest); err != nil {
		return fmt.Errorf("digest %q is not hexadecimal", f.Digest)
	}
	return nil
}

func cloneSpec(spec Spec) Spec {
	cloned := spec
	cloned.Env = append([]string(nil), spec.Env...)
	cloned.Entrypoint = append([]string(nil), spec.Entrypoint...)
	cloned.ExposedPorts = append([]string(nil), spec.ExposedPorts...)
	cloned.Layers = make([]Layer, len(spec.Layers))
	for i, layer := range spec.Layers {
		cloned.Layers[i] = layer
		cloned.Layers[i].Files = append([]File(nil), layer.Files...)
	}
	return cloned
}
