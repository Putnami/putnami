package memberprobe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/pinnedarchive"
)

// PutEcosystem is the ecosystem id of an archive member on the put registry.
const PutEcosystem = "put"

// Archive is one archive of a put member: the registry it is published to, its
// coordinate and version, and the platform it was built for. The put registry
// serves one archive per platform, so a member published for several platforms
// is probed once per platform.
type Archive struct {
	// Registry is the put registry endpoint, such as https://put.putnami.dev.
	Registry string
	// Coordinate is the member's "<namespace>/<package>" name.
	Coordinate string
	// Version is the exact version the real publish would write.
	Version string
	// OS and Arch name the platform of the archive.
	OS, Arch string
	// Digest is the digest of the local archive, as "sha256:" followed by 64
	// lowercase hex characters, or empty when the dry run built none.
	Digest string
	// Token is the bearer to send, or empty for an anonymous request. A probe
	// never fails because no credential resolved.
	Token string
	// Client sends the request. Nil selects NewHTTPClient.
	Client *http.Client
}

// ProbeArchive asks the put registry whether it already holds one archive of a
// member at an exact version. It sends a single HEAD to the download endpoint
// and compares the digest the registry advertises with the local archive's, so
// no archive is transferred.
//
// An anonymous request for a private archive that exists is answered 404, as
// for one that does not exist. Such a probe reports absent with Anonymous set,
// and the orchestrator says that the verdict is weaker.
func ProbeArchive(ctx context.Context, archive Archive) extproto.MemberProbe {
	subject := Subject{
		Ecosystem:      PutEcosystem,
		Coordinate:     archive.Coordinate,
		Version:        archive.Version,
		Platform:       archive.OS + "/" + archive.Arch,
		Registry:       archive.Registry,
		ArtifactDigest: archive.Digest,
		Anonymous:      archive.Token == "",
	}
	endpoint, err := archiveProbeURL(archive)
	if err != nil {
		return subject.Unverified(err.Error())
	}
	client := archive.Client
	if client == nil {
		if client, err = NewHTTPClient(); err != nil {
			return subject.Unverified(err.Error())
		}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return subject.Unverified("the put registry request could not be built")
	}
	if archive.Token != "" {
		request.Header.Set("Authorization", "Bearer "+archive.Token)
	}
	response, err := client.Do(request) //nolint:gosec // G704: the endpoint is archiveProbeURL's validated registry origin and escaped coordinate
	if err != nil {
		return subject.Unreachable(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))

	switch response.StatusCode {
	case http.StatusOK:
		// An answer that names another version is unverified.
		if resolved := response.Header.Get("X-Resolved-Version"); resolved != "" && resolved != archive.Version {
			return subject.Unverified(fmt.Sprintf("the registry resolved version %s, not %s", Redact(resolved, archive.Token), archive.Version))
		}
		return subject.Held(advertisedArchiveDigest(response.Header))
	case http.StatusNotFound:
		return subject.Absent()
	default:
		return subject.Refused(response.StatusCode, "")
	}
}

// archiveProbeURL builds the download URL of one archive. It refuses a registry
// URL that carries user information, a query or a fragment, and plain HTTP
// anywhere but loopback.
func archiveProbeURL(archive Archive) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(archive.Registry))
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("the put registry URL must be an absolute HTTP(S) URL without credentials")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("the put registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		ip := net.ParseIP(hostname)
		if hostname != "localhost" && !strings.HasSuffix(hostname, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("the put registry URL must use HTTPS (HTTP is allowed only for loopback)")
		}
	default:
		return "", fmt.Errorf("the put registry URL must use HTTP(S)")
	}
	namespace, name, found := strings.Cut(archive.Coordinate, "/")
	if !found || namespace == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("the put coordinate must be <namespace>/<package>")
	}
	if archive.Version == "" || archive.OS == "" || archive.Arch == "" {
		return "", fmt.Errorf("the put archive needs a version, an os and an arch")
	}
	query := url.Values{}
	query.Set("channel", archive.Version)
	query.Set("os", archive.OS)
	query.Set("arch", archive.Arch)
	return strings.TrimRight(parsed.String(), "/") + "/" + url.PathEscape(namespace) + "/" + url.PathEscape(name) + "/download?" + query.Encode(), nil
}

// advertisedArchiveDigest returns the SHA-256 digest the registry advertises
// for the archive, as "sha256:" and 64 lowercase hex characters, or empty when
// it advertises none in a form pinnedarchive.NormalizeIntegrity accepts.
func advertisedArchiveDigest(header http.Header) string {
	digest, err := pinnedarchive.NormalizeIntegrity(pinnedarchive.ReadAdvertisedIntegrity(header))
	if err != nil {
		return ""
	}
	return "sha256:" + digest
}
