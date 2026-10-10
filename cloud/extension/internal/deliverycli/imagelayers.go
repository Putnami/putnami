package deliverycli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.putnami.dev/client"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/registrycred"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// putnami cloud image-layers — the host-side layer producers for a
// content-addressed image project.
//
// The framework assembles a `type: "image"` project daemonlessly from a
// declared OCI spec and keys it by a SHA-256 over the whole spec INCLUDING the
// layer bytes. It normalizes the layers it builds itself (`files` layers:
// fixed timestamps, sorted entries, explicit modes — tooling/extension-sdk/oci),
// but a `{tarball}` layer is hashed raw, so its bytes are entirely the
// producer's responsibility.
//
// That makes byte-determinism the load-bearing invariant of this file, not a
// nicety: if the same inputs produce two different tars on two machines, the
// image content key churns, publish-if-missing never hits, and the whole
// content-addressing model silently degrades into "rebuild and republish every
// time" (image invariant 1).
//
// Two rules keep it locally checkable:
//
//   - Every emitted tar header is SYNTHESIZED, never copied: epoch mtime,
//     numeric uid/gid 0 with empty owner names, an explicit mode derived only
//     from the source's execute bit, no access/change times, no xattrs, no PAX
//     records beyond what an over-long path forces. Nothing from the host's
//     umask, locale, clock, or tar implementation can reach the output.
//   - No archive path is ever handed to the host filesystem. Entries are read
//     straight out of the downloaded tarball and each regular file's bytes are
//     spilled to a SEQUENTIALLY NUMBERED blob, so a case-insensitive or
//     unicode-normalizing filesystem (APFS is both) cannot reshape the entry
//     set between a darwin/arm64 host and a Linux one.
//
// Pins stay derived (image invariant 2): the Go version comes from go.work
// through imageBuildGoWorkVersion — the workspace derivation table in
// imagepins.go, which every layer resolves through. There is no version
// literal here.
//
// And an unverified artifact never becomes a layer (image invariant 3): the
// toolchain tarball is sha256-verified against the checksum go.dev publishes
// BEFORE a single entry is read out of it. The retired Dockerfile downloaded it
// with a bare `curl` and no verification at all.
//
// The single-file layers — golangci-lint, staticcheck, crane and
// the putnami CLI — land as plain files under `.gen/layers/bin/`, because the
// framework normalizes the `files` layers it builds itself and there is no tar
// for this producer to get wrong. What IS this producer's job is proving what
// those files are, and proving it WITHOUT running them: a linux/amd64 binary
// cannot execute on the darwin host that produces it, and the runtime
// assertions the retired Dockerfile ran only ever ran where docker did (they
// live in the image test suite now — tests/suites/pins_test.sh). So each
// compiled tool is
// checked through `debug/buildinfo` — the library form of `go version -m` — for
// the pinned Go toolchain, the pinned module version and the linux/amd64
// cross-build settings, and the CLI download is checked against the sha256 the
// lock already records. The toolchain check is not decoration: `go install
// pkg@version` resolves the toolchain from the TOOL's own go.mod, so a tool
// built with the wrong Go makes `putnami lint` panic on every CI run.

// imageLayersManifestName is the producer declaration AND the task's activation
// marker. The verb refuses to run without it: the file is what says which
// layers this project expects, so a bare invocation elsewhere is a caller
// mistake, not a skip.
const imageLayersManifestName = "image-layers.json"

// imageLayersEpoch is the fixed timestamp stamped on every emitted entry. It
// matches the framework's own layer epoch (tooling/extension-sdk/oci), so a
// producer-built tarball layer and a framework-built files layer agree.
var imageLayersEpoch = time.Unix(0, 0).UTC()

const (
	// imageLayersDirMode, imageLayersFileModeExec and imageLayersFileModePlain
	// are the ONLY modes this producer emits. A source mode is reduced to "is
	// it executable", which is the only bit the runner image needs and the only
	// one that cannot vary with how the archive was produced.
	imageLayersDirMode       int64 = 0o755
	imageLayersFileModeExec  int64 = 0o755
	imageLayersFileModePlain int64 = 0o644
	imageLayersSymlinkMode   int64 = 0o777
)

const (
	// imageLayersMaxDownloadBytes bounds the toolchain download. A body that
	// reaches the cap is truncated, which fails the checksum comparison — the
	// bound therefore fails closed rather than producing a short layer.
	imageLayersMaxDownloadBytes int64 = 1 << 30
	// imageLayersMaxUnpackedBytes bounds the total spilled bytes so a hostile
	// or corrupt archive cannot fill the disk behind a valid checksum.
	imageLayersMaxUnpackedBytes int64 = 4 << 30
)

// imageLayersNamePattern constrains a layer name to a safe path segment: the
// name becomes the output filename, so anything with a separator or a dot-dot
// would let the manifest write outside `.gen/layers`.
var imageLayersNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// imageLayersSHA256Pattern is the shape a published checksum must have. A
// value that is not a bare lowercase hex digest is drift in the upstream index,
// and comparing against it would compare two different alphabets.
var imageLayersSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// imageLayersManifest is the project's image-layers.json.
type imageLayersManifest struct {
	Layers []imageLayerSpec `json:"layers"`
}

// imageLayerSpec declares one layer to produce.
type imageLayerSpec struct {
	// Name is the layer id and the output basename (<name>.tar or bin/<name>,
	// plus <name>.json).
	Name string `json:"name"`
	// Producer selects the implementation in imageLayerProducers.
	Producer string `json:"producer"`
	// Path is the absolute in-image destination: the root a tarball layer
	// unpacks to ("/usr/local/go"), the full file path a single-file layer is
	// installed at ("/usr/local/bin/crane"), or — for a `dirs` layer — the
	// directory to create and nothing else ("/workspace"). It lives in the
	// manifest rather than in code because the `options.package.image` spec
	// needs the same value.
	Path string `json:"path"`
	// Package is the main package a `go-tool` layer compiles, exactly as the
	// retired Dockerfile's `go install` named it.
	Package string `json:"package,omitempty"`
	// Module is the module Package belongs to — the path the produced binary's
	// embedded build info must report back. It cannot be derived from Package
	// (a /v2 suffix and a cmd/ subdirectory are indistinguishable from path
	// segments), and it is a location rather than a version, so declaring it
	// here adds no pin to keep in sync.
	Module string `json:"module,omitempty"`
	// Tool names the key a `go-tool` layer's version is DERIVED from in the
	// @putnami/go extension's tools/versions.json (the source of truth for
	// the lint tools). Mutually exclusive with Version.
	Tool string `json:"tool,omitempty"`
	// Version is an IMAGE-OWNED pin, for a tool or an image no workspace file
	// declares. Today that is crane and uv, relocated from the retired Dockerfile's
	// `ARG CRANE_VERSION` / `ARG UV_VERSION`. Mutually exclusive with Tool and
	// Pin: a layer that CAN derive its version must, or the manifest becomes the
	// second copy of a pin the workspace already holds.
	Version string `json:"version,omitempty"`
	// Image is the OCI repository an `oci-file` layer extracts from, written
	// exactly as the retired Dockerfile's `FROM` wrote it ("oven/bun", "node",
	// "ghcr.io/astral-sh/uv") — repository only, no tag and no digest.
	Image string `json:"image,omitempty"`
	// Tag is the tag template the resolved version is substituted into
	// ("{version}-slim"). The version never appears here as a literal.
	Tag string `json:"tag,omitempty"`
	// Digest pins an IMAGE-OWNED reference by content, and is therefore the
	// pin itself rather than a copy of one: a reference that resolves to
	// anything else is refused. It is only legal beside Version — committing a
	// digest for a DERIVED pin would make the digest a second, silent pin,
	// so the derived layers record theirs as evidence instead.
	Digest string `json:"digest,omitempty"`
	// Source is the absolute in-image path an `oci-file` layer extracts,
	// exactly as the retired Dockerfile's `COPY --from` named it ("/usr/local/bin/bun",
	// "/uv").
	Source string `json:"source,omitempty"`
	// Pin names the entry in imageBuildPins() whose reader DERIVES this
	// layer's version from a workspace source — "BUN_VERSION" reads
	// package.json's `packageManager`, "NODE_VERSION" its `engines.node`. That
	// table is the workspace's single derivation, so a layer and the release's
	// recorded toolchain identity can never disagree. Mutually exclusive with
	// Version.
	Pin string `json:"pin,omitempty"`
	// Description documents the layer for a reader of the manifest.
	Description string `json:"description,omitempty"`
}

// imageLayerRecord is the sidecar JSON written next to each produced tar: the
// layer's own sha256 (what the framework folds into the image content key), the
// resolved version, and the verified upstream artifact it came from. Every
// field is derived from the run's inputs, so the record is as reproducible as
// the tar it describes — no timestamps, no host paths.
type imageLayerRecord struct {
	Name     string `json:"name"`
	Producer string `json:"producer"`
	Path     string `json:"path"`
	// Tar and File are the produced artifact, relative to the output directory:
	// exactly one is set, depending on whether the layer is a tarball or a
	// single file.
	Tar     string           `json:"tar,omitempty"`
	File    string           `json:"file,omitempty"`
	SHA256  string           `json:"sha256"`
	Size    int64            `json:"size"`
	Version string           `json:"version"`
	Source  imageLayerSource `json:"source"`
	Pruned  []string         `json:"pruned,omitempty"`
}

// imageLayerSource is the upstream artifact a layer was built from, with the
// evidence that was actually checked against it — a published checksum for a
// download, the embedded build info for a compiled tool.
type imageLayerSource struct {
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Package is the `pkg@vX.Y.Z` reference a Go tool was compiled from.
	Package string `json:"package,omitempty"`
	// Module and Toolchain are what the produced binary's own build info
	// reported: the record states the verification's inputs, not just its
	// verdict, so a reader can re-run `go version -m` and get the same answer.
	Module    string `json:"module,omitempty"`
	Toolchain string `json:"toolchain,omitempty"`
	// Image is the OCI reference an extracted binary was pulled from, in the
	// exact form this run built it — repository, resolved tag, and the pinned
	// digest when the reference carries one.
	Image string `json:"image,omitempty"`
	// Digest is what that reference RESOLVED to at produce time: the
	// multi-platform index for a tag, the manifest itself for a single-platform
	// image. For a derived pin it is EVIDENCE, recorded here and committed
	// nowhere — the version is the pin. For an image-owned
	// reference it is the pin, and this records that it held.
	Digest string `json:"digest,omitempty"`
	// Manifest is the linux/amd64 image manifest selected out of that
	// reference, and Layer is the layer blob the extracted bytes came from —
	// the two digests that make "which bytes, exactly" answerable later.
	Manifest string `json:"manifest,omitempty"`
	Layer    string `json:"layer,omitempty"`
	// Entry is the in-image path that was extracted.
	Entry string `json:"entry,omitempty"`
	// Extensions is the verified extension set a warm store layer carries: one
	// row per materialized artifact, with the lock integrity its bytes were
	// checked against. A warm layer has no single upstream artifact, so this is
	// where "which bytes, exactly" is answered for it.
	Extensions []imageLayerExtension `json:"extensions,omitempty"`
	// Tools is the lint-tool pin table the warmed @putnami/go extension carries,
	// as it was compared against what the go-tool layers derive. Recording the
	// values rather than the verdict keeps the record re-checkable.
	Tools map[string]string `json:"tools,omitempty"`
}

// imageLayerExtension is one extension inside a warm store layer.
type imageLayerExtension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// SHA256 is putnami.lock.json's integrity for the layer's platform, and the
	// content-addressed directory name the artifact was materialized under —
	// they are the same value, which is exactly what makes the store verifiable.
	SHA256 string `json:"sha256"`
}

// imageLayerProducer builds one layer into outDir and returns its record.
type imageLayerProducer func(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, warm *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error)

// imageLayerProducerDef is one registry entry: how to build the layer, and the
// producer-specific manifest checks that run with the rest of the manifest
// validation — before ANY producer starts, so a manifest this binary cannot
// execute exactly as written never produces a partial set of layers.
type imageLayerProducerDef struct {
	produce  imageLayerProducer
	validate func(spec imageLayerSpec) error
	// tarball marks a producer that emits a whole tree as <name>.tar rather than
	// a single file under bin/. The image spec consumes the two differently (a
	// `{tarball}` layer versus a `files` layer), so the distinction is stated
	// once here instead of being re-derived from the producer name wherever it
	// matters.
	tarball bool
}

// imageLayerProducers is the producer registry. A manifest naming anything else
// is refused before any work starts — an unknown producer means the manifest
// and this binary disagree about what the image is made of.
var imageLayerProducers = map[string]imageLayerProducerDef{
	"go-toolchain": {produce: imageLayersProduceGoToolchain, validate: imageLayersValidateNoToolFields, tarball: true},
	"go-tool":      {produce: imageLayersProduceGoTool, validate: imageLayersValidateGoTool},
	"putnami-cli":  {produce: imageLayersProducePutnamiCLI, validate: imageLayersValidateNoToolFields},
	"cloud-cli":    {produce: imageLayersProduceCloudCLI, validate: imageLayersValidateNoToolFields},
	"oci-file":     {produce: imageLayersProduceOCIFile, validate: imageLayersValidateOCIFile},
	"putnami-warm": {produce: imageLayersProduceWarm, validate: imageLayersValidateNoToolFields, tarball: true},
	"dirs":         {produce: imageLayersProduceDirs, validate: imageLayersValidateDirs, tarball: true},
}

