// The publication outbox: a publication job packs, the engine uploads.
//
// A publication job that finds PublicationOutboxEnv set uploads no managed
// member and holds no registry credential for one: a member on a route the
// workspace manages itself, with its own registry and credentials, publishes as
// it does without the variable. The job writes every artifact of a managed
// member under that directory and describes them in one descriptor,
// PublicationOutboxDescriptor, at the directory's root. The engine reads the
// descriptor with ParsePublicationOutbox, resolves every path with
// ResolveOutboxPath, hashes the bytes it is about to upload, refuses any digest
// or size that differs from the descriptor, and uploads with a credential it
// obtains itself.
//
// The descriptor is written by repository-controlled code, so it carries no
// credential, and the parser accepts one exact shape: a bounded document, no
// unknown, duplicate or null member at any depth, and every path relative to
// the outbox root. An npm member names the registry the job resolved with the
// managed npm rules (ManagedNPMRegistry) and an OCI member names its registry
// host; the engine refuses one that the project's registries declaration
// contradicts, and sends a bearer only to a host the provider's credential
// serves. A put or archive member names no registry: the engine uploads it to
// the project's Put registry.

package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

// PublicationOutboxEnv names the private directory a publication job writes
// its packed artifacts and descriptor into. A job that finds it set uploads
// nothing.
const PublicationOutboxEnv = "PUTNAMI_PUBLICATION_OUTBOX"

// PublicationOutboxDescriptor is the file name of the descriptor at the root
// of an outbox directory. No artifact path may name it.
const PublicationOutboxDescriptor = "outbox.json"

// PublicationOutboxVersion is the only protocolVersion a descriptor carries.
const PublicationOutboxVersion = 1

// Bounds of one descriptor and of the artifacts it names.
const (
	// MaxPublicationOutboxBytes bounds the descriptor document.
	MaxPublicationOutboxBytes = 256 << 10
	// MaxPublicationOutboxMembers bounds the members of one descriptor.
	MaxPublicationOutboxMembers = 64
	// MaxOutboxPathBytes bounds one artifact path.
	MaxOutboxPathBytes = 1024
	// MaxOutboxIdentityBytes bounds a coordinate, a version and a project.
	MaxOutboxIdentityBytes = 512
	// MaxOutboxTags bounds the tags of one OCI member.
	MaxOutboxTags = 16
	// MaxOutboxNPMTarballBytes bounds an npm tarball.
	MaxOutboxNPMTarballBytes = 256 << 20
	// MaxOutboxNPMManifestBytes bounds an npm version manifest.
	MaxOutboxNPMManifestBytes = 16 << 20
	// MaxOutboxGoZipBytes bounds a Go module zip, the Go toolchain's own limit.
	MaxOutboxGoZipBytes = 500 << 20
	// MaxOutboxGoModBytes bounds a go.mod file, the Go toolchain's own limit.
	MaxOutboxGoModBytes = 16 << 20
	// MaxOutboxGoInfoBytes bounds a Go module .info file.
	MaxOutboxGoInfoBytes = 64 << 10
	// MaxOutboxPutManifestBytes bounds the manifest payload of a put block.
	MaxOutboxPutManifestBytes = 4 << 20
	// MaxOutboxPutBlobBytes bounds one blob of a put block.
	MaxOutboxPutBlobBytes = 512 << 20
	// MaxOutboxPutBlobs bounds the blobs of one put block.
	MaxOutboxPutBlobs = 32
	// MaxOutboxMediaTypeBytes bounds a media type of a put block.
	MaxOutboxMediaTypeBytes = 255
)

// maxOutboxDepth bounds the container nesting of a descriptor: the root
// object is 0, the members array 1, a member 2, its ecosystem block 3, an
// artifact file, the tags array or the blobs array 4, and a blob 5.
const maxOutboxDepth = 5

// The ecosystems an outbox member may belong to. Each names the one block the
// member carries.
const (
	OutboxEcosystemNPM = "npm"
	OutboxEcosystemGo  = "go"
	OutboxEcosystemOCI = "oci"
	// OutboxEcosystemPut is a member of the Put registry that is not a release
	// archive: a config, a migration or a doc member. It carries the put block.
	OutboxEcosystemPut = "put"
	// OutboxEcosystemArchive is a release archive on the Put registry. It
	// carries the put block.
	OutboxEcosystemArchive = "archive"
)

