// Package putpublish uploads one Put registry member version to a registry
// that implements the put-write protocol (go.putnami.dev/protocol/put): the
// blobs its manifest references, then its manifest, without a channel.
//
// Every function takes the bearer as an argument and attaches it only to
// requests for the registry URL it was given. No function reads a credential
// from the environment or starts a process. No error carries the bearer or a
// registry response body.
package putpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	put "go.putnami.dev/protocol/put"
)

// MaxResponseBytes bounds a registry answer: a stored manifest payload and the
// envelope around it.
const MaxResponseBytes = put.MaxManifestBytes + 64<<10

// Blob is one blob a member's manifest references. Publish reads it only
// when it uploads it, so a member holds one blob in memory at a time.
type Blob struct {
	// MediaType is the media type the blob is uploaded with.
	MediaType string
	// Digest is "sha256:" and the hex SHA-256 of the blob bytes.
	Digest string
	// Size is the length of the blob bytes, at least 1.
	Size int64
	// Read returns the blob bytes. Bytes whose digest or size is not the
	// blob's are refused before they are sent.
	Read func() ([]byte, error)
}

// BytesBlob is the blob of data, uploaded with mediaType.
func BytesBlob(mediaType string, data []byte) Blob {
	return Blob{
		MediaType: mediaType, Digest: put.Digest(data), Size: int64(len(data)),
		Read: func() ([]byte, error) { return data, nil },
	}
}

// Member is one packed Put registry member version.
type Member struct {
	// Kind is the release-set member kind. It selects the media types the
	// member may publish (put.ProfileFor).
	Kind distribution.MemberKind
	// Coordinate is "<namespace>/<package>".
	Coordinate string
	// Version is the version to publish.
	Version string
	// MediaType is the manifest media type.
	MediaType string
	// Manifest is the manifest payload, in canonical form.
	Manifest []byte
	// Blobs are exactly the blobs the manifest references.
	Blobs []Blob
}

// Published is what a publication stored.
type Published struct {
	// Digest is "sha256:" and the hex SHA-256 of the manifest payload the
	// registry stores: the member digest.
	Digest string
	// Platforms maps "<os>/<arch>" to the blob digest of that platform, for an
	// archive member. It is nil for any other kind.
	Platforms map[string]string
	// Reused reports that the registry already held the version with the same
	// manifest.
	Reused bool
}

// ValidateRegistryURL returns raw as an absolute HTTPS URL, or an HTTP URL on a
// loopback host, with no trailing slash. A URL that carries userinfo, a query
// or a fragment is refused, so no credential travels in a URL.
func ValidateRegistryURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("put registry URL must be an absolute HTTP(S) URL without credentials")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "https":
	case "http":
		hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(hostname)
		loopback := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
		if !loopback {
			return "", errors.New("put registry URL must use HTTPS (HTTP is allowed only for loopback)")
		}
	default:
		return "", errors.New("put registry URL must use HTTP(S)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// NewHTTPClient returns the client a publication sends through: no proxy, so
// the bearer has no second recipient, no redirect, and a five-minute timeout.
func NewHTTPClient() (*http.Client, error) {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("put registry transport is unavailable")
	}
	transport := baseTransport.Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     transport,
		CheckRedirect: refuseRedirect,
	}, nil
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// withoutRedirects returns a copy of client that never follows a redirect, so
// a request carrying the bearer reaches the registry it names and no other.
func withoutRedirects(client *http.Client) (*http.Client, error) {
	if client == nil {
		return nil, errors.New("put publication requires an HTTP client")
	}
	bounded := *client
	bounded.CheckRedirect = refuseRedirect
	return &bounded, nil
}