// ImageLayers backs `putnami cloud image-layers`: read the project's
// image-layers.json and run each declared producer, writing normalized layer
// tarballs plus their checksum sidecars under <project>/.gen/layers/.
//
// Params (all optional):
//
//	project — image project directory (default: the resolved project root)
//	check   — verify the produced layers instead of producing them
//	          (imagelayerscheck.go: the packaging precondition, read-only)
//
// There is no soft-skip path: every producer runs host-side with no daemon, so
// a failure here is a real failure everywhere.
func ImageLayers(params map[string]any, _ []string, workspaceRoot string, _ map[string]string, ioctx clicore.IO) error {
	if clicore.Truthy(clicore.Param(params, imageLayersCheckParam)) {
		return imageLayersVerify(params, workspaceRoot, ioctx)
	}
	dir, err := imageProjectRoot(params, workspaceRoot, "project", "context")
	if err != nil {
		return clicore.NewError("cloud image-layers: resolve project: "+err.Error(), clicore.ExitUsage)
	}
	manifest, err := imageLayersReadManifest(dir)
	if err != nil {
		return err
	}

	outDir := imageLayersOutputDir(dir)
	if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
		return fmt.Errorf("cloud image-layers: create %s: %w", outDir, mkErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The lock-pinned linux/amd64 extension set is materialized AT MOST ONCE per
	// run and lent to every layer built out of it — the derived tool layers and
	// the warm store layer are three consumers of one download. The run owns the
	// scratch tree, so it is removed whether a producer succeeded or failed.
	warm := newImageLayersWarmStore(workspaceRoot, outDir)
	defer warm.close()

	records := make([]imageLayerRecord, 0, len(manifest.Layers))
	for _, spec := range manifest.Layers {
		ioctx.Stdout(fmt.Sprintf("Producing layer %s (%s) …", spec.Name, spec.Producer))
		record, produceErr := imageLayerProducers[spec.Producer].produce(ctx, spec, workspaceRoot, outDir, warm, ioctx)
		if produceErr != nil {
			return fmt.Errorf("cloud image-layers: %s: %w", spec.Name, produceErr)
		}
		if writeErr := imageLayersWriteRecord(outDir, record); writeErr != nil {
			return fmt.Errorf("cloud image-layers: %s: %w", spec.Name, writeErr)
		}
		records = append(records, record)
	}

	out := map[string]any{"status": "produced", "project": dir, "outputDir": outDir, "layers": records}
	summary := make([]string, 0, len(records))
	for _, record := range records {
		summary = append(summary, fmt.Sprintf("%s (%s, %d bytes)", record.artifact(), record.SHA256, record.Size))
	}
	clicore.WriteResult(out, params, ioctx,
		fmt.Sprintf("Produced %d layer(s) in %s: %s.", len(records), outDir, strings.Join(summary, ", ")))
	return nil
}

// imageLayersOutputDir is where produced layers land: inside the project, never
// beside it. This repository has a cross-worktree `.gen` poisoning history, so
// the producer writing anywhere else — and the task being cacheable — are both
// ways to serve one worktree's bytes to another.
func imageLayersOutputDir(projectDir string) string {
	return filepath.Join(projectDir, ".gen", "layers")
}

// imageLayersReadManifest loads and fully validates image-layers.json. Every
// check runs before any producer does: a manifest this binary cannot execute
// exactly as written must not produce a partial set of layers.
func imageLayersReadManifest(dir string) (imageLayersManifest, error) {
	manifestPath := filepath.Join(dir, imageLayersManifestName)
	data, err := os.ReadFile(manifestPath) //nolint:gosec // G304: a resolved project-root manifest path, not user input
	if errors.Is(err, os.ErrNotExist) {
		return imageLayersManifest{}, clicore.NewError(
			"cloud image-layers: no "+imageLayersManifestName+" at "+manifestPath+
				" — run from an image project that declares its layers (e.g. images/ci-runner)",
			clicore.ExitUsage)
	}
	if err != nil {
		return imageLayersManifest{}, fmt.Errorf("cloud image-layers: read %s: %w", manifestPath, err)
	}
	var manifest imageLayersManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return imageLayersManifest{}, fmt.Errorf("cloud image-layers: parse %s: %w", manifestPath, err)
	}
	if len(manifest.Layers) == 0 {
		return imageLayersManifest{}, clicore.NewError(
			"cloud image-layers: "+manifestPath+" declares no layers", clicore.ExitUsage)
	}
	seen := make(map[string]bool, len(manifest.Layers))
	for _, spec := range manifest.Layers {
		switch {
		case !imageLayersNamePattern.MatchString(spec.Name):
			return imageLayersManifest{}, clicore.NewError(
				fmt.Sprintf("cloud image-layers: %s declares an invalid layer name %q (lowercase letters, digits and dashes)", manifestPath, spec.Name),
				clicore.ExitUsage)
		case seen[spec.Name]:
			return imageLayersManifest{}, clicore.NewError(
				fmt.Sprintf("cloud image-layers: %s declares layer %q twice", manifestPath, spec.Name), clicore.ExitUsage)
		case imageLayerProducers[spec.Producer].produce == nil:
			return imageLayersManifest{}, clicore.NewError(
				fmt.Sprintf("cloud image-layers: %s declares layer %q with unknown producer %q (known: %s)",
					manifestPath, spec.Name, spec.Producer, strings.Join(imageLayersProducerNames(), ", ")),
				clicore.ExitUsage)
		}
		if _, err := imageLayersPrefix(spec.Path); err != nil {
			return imageLayersManifest{}, clicore.NewError(
				fmt.Sprintf("cloud image-layers: %s declares layer %q with %s", manifestPath, spec.Name, err.Error()),
				clicore.ExitUsage)
		}
		if validate := imageLayerProducers[spec.Producer].validate; validate != nil {
			if err := validate(spec); err != nil {
				return imageLayersManifest{}, clicore.NewError(
					fmt.Sprintf("cloud image-layers: %s declares layer %q with %s", manifestPath, spec.Name, err.Error()),
					clicore.ExitUsage)
			}
		}
		seen[spec.Name] = true
	}
	if err := imageLayersValidateSharedImages(manifestPath, manifest.Layers); err != nil {
		return imageLayersManifest{}, err
	}
	return manifest, nil
}

// imageLayersValidateSharedImages keeps two layers that extract from the SAME
// repository on the same reference. A layer produces exactly one file, so a
// `COPY --from=uv /uv /uvx` becomes two layers, and the image-owned pin is then
// written twice — the one place in this manifest where a value is duplicated.
// Duplicated is fine; silently diverging is not, because the image would ship a
// uv and a uvx from two different builds.
func imageLayersValidateSharedImages(manifestPath string, layers []imageLayerSpec) error {
	type reference struct{ name, tag, version, digest string }
	first := make(map[string]reference, len(layers))
	for _, spec := range layers {
		if spec.Image == "" {
			continue
		}
		current := reference{name: spec.Name, tag: spec.Tag, version: spec.Version, digest: spec.Digest}
		previous, ok := first[spec.Image]
		if !ok {
			first[spec.Image] = current
			continue
		}
		if previous.tag != current.tag || previous.version != current.version || previous.digest != current.digest {
			return clicore.NewError(
				fmt.Sprintf("cloud image-layers: %s declares layers %q and %q against the same image %q on different references — one image, one pin",
					manifestPath, previous.name, current.name, spec.Image),
				clicore.ExitUsage)
		}
	}
	return nil
}

// imageLayersProducerNames lists the registry in a stable order for diagnostics.
func imageLayersProducerNames() []string {
	names := make([]string, 0, len(imageLayerProducers))
	for name := range imageLayerProducers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// imageLayersPrefix turns a declared absolute in-image path into the tar entry
// prefix (no leading slash), the form a layer tarball uses.
func imageLayersPrefix(imagePath string) (string, error) {
	trimmed := strings.TrimSpace(imagePath)
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("a non-absolute in-image path %q", imagePath)
	}
	prefix := strings.Trim(path.Clean(trimmed), "/")
	if prefix == "" || prefix == "." {
		return "", fmt.Errorf("an in-image path %q that resolves to the filesystem root", imagePath)
	}
	return prefix, nil
}

// artifact names the produced file, relative to the output directory.
func (r imageLayerRecord) artifact() string {
	if r.Tar != "" {
		return r.Tar
	}
	return r.File
}

// imageLayersWriteRecord writes the sidecar next to the tar it describes.
func imageLayersWriteRecord(outDir string, record imageLayerRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the %s record: %w", record.Name, err)
	}
	recordPath := filepath.Join(outDir, record.Name+".json")
	if err := clicore.WriteFileAtomic(recordPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", recordPath, err)
	}
	return nil
}

// --- the go.dev/dl source seam ------------------------------------------------

// imageLayersGoPlatform is the runner's execution platform. Cloud Run runs
// linux/amd64, and the layer is produced for the image regardless of the host
// that produces it.
const imageLayersGoPlatform = "linux-amd64"

// imageLayersGoArchiveRoot is the single top-level directory the upstream Go
// tarball packs everything under.
const imageLayersGoArchiveRoot = "go"

// imageLayersGoPrune mirrors the retired Dockerfile's `rm -rf /usr/local/go/{test,doc,api,misc}`:
// the release tests, docs, API history and misc examples are not used by `go
// build/test` in the runner. Sorted so the record's `pruned` list is stable.
var imageLayersGoPrune = []string{"api", "doc", "misc", "test"}

const (
	// goDevDownloadBase is the published download root.
	goDevDownloadBase = "https://go.dev/dl/"
	// goDevIndexQuery selects go.dev's machine-readable download index — the
	// source of the sha256 each artifact must hash to. The bare
	// `<file>.sha256` URL serves an HTML redirect page, so the JSON index is
	// the only machine-readable form.
	goDevIndexQuery = "?mode=json&include=all"
)

// goDistFile is one artifact row in the go.dev download index.
type goDistFile struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
}

// goDistRelease is one release in the go.dev download index.
type goDistRelease struct {
	Version string       `json:"version"`
	Files   []goDistFile `json:"files"`
}

// imageLayersGoDist is the go.dev/dl surface the toolchain producer needs.
type imageLayersGoDist interface {
	// URL is filename's published location, recorded in the sidecar.
	URL(filename string) string
	// Checksum is the sha256 go.dev publishes for filename. An absent entry is
	// an error: producing a layer from an artifact nobody published a checksum
	// for is exactly the state this replaces.
	Checksum(filename string) (string, error)
	// Open streams filename's bytes.
	Open(ctx context.Context, filename string) (io.ReadCloser, error)
}

// imageLayersGoDistFor builds the go.dev source for a run. A package var so
// tests drive the whole producer — including the checksum-mismatch hard fail —
// from fixtures, with no network. The real verb keeps the real path.
var imageLayersGoDistFor = func(client *http.Client) imageLayersGoDist {
	return goDevDist{client: client}
}

// goDevDist reads the published go.dev download index and artifacts.
type goDevDist struct {
	client *http.Client
	// base overrides the published download root (it must end in "/"). Empty
	// means go.dev itself; tests point it at a local server so the index reader
	// and the download path are both exercised without a network.
	base string
}

func (d goDevDist) root() string {
	if d.base != "" {
		return d.base
	}
	return goDevDownloadBase
}

func (d goDevDist) URL(filename string) string { return d.root() + filename }

func (d goDevDist) Checksum(filename string) (string, error) {
	releases, err := readGoDevIndex(d.client, d.root()+goDevIndexQuery)
	if err != nil {
		return "", fmt.Errorf("read the go.dev download index: %w", err)
	}
	for _, release := range releases {
		for _, file := range release.Files {
			if file.Filename == filename {
				return strings.TrimSpace(file.SHA256), nil
			}
		}
	}
	return "", fmt.Errorf("the go.dev download index publishes no %s — the workspace pins a Go release go.dev does not list", filename)
}

func (d goDevDist) Open(ctx context.Context, filename string) (io.ReadCloser, error) {
	target := d.URL(filename)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", target, err)
	}
	clicore.SetUserAgent(req)
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	//nolint:gosec // G704: not user-controlled input — the URL is the published go.dev download root plus a filename this producer derived from the workspace's own go.work pin, and the bytes are sha256-verified against the published checksum before anything is done with them
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", target, clicore.StatusLine(resp.StatusCode))
	}
	return resp.Body, nil
}

// --- the Go toolchain producer ------------------------------------------------

// imageLayersProduceGoToolchain downloads the pinned Go toolchain, verifies it
// against the checksum go.dev publishes, prunes what the runner never uses, and
// re-emits the remainder as a normalized tar.
func imageLayersProduceGoToolchain(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, _ *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	version, err := imageBuildGoWorkVersion(workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if version == "" {
		return imageLayerRecord{}, fmt.Errorf(
			"go.work under %q declares no Go version — the toolchain layer's version is DERIVED from the workspace (imageBuildGoWorkVersion), never a literal, so there is nothing to produce",
			workspaceRoot)
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}

	filename := "go" + version + "." + imageLayersGoPlatform + ".tar.gz"
	dist := imageLayersGoDistFor(ioctx.Client)
	published, err := dist.Checksum(filename)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if !imageLayersSHA256Pattern.MatchString(published) {
		return imageLayerRecord{}, fmt.Errorf("the go.dev download index publishes an unreadable checksum %q for %s", published, filename)
	}

	// The scratch tree lives inside the project's own .gen so nothing is
	// written beside it and the final rename stays on one filesystem.
	work, err := os.MkdirTemp(outDir, ".work-")
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("create a scratch directory under %s: %w", outDir, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	archive := filepath.Join(work, "download.tar.gz")
	body, err := dist.Open(ctx, filename)
	if err != nil {
		return imageLayerRecord{}, err
	}
	downloaded, err := imageLayersDownload(body, archive, filename)
	if err != nil {
		return imageLayerRecord{}, err
	}
	// An unverified artifact never becomes a layer: this comparison happens
	// before a single entry is read out of the archive.
	if downloaded != published {
		return imageLayerRecord{}, fmt.Errorf(
			"checksum mismatch for %s: go.dev publishes %s, the download hashes to %s — refusing to build a layer from an unverified artifact",
			dist.URL(filename), published, downloaded)
	}
	ioctx.Stdout(fmt.Sprintf("Verified %s against the go.dev published sha256 %s.", filename, published))

	entries, err := imageLayersReadGoArchive(archive, prefix, work)
	if err != nil {
		return imageLayerRecord{}, err
	}

	tarPath := filepath.Join(outDir, spec.Name+".tar")
	sum, size, err := imageLayersWriteTarFile(tarPath, entries)
	if err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		Tar:      filepath.Base(tarPath),
		SHA256:   sum,
		Size:     size,
		Version:  version,
		Source:   imageLayerSource{URL: dist.URL(filename), SHA256: published},
		Pruned:   slices.Clone(imageLayersGoPrune),
	}, nil
}

// imageLayersDownload streams an opened artifact to dest, hashing as it lands,
// and returns the hex sha256 of what was actually received. It closes body.
func imageLayersDownload(body io.ReadCloser, dest, filename string) (string, error) {
	defer func() { _ = body.Close() }()

	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a path this function just created under its own scratch dir
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dest, err)
	}
	digest := sha256.New()
	// A body that reaches the cap is truncated, which cannot match the
	// published checksum — the bound fails closed instead of producing a short
	// layer.
	if _, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, imageLayersMaxDownloadBytes)); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("download %s: %w", filename, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", dest, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// imageLayerEntry is one normalized entry of the layer being emitted. It holds
// only what the output header needs — every other attribute of the source
// archive is deliberately dropped.
type imageLayerEntry struct {
	// name is the in-archive path with no leading slash; directories carry a
	// trailing "/".
	name     string
	typeflag byte
	mode     int64
	linkname string
	size     int64
	// blob is the spilled content of a regular file, named by read order so no
	// archive path ever reaches the host filesystem.
	blob string
}

// imageLayersReadGoArchive reads the verified toolchain tarball once: it
// rewrites each surviving entry under prefix, drops the pruned trees, and
// spills regular file contents into spillDir under sequential names.
func imageLayersReadGoArchive(archive, prefix, spillDir string) ([]imageLayerEntry, error) {
	file, err := os.Open(archive) //nolint:gosec // G304: the scratch path this producer just downloaded to
	if err != nil {
		return nil, fmt.Errorf("open the downloaded toolchain: %w", err)
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read the downloaded toolchain: %w", err)
	}
	defer func() { _ = gz.Close() }()

	reader := tar.NewReader(gz)
	budget := imageLayersMaxUnpackedBytes
	entries := make([]imageLayerEntry, 0, 16384)
	seen := map[string]bool{}
	for index := 0; ; index++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the downloaded toolchain: %w", err)
		}
		rel, keep, err := imageLayersGoRelPath(header.Name)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		entry := imageLayerEntry{name: imageLayersJoin(prefix, rel)}
		switch header.Typeflag {
		case tar.TypeDir:
			entry.name += "/"
			entry.typeflag = tar.TypeDir
			entry.mode = imageLayersDirMode
		case tar.TypeReg:
			entry.typeflag = tar.TypeReg
			entry.mode = imageLayersFileMode(header.Mode)
			entry.size = header.Size
			entry.blob = filepath.Join(spillDir, fmt.Sprintf("%08d", index))
			if err := imageLayersSpill(reader, entry.blob, header.Size, &budget); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			entry.typeflag = tar.TypeSymlink
			entry.mode = imageLayersSymlinkMode
			entry.linkname = header.Linkname
		case tar.TypeLink:
			// A hard link names another archive member, so it has to be
			// rewritten through the same rule as the entry itself.
			target, targetKept, err := imageLayersGoRelPath(header.Linkname)
			if err != nil {
				return nil, err
			}
			if !targetKept {
				return nil, fmt.Errorf("the Go tarball entry %q hard-links %q, which this producer prunes", header.Name, header.Linkname)
			}
			entry.typeflag = tar.TypeLink
			entry.mode = imageLayersFileMode(header.Mode)
			entry.linkname = imageLayersJoin(prefix, target)
		default:
			return nil, fmt.Errorf("the Go tarball entry %q has unsupported type %q — refusing to guess how it should be normalized", header.Name, string(header.Typeflag))
		}
		if seen[entry.name] {
			return nil, fmt.Errorf("the Go tarball declares %q twice — the layer's entry order would not be well defined", entry.name)
		}
		seen[entry.name] = true
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("the Go tarball contains no %s/ entries", imageLayersGoArchiveRoot)
	}
	return entries, nil
}