// PublicationOutbox is the descriptor a publication job writes at the root of
// its outbox directory.
type PublicationOutbox struct {
	// ProtocolVersion is PublicationOutboxVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Members lists every managed member the job packed, at most
	// MaxPublicationOutboxMembers. An empty list states that the job packed
	// none.
	Members []OutboxMember `json:"members"`
}

// OutboxMember is one managed release-set member and the artifacts that
// publish it. Exactly one block is set: NPM, Go or OCI for the ecosystem of
// that name, Put for OutboxEcosystemPut and OutboxEcosystemArchive.
type OutboxMember struct {
	// Ecosystem is OutboxEcosystemNPM, OutboxEcosystemGo, OutboxEcosystemOCI,
	// OutboxEcosystemPut or OutboxEcosystemArchive.
	Ecosystem string `json:"ecosystem"`
	// Coordinate is the member's coordinate in the release-set plan: the npm
	// package name, the complete Go module path, the OCI repository path
	// without its registry host, or the Put "<namespace>/<package>".
	Coordinate string `json:"coordinate"`
	// Version is the member's planned version.
	Version string `json:"version"`
	// Project is the identity of the project the plan assigns the member to.
	Project string `json:"project"`
	// NPM is the npm artifact of an npm member.
	NPM *OutboxNPM `json:"npm,omitempty"`
	// Go is the module artifact of a Go member.
	Go *OutboxGo `json:"go,omitempty"`
	// OCI is the image layout of an OCI member.
	OCI *OutboxOCI `json:"oci,omitempty"`
	// Put is the immutable version of a put or archive member.
	Put *OutboxPut `json:"put,omitempty"`
}

// OutboxFile is one regular file inside the outbox.
type OutboxFile struct {
	// Path is relative to the outbox root, in the form ValidOutboxPath accepts.
	Path string `json:"path"`
	// Digest is "sha256:" and the 64 lowercase hex characters of the file's
	// SHA-256.
	Digest string `json:"digest"`
	// Size is the file's length in bytes, at least 1.
	Size int64 `json:"size"`
}

// OutboxNPM is what the managed npm PUT carries for one version, and where it
// goes.
type OutboxNPM struct {
	// Registry is the registry the job publishes the version to, resolved and
	// validated as a managed npm publication without an outbox does
	// (ManagedNPMRegistry): the job's registry parameter, else the project's
	// registries.npm.publish, else https://registry.npmjs.org.
	Registry string `json:"registry"`
	// Tarball is the packed archive the PUT attaches and the registry serves.
	Tarball OutboxFile `json:"tarball"`
	// Manifest is the version document of the PUT: the staged package.json
	// whose name and version equal the member's coordinate and version.
	Manifest OutboxFile `json:"manifest"`
}

// OutboxGo is one module version in the Go module proxy layout.
type OutboxGo struct {
	// Zip is the module zip the blob upload carries.
	Zip OutboxFile `json:"zip"`
	// Mod is the go.mod the version PUT carries.
	Mod OutboxFile `json:"mod"`
	// Info is the version's .info document.
	Info OutboxFile `json:"info"`
}

// OutboxOCI is one image in an OCI image layout.
type OutboxOCI struct {
	// Layout is the layout directory, relative to the outbox root. Every entry
	// under it is a regular file or a directory.
	Layout string `json:"layout"`
	// Repository is the registry host and repository path the image is pushed
	// to. Its path is the member's coordinate.
	Repository string `json:"repository"`
	// Digest is the manifest digest the layout holds and the registry must
	// answer.
	Digest string `json:"digest"`
	// Tags are assigned to Digest after the push. Absent or empty assigns none.
	Tags []string `json:"tags,omitempty"`
}

// OutboxPut is one immutable version on the Put registry, in the Put write
// protocol (go.putnami.dev/protocol/put): the blobs it references, uploaded
// first, and the manifest the version publishes.
type OutboxPut struct {
	// MediaType is the media type the manifest is published with.
	MediaType string `json:"mediaType"`
	// Manifest is the manifest payload: one JSON object, in the canonical form
	// the registry stores, at most MaxOutboxPutManifestBytes.
	Manifest OutboxFile `json:"manifest"`
	// Blobs are the blobs the manifest references, at most MaxOutboxPutBlobs,
	// each digest once. Absent or empty uploads none.
	Blobs []OutboxPutBlob `json:"blobs,omitempty"`
}

