package oci

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"go.putnami.dev/sdk/extension/scratch"
)

// layerCompressionLevel is part of the image identity: changing it changes
// every assembled layer blob and therefore every digest. Never vary it at
// runtime.
const layerCompressionLevel = gzip.DefaultCompression

// Assemble builds the image described by spec on top of base. The result is
// fully deterministic for identical (spec, base, file contents): layer tars
// use fixed timestamps and explicit modes, history and config carry no real
// times, and the gzip level is pinned. workDir holds the staged compressed
// layer blobs and must outlive uses of the returned image (layers are read
// lazily).
func Assemble(spec Spec, base v1.Image, workDir string) (v1.Image, error) {
	return assemble(spec, nil, base, workDir, "")
}

// AssembleWithLayerCache is Assemble with a shared, content-addressed cache for
// generated file layers. Tarball layers keep their existing path. An empty
// cache root is equivalent to Assemble.
func AssembleWithLayerCache(spec Spec, base v1.Image, workDir, layerCacheRoot string) (v1.Image, error) {
	return assemble(spec, nil, base, workDir, layerCacheRoot)
}

// AssemblePlan builds a previously fingerprinted plan without reopening
// sources to rediscover layer identities.
func AssemblePlan(plan *ImagePlan, base v1.Image, workDir string) (v1.Image, error) {
	return AssemblePlanWithLayerCache(plan, base, workDir, "")
}

// AssemblePlanWithLayerCache is AssemblePlan with shared layer reuse.
func AssemblePlanWithLayerCache(plan *ImagePlan, base v1.Image, workDir, layerCacheRoot string) (v1.Image, error) {
	if err := plan.BindDerivedFiles(); err != nil {
		return nil, err
	}
	return assemble(plan.spec, plan, base, workDir, layerCacheRoot)
}