// imageLayersJoin appends a (possibly empty) relative path to the layer prefix.
func imageLayersJoin(prefix, rel string) string {
	if rel == "" {
		return prefix
	}
	return prefix + "/" + rel
}

// imageLayersGoRelPath maps an upstream archive name to its path under the
// layer prefix. It reports keep=false for the pruned trees, and errors on
// anything that escapes the archive root or sits outside `go/` — a layout
// change upstream must be loud, not silently half-copied.
func imageLayersGoRelPath(name string) (rel string, keep bool, err error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if clean == "." || clean == "" {
		return "", false, nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("the Go tarball entry %q escapes the archive root", name)
	}
	if clean == imageLayersGoArchiveRoot {
		return "", true, nil
	}
	rest, ok := strings.CutPrefix(clean, imageLayersGoArchiveRoot+"/")
	if !ok {
		return "", false, fmt.Errorf("the Go tarball entry %q is not under %s/ — the upstream archive layout changed", name, imageLayersGoArchiveRoot)
	}
	first, _, _ := strings.Cut(rest, "/")
	if slices.Contains(imageLayersGoPrune, first) {
		return "", false, nil
	}
	return rest, true, nil
}

// imageLayersFileMode reduces a source mode to the only distinction the layer
// keeps: executable or not. Everything else in the source mode (setuid bits, a
// group-writable bit from whoever packed the archive) is dropped by
// construction rather than trusted.
func imageLayersFileMode(mode int64) int64 {
	if mode&0o111 != 0 {
		return imageLayersFileModeExec
	}
	return imageLayersFileModePlain
}

// imageLayersSpill copies exactly size bytes of the current archive entry into
// dest, charging the shared unpack budget.
func imageLayersSpill(reader io.Reader, dest string, size int64, budget *int64) error {
	if size < 0 {
		return fmt.Errorf("the Go tarball declares a negative size %d", size)
	}
	if size > *budget {
		return fmt.Errorf("the Go tarball expands past the %d-byte unpack budget", imageLayersMaxUnpackedBytes)
	}
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a sequentially named path under this producer's own scratch dir
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.CopyN(file, reader, size); err != nil {
		_ = file.Close()
		return fmt.Errorf("read %d bytes of the Go tarball: %w", size, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	*budget -= size
	return nil
}

// imageLayersWriteTarFile writes the normalized tar to path (through a sibling
// temp file plus a rename, so an interrupted run never leaves a half tar for a
// later `putnami package` to hash) and returns its sha256 and size.
func imageLayersWriteTarFile(target string, entries []imageLayerEntry) (string, int64, error) {
	temp := target + ".tmp"
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G302,G304: a world-readable build output beside its declared target
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", temp, err)
	}
	digest := sha256.New()
	counter := &imageLayersCounter{hash: digest}
	if err := imageLayersWriteTar(io.MultiWriter(file, counter), entries); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return "", 0, fmt.Errorf("close %s: %w", temp, err)
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return "", 0, fmt.Errorf("rename %s: %w", temp, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), counter.written, nil
}

// imageLayersCounter hashes and counts the layer bytes in one pass.
type imageLayersCounter struct {
	hash    hash.Hash
	written int64
}

func (c *imageLayersCounter) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	return c.hash.Write(p)
}

// imageLayersWriteTar emits the layer: every ancestor directory synthesized,
// entries sorted lexicographically by their in-archive name, and every header
// field either fixed or derived from the entry itself. This is the function the
// content key depends on, so it reads exactly one thing off each entry and
// invents the rest.
func imageLayersWriteTar(w io.Writer, entries []imageLayerEntry) error {
	all := imageLayersWithParentDirs(entries)
	// A single lexicographic order over names that carry a trailing "/" for
	// directories: it is total, host-independent, and puts every directory
	// ahead of its own children ("a/" < "a/b").
	slices.SortFunc(all, func(a, b imageLayerEntry) int { return strings.Compare(a.name, b.name) })

	writer := tar.NewWriter(w)
	for _, entry := range all {
		header := &tar.Header{
			Typeflag: entry.typeflag,
			Name:     entry.name,
			Linkname: entry.linkname,
			Mode:     entry.mode,
			// Numeric root ownership with EMPTY owner names: a uname/gname
			// would be a string from whoever packed the source archive.
			Uid:   0,
			Gid:   0,
			Uname: "",
			Gname: "",
			// The epoch, and only the epoch. AccessTime/ChangeTime stay zero so
			// archive/tar never needs a PAX record to carry them.
			ModTime: imageLayersEpoch,
		}
		if entry.typeflag == tar.TypeReg {
			header.Size = entry.size
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write the %s header: %w", entry.name, err)
		}
		if entry.typeflag != tar.TypeReg {
			continue
		}
		if err := imageLayersCopyBlob(writer, entry); err != nil {
			return err
		}
	}
	return writer.Close()
}

// imageLayersCopyBlob streams one spilled file into the tar.
func imageLayersCopyBlob(writer io.Writer, entry imageLayerEntry) error {
	blob, err := os.Open(entry.blob) //nolint:gosec // G304: a sequentially named path this producer wrote under its own scratch dir
	if err != nil {
		return fmt.Errorf("open the spilled content of %s: %w", entry.name, err)
	}
	defer func() { _ = blob.Close() }()
	written, err := io.Copy(writer, blob)
	if err != nil {
		return fmt.Errorf("write %s: %w", entry.name, err)
	}
	if written != entry.size {
		return fmt.Errorf("write %s: wrote %d bytes for a %d-byte entry", entry.name, written, entry.size)
	}
	return nil
}

// imageLayersWithParentDirs adds an explicit directory entry for every ancestor
// the source archive did not declare. A layer that relies on the extractor to
// invent missing parents inherits that extractor's default mode; declaring them
// keeps the mode in the bytes.
func imageLayersWithParentDirs(entries []imageLayerEntry) []imageLayerEntry {
	seen := make(map[string]bool, len(entries)*2)
	for _, entry := range entries {
		seen[entry.name] = true
	}
	out := slices.Clone(entries)
	for _, entry := range entries {
		for dir := path.Dir(strings.TrimSuffix(entry.name, "/")); dir != "." && dir != "/"; dir = path.Dir(dir) {
			name := dir + "/"
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, imageLayerEntry{name: name, typeflag: tar.TypeDir, mode: imageLayersDirMode})
		}
	}
	return out
}

// --- single-file layers --------------------------------------------------------
//
// golangci-lint, staticcheck, crane and the putnami CLI are one file each. They
// are produced into `.gen/layers/bin/` and consumed by `files` layers, which the
// framework normalizes itself — so this half has no tar work, and the whole
// weight of it is proving what each file IS.
//
// That proof is host-side and EXECUTION-FREE, which is the property the
// retired Dockerfile's runtime assertions could not have: they ran a linux/amd64 binary, so
// they only work inside the image, on a machine with docker. `debug/buildinfo`
// reads the same embedded metadata `go version -m` prints, straight out of the
// ELF, from any host — so a darwin/arm64 laptop can prove the pinned toolchain
// and the pinned module version of a linux binary it just cross-compiled.

const (
	// imageLayersBinDir is the output subdirectory single-file layers land in.
	imageLayersBinDir = "bin"
	// imageLayersToolGOOS and imageLayersToolGOARCH are the runner's execution
	// platform. Cloud Run runs linux/amd64 whatever the producing host is.
	imageLayersToolGOOS   = "linux"
	imageLayersToolGOARCH = "amd64"
	// imageLayersToolGOAMD64 is the microarchitecture level every tool layer
	// targets. The compiled path sets it; the restored path asserts it. The
	// runner fleet is not guaranteed to be newer than the baseline.
	imageLayersToolGOAMD64 = "v1"
	// imageLayersToolFileMode is the mode every produced binary is written
	// with, so a loose host umask cannot reach the output tree.
	imageLayersToolFileMode os.FileMode = 0o755
)