// OutboxPutBlob is one blob of a put block: a regular file inside the outbox
// and the media type it is uploaded with.
type OutboxPutBlob struct {
	// Path is relative to the outbox root, in the form ValidOutboxPath accepts.
	Path string `json:"path"`
	// Digest is "sha256:" and the 64 lowercase hex characters of the file's
	// SHA-256.
	Digest string `json:"digest"`
	// Size is the file's length in bytes, at least 1.
	Size int64 `json:"size"`
	// MediaType is the Content-Type the blob upload carries.
	MediaType string `json:"mediaType"`
}

// File is the outbox file the blob names.
func (b OutboxPutBlob) File() OutboxFile {
	return OutboxFile{Path: b.Path, Digest: b.Digest, Size: b.Size}
}

// outboxMediaTypePattern is the lowercase "type/subtype" form of a media type
// in a put block, without parameters.
var outboxMediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// ociTagPattern is the OCI distribution tag grammar.
var ociTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// outboxDiagnostic is the code of every descriptor finding.
const outboxDiagnostic = "invalid-publication-outbox"

// ParsePublicationOutbox decodes a descriptor strictly and validates it. The
// document is at most MaxPublicationOutboxBytes of valid UTF-8 holding one
// JSON object with no null, no duplicate object member and no member name that
// is not the exact name of a field, at any depth.
func ParsePublicationOutbox(data []byte) (*PublicationOutbox, []diag.Diagnostic) {
	if err := strictOutboxDocument(data); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(outboxDiagnostic, "", "%v", err)}
	}
	if err := exactOutboxMembers(data, reflect.TypeOf(PublicationOutbox{})); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(outboxDiagnostic, "", "%v", err)}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var outbox PublicationOutbox
	if err := decoder.Decode(&outbox); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(outboxDiagnostic, "", "failed to parse publication outbox: %v", err)}
	}
	if diags := ValidatePublicationOutbox(&outbox); len(diags) > 0 {
		return nil, diags
	}
	return &outbox, nil
}

