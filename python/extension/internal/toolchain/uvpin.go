package toolchain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

// UVLockIdentity is the workspace lock identity of uv: the key of its pin in
// the lock's toolchains. The uv runtime toolchain of the extension manifest
// locks it, and the manifest's runtime.resolves lists it.
const UVLockIdentity = "uv"

// Bounds of the checksum file read for a release the extension ships no
// digests for. A uv checksum file lists a few dozen archives. The timeout is
// shorter than the one the CLI gives the toolchain-resolve call, so a read
// that stalls is answered as a problem.
const (
	uvChecksumsTimeout  = 30 * time.Second
	uvChecksumsMaxBytes = 1 << 20
)

// uvProblemMaxBytes is the bound the toolchain-resolve protocol sets on the
// message and on the remedy of a problem.
const uvProblemMaxBytes = 1024

// uvReleaseURL is the URL the archives of a release are published under, with
// a trailing slash. The release's checksum file (uvtoolchain.ChecksumsFile)
// sits beside them. Tests point it at a local server.
var uvReleaseURL = uvtoolchain.ReleaseURL

// uvLockOrigin names the committed uv pin in a message.
const uvLockOrigin = "toolchains.uv in " + WorkspaceLockFile

// ResolveUVPin answers the toolchain-resolve request for UVLockIdentity in
// the workspace at workspaceRoot: the uv pin the lock records next, or the
// problem that keeps the extension from pinning one. The committed pin is
// request.Locked[UVLockIdentity].
//
//   - A [tool.uv] required-version of the root pyproject.toml that names no
//     release Putnami runs (uvtoolchain Declaration.Problem) is a problem.
//   - A committed pin of the declared release, or any committed pin when the
//     file declares none, is kept: a lock version 5 entry as is, and a version
//     3 or 4 entry as the archives of its source
//     (uvLockPin). A kept release must be one Putnami runs.
//   - A declared release the lock does not pin is pinned: DefaultUVVersion
//     with the digests the extension ships, and any other release with the
//     digests its checksum file lists, which an offline request does not
//     read.
//   - With neither a declaration nor a committed pin, DefaultUVRelease is
//     pinned: the release workspace-install declares for such a workspace.
//
// Every pin records an archive and a SHA-256 for request.Platform; a pin that
// cannot serve that platform is answered as a problem. The error reports a
// workspace whose pyproject.toml cannot be read.
func ResolveUVPin(workspaceRoot string, request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolution, error) {
	declaration, err := uvtoolchain.ReadWorkspaceDeclaration(workspaceRoot)
	if err != nil {
		return wsproto.ToolchainResolution{}, err
	}
	if problem := declaration.Problem(); problem != nil {
		return uvProblem(problem.Error(),
			"Write that declaration in the [tool.uv] table of "+RootPyproject+", then run `putnami install`."), nil
	}
	committed, pinned := request.Locked[UVLockIdentity]
	version := strings.TrimSpace(committed.Version)
	switch {
	case pinned && version != "" && (declaration.Version == "" || declaration.Version == version):
		return keepUVPin(committed, request.Platform), nil
	case declaration.Version != "":
		return pinDeclaredUV(declaration.Version, request), nil
	default:
		return uvPinServing(DefaultUVRelease(), request.Platform, "the @putnami/python extension", ""), nil
	}
}

// keepUVPin returns the committed pin as the lock version 5 pin of platform.
func keepUVPin(committed wsproto.LockToolchainEntry, platform string) wsproto.ToolchainResolution {
	version := strings.TrimSpace(committed.Version)
	if err := uvtoolchain.Supported(version); err != nil {
		return uvProblem(fmt.Sprintf("%s pins a release Putnami cannot run: %v", uvLockOrigin, err),
			fmt.Sprintf("Declare required-version = \"==%s\" or a later release in the [tool.uv] table of %s, then run `putnami install`.",
				uvtoolchain.MinimumVersion, RootPyproject))
	}
	repin := "Remove toolchains.uv from " + WorkspaceLockFile + ", then run `putnami install` to pin uv again."
	if len(committed.Archives) > 0 {
		return uvServes(committed, platform, uvLockOrigin, repin)
	}
	release := UVRelease{Version: version, Integrities: committed.Integrities, Source: committed.Source}
	return uvPinServing(release, platform, uvLockOrigin, repin)
}

