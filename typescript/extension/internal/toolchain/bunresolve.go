package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pinnedarchive"
)

// bunLock is the workspace lock identity of Bun, the toolchain of the task
// runtime.
const bunLock = "bun"

// ResolvedLocks returns the lock identities ResolveToolchains answers for. The
// extension manifest lists the same identities in runtime.resolves.
func ResolvedLocks() []string {
	return []string{bunLock}
}

var (
	// bunReleaseAPIURL is the GitHub release document of a Bun release, which
	// lists each archive with its SHA-256 digest.
	bunReleaseAPIURL = func(version string) string {
		return "https://api.github.com/repos/oven-sh/bun/releases/tags/bun-v" + version
	}
	// bunReleaseClient reads that document. Its timeout is shorter than the
	// one the CLI gives the toolchain-resolve call, so a lookup that stalls is
	// answered as a problem.
	bunReleaseClient = &http.Client{Timeout: 30 * time.Second}
)

// bunReleaseMetadataMaxBytes bounds the release document. One is about 100
// KB; the bound keeps a misrouted endpoint from streaming without end.
const bunReleaseMetadataMaxBytes = 16 << 20

// maxProblemBytes bounds the message and the remedy of a problem, as the
// toolchain-resolve contract does.
const maxProblemBytes = 1024

// ResolveToolchains answers a toolchain-resolve request for the workspace
// rooted at workspaceRoot: for each asked identity, the pin the lock records
// next, or the problem that keeps the extension from pinning it. Only bun has
// a pin (resolveBunPin); any other identity is a problem. A release lookup
// sends userAgent when it is not empty. The caller sets the result's
// Extension.
func ResolveToolchains(ctx context.Context, workspaceRoot, userAgent string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolveResult {
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Toolchains: make(map[string]wsproto.ToolchainResolution, len(request.Identities)),
	}
	for _, identity := range request.Identities {
		if identity != bunLock {
			result.Toolchains[identity] = toolchainProblem(
				fmt.Sprintf("the TypeScript extension knows no release of the toolchain %q; it answers for %s only", identity, bunLock), "")
			continue
		}
		result.Toolchains[identity] = resolveBunPin(ctx, workspaceRoot, userAgent, request)
	}
	return result
}

// resolveBunPin returns the Bun pin the lock records next, or the problem
// that keeps the extension from pinning one.
//
// The declaration is the release the root package.json names in
// packageManager (readDeclaredBunVersion). A committed pin of the declared
// release, or any committed pin when nothing is declared, is kept: a version
// 5 entry as it is, an older one with its archives derived offline from its
// source. A declared release the lock does not pin is resolved: the default
// release from the digests the extension ships, another one from the SHA-256
// digests its GitHub release lists, which an offline request cannot read.
// Nothing declared and nothing committed pins the default release, the one
// workspace-install declares for such a workspace.
//
// Every pin serves the request's platform, or the answer is a problem.
func resolveBunPin(ctx context.Context, workspaceRoot, userAgent string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolution {
	declared, err := readDeclaredBunVersion(workspaceRoot)
	if err != nil {
		return toolchainProblem("cannot read the Bun release the workspace declares: "+err.Error(),
			"fix the root package.json, then run `putnami install`")
	}
	committed, pinned := request.Locked[bunLock]
	pinned = pinned && strings.TrimSpace(committed.Version) != ""

	switch {
	case pinned && (declared == "" || committed.Version == declared):
		if len(committed.Archives) > 0 {
			return bunAnswer(committed, request.Platform, WorkspaceLockFile)
		}
		release := BunRelease{Version: committed.Version, Integrities: committed.Integrities, Source: committed.Source}
		return bunAnswer(bunPinOf(release), request.Platform, WorkspaceLockFile)
	case declared == "":
		return bunAnswer(bunPinOf(DefaultBunRelease()), request.Platform, "the default release of the extension")
	case !IsPlainBunRelease(declared):
		return toolchainProblem(
			fmt.Sprintf("package.json#packageManager declares bun@%s, which names no published Bun release", declared),
			"declare a release as bun@MAJOR.MINOR.PATCH in package.json#packageManager, then run `putnami install`")
	case declared == DefaultBunVersion:
		return bunAnswer(bunPinOf(DefaultBunRelease()), request.Platform, "the default release of the extension")
	case request.Offline:
		return toolchainProblem(
			fmt.Sprintf("package.json#packageManager declares Bun %s, which %s does not pin, and an offline resolution reads no release metadata",
				declared, WorkspaceLockFile),
			fmt.Sprintf("run `putnami install` with network access, so the extension reads the SHA-256 digests of Bun %s from its GitHub release", declared))
	}
	release, err := lookupBunRelease(ctx, declared, userAgent)
	if err != nil {
		return toolchainProblem(fmt.Sprintf("cannot read the SHA-256 digests of Bun %s: %v", declared, err),
			fmt.Sprintf("check that Bun %s is a published release and that GitHub is reachable, then run `putnami install` again", declared))
	}
	return bunAnswer(bunPinOf(release), request.Platform, "its GitHub release")
}

// bunPinOf returns release as a lock version 5 pin. Each platform that has a
// Bun target and a digest gets the archive bunArchiveFor names, a zip whose
// executable is bun-<target>/<program>, beside that digest. Any other platform
// is dropped, so every archive has an integrity and every integrity an
// archive. The pin records no source: each archive names its own URL.
func bunPinOf(release BunRelease) wsproto.LockToolchainEntry {
	pin := wsproto.LockToolchainEntry{
		Version:     release.Version,
		Archives:    make(map[string]wsproto.LockToolchainArchive, len(release.Integrities)),
		Integrities: make(map[string]string, len(release.Integrities)),
	}
	for platform, digest := range release.Integrities {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, ok := bunArchiveFor(release, goos, goarch)
		if !ok || strings.TrimSpace(digest) == "" {
			continue
		}
		if normalized, err := pinnedarchive.NormalizeIntegrity(digest); err == nil {
			digest = normalized
		}
		pin.Archives[platform] = wsproto.LockToolchainArchive{
			URL:        archive.pin.URL,
			Format:     string(archive.pin.Format),
			Executable: archive.dir + "/" + bunProgram(goos),
		}
		pin.Integrities[platform] = digest
	}
	return pin
}

// bunAnswer returns pin as the answer when it serves platform and follows the
// lock's version 5 entry rules, and a problem otherwise. from names where the
// release comes from, for the problem.
func bunAnswer(pin wsproto.LockToolchainEntry, platform, from string) wsproto.ToolchainResolution {
	_, archived := pin.Archives[platform]
	_, verified := pin.Integrities[platform]
	if !archived || !verified {
		return toolchainProblem(
			fmt.Sprintf("Bun %s from %s has no archive for %s, the platform of this machine", pin.Version, from, platform),
			fmt.Sprintf("declare a Bun release that publishes %s as bun@MAJOR.MINOR.PATCH in package.json#packageManager, then run `putnami install`", platform))
	}
	check := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Toolchains: map[string]wsproto.ToolchainResolution{bunLock: {Pin: &pin}},
	}
	var reasons []string
	for _, d := range diag.Errors(wsproto.ValidateToolchainResolveResult(&check)) {
		if strings.HasPrefix(d.Field, "toolchains."+bunLock) {
			reasons = append(reasons, d.Message)
		}
	}
	if len(reasons) > 0 {
		reason := reasons[0]
		if len(reasons) > 1 {
			reason += fmt.Sprintf(" (and %d more)", len(reasons)-1)
		}
		return toolchainProblem(
			fmt.Sprintf("Bun %s from %s is not a valid lock entry: %s", pin.Version, from, reason),
			"correct or remove toolchains.bun in "+WorkspaceLockFile+", then run `putnami install`")
	}
	return wsproto.ToolchainResolution{Pin: &pin}
}