// ValidatePublicationOutbox checks every rule a descriptor answers to beyond
// its JSON shape: the version, the member bound, each member's identity and
// single ecosystem block, an npm member's registry, a put block's media types
// and blob bound, artifact paths, digests and sizes, and that no two members
// share an identity, no two blobs of a put block share a digest and no two
// artifacts share or nest a path.
func ValidatePublicationOutbox(outbox *PublicationOutbox) []diag.Diagnostic {
	if outbox == nil {
		return []diag.Diagnostic{diag.Errorf(outboxDiagnostic, "", "publication outbox is nil")}
	}
	var diags []diag.Diagnostic
	add := func(field, format string, args ...any) {
		diags = append(diags, diag.Errorf(outboxDiagnostic, field, format, args...))
	}
	if outbox.ProtocolVersion != PublicationOutboxVersion {
		add("protocolVersion", "protocolVersion %d is not %d", outbox.ProtocolVersion, PublicationOutboxVersion)
	}
	if outbox.Members == nil {
		add("members", "members is required")
	}
	if len(outbox.Members) > MaxPublicationOutboxMembers {
		add("members", "%d members exceed the bound of %d", len(outbox.Members), MaxPublicationOutboxMembers)
		return diags
	}
	identities := map[string]int{}
	var paths []outboxPathOwner
	for index, member := range outbox.Members {
		field := fmt.Sprintf("members[%d]", index)
		for _, identity := range [...]struct{ name, value string }{
			{"coordinate", member.Coordinate}, {"version", member.Version}, {"project", member.Project},
		} {
			if err := validOutboxIdentity(identity.value); err != nil {
				add(field+"."+identity.name, "%s %v", identity.name, err)
			}
		}
		identity := member.Ecosystem + "\x00" + member.Coordinate
		if previous, duplicate := identities[identity]; duplicate {
			add(field, "member %s %q repeats members[%d]", member.Ecosystem, member.Coordinate, previous)
		} else {
			identities[identity] = index
		}
		blocks := 0
		for _, present := range []bool{member.NPM != nil, member.Go != nil, member.OCI != nil, member.Put != nil} {
			if present {
				blocks++
			}
		}
		if blocks != 1 {
			add(field, "member carries %d ecosystem blocks, want exactly the %q block", blocks, outboxBlockName(member.Ecosystem))
		}
		switch member.Ecosystem {
		case OutboxEcosystemNPM:
			if member.NPM == nil {
				add(field+".npm", "an npm member requires the npm block")
				continue
			}
			checkOutboxNPMRegistry(add, field+".npm.registry", member.NPM.Registry)
			paths = checkOutboxFile(add, paths, field+".npm.tarball", member.NPM.Tarball, MaxOutboxNPMTarballBytes)
			paths = checkOutboxFile(add, paths, field+".npm.manifest", member.NPM.Manifest, MaxOutboxNPMManifestBytes)
		case OutboxEcosystemGo:
			if member.Go == nil {
				add(field+".go", "a go member requires the go block")
				continue
			}
			paths = checkOutboxFile(add, paths, field+".go.zip", member.Go.Zip, MaxOutboxGoZipBytes)
			paths = checkOutboxFile(add, paths, field+".go.mod", member.Go.Mod, MaxOutboxGoModBytes)
			paths = checkOutboxFile(add, paths, field+".go.info", member.Go.Info, MaxOutboxGoInfoBytes)
		case OutboxEcosystemOCI:
			if member.OCI == nil {
				add(field+".oci", "an oci member requires the oci block")
				continue
			}
			paths = checkOutboxOCI(add, paths, field+".oci", member.Coordinate, *member.OCI)
		case OutboxEcosystemPut, OutboxEcosystemArchive:
			if member.Put == nil {
				add(field+".put", "a member of ecosystem %q requires the put block", member.Ecosystem)
				continue
			}
			paths = checkOutboxPut(add, paths, field+".put", *member.Put)
		default:
			add(field+".ecosystem", "ecosystem %q is not %q, %q, %q, %q or %q", member.Ecosystem,
				OutboxEcosystemNPM, OutboxEcosystemGo, OutboxEcosystemOCI, OutboxEcosystemPut, OutboxEcosystemArchive)
		}
	}
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if outboxPathsOverlap(paths[i].path, paths[j].path) {
				add(paths[j].field, "path %q overlaps %q of %s", paths[j].path, paths[i].path, paths[i].field)
			}
		}
	}
	return diags
}

// outboxBlockName is the name of the one block a member of ecosystem carries.
func outboxBlockName(ecosystem string) string {
	if ecosystem == OutboxEcosystemArchive {
		return OutboxEcosystemPut
	}
	return ecosystem
}

// outboxPathOwner pairs an artifact path with the field that names it.
type outboxPathOwner struct {
	field string
	path  string
}

func checkOutboxFile(add func(string, string, ...any), paths []outboxPathOwner, field string, file OutboxFile, limit int64) []outboxPathOwner {
	if err := ValidOutboxPath(file.Path); err != nil {
		add(field+".path", "%v", err)
	} else {
		paths = append(paths, outboxPathOwner{field: field, path: file.Path})
	}
	if !digestPattern.MatchString(file.Digest) {
		add(field+".digest", "digest %q must be sha256: followed by 64 lowercase hex characters", file.Digest)
	}
	if file.Size < 1 || file.Size > limit {
		add(field+".size", "size %d is outside 1..%d", file.Size, limit)
	}
	return paths
}

// checkOutboxNPMRegistry reports a registry that ManagedNPMRegistry refuses,
// without repeating it: a refused registry may carry a credential.
func checkOutboxNPMRegistry(add func(string, string, ...any), field, registry string) {
	if registry == "" {
		add(field, "registry is required")
		return
	}
	if _, err := ManagedNPMRegistry(registry); err != nil {
		add(field, "%v", err)
	}
}

