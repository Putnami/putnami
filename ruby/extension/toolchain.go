package ruby

import (
	"fmt"
	"io"

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
	return stacks.ServeToolchainResolve(args, stdin, stdout, ExtensionName, LockIdentity, pinResolver(profile))
}

// pinResolver answers the release the workspace pins, as resolvePin does.
func pinResolver(profile stacks.Profile) stacks.PinResolver {
	return func(root string, request wsproto.ToolchainResolveRequest) (wsproto.LockToolchainEntry, bool) {
		return resolvePin(root, request, profile)
	}
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
	return stacks.Accepted(candidates, release.name, func(version string) bool {
		return !toolchain.Compare(expected, version).Fail
	})
}

// name is the version of the release.
func (r release) name() string { return r.version }

// choose picks among accepted releases, oldest first: the committed one when
// accepted, else the newest.
func choose(accepted []release, committed string) (release, bool) {
	return stacks.Choose(accepted, release.name, committed)
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
