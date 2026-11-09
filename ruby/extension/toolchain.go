package ruby

import (
	"fmt"
	"io"
	"os"
	"slices"

	wsproto "go.putnami.dev/protocol/workspace"
	rubydetect "go.putnami.dev/tooling/observe/detect/ruby"
	"go.putnami.dev/tooling/stacks"
)

// LockIdentity is the key of the Ruby pin in the workspace lock.
const LockIdentity = "ruby"

// rvRubyRelease is the spinel-coop/rv-ruby release the archives come from.
const rvRubyRelease = "20261023"

const rvRubyURL = "https://github.com/spinel-coop/rv-ruby/releases/download/" + rvRubyRelease + "/ruby-%s.%s.tar.gz"

// rvRubyPlatforms names the rv-ruby build of each platform. Linux takes the
// glibc build: the lock keys a platform by system and architecture only.
var rvRubyPlatforms = map[string]string{
	"darwin/amd64": "ventura",
	"darwin/arm64": "arm64_sonoma",
	"linux/amd64":  "x86_64_linux",
	"linux/arm64":  "arm64_linux",
}

// release is a Ruby release the extension can pin, with the SHA-256 of its
// archive for each platform.
type release struct {
	version string
	digests map[string]string
}

// releases lists the shipped releases, oldest first: the latest stable patch
// of each minor the rv-ruby release holds.
var releases = []release{
	{"3.2.11", map[string]string{
		"darwin/amd64": "b40be21a5b224329e59d4276131c787cb0ae68617884511e0f256745cd8d5f10",
		"darwin/arm64": "a6cbd7d16b735572ec98562d6f4ef13afe40c9318a1c0919ff9971f9d6d7d7e2",
		"linux/amd64":  "773beb5b6b9b4f1c3a03dcbfc3b76025b574a409f1d79233b119b6cbbf1c9eb9",
		"linux/arm64":  "869d0cb8cccaab7029b1ecd22408b035f74c4b3201c77be0e12df8c27562758d",
	}},
	{"3.3.13", map[string]string{
		"darwin/amd64": "5888338a10eca3c337919688af7a4c670be27c1f3527e42b4e333f915cad50d3",
		"darwin/arm64": "4b20f7b1a07327c54c2cb88ec6e36490b4dbb7703d0296e794526d9cc37967d8",
		"linux/amd64":  "e122ba0e5ef87b4e948c73a08696677df81bb0b7991c9b656918540ec940b2c9",
		"linux/arm64":  "8844a7fb0a1debd8bc3ddad626000d55c27123b7d67d7650c6b16005c5ec0db0",
	}},
	{"3.4.12", map[string]string{
		"darwin/amd64": "f0486193baf65317f875927fc8656b3fe5cec06a165648a401054b3f206f96bf",
		"darwin/arm64": "ca533624180ee28a740017fd7adb310d6b0fd067a3409c7d3c1b538f94cf9494",
		"linux/amd64":  "870cb1547941444754ca4eff50732d5a07a26feb59fbf932f9f6650ed05c18a7",
		"linux/arm64":  "a7cdda85baa73ab9d39d1efe730d613762bfdf1f7b4d6fc168486df971744dff",
	}},
	{"4.0.9", map[string]string{
		"darwin/amd64": "46525402025acde1428a014cf345a3653ecbc496a0606eb4a60ef08999632f86",
		"darwin/arm64": "38130a97c314e2e44198f37fc6555a66ab9a8011ecc5980d4ae36f2858ed11b5",
		"linux/amd64":  "16b24f5b3aac3925f95cbbdd08e55c2877aec4f2614edf44af236a59a6591aa4",
		"linux/arm64":  "6b49808f690ecfe0f51f266b9c3477908f69d7a927d90355565368c40c875d22",
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

// resolvePin returns the Ruby release the workspace at root pins, among the
// releases every component accepts, or the root accepts when no release suits
// every component. A committed pin of an accepted shipped release is kept;
// otherwise it is the highest accepted shipped release. A workspace with no
// Ruby component, or whose root states a version it cannot read, pins
// nothing: its tasks report that statement.
func resolvePin(root string, request wsproto.ToolchainResolveRequest, profile stacks.Profile) (wsproto.LockToolchainEntry, bool) {
	components := rubydetect.Detect(root).Components
	if len(components) == 0 {
		return wsproto.LockToolchainEntry{}, false
	}
	expected, err := profile.Toolchain.ExpectedVersion(root, wsproto.ProbeRootPath, wsproto.ProbeRootPath)
	if err != nil {
		return wsproto.LockToolchainEntry{}, false
	}
	accepted := acceptedBy(profile.Toolchain, releases, expected)
	everyComponent := accepted
	for _, c := range components {
		if statement, err := profile.Toolchain.ExpectedVersion(root, c.ID, bundleRoot(root, c.ID)); err == nil {
			everyComponent = acceptedBy(profile.Toolchain, everyComponent, statement)
		}
	}
	if len(everyComponent) > 0 {
		accepted = everyComponent
	}
	chosen, ok := choose(accepted, request.Locked[LockIdentity].Version)
	if !ok {
		return wsproto.LockToolchainEntry{}, false
	}
	if _, served := chosen.digests[request.Platform]; !served {
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

// choose picks among accepted releases, oldest first: the committed one when
// accepted, else the newest.
func choose(accepted []release, committed string) (release, bool) {
	if len(accepted) == 0 {
		return release{}, false
	}
	if i := slices.IndexFunc(accepted, func(r release) bool { return r.version == committed }); i >= 0 {
		return accepted[i], true
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

// archive returns the archive of the release for platform. Every rv-ruby
// archive holds its Ruby under rv-ruby@<version>/<version>.
func (r release) archive(platform string) wsproto.LockToolchainArchive {
	return wsproto.LockToolchainArchive{
		URL:        fmt.Sprintf(rvRubyURL, r.version, rvRubyPlatforms[platform]),
		Format:     wsproto.LockArchiveFormatTarGz,
		Executable: "rv-ruby@" + r.version + "/" + r.version + "/bin/ruby",
	}
}