// bunReleaseAssets is the part of a GitHub release document a lookup reads.
type bunReleaseAssets struct {
	Assets []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// lookupBunRelease reads the SHA-256 digest of each archive of Bun version
// from its GitHub release (bunReleaseAPIURL). An archive is the asset whose
// name is the file name of the URL bunArchiveFor builds for a platform. The
// release downloads from the vendor's release downloads.
func lookupBunRelease(ctx context.Context, version, userAgent string) (BunRelease, error) {
	url := bunReleaseAPIURL(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return BunRelease{}, fmt.Errorf("build the request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	// The URL is the package's GitHub endpoint, replaced only by tests.
	resp, err := bunReleaseClient.Do(req) //nolint:gosec // G704: no user-controlled destination
	if err != nil {
		return BunRelease{}, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // a read error is reported below
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BunRelease{}, fmt.Errorf("fetch %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bunReleaseMetadataMaxBytes+1))
	if err != nil {
		return BunRelease{}, fmt.Errorf("read %s: %w", url, err)
	}
	if len(body) > bunReleaseMetadataMaxBytes {
		return BunRelease{}, fmt.Errorf("read %s: the document exceeds %d bytes", url, bunReleaseMetadataMaxBytes)
	}
	var document bunReleaseAssets
	if err := json.Unmarshal(body, &document); err != nil {
		return BunRelease{}, fmt.Errorf("decode %s: %w", url, err)
	}

	release := BunRelease{Version: version, Source: bunReleaseSource(version)}
	platforms := make(map[string]string, len(bunTargets))
	for platform := range bunTargets {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, _ := bunArchiveFor(release, goos, goarch)
		platforms[path.Base(archive.pin.URL)] = platform
	}
	release.Integrities = make(map[string]string, len(platforms))
	for _, asset := range document.Assets {
		platform, ok := platforms[asset.Name]
		if !ok {
			continue
		}
		if digest, err := pinnedarchive.NormalizeIntegrity(asset.Digest); err == nil {
			release.Integrities[platform] = digest
		}
	}
	if len(release.Integrities) == 0 {
		return BunRelease{}, fmt.Errorf("%s lists no Bun archive with a SHA-256 digest", url)
	}
	return release, nil
}

// toolchainProblem is the answer for an identity the extension cannot pin,
// with the message and the remedy each held to the contract's bound.
func toolchainProblem(message, remedy string) wsproto.ToolchainResolution {
	return wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
		Message: boundedText(message),
		Remedy:  boundedText(remedy),
	}}
}

// urlCredentials matches the user information of a URL in text: the scheme
// is kept, and the credentials up to the "@" are dropped.
var urlCredentials = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/?#@\s"']*@`)

// boundedText returns text without the credentials of any URL in it, as valid
// UTF-8 of at most maxProblemBytes bytes, cut on a rune boundary and marked
// with "..." when it is longer. A problem is printed, and a lock source may
// carry credentials.
func boundedText(text string) string {
	text = urlCredentials.ReplaceAllString(strings.ToValidUTF8(text, "\uFFFD"), "$1")
	if len(text) <= maxProblemBytes {
		return text
	}
	cut := maxProblemBytes - len("...")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}