func assemble(spec Spec, plan *ImagePlan, base v1.Image, workDir, layerCacheRoot string) (v1.Image, error) {
	baseMediaType, err := base.MediaType()
	if err != nil {
		return nil, fmt.Errorf("reading base media type: %w", err)
	}
	layerMediaType := types.DockerLayer
	if baseMediaType == types.OCIManifestSchema1 {
		layerMediaType = types.OCILayer
	}

	adds := make([]mutate.Addendum, 0, len(spec.Layers))
	for i, l := range spec.Layers {
		if l.Tarball != "" && len(l.Files) > 0 {
			return nil, fmt.Errorf("layer %d declares both files and tarball", i)
		}
		var layer v1.Layer
		if l.Tarball != "" {
			if plan == nil {
				layer, err = tarball.LayerFromFile(l.Tarball,
					tarball.WithCompressionLevel(layerCompressionLevel),
					tarball.WithMediaType(layerMediaType))
			} else {
				layer, err = plannedTarballLayer(l.Tarball, *plan.Layers[i].Tarball, layerMediaType)
			}
		} else {
			blobPath := filepath.Join(workDir, fmt.Sprintf("layer-%d.tar.gz", i))
			if plan == nil {
				layer, _, err = loadOrBuildFileLayer(l, blobPath, layerCacheRoot, layerMediaType)
			} else {
				layer, _, err = loadOrBuildPlannedFileLayer(l, &plan.Layers[i], blobPath, layerCacheRoot, layerMediaType)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("creating layer %d: %w", i, err)
		}
		adds = append(adds, mutate.Addendum{
			Layer: layer,
			History: v1.History{
				Created:   v1.Time{Time: epoch},
				CreatedBy: "putnami package-docker",
			},
		})
	}

	img, err := mutate.Append(base, adds...)
	if err != nil {
		return nil, fmt.Errorf("appending layers: %w", err)
	}

	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg := cf.Config
	cfg.Env = mergeEnv(cfg.Env, spec.Env)
	if len(spec.Entrypoint) > 0 {
		cfg.Entrypoint = spec.Entrypoint
		// docker build resets an inherited CMD when the child sets ENTRYPOINT.
		cfg.Cmd = nil
	}
	if spec.WorkingDir != "" {
		cfg.WorkingDir = spec.WorkingDir
	}
	if spec.User != "" {
		cfg.User = spec.User
	}
	if len(spec.ExposedPorts) > 0 {
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range spec.ExposedPorts {
			cfg.ExposedPorts[p] = struct{}{}
		}
	}
	img, err = mutate.Config(img, cfg)
	if err != nil {
		return nil, fmt.Errorf("setting config: %w", err)
	}
	img, err = mutate.CreatedAt(img, v1.Time{Time: epoch})
	if err != nil {
		return nil, fmt.Errorf("setting created: %w", err)
	}
	return img, nil
}

// mergeEnv merges spec env entries onto the base env with docker build
// semantics: a matching KEY overrides in place, a new KEY appends. The base
// env (PATH, SSL_CERT_FILE, ...) must survive — losing it would change
// runtime behavior versus the docker-built image.
func mergeEnv(base, overrides []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range overrides {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		replaced := false
		for i, existing := range out {
			if ek, _, _ := strings.Cut(existing, "="); ek == key {
				out[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// LayoutOptions controls base resolution and reusable layer storage.
type LayoutOptions struct {
	BaseCacheDir  string
	LayerCacheDir string
	Keychain      authn.Keychain
	// BaseLayout selects an already packaged local OCI artifact instead of a
	// registry fetch. BaseDigest binds that artifact to the dependency's typed
	// candidate metadata and is verified before it can become a child image.
	BaseLayout string
	BaseDigest string
}

// AssembleToLayout assembles the image and writes it as a self-contained OCI
// layout (base blobs included) at layoutDir, replacing any previous layout
// there. It returns the image digest — known before any push.
func AssembleToLayout(spec Spec, layoutDir string, opts LayoutOptions) (string, error) {
	return assembleToLayout(spec, nil, layoutDir, opts)
}

func assembleToLayout(spec Spec, plan *ImagePlan, layoutDir string, opts LayoutOptions) (string, error) {
	keychain := opts.Keychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	var base v1.Image
	var err error
	if opts.BaseLayout != "" {
		base, err = LoadFromLayout(opts.BaseLayout)
		if err == nil && opts.BaseDigest != "" {
			var digest v1.Hash
			digest, err = base.Digest()
			if err == nil && digest.String() != opts.BaseDigest {
				err = fmt.Errorf("local base layout digest %s does not match candidate digest %s", digest, opts.BaseDigest)
			}
		}
		if err != nil {
			return "", fmt.Errorf("loading local base candidate: %w", err)
		}
	} else {
		base, err = FetchBaseWithKeychain(spec.BaseRef, spec.Platform, opts.BaseCacheDir, keychain)
		if err != nil {
			return "", fmt.Errorf("fetching base %s: %w", spec.BaseRef, err)
		}
	}
	work, err := scratch.New("putnami-oci-")
	if err != nil {
		return "", err
	}
	defer func() { _ = work.Remove() }()
	workDir := work.Path()

	img, err := assemble(spec, plan, base, workDir, opts.LayerCacheDir)
	if err != nil {
		return "", err
	}
	return WriteLayout(layoutDir, img)
}

// AssemblePlanToLayout resolves the base and materializes a previously
// fingerprinted image plan. It is the preferred package path when callers need
// the content identity before assembly.
func AssemblePlanToLayout(plan *ImagePlan, layoutDir string, opts LayoutOptions) (string, error) {
	if err := plan.BindDerivedFiles(); err != nil {
		return "", err
	}
	return assembleToLayout(plan.spec, plan, layoutDir, opts)
}

// WriteLayout writes img as a self-contained OCI layout at dir (replacing any
// existing layout) and returns the image digest.
func WriteLayout(dir string, img v1.Image) (string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return "", fmt.Errorf("creating layout: %w", err)
	}
	if err := lp.AppendImage(img); err != nil {
		return "", fmt.Errorf("writing layout: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	return digest.String(), nil
}

// LoadFromLayout reads the single image back from an OCI layout written by
// WriteLayout.
func LoadFromLayout(dir string) (v1.Image, error) {
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("reading layout %s: %w", dir, err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	if len(manifest.Manifests) == 0 {
		return nil, fmt.Errorf("layout %s contains no images", dir)
	}
	return idx.Image(manifest.Manifests[0].Digest)
}

// ExportDaemonImageToLayout snapshots an execution-local runtime image into a
// self-contained package artifact. This is the compatibility boundary for
// packagers that need a non-native builder: downstream publish consumes the
// layout and never depends on daemon state surviving or crossing runners.
func ExportDaemonImageToLayout(ref, dir string) (string, error) {
	tmp, err := os.CreateTemp("", "putnami-daemon-image-*.tar")
	if err != nil {
		return "", err
	}
	tarPath := tmp.Name()
	defer os.Remove(tarPath)
	if err := tmp.Close(); err != nil {
		return "", err
	}

	output, err := exec.Command("docker", "save", "--output", tarPath, ref).CombinedOutput() //nolint:gosec // fixed local runtime command; ref is a discrete image-reference argument
	if err != nil {
		return "", fmt.Errorf("exporting local runtime image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	image, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		return "", fmt.Errorf("reading exported runtime image: %w", err)
	}
	return WriteLayout(dir, image)
}

// WriteDockerTarball writes the layout's image as a docker-load compatible
// tarball tagged ref, for opt-in loading into a local daemon. It closes every
// layer blob it opened before it returns, so the layout can be removed at
// once: Windows refuses to remove a file that is still open.
func WriteDockerTarball(layoutDir, ref, tarPath string) error {
	img, err := LoadFromLayout(layoutDir)
	if err != nil {
		return err
	}
	tag, err := parseTag(ref)
	if err != nil {
		return err
	}
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	streams := &layerStreams{}
	writeErr := tarball.Write(tag, streams.image(img), f)
	closeErr := streams.close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// layerStreams records the compressed layer streams an image hands out, so
// they can be closed together: tarball.Write reads each one to the end but
// never closes it, and a layout layer stream is an open blob file.
type layerStreams struct {
	mu   sync.Mutex
	open []io.Closer
}

// image returns img with every layer's Compressed stream recorded in s.
func (s *layerStreams) image(img v1.Image) v1.Image {
	return streamRecordingImage{Image: img, streams: s}
}

// close closes every recorded stream and returns the first error.
func (s *layerStreams) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, c := range s.open {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.open = nil
	return first
}

type streamRecordingImage struct {
	v1.Image
	streams *layerStreams
}

func (i streamRecordingImage) Layers() ([]v1.Layer, error) {
	layers, err := i.Image.Layers()
	if err != nil {
		return nil, err
	}
	recording := make([]v1.Layer, len(layers))
	for n, layer := range layers {
		recording[n] = streamRecordingLayer{Layer: layer, streams: i.streams}
	}
	return recording, nil
}

type streamRecordingLayer struct {
	v1.Layer
	streams *layerStreams
}

func (l streamRecordingLayer) Compressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	l.streams.mu.Lock()
	l.streams.open = append(l.streams.open, rc)
	l.streams.mu.Unlock()
	return rc, nil
}