// Check validates member before any request and returns the platforms of an
// archive member. The kind must be one put-write/v1 publishes, the manifest
// media type and every blob media type must be the kind's, the payload must
// be canonical, and the blobs must be exactly the blobs the payload
// references. An archive payload must name each platform's blob at that
// blob's size.
func Check(member Member) (map[string]string, error) {
	profile, ok := put.ProfileFor(member.Kind)
	if !ok {
		return nil, fmt.Errorf("put-write/v1 publishes no %q member", member.Kind)
	}
	if _, _, err := put.SplitCoordinate(member.Coordinate); err != nil {
		return nil, err
	}
	if err := put.ValidVersion(member.Version); err != nil {
		return nil, err
	}
	if member.MediaType != profile.ManifestMediaType {
		return nil, fmt.Errorf("a member of kind %s publishes its manifest as %s, not %q", member.Kind, profile.ManifestMediaType, member.MediaType)
	}
	if diags := put.ValidatePayload(member.Manifest); diag.HasErrors(diags) {
		return nil, fmt.Errorf("manifest: %s", diags[0].Message)
	}
	sizes := make(map[string]int64, len(member.Blobs))
	for index, blob := range member.Blobs {
		if !slices.Contains(profile.BlobMediaTypes, blob.MediaType) {
			return nil, fmt.Errorf("blob %d: a member of kind %s uploads no %q blob", index, member.Kind, blob.MediaType)
		}
		if !put.ValidDigest(blob.Digest) {
			return nil, fmt.Errorf("blob %d names no sha256 digest", index)
		}
		if blob.Size < 1 || blob.Read == nil {
			return nil, fmt.Errorf("blob %d is empty", index)
		}
		if _, repeated := sizes[blob.Digest]; repeated {
			return nil, fmt.Errorf("blob %d repeats blob %s", index, blob.Digest)
		}
		sizes[blob.Digest] = blob.Size
	}
	references, err := put.BlobReferences(member.Manifest)
	if err != nil {
		return nil, err
	}
	uploaded := make([]string, 0, len(sizes))
	for digest := range sizes {
		uploaded = append(uploaded, digest)
	}
	slices.Sort(uploaded)
	if !slices.Equal(references, uploaded) {
		return nil, fmt.Errorf("the manifest references %d blobs and the member carries %d others; they must be the same blobs", len(references), len(uploaded))
	}
	if member.Kind != distribution.KindArchive {
		return nil, nil
	}
	archive, diags := put.ParseArchivePayload(member.Manifest)
	if archive == nil {
		return nil, fmt.Errorf("archive manifest: %s", firstMessage(diags))
	}
	for platform, artifact := range archive.Artifacts {
		if sizes[artifact.Digest] != artifact.Size {
			return nil, fmt.Errorf("archive manifest: platform %s names a blob of %d bytes; the member carries %d", platform, artifact.Size, sizes[artifact.Digest])
		}
	}
	return archive.Platforms(), nil
}

func firstMessage(diags []diag.Diagnostic) string {
	if len(diags) == 0 {
		return "invalid"
	}
	return diags[0].Message
}

// UploadBlob reads blob and uploads it to the blob endpoint of coordinate.
// Bytes whose digest or size is not the blob's are refused before they are
// sent, and a receipt that names other bytes is refused.
func UploadBlob(ctx context.Context, client *http.Client, registryURL, token, coordinate string, blob Blob) error {
	namespace, pkg, err := put.SplitCoordinate(coordinate)
	if err != nil {
		return err
	}
	if blob.Read == nil {
		return errors.New("the blob has no bytes")
	}
	data, err := blob.Read()
	if err != nil {
		return fmt.Errorf("read the blob: %w", err)
	}
	if int64(len(data)) != blob.Size || put.Digest(data) != blob.Digest {
		return fmt.Errorf("the blob bytes are not blob %s of %d bytes", blob.Digest, blob.Size)
	}
	body, status, err := send(ctx, client, http.MethodPost, registryURL+put.BlobUploadPath(namespace, pkg), blob.MediaType, token, data)
	if err != nil {
		return fmt.Errorf("blob upload: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("blob upload returned %d", status)
	}
	// The answer is untrusted and can reflect the Authorization value in any
	// field, so its diagnostics stay out of the error.
	receipt, diags := put.ParseBlobReceipt(body)
	if receipt == nil || diag.HasErrors(diags) {
		return errors.New("invalid blob upload response")
	}
	if diag.HasErrors(put.ValidateBlobReceiptFor(*receipt, blob.Digest, blob.Size)) {
		return errors.New("the registry stored another blob than the one uploaded")
	}
	return nil
}

// PublishManifest publishes member's manifest without a channel and returns
// the manifest the registry stores. A 409 means the version is already
// published: the stored manifest is read back, and the version is a reuse only
// when its media type and payload bytes equal member's.
func PublishManifest(ctx context.Context, client *http.Client, registryURL, token string, member Member) (stored *put.Manifest, reused bool, err error) {
	namespace, pkg, err := put.SplitCoordinate(member.Coordinate)
	if err != nil {
		return nil, false, err
	}
	request := put.PublishRequest{Version: member.Version, MediaType: member.MediaType, Payload: member.Manifest}
	if diags := put.ValidatePublishRequest(request); diag.HasErrors(diags) {
		return nil, false, fmt.Errorf("publish request: %s", diags[0].Message)
	}
	// A canonical payload is what encoding/json writes for it, so the request
	// carries the payload byte for byte.
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, false, errors.New("the publish request cannot be encoded")
	}
	body, status, err := send(ctx, client, http.MethodPost, registryURL+put.PublishPath(namespace, pkg), put.PublishContentType, token, encoded)
	if err != nil {
		return nil, false, fmt.Errorf("publish: %w", err)
	}
	switch status {
	case http.StatusCreated:
		response, diags := put.ParsePublishResponse(body)
		if response == nil || diag.HasErrors(diags) {
			return nil, false, errors.New("invalid publish response")
		}
		if diag.HasErrors(put.ValidatePublishResponseFor(*response, member.Coordinate, member.Version, member.MediaType, member.Manifest)) {
			return nil, false, errors.New("the registry published another version than the one requested")
		}
		return &response.Manifest, false, nil
	case http.StatusConflict:
		manifest, err := ReadManifest(ctx, client, registryURL, token, member.Coordinate, member.Version)
		if err != nil {
			return nil, false, fmt.Errorf("version %s is already published, and its manifest cannot be read: %w", member.Version, err)
		}
		if diag.HasErrors(put.ValidateManifestFor(*manifest, member.MediaType, member.Manifest)) {
			return nil, false, fmt.Errorf("version %s is already published with another manifest", member.Version)
		}
		return manifest, true, nil
	}
	return nil, false, fmt.Errorf("publish returned %d", status)
}