// ManagedNPMRegistry validates the registry a managed npm publication uploads
// to and returns it normalized. The registry is an absolute https URL, or an
// http URL whose host is loopback (localhost, a name under .localhost, or a
// loopback IP), with no userinfo, query or fragment. Surrounding spaces are
// ignored, the scheme is lowercased and trailing slashes leave the path. No
// error repeats raw, which may carry a credential.
func ManagedNPMRegistry(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("managed npm registry must be an absolute HTTP(S) URL without credentials")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("managed npm registry must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(hostname)
		loopback := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
		if scheme != "http" || !loopback {
			return "", errors.New("managed npm registry must use HTTPS (HTTP is allowed only for loopback)")
		}
	}
	u.Scheme = scheme
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func checkOutboxOCI(add func(string, string, ...any), paths []outboxPathOwner, field, coordinate string, image OutboxOCI) []outboxPathOwner {
	if err := ValidOutboxPath(image.Layout); err != nil {
		add(field+".layout", "%v", err)
	} else {
		paths = append(paths, outboxPathOwner{field: field + ".layout", path: image.Layout})
	}
	host, repositoryPath, found := strings.Cut(image.Repository, "/")
	if err := validOutboxIdentity(image.Repository); err != nil || !found || host == "" || repositoryPath != coordinate {
		add(field+".repository", "repository %q must be a registry host followed by the coordinate %q", image.Repository, coordinate)
	}
	if !digestPattern.MatchString(image.Digest) {
		add(field+".digest", "digest %q must be sha256: followed by 64 lowercase hex characters", image.Digest)
	}
	if len(image.Tags) > MaxOutboxTags {
		add(field+".tags", "%d tags exceed the bound of %d", len(image.Tags), MaxOutboxTags)
	}
	seen := map[string]bool{}
	for index, tag := range image.Tags {
		if !ociTagPattern.MatchString(tag) {
			add(fmt.Sprintf("%s.tags[%d]", field, index), "tag %q is not an OCI tag", tag)
		}
		if seen[tag] {
			add(fmt.Sprintf("%s.tags[%d]", field, index), "tag %q is repeated", tag)
		}
		seen[tag] = true
	}
	return paths
}

// checkOutboxPut checks a put block: its media types, its manifest file, and
// its blob files, bound and unique digests. Which media types a member kind
// publishes and which blobs its manifest references are rules of the Put
// write protocol, checked where the member is uploaded.
func checkOutboxPut(add func(string, string, ...any), paths []outboxPathOwner, field string, block OutboxPut) []outboxPathOwner {
	checkOutboxMediaType(add, field+".mediaType", block.MediaType)
	paths = checkOutboxFile(add, paths, field+".manifest", block.Manifest, MaxOutboxPutManifestBytes)
	if len(block.Blobs) > MaxOutboxPutBlobs {
		add(field+".blobs", "%d blobs exceed the bound of %d", len(block.Blobs), MaxOutboxPutBlobs)
		return paths
	}
	digests := map[string]int{}
	for index, blob := range block.Blobs {
		blobField := fmt.Sprintf("%s.blobs[%d]", field, index)
		paths = checkOutboxFile(add, paths, blobField, blob.File(), MaxOutboxPutBlobBytes)
		checkOutboxMediaType(add, blobField+".mediaType", blob.MediaType)
		if previous, repeated := digests[blob.Digest]; repeated {
			add(blobField+".digest", "digest %q repeats %s.blobs[%d]", blob.Digest, field, previous)
		} else {
			digests[blob.Digest] = index
		}
	}
	return paths
}

func checkOutboxMediaType(add func(string, string, ...any), field, mediaType string) {
	if len(mediaType) > MaxOutboxMediaTypeBytes || !outboxMediaTypePattern.MatchString(mediaType) {
		add(field, "media type %q must be a lowercase type/subtype of at most %d bytes", mediaType, MaxOutboxMediaTypeBytes)
	}
}

// validOutboxIdentity accepts a non-empty bounded value of printable,
// non-space characters.
func validOutboxIdentity(value string) error {
	if value == "" {
		return errors.New("is required")
	}
	if len(value) > MaxOutboxIdentityBytes {
		return fmt.Errorf("exceeds %d bytes", MaxOutboxIdentityBytes)
	}
	for _, r := range value {
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("%q holds a space, control or invalid character", value)
		}
	}
	return nil
}

