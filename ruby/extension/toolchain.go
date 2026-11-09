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
// archive for each platform. releases.go lists them; to move to another
// rv-ruby release, run scripts/rv-ruby-releases.sh with its tag.
type release struct {
	version string
	digests map[string]string
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
