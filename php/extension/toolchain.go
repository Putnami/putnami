package php

import (
	"fmt"
	"io"
	"os"
	"slices"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/composer"
	"go.putnami.dev/tooling/stacks"
)

// LockIdentity is the key of the PHP pin in the workspace lock.
const LockIdentity = "php"

// defaultVersion is the release a repository gets when it accepts it.
const defaultVersion = "8.4.23"

const (
	staticPHPURL  = "https://dl.static-php.dev/static-php-cli/bulk/php-%s-cli-%s-%s.tar.gz"
	windowsPHPURL = "https://windows.php.net/downloads/releases/archives/php-%s-nts-Win32-%s-x64.zip"
)

// release is a PHP release the extension can pin, with the SHA-256 of its
// archive for each platform.
type release struct {
	version string
	// windowsCompiler names the compiler in the URL of the Windows archive.
	windowsCompiler string
	digests         map[string]string
}

// releases lists the shipped releases, oldest first.
var releases = []release{
	{"8.3.32", "vs16", map[string]string{
		"darwin/amd64":  "6e059d66d149f2faa1d216e494a660752ef7e5769f2d4fded9a0e089574949a2",
		"darwin/arm64":  "ac297da5c45c525c660b8d0c73f5aa478ad9005d7c85096dc841220aa9c192e4",
		"linux/amd64":   "7928396c17aabcd5bb074fe82e07b4cb693c6d04360a4415799a143a9f9686c6",
		"linux/arm64":   "4c29858ecfa30d93e854d17494f422b593afa6da8ec80ae979191e69604dc955",
		"windows/amd64": "67c724e7b675b50d8f0476d816c3e2a3064ce3a53d572575d63c321cc0a3a6cf",
	}},
	{"8.4.23", "vs17", map[string]string{
		"darwin/amd64":  "be88a71134d43e8800946f8372da931ee1796c400a1fb4cf2afd718644251ffc",
		"darwin/arm64":  "4a5dca6df0211f7fb21425cf1c867968b2dfb7c079f98cf37770c5728e0bf709",
		"linux/amd64":   "5fa2b5f1cc9d7f79b19718926b4f4f0bb6949db52073b2fabf8ae52de3993af5",
		"linux/arm64":   "1faabbea9c500daab9ba202cb75ea9876191a2a8f902244e9d0464addb9b55b1",
		"windows/amd64": "826efa189b21f46314ad497ff31467de9f0953292f42b235542be4feea182b48",
	}},
	{"8.5.8", "vs17", map[string]string{
		"darwin/amd64":  "d5a9a505ebce66c7f6b4f4e16629c36f385f2d360df915875722d67fe8bb2161",
		"darwin/arm64":  "5e5032e8244a2367b1e8a9c70ff6f793dee7433966c9291da85fadf2167cd55f",
		"linux/amd64":   "bfaa12e0f6f5788259bc20d0d3f08e1e851bce7f05d43e38fb77041e12a32663",
		"linux/arm64":   "86ea1fe2f6d2415eff78c2e0bcb479b221d5637c89d72e19809b65c4c0527c27",
		"windows/amd64": "63a3f6493f37c9ff3e288ec16621222a6cda5167dd1abffec0019e7f18c8e7e9",
	}},
}

// HandleToolchainResolve serves the toolchain-resolve control call. The bool
// reports whether args name that call.
func HandleToolchainResolve(args []string, stdin io.Reader, stdout io.Writer, profile stacks.Profile) (bool, error) {
	return wsproto.ServeToolchainResolve(args, stdin, stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve the workspace root: %w", err)
		}
		return answerToolchainResolve(root, request, profile), nil
	})
}

// answerToolchainResolve answers one request against the workspace at root. An
// identity it pins nothing for is omitted, which leaves the lock without it.
func answerToolchainResolve(root string, request wsproto.ToolchainResolveRequest, profile stacks.Profile) wsproto.ToolchainResolveResult {
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  ExtensionName,
		Toolchains: map[string]wsproto.ToolchainResolution{},
	}
	for _, identity := range request.Identities {
		if identity != LockIdentity {
			result.Toolchains[identity] = wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
				Message: fmt.Sprintf("%s pins the %s toolchain only, not %q", ExtensionName, LockIdentity, identity),
			}}
			continue
		}
		if pin, ok := resolvePin(root, request, profile); ok {
			result.Toolchains[identity] = wsproto.ToolchainResolution{Pin: &pin}
		}
	}
	return result
}