// imageLayersToolVersionPattern is the shape an image-owned version pin may
// take: a bare semver, no leading `v`, exactly like the versions the workspace
// sources publish. The value becomes both a module requirement and the string
// the built binary's embedded module version must equal.
var imageLayersToolVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// imageLayersModulePattern is a coarse module/package path shape: enough to
// refuse a version suffix, a shell metacharacter or a stray space reaching the
// go command line, without re-implementing module path validation.
var imageLayersModulePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~/-]*$`)

// imageLayersValidateGoTool checks the fields a `go-tool` layer needs, with the
// rest of the manifest. The exclusive tool/version pair is the load-bearing
// rule: a layer whose version CAN be derived from a workspace source must
// derive it, or the manifest quietly becomes a second copy of the pin.
func imageLayersValidateGoTool(spec imageLayerSpec) error {
	if err := imageLayersValidateUnreadFields(spec, "package", "module", "tool", "version"); err != nil {
		return err
	}
	switch {
	case !imageLayersModulePattern.MatchString(spec.Package):
		return fmt.Errorf("an unusable go-tool package %q", spec.Package)
	case !imageLayersModulePattern.MatchString(spec.Module):
		return fmt.Errorf("an unusable go-tool module %q", spec.Module)
	case !strings.HasPrefix(spec.Package+"/", spec.Module+"/"):
		return fmt.Errorf("package %q outside its declared module %q", spec.Package, spec.Module)
	case spec.Tool != "" && spec.Version != "":
		return fmt.Errorf(
			"both a derived tool pin (%q) and an image-owned version (%q) — a version the workspace declares must be DERIVED, never copied",
			spec.Tool, spec.Version)
	case spec.Tool == "" && spec.Version == "":
		return errors.New("neither a `tool` to derive its version from nor an image-owned `version`")
	case spec.Version != "" && !imageLayersToolVersionPattern.MatchString(spec.Version):
		return fmt.Errorf("an unreadable image-owned version %q (bare semver, no leading v)", spec.Version)
	case spec.Tool != "" && !imageLayersNamePattern.MatchString(spec.Tool):
		return fmt.Errorf("an unusable tool name %q", spec.Tool)
	}
	return nil
}

// imageLayersValidateNoToolFields refuses every producer-specific field on a
// producer that reads none of them: a `version` nobody reads is a pin that
// looks maintained and is not.
func imageLayersValidateNoToolFields(spec imageLayerSpec) error {
	return imageLayersValidateUnreadFields(spec)
}

// imageLayersValidateUnreadFields refuses any producer-specific field the named
// producer does not read. `read` lists the ones it does.
func imageLayersValidateUnreadFields(spec imageLayerSpec, read ...string) error {
	for _, field := range []struct{ name, value string }{
		{"package", spec.Package}, {"module", spec.Module},
		{"tool", spec.Tool}, {"version", spec.Version},
		{"image", spec.Image}, {"tag", spec.Tag}, {"digest", spec.Digest},
		{"source", spec.Source}, {"pin", spec.Pin},
	} {
		if field.value != "" && !slices.Contains(read, field.name) {
			return fmt.Errorf("a %q field the %s producer does not read", field.name, spec.Producer)
		}
	}
	return nil
}

// imageLayersGoRun executes one `go` invocation for a tool build, streaming its
// output through the command IO. A package var so tests drive the whole
// producer — including the verification failures — without a compiler or a
// network.
var imageLayersGoRun = func(ctx context.Context, dir string, env, args []string, ioctx clicore.IO) error {
	tool, err := imageBuildLookPath("go")
	if err != nil {
		return fmt.Errorf(
			"the go command is not on PATH — the tool layers are CROSS-COMPILED host-side with the pinned toolchain, so a Go install is required even on a machine with no docker: %w", err)
	}
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // G204: argv is built by imageLayersGoBuildArgs from manifest fields this file validated; compiling the pinned tool is the feature
	cmd.Dir = dir
	cmd.Env = env
	stdout := &imageBuildLineWriter{emit: ioctx.Stdout}
	stderr := &imageBuildLineWriter{emit: ioctx.Stderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	stdout.flush()
	stderr.flush()
	return runErr
}

// imageLayersReadBuildInfo reads a binary's embedded build info — the library
// form of `go version -m`, and the reason verification needs no linux runtime.
// A package var so tests exercise the verification rules against constructed
// build info instead of compiling four binaries.
var imageLayersReadBuildInfo = buildinfo.ReadFile

// imageLayersProduceGoTool produces one pinned Go tool layer for linux/amd64.
//
// There are two kinds, and they no longer reach the bytes the same way:
//
//   - A DERIVED tool (`tool` names a key in the @putnami/go extension's
//     tools/versions.json) is RESTORED from the same lock-pinned linux/amd64
//     extension artifact the warm store layer bakes. The framework builds
//     golangci-lint and staticcheck once per release and ships them at
//     compiled/tools/<tool>, so compiling them a second
//     time here bought nothing but a very large build — and it was the only
//     reason the CI entrypoint had to pre-download their module graphs before
//     the offline task graph.
//   - An IMAGE-OWNED tool (`version` is the manifest's own pin, today only
//     crane) has no upstream artifact to restore from, so it is still
//     cross-compiled with the toolchain go.work pins.
//
// Both end at the same place: an execution-free check of the binary's own
// embedded build info before it becomes a layer.
func imageLayersProduceGoTool(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, warm *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	if spec.Tool != "" {
		return imageLayersRestoreGoTool(ctx, spec, workspaceRoot, outDir, warm, ioctx)
	}
	return imageLayersCompileGoTool(ctx, spec, workspaceRoot, outDir, ioctx)
}

// imageLayersCompileGoTool cross-compiles one image-owned Go tool for
// linux/amd64 with the pinned toolchain and verifies the result without running
// it.
func imageLayersCompileGoTool(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, ioctx clicore.IO) (imageLayerRecord, error) {
	goVersion, err := imageBuildGoWorkVersion(workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if goVersion == "" {
		return imageLayerRecord{}, fmt.Errorf(
			"go.work under %q declares no Go version — %s must be built with the toolchain the workspace pins (a mismatch panics `putnami lint` on every run), and that pin is DERIVED, never a literal",
			workspaceRoot, spec.Name)
	}
	version, err := imageLayersToolVersion(spec, workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}

	// The scratch module lives inside the project's own .gen so nothing is
	// written beside it and the final rename stays on one filesystem.
	work, err := os.MkdirTemp(outDir, ".work-")
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("create a scratch directory under %s: %w", outDir, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(imageLayersGoMod(goVersion, spec.Module, version)), 0o600); err != nil {
		return imageLayerRecord{}, fmt.Errorf("write the scratch go.mod for %s: %w", spec.Name, err)
	}
	built := filepath.Join(work, spec.Name)
	reference := spec.Package + "@v" + version
	ioctx.Stdout(fmt.Sprintf("Building %s for %s/%s with go%s …", reference, imageLayersToolGOOS, imageLayersToolGOARCH, goVersion))
	if err := imageLayersGoRun(ctx, work, imageLayersGoEnv(goVersion), imageLayersGoBuildArgs(spec.Package, built), ioctx); err != nil {
		return imageLayerRecord{}, fmt.Errorf("build %s: %w", reference, err)
	}

	info, err := imageLayersReadBuildInfo(built)
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("read the build info of the %s binary: %w", spec.Name, err)
	}
	if err := imageLayersVerifyGoTool(info, goVersion, spec.Module, version); err != nil {
		return imageLayerRecord{}, fmt.Errorf("%s: %w", reference, err)
	}
	ioctx.Stdout(fmt.Sprintf("Verified %s: built with %s, module %s %s (embedded build info, not executed).",
		spec.Name, info.GoVersion, info.Main.Path, info.Main.Version))

	sum, size, err := imageLayersInstallFile(built, outDir, spec.Name)
	if err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		File:     imageLayersBinDir + "/" + spec.Name,
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersGoToolLabel(spec, version, ""),
		Source: imageLayerSource{
			Package:   reference,
			Module:    info.Main.Path,
			Toolchain: info.GoVersion,
		},
	}, nil
}

// imageLayersRestoreGoTool takes a DERIVED tool's linux/amd64 binary out of the
// lock-pinned @putnami/go artifact instead of compiling it.
//
// The bytes come from the SAME materialized store the warm layer bakes, so a
// run downloads that artifact once no matter how many layers read it, and the
// tool the image bakes at /usr/local/bin is byte-identical to the tool the
// warmed extension would install into a checkout.
func imageLayersRestoreGoTool(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, warm *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	goVersion, err := imageBuildGoWorkVersion(workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if goVersion == "" {
		return imageLayerRecord{}, fmt.Errorf(
			"go.work under %q declares no Go version — %s is only usable against the Go minor the workspace pins, and that pin is DERIVED, never a literal",
			workspaceRoot, spec.Name)
	}
	version, err := imageLayersToolVersion(spec, workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}

	store, artifacts, err := warm.open(ctx, ioctx)
	if err != nil {
		return imageLayerRecord{}, err
	}
	source, err := imageLayersWarmToolSource(artifacts)
	if err != nil {
		return imageLayerRecord{}, err
	}
	restored := filepath.Join(imageLayersWarmArtifactDir(store, source), filepath.FromSlash(imageLayersWarmToolBinary(spec.Tool)))
	reference := spec.Package + "@v" + version
	ioctx.Stdout(fmt.Sprintf("Restoring %s for %s/%s from %s %s …",
		reference, imageLayersToolGOOS, imageLayersToolGOARCH, source.name, source.version))

	info, err := imageLayersReadBuildInfo(restored)
	if errors.Is(err, os.ErrNotExist) {
		return imageLayerRecord{}, fmt.Errorf(
			"the materialized %s %s carries no %s — the layer restores the pinned tool from the extension artifact and never compiles a replacement",
			source.name, source.version, imageLayersWarmToolBinary(spec.Tool))
	}
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("read the build info of the restored %s binary: %w", spec.Name, err)
	}
	if err := imageLayersVerifyRestoredGoTool(info, goVersion, spec, version); err != nil {
		return imageLayerRecord{}, fmt.Errorf("%s: %w", reference, err)
	}
	ioctx.Stdout(fmt.Sprintf("Verified %s: built with %s, module %s %s, command %s (embedded build info, not executed).",
		spec.Name, info.GoVersion, info.Main.Path, info.Main.Version, info.Path))

	// The store is lent, not owned: copy into this producer's own scratch so
	// imageLayersInstallFile's rename stays the atomicity boundary and the layer
	// the warm producer emits next still carries the artifact intact.
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
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		File:     imageLayersBinDir + "/" + spec.Name,
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersGoToolLabel(spec, version, imageLayersGoToolArtifactLabel(source)),
		Source: imageLayerSource{
			Package:   reference,
			Module:    info.Main.Path,
			Toolchain: info.GoVersion,
			Extensions: []imageLayerExtension{
				{Name: source.name, Version: source.version, SHA256: source.sha256},
			},
		},
	}, nil
}

// imageLayersCopyFile copies a regular file's bytes. The destination is created
// exclusively, so a stale scratch entry is an error rather than a silent
// overwrite of whatever a previous run left behind.
func imageLayersCopyFile(source, dest string) error {
	input, err := os.Open(source) //nolint:gosec // G304: a path built from this run's own scratch store and a lock-recorded digest
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	return imageLayersCopyEntry(input, dest, info.Size())
}

// imageLayersGoToolLabel is what a compiled tool layer records as its version,
// and therefore what `--check` compares the manifest against on the next run.
//
// It is not the version alone. What selects these bytes is the whole build
// input — the module required at that version and the main package built out of
// it — and both live in the manifest, where a hand edit moves them without
// moving any version string: repoint `package` at a sibling command and the
// produced binary is a DIFFERENT program under an unchanged pin. Folding the
// three into one label is the same shape the warm layer already uses
// (imageLayersWarmLabel), and the point of it is that produce and check read
// this one function rather than two agreeing derivations.
//
// The `tool` key itself is deliberately absent: it names where the version was
// DERIVED from, not which bytes were selected, so switching a layer between a
// derived and an image-owned pin of the same value is not staleness.
//
// `artifact` is the fourth input, and it exists because a RESTORED tool is not
// selected by the build input at all: the bytes are whatever the pinned
// @putnami/go release shipped. The framework can republish the same
// golangci-lint pin built with another Go, and the layer would then be stale at
// an unchanged module, version and package — so the supplying artifact's
// version and integrity are part of the identity. A compiled tool passes "" and
// its label stays module, version and package, so crane's `--check` answer
// does not change.
func imageLayersGoToolLabel(spec imageLayerSpec, version, artifact string) string {
	label := spec.Module + " v" + version + " " + spec.Package
	if artifact != "" {
		label += " from " + artifact
	}
	return label
}

// imageLayersGoToolArtifactLabel names the extension release a restored tool's
// bytes came out of: the version the lock pins and the linux/amd64 integrity
// the store is content-addressed by. Both are committed, so `--check` re-derives
// this without materializing anything.
func imageLayersGoToolArtifactLabel(artifact imageLayersWarmArtifact) string {
	return artifact.name + " " + artifact.version + " " + artifact.sha256
}

// imageLayersGoToolArtifact resolves the label's artifact half for one layer:
// empty for an image-owned tool, and the lock-pinned tool-bearing extension for
// a derived one. Produce and check share it so neither invents its own.
func imageLayersGoToolArtifact(spec imageLayerSpec, workspaceRoot string) (string, error) {
	if spec.Tool == "" {
		return "", nil
	}
	artifact, err := imageLayersWarmToolArtifact(workspaceRoot)
	if err != nil {
		return "", err
	}
	return imageLayersGoToolArtifactLabel(artifact), nil
}

// imageLayersToolVersion resolves a tool's pinned version: derived from the
// @putnami/go extension's tools/versions.json when the layer names a `tool`,
// otherwise the image-owned pin the manifest declares. A derived pin that reads
// back empty is a hard failure — the extension manifest is the source of truth
// and falling back to a literal is exactly what this design forbids.
func imageLayersToolVersion(spec imageLayerSpec, workspaceRoot string) (string, error) {
	if spec.Tool == "" {
		return spec.Version, nil
	}
	version, err := imageBuildGoToolVersion(spec.Tool)(workspaceRoot)
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf(
			"%s declares no %q version — the layer's pin is DERIVED from the @putnami/go extension manifest (run `putnami install`), never a literal, so there is nothing to build",
			imageBuildGoToolPath(workspaceRoot), spec.Tool)
	}
	if !imageLayersToolVersionPattern.MatchString(version) {
		return "", fmt.Errorf("%s declares an unreadable %q version %q", imageBuildGoToolPath(workspaceRoot), spec.Tool, version)
	}
	return version, nil
}

// imageLayersGoMod is the scratch main module the tool is built from. It exists
// because `go build` has no `pkg@version` form and `go install pkg@version`
// refuses to write a CROSS-COMPILED binary to an explicit GOBIN; requiring the
// tool module at its exact version reproduces the same resolution — module
// versions come from that module's own go.mod through MVS, so nothing here
// depends on the host.
func imageLayersGoMod(goVersion, module, version string) string {
	return "module putnami.local/imagelayers\n\ngo " + goVersion + "\n\nrequire " + module + " v" + version + "\n"
}

// imageLayersGoEnv is the build environment. Everything that can reach the
// output bytes is set EXPLICITLY (an environment variable overrides the host's
// `go env -w` file), so the same inputs compile to the same binary on any
// machine:
//
//   - GOTOOLCHAIN pins the compiler to the workspace's Go, downloading it if the
//     host runs another one. This is the whole point of the layer.
//   - GOOS/GOARCH/GOAMD64/CGO_ENABLED fix the target, including the microarch
//     level and the pure-Go build the runner relies on.
//   - GOWORK=off keeps this repository's go.work — the scratch module is created
//     UNDER it — from joining the build.
//   - GOFLAGS and GOEXPERIMENT are emptied: a host `-mod=vendor` or an opt-in
//     experiment would silently change what gets compiled.
//
// Proxy and module-cache configuration is deliberately left to the host: it
// decides where the source comes FROM, while Go's checksum database decides
// whether those bytes are authentic.
func imageLayersGoEnv(goVersion string) []string {
	return append(os.Environ(),
		"GOTOOLCHAIN=go"+goVersion,
		"GOOS="+imageLayersToolGOOS,
		"GOARCH="+imageLayersToolGOARCH,
		"GOAMD64="+imageLayersToolGOAMD64,
		"CGO_ENABLED=0",
		"GOWORK=off",
		"GO111MODULE=on",
		"GOFLAGS=",
		"GOEXPERIMENT=",
	)
}

// imageLayersGoBuildArgs is the build argv. Split out so a unit test pins the
// exact shape without a compiler: -p=2 bounds the tool build's internal package
// fan-out to the two CPU units its pipeline steps reserve with cpuWeight=2. That
// makes the subprocess ceiling and scheduler admission agree on both small and
// standard runners instead of hiding oversubscription inside one task. -trimpath
// drops the build's absolute paths, -buildvcs=false drops the repository stamp
// the scratch module would otherwise inherit from whatever checkout .gen sits
// in, and both are what make two runs on two machines byte-equal. -ldflags='-s
// -w' matches the retired Dockerfile's stripped install. The explicit -p
// survives the hermetic GOFLAGS reset above instead of inheriting host policy.
func imageLayersGoBuildArgs(pkg, output string) []string {
	return []string{"build", "-p=2", "-mod=mod", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", output, pkg}
}

// imageLayersVerifyGoTool is the execution-free check: the binary's own
// embedded build info must report the pinned toolchain, the pinned module at the
// pinned version, and the linux/amd64 pure-Go settings. It mirrors the
// retired Dockerfile's `go version -m … | grep` assertions, minus their need for a
// linux runtime — and every failure names what was found, because "the tool is
// on the wrong Go" is otherwise diagnosed from a panic in an unrelated CI run.
func imageLayersVerifyGoTool(info *debug.BuildInfo, goVersion, module, version string) error {
	if info == nil {
		return errors.New("the binary carries no embedded build info — it was not produced by the Go toolchain")
	}
	if want := "go" + goVersion; info.GoVersion != want {
		return fmt.Errorf(
			"built with %s, not the pinned %s — a lint tool on the wrong toolchain panics on every `putnami lint` run",
			info.GoVersion, want)
	}
	if info.Main.Path != module {
		return fmt.Errorf("built from module %q, not the declared %q", info.Main.Path, module)
	}
	if want := "v" + version; info.Main.Version != want {
		return fmt.Errorf("reports module version %q, not the pinned %q", info.Main.Version, want)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{
		"GOOS":        imageLayersToolGOOS,
		"GOARCH":      imageLayersToolGOARCH,
		"CGO_ENABLED": "0",
		"-trimpath":   "true",
	} {
		if got := settings[key]; got != want {
			return fmt.Errorf("was built with %s=%q, want %q", key, got, want)
		}
	}
	return nil
}

// imageLayersVerifyRestoredGoTool is the same execution-free proof for a tool
// this producer did NOT build. Three of the questions are unchanged — which
// module, at which version, for which platform — and three deliberately differ:
//
//   - The MICROARCHITECTURE is now checked (GOAMD64). The compiled path pins it
//     to v1 in imageLayersGoEnv, so it holds there BY CONSTRUCTION; a restored
//     binary was built on another repository's machine, where nothing clears an
//     ambient GOAMD64. Go records the setting for every amd64 build, so the
//     answer is in the bytes: a tool compiled at v3 executes AVX2 instructions
//     and dies with SIGILL on a pre-Haswell CI cell, diagnosed from a crash in
//     an unrelated run.
//   - The MAIN PACKAGE is now checked (info.Path). A compiled tool is selected
//     by the package this producer handed `go build`; a restored one is selected
//     by a file name inside someone else's archive, so `package` would otherwise
//     become a manifest field nobody reads. Checking it proves the archive
//     shipped the pinned command and not a sibling out of the same module.
//   - The TOOLCHAIN is compared by Go MINOR, not exactly. The upstream tool is
//     built with the FRAMEWORK's Go, which is a different repository's pin and
//     will legitimately sit on another patch release. The patch was never what
//     the old check protected: a lint tool panics — "unsupported version" out of
//     go/types — when its Go is a MINOR behind the language the workspace pins,
//     and that is exactly the granularity the framework itself restores at
//     (`InstallTool` reuses the shipped binary when its Go
//     minor matches the local toolchain and compiles otherwise). Requiring the
//     same minor here means the image runs the tool every developer's checkout
//     runs. A divergence fails the produce with both minors named, because the
//     image has no compile fallback left and a quiet mismatch resurfaces as a
//     lint panic in an unrelated CI run.
//
// -trimpath is not asserted: it is the framework's build flag, not this
// producer's, and a restored layer's reproducibility comes from the artifact's
// lock integrity rather than from flags this repository chose.
func imageLayersVerifyRestoredGoTool(info *debug.BuildInfo, goVersion string, spec imageLayerSpec, version string) error {
	if info == nil {
		return errors.New("the binary carries no embedded build info — it was not produced by the Go toolchain")
	}
	if want, got := imageLayersGoMinor("go"+goVersion), imageLayersGoMinor(info.GoVersion); got != want {
		return fmt.Errorf(
			"built with %s (%s), and the workspace pins %s (%s) — a lint tool a Go minor away from the language it analyses fails every `putnami lint` run, and a restored layer has no compile fallback",
			info.GoVersion, got, "go"+goVersion, want)
	}
	if info.Main.Path != spec.Module {
		return fmt.Errorf("built from module %q, not the declared %q", info.Main.Path, spec.Module)
	}
	if want := "v" + version; info.Main.Version != want {
		return fmt.Errorf("reports module version %q, not the pinned %q", info.Main.Version, want)
	}
	if info.Path != spec.Package {
		return fmt.Errorf("is the %q command, not the declared %q", info.Path, spec.Package)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{
		"GOOS":        imageLayersToolGOOS,
		"GOARCH":      imageLayersToolGOARCH,
		"GOAMD64":     imageLayersToolGOAMD64,
		"CGO_ENABLED": "0",
	} {
		if got := settings[key]; got != want {
			return fmt.Errorf("was built with %s=%q, want %q", key, got, want)
		}
	}
	return nil
}

// imageLayersGoMinor reduces a Go version to the language minor it implements
// ("go1.26.1" and "go1.26" both answer "go1.26"). That is the axis a type
// checker cares about; the patch is a bug-fix release of the same language.
func imageLayersGoMinor(version string) string {
	digits := strings.TrimPrefix(version, "go")
	major, rest, found := strings.Cut(digits, ".")
	if !found {
		return "go" + digits
	}
	minor, _, _ := strings.Cut(rest, ".")
	return "go" + major + "." + minor
}

// imageLayersInstallFile moves a produced file into <outDir>/bin/<name> and
// returns its sha256 and size. The rename is the atomicity boundary: an
// interrupted run leaves no half-written binary for a later `putnami package`
// to hash, and the scratch source is inside the same .gen tree, so the rename
// never crosses a filesystem.
func imageLayersInstallFile(source, outDir, name string) (string, int64, error) {
	binDir := filepath.Join(outDir, imageLayersBinDir)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("create %s: %w", binDir, err)
	}
	if err := os.Chmod(source, imageLayersToolFileMode); err != nil {
		return "", 0, fmt.Errorf("set the mode of %s: %w", name, err)
	}
	sum, size, err := imageLayersHashFile(source)
	if err != nil {
		return "", 0, err
	}
	target := filepath.Join(binDir, name)
	if err := os.Rename(source, target); err != nil {
		return "", 0, fmt.Errorf("rename %s to %s: %w", source, target, err)
	}
	return sum, size, nil
}

// imageLayersHashFile hashes a produced file and reports its size.
func imageLayersHashFile(path string) (string, int64, error) {
	file, err := os.Open(path) //nolint:gosec // G304: a path this producer just wrote under its own scratch dir
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

// --- the putnami CLI producer ---------------------------------------------------

// imageLayersCLIPlatform is the lock's integrity key for the artifact this
// layer carries. The lock records one digest per platform, and the runner needs
// the linux/amd64 one whatever host produces the layer.
const imageLayersCLIPlatform = imageLayersToolGOOS + "/" + imageLayersToolGOARCH

// putServerDownloadURL is the put-server versioned download endpoint. It is
// used instead of install.sh deliberately: the installer v-prefixes semver-ish
// versions while the registry only knows the un-prefixed string, so a pinned
// install.sh run 404s.
const putServerDownloadURL = putServerOrigin + "/" + imageLayersCLINamespace + "/" + imageLayersCLIPackage + "/download"

// putServerOrigin is the published put-server origin, and putnami/cli the
// package the CLI layer downloads from it.
const (
	putServerOrigin         = "https://put.putnami.dev"
	imageLayersCLINamespace = "putnami"
	imageLayersCLIPackage   = "cli"
)

// imageLayersCLIDist is the put-server surface the CLI producer needs.
type imageLayersCLIDist interface {
	// URL is the published location of the linux/amd64 CLI at version.
	URL(version string) string
	// Open streams that artifact's bytes.
	Open(ctx context.Context, version string) (io.ReadCloser, error)
}

// imageLayersCLIDistFor builds the put-server source for a run. A package var
// so tests drive the producer — including the integrity mismatch — from
// fixtures, with no network.
var imageLayersCLIDistFor = func(client *http.Client) imageLayersCLIDist {
	return putServerDist{client: client}
}

// imageLayersRegistryToken resolves the native registry's host-only credential
// through the shared framework seam. Image packaging can consume an immutable
// private CLI without changing its visibility or inventing a package grant.
var imageLayersRegistryToken = registrycred.ResolveToken

// putServerDist downloads the published CLI through put-server's generated
// client.
type putServerDist struct {
	client *http.Client
	// base overrides the put-server origin; tests point it at a local server
	// so the real query shape and streaming path are both exercised.
	base string
}

func (d putServerDist) origin() string {
	if d.base != "" {
		return strings.TrimRight(d.base, "/")
	}
	return putServerOrigin
}

func (d putServerDist) endpoint() string {
	if d.base == "" {
		return putServerDownloadURL
	}
	return d.origin() + "/" + imageLayersCLINamespace + "/" + imageLayersCLIPackage + "/download"
}

func (d putServerDist) URL(version string) string {
	query := url.Values{
		"channel": {version},
		"os":      {imageLayersToolGOOS},
		// The endpoint speaks the JavaScript arch vocabulary, not Go's.
		"arch": {"x64"},
	}
	return d.endpoint() + "?" + query.Encode()
}

// Open requests the same URL as URL(version): the generated client sends the
// root download route with the same sorted query. A credential travels only
// when the native registry holds one for this host; without one the call is
// anonymous, as the public install path is. The generated client refuses
// redirects, so a credential cannot escape to another origin, and bounds the
// call and its body to the operation's five minutes.
func (d putServerDist) Open(ctx context.Context, version string) (io.ReadCloser, error) {
	target := d.URL(version)
	origin := d.origin()
	put, err := clicore.NewServiceClient[putserverclient.PutClient](putserverclient.RegisterPutClient,
		clicore.ForwardedServiceBinding(origin, d.client, putServerReaderProfile))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", target, err)
	}
	if parsed, err := url.Parse(origin); err == nil {
		if token, _ := imageLayersRegistryToken(parsed.Host); token != "" {
			ctx = client.WithForwardedUserToken(ctx, token)
		}
	}
	channel, goos, arch := version, imageLayersToolGOOS, "x64"
	answer, err := put.GetDownload(ctx, putserverclient.GetDownloadInput{
		Path:  putserverclient.GetDownloadPath{Namespace: imageLayersCLINamespace, Package: imageLayersCLIPackage},
		Query: putserverclient.GetDownloadQuery{Channel: &channel, Os: &goos, Arch: &arch},
	})
	if status := clicore.ServiceStatus(err); status != 0 {
		return nil, fmt.Errorf("download %s: %s", target, clicore.StatusLine(status))
	}
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", target, err)
	}
	return answer.Body, nil
}

// imageLayersProducePutnamiCLI downloads the CLI the workspace lock pins and
// verifies it against the sha256 that same lock records.
//
// This replaces the retired Dockerfile's `--version` smoke test, which proved only that
// the download executes: an HTML error body is caught, a substituted binary is
// not. The lock already carries per-platform integrity digests, so the layer
// gets cryptographic provenance for free — and gets it on a host that cannot
// run the artifact at all.
func imageLayersProducePutnamiCLI(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, _ *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	version, integrity, err := imageLayersLockCLI(workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}

	work, err := os.MkdirTemp(outDir, ".work-")
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("create a scratch directory under %s: %w", outDir, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	dist := imageLayersCLIDistFor(ioctx.Client)
	body, err := imageLayersCachedCLI(integrity)
	if err != nil {
		return imageLayerRecord{}, err
	}
	if body == nil {
		body, err = dist.Open(ctx, version)
		if err != nil {
			return imageLayerRecord{}, err
		}
	}
	download := filepath.Join(work, spec.Name)
	downloaded, err := imageLayersDownload(body, download, "the putnami CLI "+version)
	if err != nil {
		return imageLayerRecord{}, err
	}
	// An unverified artifact never becomes a layer.
	if downloaded != integrity {
		return imageLayerRecord{}, fmt.Errorf(
			"integrity mismatch for %s: putnami.lock.json records %s for %s, the download hashes to %s — refusing to build a layer from an unverified artifact",
			dist.URL(version), integrity, imageLayersCLIPlatform, downloaded)
	}
	ioctx.Stdout(fmt.Sprintf("Verified the putnami CLI %s against the lock's %s integrity %s.", version, imageLayersCLIPlatform, integrity))

	sum, size, err := imageLayersInstallFile(download, outDir, spec.Name)
	if err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		File:     imageLayersBinDir + "/" + spec.Name,
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersCLILabel(version, integrity),
		Source:   imageLayerSource{URL: dist.URL(version), SHA256: integrity},
	}, nil
}

// imageLayersCachedCLI reuses the launcher's content-addressed CLI store. The
// hosted runner seeds this same entry before handing the store to the checkout.
// The producer still hashes the bytes against the target platform's lock digest;
// a cache path, version label or executable bit is never integrity evidence.
func imageLayersCachedCLI(integrity string) (io.ReadCloser, error) {
	root := os.Getenv("PUTNAMI_ARTIFACT_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		root = filepath.Join(home, ".putnami", "artifacts")
	}
	file, err := os.Open(filepath.Join(root, "cli", integrity, "putnami")) //nolint:gosec // G304: exact lock-derived digest in the configured launcher store; bytes are verified before installation
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cached putnami CLI: %w", err)
	}
	return file, nil
}

// imageLayersCLILabel is what the CLI layer records as its version, and what
// `--check` compares the lock against on the next run: the pinned version AND
// the integrity it was verified against, because the lock declares both and
// either one moving makes the produced binary something this checkout no longer
// describes. A re-published build under an unchanged version string is exactly
// the case the version alone reads as current — the same hole the warm layer
// closed by folding its integrities into its own label.
func imageLayersCLILabel(version, integrity string) string {
	return version + " " + integrity
}

// imageLayersLockCLI reads the CLI pin AND its linux/amd64 integrity from
// putnami.lock.json.
//
// Both are hard requirements here, unlike the pin table itself, where an absent
// workspace source simply derives nothing: this producer has nothing to
// download without the version and nothing to check the download against
// without the digest. The integrity entry has gone missing across CLI bumps
// before, and the failure mode of tolerating that is a runner image built from
// whatever the endpoint happened to serve.
func imageLayersLockCLI(workspaceRoot string) (version, integrity string, err error) {
	lockPath := filepath.Join(workspaceRoot, "putnami.lock.json")
	var lock struct {
		CLI struct {
			Version     string            `json:"version"`
			Integrities map[string]string `json:"integrities"`
		} `json:"cli"`
	}
	if err := imageBuildReadJSON(lockPath, &lock); err != nil {
		return "", "", err
	}
	version = strings.TrimSpace(lock.CLI.Version)
	if version == "" {
		return "", "", fmt.Errorf(
			"%s pins no cli.version — the CLI layer's version is DERIVED from the workspace lock, never a literal, so there is nothing to download",
			lockPath)
	}
	integrity = strings.TrimSpace(lock.CLI.Integrities[imageLayersCLIPlatform])
	if integrity == "" {
		return "", "", fmt.Errorf(
			"%s records no cli.integrities[%q] for version %s — refusing to bake a CLI nothing can verify (the entry has gone missing across bumps before; re-run `putnami install` to repopulate it)",
			lockPath, imageLayersCLIPlatform, version)
	}
	if !imageLayersSHA256Pattern.MatchString(integrity) {
		return "", "", fmt.Errorf("%s records an unreadable cli.integrities[%q] value %q", lockPath, imageLayersCLIPlatform, integrity)
	}
	return version, integrity, nil
}

// --- the official-image producer ------------------------------------------------
//
// bun, node, uv and uvx are not built here — upstream publishes them inside
// official images, and the retired Dockerfile took them with four `COPY --from`
// stages. Those stages are the last reason the runner image needs a docker
// daemon to assemble at all, so this producer does the same extraction
// DAEMONLESSLY: resolve the reference over the registry API, pull the
// linux/amd64 manifest, and read exactly the COPY'd path out of the layer
// stream.
//
// The derivation rule (image invariant 2) splits the four in two:
//
//   - bun and node have a workspace source. package.json's `packageManager` and
//     `engines.node` are the pins, read through the SAME imageBuildPins() readers
//     every other derived axis resolves through, so no two consumers of the
//     workspace can resolve different versions. Their
//     digests are RESOLVED at produce time and recorded in the sidecar as
//     evidence — committing one would create a second pin nobody bumps with the
//     first, which is exactly what derived pins exist to avoid.
//   - uv has none: no workspace file declares a Python toolchain. Its tag AND
//     digest are therefore image-owned and live in image-layers.json, relocated
//     from the retired Dockerfile's `ARG UV_VERSION` and its `FROM …@sha256:…`. Here the
//     digest IS the pin, so a reference that resolves to anything else is a
//     substituted image and fails hard.
//
// And an unverified artifact never becomes a layer (image invariant 3): the
// digest check runs before a scratch directory even exists, and the extracted
// file is installed into `.gen/layers/bin/` only after it has been found, at the
// declared path, as a regular file. The retired Dockerfile's `bun --version` style
// assertions are not reproduced: they execute a linux/amd64 binary, which the
// producing host generally cannot do. Those checks belong to the image test
// suite, which runs where the image runs.

const (
	// imageLayersOCIVersionPlaceholder is what a layer's `tag` template
	// substitutes the resolved version into. It exists so the tag SHAPE
	// ("{version}-slim") lives in the manifest while the value stays derived.
	imageLayersOCIVersionPlaceholder = "{version}"
	// imageLayersOpaqueWhiteout empties its own directory's lower contents in
	// the OCI layer format, and imageLayersWhiteoutPrefix marks one deleted
	// name. Both mean the same thing to this producer: the path it was asked to
	// extract is gone by the time the image is assembled.
	imageLayersOpaqueWhiteout = ".wh..wh..opq"
	imageLayersWhiteoutPrefix = ".wh."
)

// imageLayersImagePattern is the shape a declared repository may take: bare
// path segments, no tag and no digest. The tag is built from the derived
// version, and letting one arrive inside `image` would put a version literal
// back in the manifest through the side door.
var imageLayersImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// imageLayersDigestPattern is the shape an image-owned digest pin must have.
var imageLayersDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imageLayersValidateOCIFile checks the fields an `oci-file` layer needs, with
// the rest of the manifest. Two rules carry the design: a version the workspace
// declares must be DERIVED through `pin`, and a digest may only accompany an
// image-owned `version` — a committed digest beside a derived pin is a second
// pin that looks like provenance.
func imageLayersValidateOCIFile(spec imageLayerSpec) error {
	if err := imageLayersValidateUnreadFields(spec, "image", "tag", "digest", "source", "pin", "version"); err != nil {
		return err
	}
	switch {
	case !imageLayersImagePattern.MatchString(spec.Image):
		return fmt.Errorf("an unusable image repository %q (repository only: the tag is built from the version, and a digest belongs in `digest`)", spec.Image)
	case !strings.Contains(spec.Tag, imageLayersOCIVersionPlaceholder):
		return fmt.Errorf("a tag template %q that never substitutes %s — the tag is where the resolved version lands", spec.Tag, imageLayersOCIVersionPlaceholder)
	case spec.Pin != "" && spec.Version != "":
		return fmt.Errorf(
			"both a derived pin (%q) and an image-owned version (%q) — a version the workspace declares must be DERIVED, never copied",
			spec.Pin, spec.Version)
	case spec.Pin == "" && spec.Version == "":
		return errors.New("neither a `pin` to derive its version from nor an image-owned `version`")
	case spec.Pin != "" && imageLayersPinReader(spec.Pin) == nil:
		return fmt.Errorf("a `pin` %q no workspace source derives (known: %s)", spec.Pin, strings.Join(imageLayersPinNames(), ", "))
	case spec.Pin != "" && spec.Digest != "":
		return fmt.Errorf(
			"a committed digest %q beside the derived pin %q — the version is the pin and the digest is evidence, so committing one makes it a second pin nobody bumps",
			spec.Digest, spec.Pin)
	case spec.Version != "" && !imageLayersToolVersionPattern.MatchString(spec.Version):
		return fmt.Errorf("an unreadable image-owned version %q (bare semver, no leading v)", spec.Version)
	case spec.Version != "" && !imageLayersDigestPattern.MatchString(spec.Digest):
		return fmt.Errorf(
			"an image-owned version %q pinned by an unusable digest %q — an image-owned reference is verified BY its digest, so a tag nothing can check is not a pin",
			spec.Version, spec.Digest)
	}
	if _, err := imageLayersOCIEntry(spec.Source); err != nil {
		return err
	}
	return nil
}

// imageLayersPinReader finds the imageBuildPins() reader for a pin name. The
// producer derives its version through the workspace's one derivation table
// (imagepins.go), so no two layers can read the same axis from two different
// sources.
func imageLayersPinReader(arg string) func(workspaceRoot string) (string, error) {
	for _, pin := range imageBuildPins() {
		if pin.arg == arg {
			return pin.read
		}
	}
	return nil
}

// imageLayersPinNames lists the derivable build args, in table order, for
// diagnostics.
func imageLayersPinNames() []string {
	pins := imageBuildPins()
	names := make([]string, 0, len(pins))
	for _, pin := range pins {
		names = append(names, pin.arg)
	}
	return names
}

// imageLayersOCIEntry turns a declared in-image source path into the tar entry
// name a layer carries (no leading slash).
func imageLayersOCIEntry(source string) (string, error) {
	trimmed := strings.TrimSpace(source)
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("a non-absolute in-image `source` %q", source)
	}
	entry := strings.Trim(path.Clean(trimmed), "/")
	if entry == "" || entry == "." {
		return "", fmt.Errorf("a `source` %q that resolves to the filesystem root", source)
	}
	return entry, nil
}

// --- the registry source seam ---------------------------------------------------

// imageLayersOCIImage is a resolved reference: the digest it pointed at, the
// linux/amd64 manifest selected out of it, and that image.
type imageLayersOCIImage struct {
	// Digest is what the REFERENCE resolved to — a multi-platform index for the
	// tags bun and node publish, the manifest itself for a single-platform
	// image.
	Digest string
	// Manifest is the linux/amd64 image manifest chosen out of Digest.
	Manifest string
	// Image is that selected image.
	Image v1.Image
}

// imageLayersOCISource is the registry surface the extractor needs.
type imageLayersOCISource interface {
	// Resolve fetches the linux/amd64 image behind a reference, and reports
	// what that reference resolved to.
	Resolve(ctx context.Context, reference string) (imageLayersOCIImage, error)
}

// imageLayersOCISourceFor builds the registry source for a run. A package var
// so tests drive the whole producer — including the digest mismatch and the
// missing-path failure — against synthetic images, with no network.
var imageLayersOCISourceFor = func(client *http.Client) imageLayersOCISource {
	return ociRegistrySource{client: client}
}

// ociRegistrySource reads published images over the registry API. No docker
// daemon is involved: go-containerregistry speaks the protocol directly, which
// is the entire reason the `COPY --from` stages could leave with the Dockerfile.
type ociRegistrySource struct {
	client        *http.Client
	authenticator authn.Authenticator
}

func (s ociRegistrySource) Resolve(ctx context.Context, reference string) (imageLayersOCIImage, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return imageLayersOCIImage{}, fmt.Errorf("parse the image reference %s: %w", reference, err)
	}
	desc, err := remote.Get(ref, s.options(ctx)...)
	if err != nil {
		return imageLayersOCIImage{}, fmt.Errorf("resolve %s: %w", reference, err)
	}
	image, err := desc.Image()
	if err != nil {
		return imageLayersOCIImage{}, fmt.Errorf("select the %s image of %s: %w", imageLayersCLIPlatform, reference, err)
	}
	manifest, err := image.Digest()
	if err != nil {
		return imageLayersOCIImage{}, fmt.Errorf("read the manifest digest of %s: %w", reference, err)
	}
	return imageLayersOCIImage{Digest: desc.Digest.String(), Manifest: manifest.String(), Image: image}, nil
}

// options is the pull configuration. The platform is ALWAYS explicit — the
// runner runs linux/amd64 whatever host produces the layer, and a library
// default is not a decision this file wants to inherit.
func (s ociRegistrySource) options(ctx context.Context) []remote.Option {
	authOption := remote.WithAuthFromKeychain(authn.DefaultKeychain)
	if s.authenticator != nil {
		authOption = remote.WithAuth(s.authenticator)
	}
	options := []remote.Option{
		remote.WithContext(ctx),
		// Anonymous is the working configuration: every image here is public.
		// The default keychain is used rather than authn.Anonymous only so that
		// a host which HAS run `docker login` gets its Docker Hub rate limit
		// instead of the shared anonymous one — credentials change who is
		// counted, never which bytes come back, and the digests recorded in the
		// sidecar prove it.
		authOption,
		remote.WithPlatform(v1.Platform{OS: imageLayersToolGOOS, Architecture: imageLayersToolGOARCH}),
		remote.WithUserAgent(clicore.CLIUserAgent()),
	}
	if s.client != nil && s.client.Transport != nil {
		options = append(options, remote.WithTransport(s.client.Transport))
	}
	return options
}

// --- the producer ---------------------------------------------------------------

// imageLayersProduceOCIFile resolves one official image, verifies it as far as
// its pin allows, and extracts a single declared path out of it.
func imageLayersProduceOCIFile(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, _ *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	version, err := imageLayersOCIVersion(spec, workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}
	entry, err := imageLayersOCIEntry(spec.Source)
	if err != nil {
		return imageLayerRecord{}, err
	}

	reference := imageLayersOCIReference(spec, version)
	resolved, err := imageLayersOCISourceFor(ioctx.Client).Resolve(ctx, reference)
	if err != nil {
		return imageLayerRecord{}, err
	}
	// An unverified artifact never becomes a layer. For an image-owned
	// reference the digest is the pin, so a reference resolving elsewhere is a
	// substituted image rather than a newer one — and nothing is written yet.
	if spec.Digest != "" && resolved.Digest != spec.Digest {
		return imageLayerRecord{}, fmt.Errorf(
			"digest mismatch for %s: the manifest pins %s, the registry resolved %s — refusing to extract from an image the pin does not name",
			reference, spec.Digest, resolved.Digest)
	}
	if spec.Digest != "" {
		ioctx.Stdout(fmt.Sprintf("Verified %s against its pinned digest %s.", reference, spec.Digest))
	} else {
		ioctx.Stdout(fmt.Sprintf("Resolved %s to %s.", reference, resolved.Digest))
	}

	// The scratch tree lives inside the project's own .gen so nothing is
	// written beside it and the final rename stays on one filesystem.
	work, err := os.MkdirTemp(outDir, ".work-")
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("create a scratch directory under %s: %w", outDir, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	extracted := filepath.Join(work, spec.Name)
	layer, err := imageLayersExtractEntry(resolved.Image, entry, extracted)
	if err != nil {
		return imageLayerRecord{}, fmt.Errorf("extract %s from %s (manifest %s): %w", spec.Source, reference, resolved.Manifest, err)
	}
	ioctx.Stdout(fmt.Sprintf("Extracted %s from %s (manifest %s, layer %s).", spec.Source, reference, resolved.Manifest, layer))

	sum, size, err := imageLayersInstallFile(extracted, outDir, spec.Name)
	if err != nil {
		return imageLayerRecord{}, err
	}
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		File:     imageLayersBinDir + "/" + spec.Name,
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersOCILabel(spec, version),
		Source: imageLayerSource{
			Image:    reference,
			Digest:   resolved.Digest,
			Manifest: resolved.Manifest,
			Layer:    layer,
			Entry:    spec.Source,
		},
	}, nil
}

// imageLayersOCIReference builds the reference this layer pulls from: the
// declared repository, the tag its template renders the resolved version into,
// and the image-owned digest when the manifest pins one. The producer resolves
// exactly this string, so the label below states what was actually fetched.
func imageLayersOCIReference(spec imageLayerSpec, version string) string {
	reference := spec.Image + ":" + strings.ReplaceAll(spec.Tag, imageLayersOCIVersionPlaceholder, version)
	if spec.Digest != "" {
		reference += "@" + spec.Digest
	}
	return reference
}

// imageLayersOCILabel is what an extracted layer records as its version, and
// what `--check` compares the manifest against on the next run.
//
// The version alone was not enough, and for uv it was not even the pin: an
// image-owned layer is pinned BY its digest, so editing `digest` under an
// unchanged `version` selects other bytes while every version string still
// agrees — and the check would read the old file as current. `image`, the tag
// TEMPLATE and the in-image `source` are three more manifest fields that choose
// the artifact without touching a version (`{version}-slim` → `{version}` is a
// different image; `/uv` → `/uvx` a different binary out of the same one). The
// label therefore states the full reference plus the extracted path, computed
// here so the producer and the check cannot derive it differently.
func imageLayersOCILabel(spec imageLayerSpec, version string) string {
	return imageLayersOCIReference(spec, version) + " " + spec.Source
}

// imageLayersOCIVersion resolves the version a layer's tag is built from:
// derived through the imageBuildPins() reader the layer names, or the
// image-owned pin the manifest declares. A derived pin that reads back empty is
// a hard failure — falling back to a literal is precisely what this design
// forbids.
func imageLayersOCIVersion(spec imageLayerSpec, workspaceRoot string) (string, error) {
	if spec.Pin == "" {
		return spec.Version, nil
	}
	read := imageLayersPinReader(spec.Pin)
	if read == nil {
		return "", fmt.Errorf("no workspace source derives %s", spec.Pin)
	}
	version, err := read(workspaceRoot)
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf(
			"the workspace under %q declares no %s — the %s layer's version is DERIVED from the workspace pin table (package.json's packageManager/engines), never a literal, so there is nothing to pull",
			workspaceRoot, spec.Pin, spec.Name)
	}
	if !imageLayersToolVersionPattern.MatchString(version) {
		return "", fmt.Errorf("the workspace derives an unreadable %s value %q", spec.Pin, version)
	}
	return version, nil
}

// imageLayersEntryOutcome is what one layer had to say about the path being
// extracted.
type imageLayersEntryOutcome int

const (
	// imageLayersEntryMissing — this layer does not mention the path.
	imageLayersEntryMissing imageLayersEntryOutcome = iota
	// imageLayersEntryFound — this layer carries it, and it has been spilled.
	imageLayersEntryFound
	// imageLayersEntryWhiteout — this layer DELETES it, so no lower layer's
	// copy survives into the assembled image.
	imageLayersEntryWhiteout
)

// imageLayersExtractEntry walks the image's layers NEWEST-FIRST and copies the
// declared entry to dest, returning the digest of the layer it came from.
//
// Newest-first is the assembled filesystem's own order: the topmost layer that
// mentions a path is the one whose version of it survives. Walking the other
// way would silently extract an older build of the same binary from a lower
// layer, which is the one failure mode of this producer that would not be
// visible in the produced bytes.
func imageLayersExtractEntry(image v1.Image, entry, dest string) (string, error) {
	layers, err := image.Layers()
	if err != nil {
		return "", fmt.Errorf("read the image layers: %w", err)
	}
	for index := len(layers) - 1; index >= 0; index-- {
		digest, err := layers[index].Digest()
		if err != nil {
			return "", fmt.Errorf("read the digest of layer %d: %w", index, err)
		}
		outcome, err := imageLayersExtractFromLayer(layers[index], entry, dest)
		if err != nil {
			return "", fmt.Errorf("read layer %s: %w", digest, err)
		}
		switch outcome {
		case imageLayersEntryFound:
			return digest.String(), nil
		case imageLayersEntryWhiteout:
			return "", fmt.Errorf("layer %s deletes /%s — the assembled image does not carry the path this layer extracts", digest, entry)
		case imageLayersEntryMissing:
		}
	}
	return "", fmt.Errorf("no layer carries /%s — the upstream image layout changed", entry)
}

// imageLayersExtractFromLayer scans one layer for the entry, spilling it to
// dest when it is there. It reports a whiteout as its own outcome rather than
// as absence: "a later layer removed the binary" and "the image never had it"
// are different upstream changes, and a reader chasing a failed image build
// needs to be told which one happened.
func imageLayersExtractFromLayer(layer v1.Layer, entry, dest string) (imageLayersEntryOutcome, error) {
	body, err := layer.Uncompressed()
	if err != nil {
		return imageLayersEntryMissing, fmt.Errorf("open the layer: %w", err)
	}
	defer func() { _ = body.Close() }()

	reader := tar.NewReader(body)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return imageLayersEntryMissing, nil
		}
		if err != nil {
			return imageLayersEntryMissing, err
		}
		name := strings.Trim(path.Clean("/"+header.Name), "/")
		if imageLayersWhiteoutCovers(name, entry) || imageLayersOpaqueCovers(name, entry) {
			return imageLayersEntryWhiteout, nil
		}
		if name != entry {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return imageLayersEntryMissing, fmt.Errorf(
				"/%s is a %q entry, not a regular file — this producer extracts a binary and will not guess what a link should resolve to",
				entry, string(header.Typeflag))
		}
		if header.Size < 0 || header.Size > imageLayersMaxDownloadBytes {
			return imageLayersEntryMissing, fmt.Errorf("/%s declares %d bytes, outside the %d-byte extraction budget", entry, header.Size, imageLayersMaxDownloadBytes)
		}
		if err := imageLayersCopyEntry(reader, dest, header.Size); err != nil {
			return imageLayersEntryMissing, fmt.Errorf("read /%s: %w", entry, err)
		}
		return imageLayersEntryFound, nil
	}
}

// imageLayersWhiteoutCovers reports whether a plain whiteout entry deletes the
// path being extracted. A `dir/.wh.name` marker removes `dir/name` — and, when
// that name is a directory, its whole subtree — so a whiteout on the entry
// itself or on any of its ancestors both mean the file is gone.
func imageLayersWhiteoutCovers(name, entry string) bool {
	dir, base := path.Split(name)
	target, ok := strings.CutPrefix(base, imageLayersWhiteoutPrefix)
	if !ok || strings.HasPrefix(base, ".wh..wh.") {
		return false
	}
	deleted := strings.Trim(path.Join(dir, target), "/")
	return deleted == entry || strings.HasPrefix(entry, deleted+"/")
}

// imageLayersOpaqueCovers reports whether an opaque-whiteout entry hides the
// path being extracted: `.wh..wh..opq` empties its own directory's lower
// contents wholesale, so anything beneath it is gone even though no per-file
// marker was written.
func imageLayersOpaqueCovers(name, entry string) bool {
	dir, base := path.Split(name)
	if base != imageLayersOpaqueWhiteout {
		return false
	}
	dir = strings.Trim(dir, "/")
	return dir == "" || strings.HasPrefix(entry, dir+"/")
}

// imageLayersCopyEntry copies exactly size bytes of the current archive entry
// into dest. The archive path never reaches the host filesystem: dest is the
// layer's own name under this producer's scratch directory.
func imageLayersCopyEntry(reader io.Reader, dest string, size int64) error {
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the layer name under this producer's own scratch dir
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.CopyN(file, reader, size); err != nil {
		_ = file.Close()
		return fmt.Errorf("copy %d bytes: %w", size, err)
	}
	return file.Close()
}

// --- the warm extension store producer -----------------------------------------
//
// The runner bakes the extension artifact store the CLI would otherwise fetch
// on every run. The retired Dockerfile warmed it by RUNNING `putnami extensions
// install` inside the image against a throwaway manifest pinned to the
// lock-resolved versions, then cross-checked the warmed @putnami/go extension's
// tools/versions.json against the lint-tool versions the image had just built.
//
// Host-side production was impossible until `putnami extensions install` gained
// a target-platform selector: a darwin/arm64 laptop
// cannot materialize a linux/amd64 store for the invoking platform. It now can,
// with `--platform linux/amd64 --dest <root>`, which materializes the artifacts
// and deliberately SKIPS the install hooks and the lock write — packaging
// output rather than an install for this workspace, which is exactly what a
// layer producer wants.
//
// Both image invariants survive the move, and one of them gets stronger:
//
//   - Pins stay derived (invariant 2). The two extension versions come from
//     imageBuildPins() — PUTNAMI_GO_EXTENSION_VERSION and
//     PUTNAMI_TYPESCRIPT_EXTENSION_VERSION, which read putnami.lock.json — so
//     the throwaway manifest this producer writes carries no literal. Those two
//     axes had no live verification anywhere before this layer existed; the
//     pin guard's table now names this producer as their enforcement.
//   - An unverified artifact never becomes a layer (invariant 3). The store is
//     content-addressed BY the same sha256 the lock records, so the check is an
//     exact set comparison: every lock-pinned linux/amd64 integrity must be
//     present as a store directory, and the store may carry nothing else. The
//     Dockerfile checked neither — it trusted whatever the install wrote.
//
// The tools/versions.json cross-check is preserved and moves host-side: the
// warmed go extension's pinned golangci-lint and staticcheck versions must
// equal what the go-tool layers derive through imageBuildGoToolVersion, so a
// store whose extension disagrees with the binaries baked beside it fails the
// produce run rather than the CI run it would have broken.