// ValidOutboxPath accepts an artifact path relative to the outbox root: at
// most MaxOutboxPathBytes, slash-separated, already clean, with no empty, "."
// or ".." segment, no leading slash, no backslash, colon or control
// character, and not PublicationOutboxDescriptor.
func ValidOutboxPath(rel string) error {
	if rel == "" {
		return errors.New("outbox path is required")
	}
	if len(rel) > MaxOutboxPathBytes {
		return fmt.Errorf("outbox path exceeds %d bytes", MaxOutboxPathBytes)
	}
	if !utf8.ValidString(rel) {
		return fmt.Errorf("outbox path %q is not valid UTF-8", rel)
	}
	for _, r := range rel {
		if r == '\\' || r == ':' || unicode.IsControl(r) {
			return fmt.Errorf("outbox path %q holds a backslash, colon or control character", rel)
		}
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("outbox path %q is absolute", rel)
	}
	if path.Clean(rel) != rel {
		return fmt.Errorf("outbox path %q is not clean", rel)
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("outbox path %q escapes the outbox root", rel)
		}
	}
	if rel == PublicationOutboxDescriptor {
		return fmt.Errorf("outbox path %q names the descriptor", rel)
	}
	return nil
}

// outboxPathsOverlap reports whether two valid paths are equal or one is a
// directory above the other.
func outboxPathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ResolveOutboxPath returns the filesystem path of rel under root. rel must
// pass ValidOutboxPath, root must be a directory that is not a symbolic link,
// and every component from root to rel must exist and be no symbolic link: a
// directory for every component but the last, which may be a regular file or
// a directory. Symbolic links above root are not inspected.
func ResolveOutboxPath(root, rel string) (string, error) {
	if err := ValidOutboxPath(rel); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("outbox root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("outbox root %s is not a directory", root)
	}
	current := root
	segments := strings.Split(rel, "/")
	for index, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("outbox path %q: %w", rel, err)
		}
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			return "", fmt.Errorf("outbox path %q crosses a symbolic link at %q", rel, strings.Join(segments[:index+1], "/"))
		case index < len(segments)-1 && !mode.IsDir():
			return "", fmt.Errorf("outbox path %q: %q is not a directory", rel, strings.Join(segments[:index+1], "/"))
		case !mode.IsDir() && !mode.IsRegular():
			return "", fmt.Errorf("outbox path %q is neither a regular file nor a directory", rel)
		}
	}
	return current, nil
}

// strictOutboxDocument checks a descriptor before any typed decode: bounded
// size, valid UTF-8, one JSON object with no trailing data, no null, no
// duplicate object member, and bounded nesting.
func strictOutboxDocument(data []byte) error {
	if len(data) == 0 || len(data) > MaxPublicationOutboxBytes {
		return fmt.Errorf("publication outbox is empty or exceeds %d bytes", MaxPublicationOutboxBytes)
	}
	if !utf8.Valid(data) {
		return errors.New("publication outbox is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("publication outbox: %w", err)
	}
	if first != json.Delim('{') {
		return errors.New("publication outbox is not a JSON object")
	}
	if err := strictOutboxContainer(decoder, '{', 0); err != nil {
		return fmt.Errorf("publication outbox: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("publication outbox: trailing data after the JSON object")
	}
	return nil
}

// strictOutboxValue checks one value whose first token the decoder has not
// read yet.
func strictOutboxValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null is not permitted")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	return strictOutboxContainer(decoder, delim, depth+1)
}

// strictOutboxContainer checks the rest of an object or array whose opening
// delimiter the decoder has read.
func strictOutboxContainer(decoder *json.Decoder, open json.Delim, depth int) error {
	if depth > maxOutboxDepth {
		return fmt.Errorf("nesting exceeds %d levels", maxOutboxDepth)
	}
	switch open {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid object member %v", key)
			}
			if seen[name] {
				return fmt.Errorf("duplicate object member %q", name)
			}
			seen[name] = true
			if err := strictOutboxValue(decoder, depth); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := strictOutboxValue(decoder, depth); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err := decoder.Token()
	return err
}

// exactOutboxMembers refuses an object member that is not the exact json name
// of a field of kind, through struct, pointer and slice fields. encoding/json
// matches names without regard to case; the descriptor does not.
func exactOutboxMembers(data []byte, kind reflect.Type) error {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	switch kind.Kind() {
	case reflect.Struct:
		var members map[string]json.RawMessage
		if json.Unmarshal(data, &members) != nil {
			return nil
		}
		fields := outboxJSONFields(kind)
		for name, value := range members {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown member %q", name)
			}
			if err := exactOutboxMembers(value, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return nil
		}
		for _, item := range items {
			if err := exactOutboxMembers(item, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// outboxJSONFields maps the json name of each exported field of kind to its
// type.
func outboxJSONFields(kind reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, kind.NumField())
	for index := 0; index < kind.NumField(); index++ {
		field := kind.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