// resolvePin returns the PHP release the workspace at root pins, among the
// releases every component accepts, or the root accepts when no release
// suits every component. A committed pin of an accepted shipped release is kept;
// otherwise it is defaultVersion when accepted, else the highest accepted
// shipped release. On a platform no shipped archive serves, a committed pin
// of the chosen release is kept as committed, and nothing else is pinned. A
// workspace with no composer.json outside vendor/, or whose root states a
// version it cannot read, pins nothing: its tasks report that statement.
func resolvePin(root string, request wsproto.ToolchainResolveRequest, profile stacks.Profile) (wsproto.LockToolchainEntry, bool) {
	found, _ := composer.Candidates(root)
	if len(found) == 0 {
		return wsproto.LockToolchainEntry{}, false
	}
	expected, err := profile.Toolchain.ExpectedVersion(root, wsproto.ProbeRootPath, wsproto.ProbeRootPath)
	if err != nil {
		return wsproto.LockToolchainEntry{}, false
	}
	accepted := acceptedBy(profile.Toolchain, releases, expected)
	everyComponent := accepted
	for _, dir := range found {
		if statement, err := profile.Toolchain.ExpectedVersion(root, dir, wsproto.ProbeRootPath); err == nil {
			everyComponent = acceptedBy(profile.Toolchain, everyComponent, statement)
		}
	}
	if len(everyComponent) > 0 {
		accepted = everyComponent
	}
	committed, locked := request.Locked[LockIdentity]
	chosen, ok := choose(accepted, committed.Version)
	if !ok {
		return wsproto.LockToolchainEntry{}, false
	}
	if _, served := chosen.digests[request.Platform]; !served {
		// No archive serves this host, so it cannot pin PHP. The committed pin
		// of the chosen release stays as committed, for the platforms it
		// serves; answering no pin would remove it from the lock.
		if locked && committed.Version == chosen.version {
			return committed, true
		}
		return wsproto.LockToolchainEntry{}, false
	}
	return chosen.lockEntry(), true
}

// acceptedBy returns the releases expected accepts, oldest first.
func acceptedBy(toolchain stacks.Toolchain, candidates []release, expected stacks.Expected) []release {
	return slices.DeleteFunc(slices.Clone(candidates), func(r release) bool {
		return toolchain.Compare(expected, r.version).Fail
	})
}

// choose picks among accepted releases, oldest first.
func choose(accepted []release, committed string) (release, bool) {
	if len(accepted) == 0 {
		return release{}, false
	}
	for _, version := range []string{committed, defaultVersion} {
		if i := slices.IndexFunc(accepted, func(r release) bool { return r.version == version }); i >= 0 {
			return accepted[i], true
		}
	}
	return accepted[len(accepted)-1], true
}

// lockEntry returns the release as a lock version 5 pin.
func (r release) lockEntry() wsproto.LockToolchainEntry {
	entry := wsproto.LockToolchainEntry{
		Version:     r.version,
		Archives:    make(map[string]wsproto.LockToolchainArchive, len(r.digests)),
		Integrities: r.digests,
	}
	for platform := range r.digests {
		entry.Archives[platform] = r.archive(platform)
	}
	return entry
}

// archive returns the archive of the release for platform.
func (r release) archive(platform string) wsproto.LockToolchainArchive {
	if platform == "windows/amd64" {
		return wsproto.LockToolchainArchive{
			URL:        fmt.Sprintf(windowsPHPURL, r.version, r.windowsCompiler),
			Format:     wsproto.LockArchiveFormatZip,
			Executable: "php.exe",
		}
	}
	system, arch := "linux", "x86_64"
	if platform == "darwin/amd64" || platform == "darwin/arm64" {
		system = "macos"
	}
	if platform == "darwin/arm64" || platform == "linux/arm64" {
		arch = "aarch64"
	}
	return wsproto.LockToolchainArchive{
		URL:        fmt.Sprintf(staticPHPURL, r.version, system, arch),
		Format:     wsproto.LockArchiveFormatTarGz,
		Executable: "php",
	}
}