const (
	// imageLayersWarmPlatform is the target the store is materialized for. The
	// runner runs linux/amd64 whatever host produces the layer, and it is also
	// the lock's integrity key for these artifacts.
	imageLayersWarmPlatform = imageLayersToolGOOS + "/" + imageLayersToolGOARCH
	// imageLayersWarmStoreDir is the content-addressed tree inside an artifact
	// store root, and the ONLY thing this layer carries: an artifact lives at
	// <root>/sha256/<first two hex chars>/<full digest>/. Anything else the CLI
	// keeps beside it (lock files, gc stamps, the CLI's own cache) is local
	// runtime state, not something an image should bake.
	imageLayersWarmStoreDir = "sha256"
	// imageLayersWarmShardLen is how many leading digest characters name the
	// shard directory.
	imageLayersWarmShardLen = 2
	// imageLayersWarmToolManifest is the lint-tool pin table inside the warmed
	// @putnami/go extension — the single source of truth for lint tools, read here
	// out of the materialized tree.
	imageLayersWarmToolManifest = "tools/versions.json"
	// imageLayersWarmWorkspaceName is the throwaway workspace manifest the
	// install runs against. `putnami extensions install` needs a workspace
	// context, and this producer deliberately does NOT fake a repository
	// checkout: the manifest declares the pinned extensions and nothing else.
	imageLayersWarmWorkspaceName = "putnami.workspace.json"
	// imageLayersWarmWorkspaceLabel names that throwaway workspace. It never
	// reaches the layer bytes — the store is content-addressed — so it is a
	// diagnostic string, not a pin.
	imageLayersWarmWorkspaceLabel = "ci-runner-warm"
)