// ReadManifest reads the stored manifest of coordinate at version.
func ReadManifest(ctx context.Context, client *http.Client, registryURL, token, coordinate, version string) (*put.Manifest, error) {
	namespace, pkg, err := put.SplitCoordinate(coordinate)
	if err != nil {
		return nil, err
	}
	if err := put.ValidVersion(version); err != nil {
		return nil, err
	}
	body, status, err := send(ctx, client, http.MethodGet, registryURL+put.ManifestPath(namespace, pkg, version), "", token, nil)
	if err != nil {
		return nil, fmt.Errorf("manifest read: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("manifest read returned %d", status)
	}
	manifest, diags := put.ParseManifest(body)
	if manifest == nil || diag.HasErrors(diags) {
		return nil, errors.New("invalid manifest read response")
	}
	return manifest, nil
}

// Publish checks member (Check), uploads its blobs, and publishes its
// manifest. The digest it returns is the SHA-256 of the payload the registry
// stores. An existing version is a reuse only when it holds the same manifest;
// anything else is an error.
func Publish(ctx context.Context, client *http.Client, registryURL, token string, member Member) (Published, error) {
	platforms, err := Check(member)
	if err != nil {
		return Published{}, err
	}
	for index, blob := range member.Blobs {
		if err := UploadBlob(ctx, client, registryURL, token, member.Coordinate, blob); err != nil {
			return Published{}, fmt.Errorf("upload blob %d: %w", index, err)
		}
	}
	stored, reused, err := PublishManifest(ctx, client, registryURL, token, member)
	if err != nil {
		return Published{}, err
	}
	return Published{Digest: put.Digest(stored.Payload), Platforms: platforms, Reused: reused}, nil
}

// send sends one request with the bearer and returns the bounded answer body
// and its status.
func send(ctx context.Context, client *http.Client, method, target, contentType, token string, body []byte) ([]byte, int, error) {
	client, err := withoutRedirects(client)
	if err != nil {
		return nil, 0, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, errors.New("the request cannot be created")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req) //nolint:gosec // registryURL is validated publish configuration
	if err != nil {
		return nil, 0, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, 0, errors.New("the registry answer cannot be read")
	}
	if len(data) > MaxResponseBytes {
		return nil, 0, fmt.Errorf("the registry answer exceeds %d bytes", MaxResponseBytes)
	}
	return data, resp.StatusCode, nil
}

// transportError keeps the kind of a failed exchange and drops its text, which
// names the request URL.
func transportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return errors.New("the registry did not answer")
}
