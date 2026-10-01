package lifecycle

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cmderr"
)

// initChannelEnv is the environment variable that chooses the channel
// `putnami init` resolves on when --channel is absent.
const initChannelEnv = "PUTNAMI_CHANNEL"

// initChannelFlag is the flag that chooses the channel `putnami init` resolves
// on.
const initChannelFlag = "--channel"

// latestChannel is the channel a resolution reads when nothing chose another.
const latestChannel = "latest"

// stableChannel is the alias of latestChannel, as in `putnami upgrade`.
const stableChannel = "stable"

// initChannelJobOption is the job option that hands the channel to the
// workspace installers. `putnami upgrade` hands its registry channel to the
// deps-upgrade job under the same name.
const initChannelJobOption = "putnami-channel"

// portableChannel matches the channel alphabet every ecosystem accepts
// (extensionproto.PortableChannelPattern): a lowercase letter or a digit, then
// lowercase letters, digits, ".", "_" and "-", 64 characters at most. A name
// in it goes unchanged into a registry query, a module proxy path and an
// `<artifact>@<channel>` reference.
var portableChannel = regexp.MustCompile(extensionproto.PortableChannelPattern)

// versionTag matches a semantic version, with or without a leading v, and
// whatever pre-release or build suffix follows it.
var versionTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([.+-].*)?$`)

// installedCLIName matches the name the installers give the CLI:
// putnami-<variant>-<tag>, where the variant is go or ts.
var installedCLIName = regexp.MustCompile(`^putnami-(?:go|ts)-(.+)$`)

// Where the channel of an init run came from, as init prints it.
const (
	initChannelFromFlag    = initChannelFlag
	initChannelFromEnv     = initChannelEnv
	initChannelFromInstall = "the channel this CLI was installed from"
)

// initChannel is the one channel every resolution of an init run reads: the
// extensions, the template and the starter's dependencies.
type initChannel struct {
	// name is the channel, latestChannel when nothing chose another.
	name string
	// origin states what chose name; empty when nothing did.
	origin string
}

// selected is the channel to hand to a resolution: "" for latestChannel,
// which a resolution reads when it is handed none.
func (c initChannel) selected() string {
	if c.name == latestChannel {
		return ""
	}
	return c.name
}

// resolveInitChannel returns the channel an init run resolves on: --channel,
// else PUTNAMI_CHANNEL, else the channel the running CLI was installed from,
// else latest. An empty PUTNAMI_CHANNEL chooses nothing. A flag or a variable
// that names no channel, a name outside the portable alphabet or an exact
// version is a usage error, so nothing is written for it. executableName is
// the file name the running executable resolves to.
func resolveInitChannel(flags initFlags, getenv func(string) string, executableName string) (initChannel, error) {
	if flags.channelSet {
		name, err := channelName(flags.channel, initChannelFromFlag)
		if err != nil {
			return initChannel{}, err
		}
		return initChannel{name: name, origin: initChannelFromFlag}, nil
	}
	if value := getenv(initChannelEnv); value != "" {
		name, err := channelName(value, initChannelFromEnv)
		if err != nil {
			return initChannel{}, err
		}
		return initChannel{name: name, origin: initChannelFromEnv}, nil
	}
	if name := installRecordChannel(executableName, runtime.GOOS); name != "" {
		return initChannel{name: name, origin: initChannelFromInstall}, nil
	}
	return initChannel{name: latestChannel}, nil
}

// channelName validates the channel a flag or a variable names and returns the
// channel to resolve on: stable reads latest. source names the flag or the
// variable in the error.
func channelName(value, source string) (string, error) {
	name := strings.TrimSpace(value)
	switch {
	case name == "":
		return "", cmderr.Usagef("%s needs a channel name, for example canary", source)
	case versionTag.MatchString(name):
		return "", cmderr.Usagef("invalid channel %q from %s: init resolves on a channel, not on an exact version, "+
			"because the release lines do not share one version", name, source)
	case !portableChannel.MatchString(name):
		return "", cmderr.Usagef("invalid channel %q from %s: a channel name starts with a lowercase letter or a digit "+
			"and holds only lowercase letters, digits, \".\", \"_\" and \"-\", 64 characters at most", name, source)
	}
	if name == stableChannel {
		return latestChannel, nil
	}
	return name, nil
}

// installRecordChannel is the channel the installer recorded in the file name
// of the running CLI, or "" when the name records none.
//
// The installers place the CLI as putnami-<variant>-<tag> and point `putnami`
// at it, so the name the executable resolves to is the record of the channel
// it came from. <tag> is a channel when it is in the portable alphabet and is
// not a semantic version, which a version install and `putnami upgrade` write,
// a source-<revision> build, or dev, the name of a local build. On Windows the
// active putnami.exe is a copy of the installed file, not a link to it: the
// name is lost and no record applies.
func installRecordChannel(executableName, goos string) string {
	name := executableName
	if goos == "windows" && len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		name = name[:len(name)-4]
	}
	match := installedCLIName.FindStringSubmatch(name)
	if match == nil {
		return ""
	}
	tag := match[1]
	if !portableChannel.MatchString(tag) || versionTag.MatchString(tag) {
		return ""
	}
	if tag == "dev" || strings.HasPrefix(tag, "source-") {
		return ""
	}
	if tag == stableChannel {
		return latestChannel
	}
	return tag
}

// runningCLIName is the file name of the running executable once its links
// resolve, or "" when the executable cannot be found. A test replaces it.
var runningCLIName = func() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Base(path)
}

// channelArtifact is the install argument that resolves name on channel for
// one install: the bare name for no channel, which resolves as the workspace
// configures it, else name@channel. The workspace config keeps the bare name
// either way, and the lock records the exact version the channel resolved.
func channelArtifact(name, channel string) string {
	if channel == "" {
		return name
	}
	return name + "@" + channel
}