// imageLayersWarmTools are the lint tools whose pins the warmed go extension
// must agree with: exactly the keys the `go-tool` layers derive through
// imageBuildGoToolVersion. Sorted, so the recorded table and every diagnostic
// are stable. TestImageLayersRunnerWarmToolsCoverTheDerivedLayers keeps this
// list and the runner manifest's `tool` layers from drifting apart.
var imageLayersWarmTools = []string{"golangci-lint", "staticcheck"}

// imageLayersWarmExtension is one extension the runner warms, named by the
// imageBuildPins() axis that DERIVES its version. Naming the axis rather than
// reading the lock directly is what keeps this producer inside the workspace's
// one derivation table.
type imageLayersWarmExtension struct {
	name string
	pin  string
	// optional extensions are warmed only when the workspace lock declares them.
	optional bool
	// tools marks the extension carrying tools/versions.json.
	tools bool
}

// imageLayersWarmExtensions is the set the runner warms: its language
// extensions and SDD when the workspace declares it. A checkout pinning
// other versions still fetches its own at run time — the layer pre-pays the
// common case and claims nothing more.
func imageLayersWarmExtensions() []imageLayersWarmExtension {
	return []imageLayersWarmExtension{
		{name: "@putnami/go", pin: "PUTNAMI_GO_EXTENSION_VERSION", tools: true},
		{name: "@putnami/typescript", pin: "PUTNAMI_TYPESCRIPT_EXTENSION_VERSION"},
		{name: "@putnami/sdd", pin: "PUTNAMI_SDD_EXTENSION_VERSION", optional: true},
	}
}

