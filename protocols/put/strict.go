package put

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict validation failures. Conformance suites and clients
// key off these: keep them in sync with ValidErrorCodes.
const (
	ErrorCodeInvalidCoordinate      = "put.invalid_coordinate"
	ErrorCodeInvalidVersion         = "put.invalid_version"
	ErrorCodeInvalidMediaType       = "put.invalid_media_type"
	ErrorCodeInvalidPayload         = "put.invalid_payload"
	ErrorCodeInvalidDigest          = "put.invalid_digest"
	ErrorCodeInvalidBlobReceipt     = "put.invalid_blob_receipt"
	ErrorCodeInvalidPublishRequest  = "put.invalid_publish_request"
	ErrorCodeInvalidPublishResponse = "put.invalid_publish_response"
	ErrorCodeInvalidManifest        = "put.invalid_manifest"
	ErrorCodeInvalidArchivePayload  = "put.invalid_archive_payload"
)

// ValidErrorCodes enumerates the canonical put error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidCoordinate:      true,
	ErrorCodeInvalidVersion:         true,
	ErrorCodeInvalidMediaType:       true,
	ErrorCodeInvalidPayload:         true,
	ErrorCodeInvalidDigest:          true,
	ErrorCodeInvalidBlobReceipt:     true,
	ErrorCodeInvalidPublishRequest:  true,
	ErrorCodeInvalidPublishResponse: true,
	ErrorCodeInvalidManifest:        true,
	ErrorCodeInvalidArchivePayload:  true,
}

var (
	// partPattern is one segment of a coordinate: a namespace or a package.
	partPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
	// digestPattern is "sha256:" and 64 lowercase hex characters.
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// mediaTypePattern is a lowercase "type/subtype" without parameters.
	mediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)
	// archivePlatformPattern is an archive payload key: "<os>-<arch>".
	archivePlatformPattern = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)
)

// maxMediaTypeBytes bounds a media type.
const maxMediaTypeBytes = 255

// SplitCoordinate returns the namespace and the package of a coordinate
// "<namespace>/<package>". Each segment is 1 to 255 characters of lowercase
// letters, digits, '.', '_' and '-', and starts with a letter or a digit.
func SplitCoordinate(coordinate string) (namespace, pkg string, err error) {
	namespace, pkg, found := strings.Cut(coordinate, "/")
	if !found || !partPattern.MatchString(namespace) || !partPattern.MatchString(pkg) {
		return "", "", fmt.Errorf("put coordinate %q must be <namespace>/<package>, each of lowercase letters, digits, '.', '_' and '-'", coordinate)
	}
	return namespace, pkg, nil
}

// ValidVersion accepts a version of 1 to MaxVersionBytes bytes, with no
// surrounding space, and no '/', NUL, CR or LF. "." and ".." are refused: a
// manifest read path names the version as one path segment.
func ValidVersion(version string) error {
	if version == "" || len(version) > MaxVersionBytes || strings.TrimSpace(version) != version ||
		strings.ContainsAny(version, "/\x00\r\n") || !utf8.ValidString(version) || version == "." || version == ".." {
		return fmt.Errorf("put version must be 1 to %d bytes with no surrounding space, '/', NUL, CR or LF, and must not be %q or %q", MaxVersionBytes, ".", "..")
	}
	return nil
}

// ValidMediaType accepts a lowercase "type/subtype" of at most 255 bytes,
// without parameters.
func ValidMediaType(mediaType string) bool {
	return len(mediaType) <= maxMediaTypeBytes && mediaTypePattern.MatchString(mediaType)
}

// ValidDigest accepts "sha256:" and 64 lowercase hex characters.
func ValidDigest(digest string) bool { return digestPattern.MatchString(digest) }

// Digest returns "sha256:" and the lowercase hex SHA-256 of data. The member
// digest of a version is the Digest of its stored manifest payload.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidatePayload checks a manifest payload: one JSON object of at most
// MaxManifestBytes of valid UTF-8, with no duplicate object member, in the
// canonical form the registry stores. The canonical form is what Go's
// encoding/json writes for the payload as a json.RawMessage: no insignificant
// space, and '<', '>', '&', U+2028 and U+2029 escaped as \u003c, \u003e,
// \u0026, \u2028 and \u2029. Its digest is the member digest.
func ValidatePayload(payload []byte) []diag.Diagnostic {
	if err := payloadError(payload); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPayload, "payload", "%v", err)}
	}
	return nil
}

func payloadError(payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxManifestBytes {
		return fmt.Errorf("manifest payload is empty or exceeds %d bytes", MaxManifestBytes)
	}
	if !utf8.Valid(payload) {
		return errors.New("manifest payload is not valid UTF-8")
	}
	if payload[0] != '{' {
		return errors.New("manifest payload is not a JSON object")
	}
	if err := noDuplicateMember(payload); err != nil {
		return fmt.Errorf("manifest payload: %w", err)
	}
	canonical, err := json.Marshal(json.RawMessage(payload))
	if err != nil {
		return fmt.Errorf("manifest payload: %w", err)
	}
	if !bytes.Equal(canonical, payload) {
		return errors.New("manifest payload is not in canonical form: compact, with '<', '>' and '&' escaped")
	}
	return nil
}