// pinDeclaredUV pins version, the release the root pyproject.toml declares
// and the lock does not pin.
func pinDeclaredUV(version string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolution {
	if version == DefaultUVVersion {
		return uvPinServing(DefaultUVRelease(), request.Platform, "the @putnami/python extension", "")
	}
	if request.Offline {
		return uvProblem(
			fmt.Sprintf("%s declares uv %s, which %s does not pin, and an offline install reads no release checksums",
				uvDeclarationOrigin, version, WorkspaceLockFile),
			"Run `putnami install` with network access: it pins uv "+version+" from the checksum file of its release.")
	}
	checksums := uvReleaseURL(version) + uvtoolchain.ChecksumsFile
	body, err := fetchUVChecksums(checksums)
	if err != nil {
		return uvProblem(fmt.Sprintf("cannot read the SHA-256 digests of uv %s: %v", version, err),
			"Check that uv "+version+" is a published release, then run `putnami install` again when its checksum file is reachable.")
	}
	redeclare := "Declare another release as required-version in the [tool.uv] table of " + RootPyproject + ", then run `putnami install`."
	digests := uvtoolchain.PlatformDigests(body)
	if len(digests) == 0 {
		return uvProblem(fmt.Sprintf("uv %s publishes no SHA-256 for a supported archive in %s", version, checksums), redeclare)
	}
	release := UVRelease{Version: version, Integrities: digests, Source: uvReleaseURL(version)}
	return uvPinServing(release, request.Platform, checksums, redeclare)
}

// uvPinServing returns release as the lock version 5 pin of platform
// (uvLockPin, uvServes). origin names where the release comes from, and remedy
// says what to change when its pin cannot serve platform.
func uvPinServing(release UVRelease, platform, origin, remedy string) wsproto.ToolchainResolution {
	pin, err := uvLockPin(release)
	if err != nil {
		return uvProblem(fmt.Sprintf("%s: %v", origin, err), remedy)
	}
	return uvServes(pin, platform, origin, remedy)
}

// uvServes returns pin when it records an archive and a SHA-256 for platform,
// and otherwise the problem that says why it cannot serve that platform.
func uvServes(pin wsproto.LockToolchainEntry, platform, origin, remedy string) wsproto.ToolchainResolution {
	_, archived := pin.Archives[platform]
	_, digested := pin.Integrities[platform]
	if archived && digested {
		return wsproto.ToolchainResolution{Pin: &pin}
	}
	goos, goarch, _ := strings.Cut(platform, "/")
	if _, ok := uvtoolchain.ArchiveFor(goos, goarch); !ok {
		return uvProblem(
			fmt.Sprintf("Putnami installs no uv on %s, the host platform: it installs uv on %s",
				platform, strings.Join(uvtoolchain.Platforms(), ", ")),
			"Run `putnami install` on one of those platforms.")
	}
	return uvProblem(fmt.Sprintf("%s records no archive of uv %s for %s, the host platform", origin, pin.Version, platform), remedy)
}

// uvLockPin returns release as a lock version 5 pin. Each platform the release
// records a SHA-256 for, and uvArchiveFor names an archive for, gets the
// archive uv publishes for it: its URL, its format, and the path of the uv
// program inside it. Any other platform is left out.
//
// The error says why the release names no archive a lock records: a source
// that is not an http or https URL with a host, a source that carries
// credentials, or a SHA-256 that is not 64 hexadecimal characters once
// trimmed and lowercased. It never repeats the source, which may hold
// credentials.
func uvLockPin(release UVRelease) (wsproto.LockToolchainEntry, error) {
	pin := wsproto.LockToolchainEntry{
		Version:     release.Version,
		Archives:    map[string]wsproto.LockToolchainArchive{},
		Integrities: map[string]string{},
	}
	for _, platform := range slices.Sorted(maps.Keys(release.Integrities)) {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, ok := uvArchiveFor(release, goos, goarch)
		if !ok {
			continue
		}
		digest := strings.ToLower(strings.TrimSpace(archive.pin.SHA256))
		if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
			return wsproto.LockToolchainEntry{}, fmt.Errorf("the SHA-256 recorded for %s is not 64 hexadecimal characters", platform)
		}
		if err := lockableArchiveURL(archive.pin.URL); err != nil {
			return wsproto.LockToolchainEntry{}, err
		}
		format := wsproto.LockArchiveFormatTarGz
		if archive.pin.Format == pinnedarchive.Zip {
			format = wsproto.LockArchiveFormatZip
		}
		pin.Archives[platform] = wsproto.LockToolchainArchive{
			URL:        archive.pin.URL,
			Format:     format,
			Executable: path.Join(archive.dir, uvProgram(goos)),
		}
		pin.Integrities[platform] = digest
	}
	return pin, nil
}

// lockableArchiveURL returns nil when rawURL is an archive URL a lock version
// 5 pin records: http or https, with a host and without credentials. Its error
// does not repeat rawURL.
func lockableArchiveURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	switch {
	case err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "":
		return errors.New("the source is not an http or https URL with a host")
	case parsed.User != nil:
		return errors.New("the source carries credentials, which a lock version 5 pin does not record")
	}
	return nil
}

// fetchUVChecksums reads the checksum file at location, within
// uvChecksumsTimeout and uvChecksumsMaxBytes.
func fetchUVChecksums(location string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uvChecksumsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request) //nolint:gosec // G704: location is the checksum file under uvReleaseURL of a release uvtoolchain.Supported accepts
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %s", location, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, uvChecksumsMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", location, err)
	}
	if len(body) > uvChecksumsMaxBytes {
		return "", fmt.Errorf("read %s: the file exceeds %d bytes", location, uvChecksumsMaxBytes)
	}
	return string(body), nil
}

// uvProblem returns a problem resolution, with message and remedy each cut to
// uvProblemMaxBytes.
func uvProblem(message, remedy string) wsproto.ToolchainResolution {
	return wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
		Message: clipProblemText(message),
		Remedy:  clipProblemText(remedy),
	}}
}

// clipProblemText returns text cut to uvProblemMaxBytes, ending in "..." when
// it is cut, and never inside a UTF-8 sequence.
func clipProblemText(text string) string {
	if len(text) <= uvProblemMaxBytes {
		return text
	}
	const ellipsis = "..."
	return strings.ToValidUTF8(text[:uvProblemMaxBytes-len(ellipsis)], "") + ellipsis
}