// imageLayersWarmArtifact is one resolved extension: the version the pin table
// derived and the linux/amd64 integrity the lock records for it.
type imageLayersWarmArtifact struct {
	imageLayersWarmExtension
	version string
	sha256  string
}

// imageLayersWarmRun materializes the pinned extension set for the layer's
// platform into an artifact-store root, streaming the CLI's output through the
// command IO. A package var so tests drive the whole producer — including every
// verification failure — with no network and no CLI on PATH.
var imageLayersWarmRun = func(ctx context.Context, workspaceDir, storeDir string, ioctx clicore.IO) error {
	tool, err := imageLayersWarmCLI()
	if err != nil {
		return fmt.Errorf(
			"the putnami CLI is not on PATH — the warm store is materialized by `putnami extensions install --platform %s`, which is the only thing that knows how to fetch and unpack an extension artifact: %w",
			imageLayersWarmPlatform, err)
	}
	cmd := exec.CommandContext(ctx, tool, imageLayersWarmInstallArgs(storeDir)...) //nolint:gosec // G204: argv is imageLayersWarmInstallArgs over a scratch path this producer just created; materializing the pinned extensions is the feature
	cmd.Dir = workspaceDir
	// This invocation is already the explicit install. A credential child must
	// not auto-install the same scratch workspace and contaminate its bare-token
	// stdout with install progress (or recursively request the same credential).
	cmd.Env = append(os.Environ(), "PUTNAMI_NO_AUTO_INSTALL=1", registryproto.CLIExecutableEnv+"="+tool)
	stdout := &imageBuildLineWriter{emit: ioctx.Stdout}
	stderr := &imageBuildLineWriter{emit: ioctx.Stderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	stdout.flush()
	stderr.flush()
	return runErr
}

// Keep materialization on the CLI that spawned this job. PATH may still name a
// previously installed CLI with a different credential or lock contract.
func imageLayersWarmCLI() (string, error) {
	if selected := os.Getenv(registryproto.CLIExecutableEnv); filepath.IsAbs(selected) {
		return selected, nil
	}
	return imageBuildLookPath("putnami")
}

// imageLayersWarmInstallArgs is the install argv. Split out so a unit test pins
// the exact shape without a CLI: `--platform` selects the target the store is
// built FOR (skipping the install hooks and the lock write), and `--dest` keeps
// the whole materialization inside this producer's scratch tree rather than in
// the host's machine-global store.
func imageLayersWarmInstallArgs(storeDir string) []string {
	return []string{"extensions", "install", "--platform", imageLayersWarmPlatform, "--dest", storeDir}
}

// imageLayersWarmStore is the run's ONE materialization of the lock-pinned
// linux/amd64 extension set.
//
// Three layers read that store now — golangci-lint and staticcheck restore
// their binary out of the @putnami/go artifact, and putnami-warm re-emits the
// whole tree — so materializing it per layer would download the same archives
// three times and, worse, let three copies disagree. One store also means one
// place where "an unverified artifact never becomes a layer" is enforced: the
// exact-set check against the lock's integrities runs HERE, before any consumer
// sees a path.
//
// It is lazy: a manifest with no warm and no derived layer (every test that
// produces crane alone, and any future image) never runs an install.
type imageLayersWarmStore struct {
	workspaceRoot string
	outDir        string

	opened    bool
	work      string
	store     string
	artifacts []imageLayersWarmArtifact
	err       error
}

// newImageLayersWarmStore binds the store to one run's workspace and output
// directory. Nothing is created until a layer asks for it.
func newImageLayersWarmStore(workspaceRoot, outDir string) *imageLayersWarmStore {
	return &imageLayersWarmStore{workspaceRoot: workspaceRoot, outDir: outDir}
}

// open materializes and verifies the store on first call and replays the same
// answer — including the same failure — to every later one.
func (s *imageLayersWarmStore) open(ctx context.Context, ioctx clicore.IO) (string, []imageLayersWarmArtifact, error) {
	if s.opened {
		return s.store, s.artifacts, s.err
	}
	s.opened = true
	s.store, s.artifacts, s.err = s.materialize(ctx, ioctx)
	return s.store, s.artifacts, s.err
}

// close removes the scratch tree the store was materialized into. The run owns
// it, because the tree outlives the producer that first asked for it.
func (s *imageLayersWarmStore) close() {
	if s.work != "" {
		_ = os.RemoveAll(s.work)
		s.work = ""
	}
}

func (s *imageLayersWarmStore) materialize(ctx context.Context, ioctx clicore.IO) (string, []imageLayersWarmArtifact, error) {
	artifacts, err := imageLayersWarmResolve(s.workspaceRoot)
	if err != nil {
		return "", nil, err
	}

	// The scratch tree lives inside the project's own .gen so nothing is written
	// beside it — including the throwaway workspace, which must not land
	// anywhere a later `putnami` invocation could mistake for a real one.
	work, err := os.MkdirTemp(s.outDir, ".warm-")
	if err != nil {
		return "", nil, fmt.Errorf("create a scratch directory under %s: %w", s.outDir, err)
	}
	s.work = work

	workspaceDir := filepath.Join(work, "workspace")
	store := filepath.Join(work, "store")
	for _, dir := range []string{workspaceDir, store} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	manifest, err := imageLayersWarmWorkspace(artifacts, s.workspaceRoot)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, imageLayersWarmWorkspaceName), manifest, 0o600); err != nil {
		return "", nil, fmt.Errorf("write the throwaway workspace manifest: %w", err)
	}

	if err := imageLayersSeedWarmStore(s.workspaceRoot, workspaceDir, store, artifacts); err != nil {
		return "", nil, err
	}

	ioctx.Stdout(fmt.Sprintf("Materializing %s for %s …", imageLayersWarmLabel(artifacts), imageLayersWarmPlatform))
	if err := imageLayersWarmRun(ctx, workspaceDir, store, ioctx); err != nil {
		return "", nil, fmt.Errorf("materialize the %s extension store: %w", imageLayersWarmPlatform, err)
	}

	// An unverified artifact never becomes a layer.
	if err := imageLayersWarmVerifyStore(store, artifacts); err != nil {
		return "", nil, err
	}
	ioctx.Stdout(fmt.Sprintf("Verified %d materialized extension(s) against putnami.lock.json's %s integrities.", len(artifacts), imageLayersWarmPlatform))
	return store, artifacts, nil
}

// imageLayersWarmArtifactDir is where one verified artifact lives inside a
// materialized store. The store is content-addressed BY the lock integrity, so
// this path is the digest and nothing else.
func imageLayersWarmArtifactDir(store string, artifact imageLayersWarmArtifact) string {
	return filepath.Join(store, imageLayersWarmStoreDir, artifact.sha256[:imageLayersWarmShardLen], artifact.sha256)
}

// imageLayersWarmToolBinary is where the @putnami/go archive ships one pinned
// tool for the platform it was built for.
func imageLayersWarmToolBinary(tool string) string {
	return "compiled/tools/" + tool
}

// imageLayersWarmToolSource picks the artifact that carries the pinned tools out
// of a resolved set.
func imageLayersWarmToolSource(artifacts []imageLayersWarmArtifact) (imageLayersWarmArtifact, error) {
	for _, artifact := range artifacts {
		if artifact.tools {
			return artifact, nil
		}
	}
	return imageLayersWarmArtifact{}, errors.New(
		"no warmed extension declares the pinned Go tools — the warm set no longer carries @putnami/go")
}

// imageLayersProduceWarm re-emits the run's verified linux/amd64 extension
// store as a normalized tar, after cross-checking the warmed lint-tool pins
// against the extension this workspace installed.
func imageLayersProduceWarm(ctx context.Context, spec imageLayerSpec, workspaceRoot, outDir string, warm *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}
	store, artifacts, err := warm.open(ctx, ioctx)
	if err != nil {
		return imageLayerRecord{}, err
	}

	tools, err := imageLayersWarmVerifyTools(store, artifacts, workspaceRoot)
	if err != nil {
		return imageLayerRecord{}, err
	}
	ioctx.Stdout(fmt.Sprintf("Verified the warmed @putnami/go tool pins against the layers restored beside them: %s.", imageLayersWarmToolLabel(tools)))

	entries, err := imageLayersWarmEntries(filepath.Join(store, imageLayersWarmStoreDir), imageLayersJoin(prefix, imageLayersWarmStoreDir))
	if err != nil {
		return imageLayerRecord{}, err
	}
	tarPath := filepath.Join(outDir, spec.Name+".tar")
	sum, size, err := imageLayersWriteTarFile(tarPath, entries)
	if err != nil {
		return imageLayerRecord{}, err
	}

	sources := make([]imageLayerExtension, 0, len(artifacts))
	for _, artifact := range artifacts {
		sources = append(sources, imageLayerExtension{Name: artifact.name, Version: artifact.version, SHA256: artifact.sha256})
	}
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		Tar:      filepath.Base(tarPath),
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersWarmLabel(artifacts),
		Source:   imageLayerSource{Extensions: sources, Tools: tools},
	}, nil
}

// imageLayersWarmResolve reads what the layer is made of: each extension's
// version through the workspace's own derivation table, and the linux/amd64
// integrity the lock records for it.
//
// Both are hard requirements. A version that reads back empty has no literal to
// fall back on (invariant 2), and an absent integrity leaves the materialized
// artifact with nothing to check it against — which is precisely the state the
// retired Dockerfile shipped in.
func imageLayersWarmResolve(workspaceRoot string) ([]imageLayersWarmArtifact, error) {
	return imageLayersWarmResolveSet(workspaceRoot, imageLayersWarmExtensions())
}

// imageLayersWarmToolArtifact resolves ONLY the extension that supplies the
// pinned Go tools. A derived `go-tool` layer's identity names that artifact, and
// `--check` must re-derive it without resolving — or failing on — an unrelated
// extension's pin.
func imageLayersWarmToolArtifact(workspaceRoot string) (imageLayersWarmArtifact, error) {
	wanted := make([]imageLayersWarmExtension, 0, 1)
	for _, extension := range imageLayersWarmExtensions() {
		if extension.tools {
			wanted = append(wanted, extension)
		}
	}
	artifacts, err := imageLayersWarmResolveSet(workspaceRoot, wanted)
	if err != nil {
		return imageLayersWarmArtifact{}, err
	}
	return imageLayersWarmToolSource(artifacts)
}

// imageLayersWarmResolveSet is the one resolution both callers read.
func imageLayersWarmResolveSet(workspaceRoot string, extensions []imageLayersWarmExtension) ([]imageLayersWarmArtifact, error) {
	lockPath := filepath.Join(workspaceRoot, "putnami.lock.json")
	var lock struct {
		Extensions map[string]struct {
			Integrities map[string]string `json:"integrities"`
		} `json:"extensions"`
	}
	if err := imageBuildReadJSON(lockPath, &lock); err != nil {
		return nil, err
	}
	artifacts := make([]imageLayersWarmArtifact, 0, len(extensions))
	for _, extension := range extensions {
		if _, declared := lock.Extensions[extension.name]; extension.optional && !declared {
			continue
		}
		read := imageLayersPinReader(extension.pin)
		if read == nil {
			return nil, fmt.Errorf("no workspace source derives %s", extension.pin)
		}
		version, err := read(workspaceRoot)
		if err != nil {
			return nil, err
		}
		if version == "" {
			return nil, fmt.Errorf(
				"%s pins no extensions[%q].version — the warm layer's versions are DERIVED from the workspace lock (%s), never literals, so there is nothing to materialize",
				lockPath, extension.name, extension.pin)
		}
		integrity := strings.TrimSpace(lock.Extensions[extension.name].Integrities[imageLayersWarmPlatform])
		if integrity == "" {
			return nil, fmt.Errorf(
				"%s records no extensions[%q].integrities[%q] for version %s — refusing to bake a store nothing can verify (re-run `putnami install` to repopulate it)",
				lockPath, extension.name, imageLayersWarmPlatform, version)
		}
		if !imageLayersSHA256Pattern.MatchString(integrity) {
			return nil, fmt.Errorf("%s records an unreadable extensions[%q].integrities[%q] value %q",
				lockPath, extension.name, imageLayersWarmPlatform, integrity)
		}
		artifacts = append(artifacts, imageLayersWarmArtifact{imageLayersWarmExtension: extension, version: version, sha256: integrity})
	}
	return artifacts, nil
}

// imageLayersWarmLabel renders the resolved set as the layer's version — the
// value `--check` compares against the workspace on the next run, so it states
// every pin the layer depends on, in table order. The lock integrity is part
// of the label: a lock whose integrity moved under an unchanged version string
// must read as stale, not current.
func imageLayersWarmLabel(artifacts []imageLayersWarmArtifact) string {
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, artifact.name+" "+artifact.version+" "+artifact.sha256)
	}
	return strings.Join(parts, ", ")
}

// imageLayersWarmToolLabel renders the verified lint-tool pins for the run log.
func imageLayersWarmToolLabel(tools map[string]string) string {
	parts := make([]string, 0, len(tools))
	for _, tool := range imageLayersWarmTools {
		if version, ok := tools[tool]; ok {
			parts = append(parts, tool+" "+version)
		}
	}
	return strings.Join(parts, ", ")
}

