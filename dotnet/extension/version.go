package dotnet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// sdkVersion is a .NET SDK version: major.minor.patch, whose patch is the
// feature band times 100 plus the patch within the band (8.0.404: band 4,
// patch 4), and an optional prerelease label, which orders it below its
// release.
var sdkVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)

// MatchSDK reports whether the machine's SDK satisfies the version
// global.json states, under its sdk.rollForward policy:
//
//   - disable: the same version;
//   - patch and latestPatch, the default: the same feature band, and a patch
//     at least the expected one;
//   - feature and latestFeature: the same major and minor, at least the
//     expected version;
//   - minor and latestMinor: the same major, at least the expected version;
//   - major and latestMajor: at least the expected version.
//
// An expected version that the toolchain.version option sets, rather than a
// global.json, keeps the default policy. A version that is no SDK version
// matches only itself.
func MatchSDK(root string, expected stacks.Expected, machine string) bool {
	e, eok := parseSDK(expected.Version)
	m, mok := parseSDK(machine)
	if !eok || !mok {
		return expected.Version == machine
	}
	if m.less(e) {
		return false
	}
	switch rollForward(root, expected.Source) {
	case "disable":
		return m == e
	case "feature", "latestfeature":
		return m.major == e.major && m.minor == e.minor
	case "minor", "latestminor":
		return m.major == e.major
	case "major", "latestmajor":
		return true
	default:
		return m.major == e.major && m.minor == e.minor && m.patch/100 == e.patch/100
	}
}

type sdk struct {
	major, minor, patch int
	prerelease          string
}

func parseSDK(v string) (sdk, bool) {
	match := sdkVersion.FindStringSubmatch(strings.TrimSpace(v))
	if len(match) != 5 {
		return sdk{}, false
	}
	major, errMajor := strconv.Atoi(match[1])
	minor, errMinor := strconv.Atoi(match[2])
	patch, errPatch := strconv.Atoi(match[3])
	if errMajor != nil || errMinor != nil || errPatch != nil {
		return sdk{}, false
	}
	return sdk{major, minor, patch, match[4]}, true
}

func (a sdk) less(b sdk) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	switch {
	case a.prerelease == b.prerelease:
		return false
	case a.prerelease == "":
		return false
	case b.prerelease == "":
		return true
	}
	return prereleaseLess(a.prerelease, b.prerelease)
}

// prereleaseLess orders two prerelease labels by their dot-separated
// identifiers: numeric ones as numbers, below alphanumeric ones, which order
// as text; a label that is a prefix of the other is the lower.
func prereleaseLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		switch {
		case aErr == nil && bErr == nil:
			return an < bn
		case aErr == nil:
			return true
		case bErr == nil:
			return false
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

// rollForward reads sdk.rollForward, lower-cased, from the workspace-relative
// global.json source; "" when the source is no global.json, or states none.
func rollForward(root, source string) string {
	if filepath.Base(filepath.FromSlash(source)) != "global.json" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(source))) //nolint:gosec // the workspace's own global.json
	if err != nil {
		return ""
	}
	var doc struct {
		SDK struct {
			RollForward string `json:"rollForward"`
		} `json:"sdk"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(doc.SDK.RollForward))
}