// noDuplicateMember refuses a JSON value whose objects repeat a member name.
func noDuplicateMember(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := duplicateFreeValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func duplicateFreeValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			if seen[name] {
				return fmt.Errorf("duplicate object member %q", name)
			}
			seen[name] = true
		}
		if err := duplicateFreeValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// BlobReferences returns the blob digests a manifest payload references,
// sorted and unique: its "blob_digest", its "artifact"."blob", and the
// "digest" of every member of its "artifacts" object. The registry links
// exactly these blobs to the package, so a publisher uploads exactly these.
// A reference that is present and not a digest is an error, as is a payload
// those members cannot be read from.
func BlobReferences(payload []byte) ([]string, error) {
	var shape struct {
		BlobDigest *string `json:"blob_digest"`
		Artifact   *struct {
			Blob *string `json:"blob"`
		} `json:"artifact"`
		Artifacts map[string]struct {
			Digest *string `json:"digest"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(payload, &shape); err != nil {
		return nil, errors.New("the manifest payload's blob references cannot be read")
	}
	var references []string
	add := func(field string, digest *string) error {
		if digest == nil {
			return nil
		}
		if !ValidDigest(*digest) {
			return fmt.Errorf("manifest payload %s is not a sha256 digest", field)
		}
		references = append(references, *digest)
		return nil
	}
	if err := add("blob_digest", shape.BlobDigest); err != nil {
		return nil, err
	}
	if shape.Artifact != nil {
		if err := add("artifact.blob", shape.Artifact.Blob); err != nil {
			return nil, err
		}
	}
	for _, artifact := range shape.Artifacts {
		if err := add("artifacts digest", artifact.Digest); err != nil {
			return nil, err
		}
	}
	slices.Sort(references)
	return slices.Compact(references), nil
}

// ParseArchivePayload strict-parses and validates an archive manifest
// payload: an "artifacts" object of 1 to MaxArchivePlatforms members, each
// keyed "<os>-<arch>" and naming a blob digest and a size of at least 1.
func ParseArchivePayload(payload []byte) (*ArchivePayload, []diag.Diagnostic) {
	var archive ArchivePayload
	if err := strictDecode(payload, &archive); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidArchivePayload, "", "invalid archive payload JSON: %v", err)}
	}
	var diags []diag.Diagnostic
	if len(archive.Artifacts) == 0 || len(archive.Artifacts) > MaxArchivePlatforms {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidArchivePayload, "artifacts",
			"an archive names 1 to %d platforms, not %d", MaxArchivePlatforms, len(archive.Artifacts)))
	}
	for platform, artifact := range archive.Artifacts {
		field := "artifacts." + platform
		if !archivePlatformPattern.MatchString(platform) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidArchivePayload, field, "platform %q must be <os>-<arch>", platform))
		}
		if !ValidDigest(artifact.Digest) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, field+".digest", "digest %q must be sha256: followed by 64 lowercase hex characters", artifact.Digest))
		}
		if artifact.Size < 1 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidArchivePayload, field+".size", "size %d is not positive", artifact.Size))
		}
	}
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return &archive, nil
}

// Platforms maps each "<os>/<arch>" of the archive to its blob digest, the
// form of a release-set member's platforms.
func (a ArchivePayload) Platforms() map[string]string {
	platforms := make(map[string]string, len(a.Artifacts))
	for platform, artifact := range a.Artifacts {
		platforms[strings.Replace(platform, "-", "/", 1)] = artifact.Digest
	}
	return platforms
}

// ParseBlobReceipt strict-parses and validates a blob upload answer.
func ParseBlobReceipt(data []byte) (*BlobReceipt, []diag.Diagnostic) {
	var receipt BlobReceipt
	if err := strictDecode(data, &receipt); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBlobReceipt, "", "invalid blob receipt JSON: %v", err)}
	}
	var diags []diag.Diagnostic
	if receipt.ID == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBlobReceipt, "id", "id is required"))
	}
	if !ValidDigest(receipt.Digest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest", "digest %q must be sha256: followed by 64 lowercase hex characters", receipt.Digest))
	}
	if receipt.Size < 1 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidBlobReceipt, "size", "size %d is not positive", receipt.Size))
	}
	if !ValidMediaType(receipt.MediaType) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMediaType, "media_type", "media type %q is not a lowercase type/subtype", receipt.MediaType))
	}
	diags = append(diags, validTime(ErrorCodeInvalidBlobReceipt, "created_at", receipt.CreatedAt)...)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return &receipt, nil
}

// ValidateBlobReceiptFor checks that a receipt names the exact blob uploaded:
// its digest and its size. The registry stores one blob per digest and
// answers the media type of the first upload of those bytes, so the media
// type is not part of the blob's identity.
func ValidateBlobReceiptFor(receipt BlobReceipt, digest string, size int64) []diag.Diagnostic {
	if receipt.Digest != digest || receipt.Size != size {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBlobReceipt, "",
			"the registry stored another blob than the one uploaded")}
	}
	return nil
}

// ParsePublishRequest strict-parses and validates a publish request.
func ParsePublishRequest(data []byte) (*PublishRequest, []diag.Diagnostic) {
	var request PublishRequest
	if err := strictDecode(data, &request); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPublishRequest, "", "invalid publish request JSON: %v", err)}
	}
	if diags := ValidatePublishRequest(request); diag.HasErrors(diags) {
		return nil, diags
	}
	return &request, nil
}

// ValidatePublishRequest checks a publish request: a version, a media type and
// a canonical payload.
func ValidatePublishRequest(request PublishRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if err := ValidVersion(request.Version); err != nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "version", "%v", err))
	}
	if !ValidMediaType(request.MediaType) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMediaType, "media_type", "media type %q is not a lowercase type/subtype", request.MediaType))
	}
	return append(diags, ValidatePayload(request.Payload)...)
}

// ParsePublishResponse strict-parses and validates a publish answer: a
// published, private version whose manifest is the response's manifest, and
// no channel.
func ParsePublishResponse(data []byte) (*PublishResponse, []diag.Diagnostic) {
	var response PublishResponse
	if err := strictDecode(data, &response); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPublishResponse, "", "invalid publish response JSON: %v", err)}
	}
	var diags []diag.Diagnostic
	add := func(field, format string, args ...any) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPublishResponse, field, format, args...))
	}
	if _, _, err := SplitCoordinate(response.Package); err != nil {
		add("package", "%v", err)
	}
	version := response.Version
	if version.ID == "" || version.PackageID == "" || version.ManifestID == "" {
		add("version", "id, package_id and manifest_id are required")
	}
	if err := ValidVersion(version.Version); err != nil {
		add("version.version", "%v", err)
	}
	if version.State != VersionStatePublished {
		add("version.state", "state %q is not %q", version.State, VersionStatePublished)
	}
	if version.Visibility != VisibilityPrivate {
		add("version.visibility", "visibility %q is not %q", version.Visibility, VisibilityPrivate)
	}
	diags = append(diags, validTime(ErrorCodeInvalidPublishResponse, "version.created_at", version.CreatedAt)...)
	diags = append(diags, validateManifest(ErrorCodeInvalidPublishResponse, "manifest.", response.Manifest)...)
	if response.Manifest.ID != version.ManifestID || response.Manifest.PackageID != version.PackageID {
		add("manifest", "the manifest is not the version's manifest")
	}
	if !bytes.Equal(response.Channel, []byte("null")) {
		add("channel", "a publish without a channel moves none: channel must be null")
	}
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return &response, nil
}

// ValidatePublishResponseFor checks that a publish answer names the exact
// version requested: the coordinate, the version, the media type and the
// payload bytes.
func ValidatePublishResponseFor(response PublishResponse, coordinate, version, mediaType string, payload []byte) []diag.Diagnostic {
	if response.Package != coordinate || response.Version.Version != version {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPublishResponse, "",
			"the registry published another version than the one requested")}
	}
	return ValidateManifestFor(response.Manifest, mediaType, payload)
}

// ParseManifest strict-parses and validates a manifest read answer.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	var manifest Manifest
	if err := strictDecode(data, &manifest); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidManifest, "", "invalid manifest JSON: %v", err)}
	}
	if diags := validateManifest(ErrorCodeInvalidManifest, "", manifest); diag.HasErrors(diags) {
		return nil, diags
	}
	return &manifest, nil
}

// ValidateManifestFor checks that a stored manifest is the one a publisher
// sent: the same media type and the same payload bytes. An immutable version
// that holds anything else is not this publisher's version.
func ValidateManifestFor(manifest Manifest, mediaType string, payload []byte) []diag.Diagnostic {
	if manifest.MediaType != mediaType || !bytes.Equal(manifest.Payload, payload) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidManifest, "",
			"the stored manifest differs from the published one")}
	}
	return nil
}

func validateManifest(code, prefix string, manifest Manifest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if manifest.ID == "" || manifest.PackageID == "" {
		diags = append(diags, diag.Errorf(code, prefix+"id", "id and package_id are required"))
	}
	if !ValidMediaType(manifest.MediaType) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMediaType, prefix+"media_type", "media type %q is not a lowercase type/subtype", manifest.MediaType))
	}
	if err := payloadError(manifest.Payload); err != nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPayload, prefix+"payload", "%v", err))
	}
	return append(diags, validTime(code, prefix+"created_at", manifest.CreatedAt)...)
}

func validTime(code, field, value string) []diag.Diagnostic {
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return []diag.Diagnostic{diag.Errorf(code, field, "%s %q is not an RFC 3339 time", field, value)}
	}
	return nil
}

// strictDecode decodes one JSON value into target: an unknown member, a
// duplicate member and trailing data are refused. Every message is closed, so
// a field this build does not know is a protocol change, not an addition to
// ignore.
func strictDecode(data []byte, target any) error {
	if err := noDuplicateMember(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON value")
	}
	return nil
}