// imageLayersWarmWorkspace is the throwaway manifest the install runs against:
// a name and the exact pinned extensions, plus the host's Cloud credential
// provider when installed. The local provider remains outside the artifact
// store; neither its path nor the host credentials become image contents.
func imageLayersWarmWorkspace(artifacts []imageLayersWarmArtifact, workspaceRoot string) ([]byte, error) {
	extensions := make(map[string]string, len(artifacts))
	for _, artifact := range artifacts {
		extensions[artifact.name] = artifact.version
	}
	provider, err := imageLayersWarmCloudProvider(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if provider != "" {
		extensions[provider] = ""
	}
	workspace := map[string]any{
		"name":       imageLayersWarmWorkspaceLabel,
		"extensions": extensions,
	}
	if provider != "" {
		// The credential provider also needs the originating Cloud workspace.
		// Keep only its public identity/origin in this disposable manifest; a
		// source provider alone cannot resolve an authenticated workspace token.
		if link, linkErr := clicore.ReadCloudLink(workspaceRoot); linkErr == nil {
			workspace["options"] = map[string]any{"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"workspace_id":      clicore.StringValue(link["workspace_id"]),
					"control_plane_url": clicore.StringValue(link["control_plane_url"]),
				},
			}}
		}
	}
	data, err := json.MarshalIndent(workspace, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the throwaway workspace manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// The CLI resolves registry credentials from the install's workspace. Preserve
// the already configured provider there so private archives are authenticated,
// without resolving or downloading another extension to obtain credentials.
func imageLayersWarmCloudProvider(workspaceRoot string) (string, error) {
	var workspace struct {
		Extensions json.RawMessage `json:"extensions"`
	}
	if err := imageBuildReadJSON(filepath.Join(workspaceRoot, imageLayersWarmWorkspaceName), &workspace); err != nil {
		return "", err
	}
	var configured []string
	if len(workspace.Extensions) > 0 && workspace.Extensions[0] == '[' {
		if err := json.Unmarshal(workspace.Extensions, &configured); err != nil {
			return "", fmt.Errorf("read warm workspace extensions: %w", err)
		}
	} else if len(workspace.Extensions) > 0 {
		var versions map[string]string
		if err := json.Unmarshal(workspace.Extensions, &versions); err != nil {
			return "", fmt.Errorf("read warm workspace extensions: %w", err)
		}
		for ref := range versions {
			configured = append(configured, ref)
		}
	}
	refs := make([]string, 0, len(configured))
	for _, ref := range configured {
		if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, ".") {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	candidates := make([]string, 0, len(refs)*2+1)
	for _, ref := range refs {
		if filepath.IsAbs(ref) {
			candidates = append(candidates, ref)
		}
		candidates = append(candidates, filepath.Join(workspaceRoot, strings.TrimPrefix(ref, "/")))
	}
	candidates = append(candidates, filepath.Join(workspaceRoot, ".putnami", "bin", "extensions", "putnami-cloud"))
	for _, candidate := range candidates {
		var manifest struct {
			Name string `json:"name"`
		}
		if err := imageBuildReadJSON(filepath.Join(candidate, "putnami.extension.json"), &manifest); err != nil {
			return "", err
		}
		if manifest.Name == "@putnami/cloud" {
			return filepath.Abs(candidate)
		}
	}
	return "", nil
}

// imageLayersWarmVerifyStore is invariant 3 for this layer, and it is an exact
// set comparison rather than a presence test: the store is content-addressed by
// the very digest the lock records, so "the pinned artifact is here" and
// "nothing else is here" are both checkable, and both matter. A missing entry
// means the install resolved something other than the pin; an extra one means
// the layer would bake an artifact no lock entry accounts for.
func imageLayersWarmVerifyStore(store string, artifacts []imageLayersWarmArtifact) error {
	materialized, err := imageLayersWarmStoreDigests(store)
	if err != nil {
		return err
	}
	pinned := make(map[string]imageLayersWarmArtifact, len(artifacts))
	for _, artifact := range artifacts {
		pinned[artifact.sha256] = artifact
		if !slices.Contains(materialized, artifact.sha256) {
			return fmt.Errorf(
				"the materialized store carries no artifact %s, which putnami.lock.json records as extensions[%q].integrities[%q] for version %s (it materialized %s) — refusing to bake a store the lock cannot account for",
				artifact.sha256, artifact.name, imageLayersWarmPlatform, artifact.version, imageLayersWarmDigestList(materialized))
		}
	}
	for _, digest := range materialized {
		if _, ok := pinned[digest]; !ok {
			return fmt.Errorf(
				"the materialized store carries an artifact %s that no putnami.lock.json extensions[*].integrities[%q] entry pins — refusing to bake an unverified artifact into the warm layer",
				digest, imageLayersWarmPlatform)
		}
	}
	return nil
}

// imageLayersWarmDigestList renders what WAS materialized, for the diagnostic
// that says the pinned artifact is absent.
func imageLayersWarmDigestList(digests []string) string {
	if len(digests) == 0 {
		return "nothing"
	}
	return strings.Join(digests, ", ")
}

// imageLayersWarmStoreDigests lists the artifacts the install materialized, by
// their content-addressed directory name. The shard is checked against the
// digest it claims to shard: an entry filed anywhere else means the store
// layout changed under this producer, and a layer built from a layout the
// runner's CLI does not read is a silently cold image.
func imageLayersWarmStoreDigests(store string) ([]string, error) {
	root := filepath.Join(store, imageLayersWarmStoreDir)
	shards, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"`putnami extensions install --platform %s` materialized no %s/ tree under the destination — the artifact store layout changed, and the layer would carry nothing the runner can read",
			imageLayersWarmPlatform, imageLayersWarmStoreDir)
	}
	if err != nil {
		return nil, fmt.Errorf("read the materialized store: %w", err)
	}
	digests := make([]string, 0, len(shards))
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the materialized store: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			digest := entry.Name()
			if !imageLayersSHA256Pattern.MatchString(digest) {
				return nil, fmt.Errorf(
					"the materialized store carries %s/%s/%s, which is not a sha256-named artifact — the artifact store layout changed",
					imageLayersWarmStoreDir, shard.Name(), digest)
			}
			if shard.Name() != digest[:imageLayersWarmShardLen] {
				return nil, fmt.Errorf(
					"the materialized store files artifact %s under shard %q — the artifact store layout changed",
					digest, shard.Name())
			}
			digests = append(digests, digest)
		}
	}
	slices.Sort(digests)
	return digests, nil
}

// imageLayersWarmVerifyTools is the retired Dockerfile's consistency check,
// moved host-side and made loud: the warmed @putnami/go extension's
// tools/versions.json is the source of truth for the lint tools, and
// the `go-tool` layers built beside this one read the SAME file out of the
// workspace's own install. If the two disagree, the image would ship a
// golangci-lint the extension driving it does not expect — a whole-fleet lint
// failure that is otherwise diagnosed from a panic in an unrelated CI run.
//
// It returns the agreed table so the record states the verification's inputs.
func imageLayersWarmVerifyTools(store string, artifacts []imageLayersWarmArtifact, workspaceRoot string) (map[string]string, error) {
	source, err := imageLayersWarmToolSource(artifacts)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(imageLayersWarmArtifactDir(store, source), filepath.FromSlash(imageLayersWarmToolManifest))
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path built from this producer's own scratch dir and a lock-recorded digest
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"the materialized %s %s carries no %s — the lint-tool pins the image bakes cannot be verified against the extension that drives them",
			source.name, source.version, imageLayersWarmToolManifest)
	}
	if err != nil {
		return nil, fmt.Errorf("read the materialized %s: %w", imageLayersWarmToolManifest, err)
	}
	var manifest struct {
		Tools map[string]struct {
			Version string `json:"version"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse the materialized %s: %w", imageLayersWarmToolManifest, err)
	}

	agreed := make(map[string]string, len(imageLayersWarmTools))
	for _, tool := range imageLayersWarmTools {
		warmed := strings.TrimPrefix(strings.TrimSpace(manifest.Tools[tool].Version), "v")
		if warmed == "" {
			return nil, fmt.Errorf(
				"the materialized %s %s declares no %q version in %s — the layer would bake a store that cannot answer for the lint tools built beside it",
				source.name, source.version, tool, imageLayersWarmToolManifest)
		}
		declared, err := imageBuildGoToolVersion(tool)(workspaceRoot)
		if err != nil {
			return nil, err
		}
		if declared == "" {
			return nil, fmt.Errorf(
				"%s declares no %q version — the warmed store is cross-checked against the extension THIS workspace installed (run `putnami install`), never against a literal",
				imageBuildGoToolPath(workspaceRoot), tool)
		}
		if warmed != declared {
			return nil, fmt.Errorf(
				"lint-tool drift: the materialized %s %s pins %s %s, but the workspace's installed extension declares %s — the image would ship a %s the extension driving it does not expect",
				source.name, source.version, tool, warmed, declared, tool)
		}
		agreed[tool] = warmed
	}
	return agreed, nil
}

// imageLayersWarmEntries reads the materialized tree back as normalized layer
// entries, rooted at prefix.
//
// Unlike the toolchain producer this reads a tree the host filesystem already
// holds — the CLI put it there — so there is nothing to spill: each regular
// file's on-disk path IS its blob. What is NOT trusted is anything the
// filesystem says about it beyond "is it executable": mode, ownership and
// mtimes are synthesized by imageLayersWriteTar exactly as they are for a
// downloaded archive.
func imageLayersWarmEntries(root, prefix string) ([]imageLayerEntry, error) {
	entries := make([]imageLayerEntry, 0, 256)
	budget := imageLayersMaxUnpackedBytes
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if slash == "." {
			slash = ""
		}
		name := imageLayersJoin(prefix, slash)
		switch {
		case entry.IsDir():
			entries = append(entries, imageLayerEntry{name: name + "/", typeflag: tar.TypeDir, mode: imageLayersDirMode})
		case entry.Type()&fs.ModeSymlink != 0:
			target, readErr := os.Readlink(current)
			if readErr != nil {
				return fmt.Errorf("read the materialized store link %s: %w", slash, readErr)
			}
			if err := imageLayersWarmCheckLink(name, target, prefix); err != nil {
				return err
			}
			entries = append(entries, imageLayerEntry{
				name: name, typeflag: tar.TypeSymlink, mode: imageLayersSymlinkMode, linkname: filepath.ToSlash(target),
			})
		case entry.Type().IsRegular():
			info, infoErr := entry.Info()
			if infoErr != nil {
				return fmt.Errorf("stat the materialized store entry %s: %w", slash, infoErr)
			}
			if info.Size() > budget {
				return fmt.Errorf("the materialized store expands past the %d-byte budget", imageLayersMaxUnpackedBytes)
			}
			budget -= info.Size()
			entries = append(entries, imageLayerEntry{
				name:     name,
				typeflag: tar.TypeReg,
				mode:     imageLayersFileMode(int64(info.Mode().Perm())),
				size:     info.Size(),
				blob:     current,
			})
		default:
			return fmt.Errorf(
				"the materialized store entry %s is a %s — refusing to guess how it should be normalized",
				slash, entry.Type().String())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("the materialized store is empty")
	}
	return entries, nil
}

// imageLayersWarmCheckLink refuses a symlink that would point outside the
// layer. The store links extensions at their content-addressed paths, so a
// target reaching past the layer root is a materialization this producer must
// not bake — the runner would resolve it against the IMAGE's filesystem.
func imageLayersWarmCheckLink(name, target, prefix string) error {
	if path.IsAbs(target) {
		return fmt.Errorf("the materialized store links %s to the absolute path %q, which resolves against the image rather than the layer", name, target)
	}
	resolved := path.Clean(path.Join(path.Dir(name), path.Clean(filepath.ToSlash(target))))
	if resolved != prefix && !strings.HasPrefix(resolved, prefix+"/") {
		return fmt.Errorf("the materialized store links %s to %q, which escapes the layer root /%s", name, target, prefix)
	}
	return nil
}

// --- the directory producer -----------------------------------------------------
//
// A layer that carries directories and nothing else. It exists because a
// directory the image must HAVE but no file lives in has no other way into a
// COPY-only assembly, and the first packaged runner shipped without one.
//
// WHAT HAPPENED. The retired Dockerfile's `WORKDIR /workspace` created the
// directory as a side effect of declaring it. The framework's assembly does not:
// `options.package.image.workingDir` only writes the OCI config field
// (tooling/extension-sdk/oci/assemble.go), and a `files` layer synthesizes the
// parent directories of the files it DECLARES and no others
// (tooling/extension-sdk/oci/tar.go). So the packaged image named /workspace as
// its working directory while no layer created it, and Cloud Run's runtime
// refused to start every container with
//
//	failed to find initial working directory "/workspace": no such file or directory
//
// The release was rolled back. Nothing caught it earlier because `docker run`
// CREATES a missing working directory, so the whole in-image test suite — which
// only ever runs under docker — passed on an image no production runtime could
// start. That gap is a runtime difference, not a missing assertion, so the guard
// is host-side and static: TestImageLayersRunnerManifestMatchesTheImageSpec
// refuses a workingDir no declared layer creates.
//
// The identity of such a layer IS its path: mode, ownership and timestamp are
// this file's fixed normalization, so the declared directory is the only input
// that can select different bytes. imageLayersDirsLabel states it, and produce
// and `--check` both derive the record's version through that one function.

// imageLayersProduceDirs emits a layer whose entries are all directories: the
// declared path plus every ancestor imageLayersWriteTar synthesizes for it,
// mode 0755, epoch mtime, uid/gid 0 — the same deterministic writer every other
// tarball layer goes through, so this layer's bytes are as stable as theirs.
//
// It touches no network and no workspace source: there is nothing to download
// and nothing to derive. That also makes it the cheapest layer in the set,
// which is why it costs nothing to keep it declared rather than remembered.
func imageLayersProduceDirs(_ context.Context, spec imageLayerSpec, _, outDir string, _ *imageLayersWarmStore, ioctx clicore.IO) (imageLayerRecord, error) {
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return imageLayerRecord{}, err
	}
	entries := []imageLayerEntry{{name: prefix + "/", typeflag: tar.TypeDir, mode: imageLayersDirMode}}
	tarPath := filepath.Join(outDir, spec.Name+".tar")
	sum, size, err := imageLayersWriteTarFile(tarPath, entries)
	if err != nil {
		return imageLayerRecord{}, err
	}
	ioctx.Stdout(fmt.Sprintf("Created the directory entry /%s (mode %#o) — the image spec cannot create it, and a runtime that requires it refuses to start without it.", prefix, imageLayersDirMode))
	return imageLayerRecord{
		Name:     spec.Name,
		Producer: spec.Producer,
		Path:     "/" + prefix,
		Tar:      filepath.Base(tarPath),
		SHA256:   sum,
		Size:     size,
		Version:  imageLayersDirsLabel(prefix),
	}, nil
}

// imageLayersDirsLabel is the identity a directory layer is produced under: the
// in-image path and the mode it is created with, which together are everything
// that selects this layer's bytes. It is a function because the producer records
// it and `--check` re-derives it — the one rule that keeps "current" from having
// two definitions.
func imageLayersDirsLabel(prefix string) string {
	return fmt.Sprintf("/%s %#o", prefix, imageLayersDirMode)
}

// imageLayersValidateDirs checks the one field this producer reads. The path
// must be NORMALIZED as declared, not merely normalizable: it is this layer's
// whole identity, and the image spec's `workingDir` is compared against it
// verbatim by the manifest/spec agreement test — a "/workspace/" here and a
// "/workspace" there would read as two different directories to that comparison
// while producing the same layer.
func imageLayersValidateDirs(spec imageLayerSpec) error {
	if err := imageLayersValidateNoToolFields(spec); err != nil {
		return err
	}
	prefix, err := imageLayersPrefix(spec.Path)
	if err != nil {
		return err
	}
	if spec.Path != "/"+prefix {
		return fmt.Errorf("an unnormalized directory path %q — declare it as %q, the form the image spec states it in", spec.Path, "/"+prefix)
	}
	return nil
}
