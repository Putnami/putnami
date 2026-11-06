// Package lockdiff compares the packages a lockfile pins at two commits:
// package-lock.json, pnpm-lock.yaml, yarn.lock, composer.lock, go.mod, uv.lock,
// poetry.lock, the PEX JSON lock of a Pants resolve, gradle.lockfile, the
// Gradle version catalog, Package.resolved, Podfile.lock, pubspec.lock,
// NuGet's packages.lock.json, the central package versions of
// Directory.Packages.props, Gemfile.lock and Cargo.lock.
//
// A reader turns a lockfile's content into the versions it pins per package
// name. Workspace links, path packages and the repository's own modules are
// not dependencies and are left out. Diff turns two such readings into
// added, removed, upgraded, downgraded and changed packages.
package lockdiff

import (
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/protocol/evidence"
)

// Ecosystems of the dependency changes.
const (
	EcosystemNPM       = "npm"
	EcosystemComposer  = "composer"
	EcosystemGo        = "go"
	EcosystemPyPI      = "pypi"
	EcosystemMaven     = "maven"
	EcosystemSwift     = "swift"
	EcosystemCocoaPods = "cocoapods"
	EcosystemPub       = "pub"
	EcosystemGem       = "gem"
	EcosystemCargo     = "cargo"
)

// Lockfile names the readers know.
const (
	PackageLock     = "package-lock.json"
	PNPMLock        = "pnpm-lock.yaml"
	YarnLock        = "yarn.lock"
	ComposerLock    = "composer.lock"
	GoMod           = "go.mod"
	UVLock          = "uv.lock"
	PoetryLock      = "poetry.lock"
	GradleLock      = "gradle.lockfile"
	SettingsLock    = "settings-gradle.lockfile"
	PackageResolved = "Package.resolved"
	PodfileLock     = "Podfile.lock"
	PubspecLock     = "pubspec.lock"
	GemfileLock     = "Gemfile.lock"
	CargoLock       = "Cargo.lock"
	// VersionCatalogSuffix ends the name of a Gradle version catalog, which
	// lies in a gradle directory: gradle/libs.versions.toml.
	VersionCatalogSuffix = ".versions.toml"
)

// Packages maps a package name to the versions a lockfile pins for it.
type Packages map[string][]string

func (p Packages) add(name, version string) {
	if name == "" || version == "" {
		return
	}
	if !slices.Contains(p[name], version) {
		p[name] = append(p[name], version)
	}
}

// Reader reads one lockfile's content.
type Reader func(data []byte) (Packages, error)

// Kind is what a lockfile name selects: its ecosystem and its reader.
type Kind struct {
	Ecosystem string
	Read      Reader
}

// KindOf returns the kind of the lockfile at a repository path, by its base
// name. go.mod has no generic reader: its caller supplies one that knows the
// repository's modules.
func KindOf(file string) (Kind, bool) {
	switch path.Base(file) {
	case PackageLock:
		return Kind{EcosystemNPM, ReadPackageLock}, true
	case PNPMLock:
		return Kind{EcosystemNPM, ReadPNPMLock}, true
	case YarnLock:
		return Kind{EcosystemNPM, ReadYarnLock}, true
	case ComposerLock:
		return Kind{EcosystemComposer, ReadComposerLock}, true
	case UVLock:
		return Kind{EcosystemPyPI, ReadUVLock}, true
	case PoetryLock:
		return Kind{EcosystemPyPI, ReadPoetryLock}, true
	case GradleLock, SettingsLock:
		return Kind{EcosystemMaven, ReadGradleLockfile}, true
	case PackageResolved:
		return Kind{EcosystemSwift, ReadPackageResolved}, true
	case PodfileLock:
		return Kind{EcosystemCocoaPods, ReadPodfileLock}, true
	case PubspecLock:
		return Kind{EcosystemPub, ReadPubspecLock}, true
	case NuGetLock:
		return Kind{EcosystemNuGet, ReadNuGetLock}, true
	case GemfileLock:
		return Kind{EcosystemGem, ReadGemfileLock}, true
	case CargoLock:
		return Kind{EcosystemCargo, ReadCargoLock}, true
	}
	if strings.HasSuffix(path.Base(file), VersionCatalogSuffix) && path.Base(path.Dir(file)) == "gradle" {
		return Kind{EcosystemMaven, ReadVersionCatalog}, true
	}
	return Kind{}, false
}

// Diff returns the changes from base to head, sorted by name. Either side may
// be nil, for a lockfile the change added or removed.
func Diff(base, head Packages) []Change {
	names := map[string]bool{}
	for name := range base {
		names[name] = true
	}
	for name := range head {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var out []Change
	for _, name := range sorted {
		from, to := sortedVersions(base[name]), sortedVersions(head[name])
		switch {
		case len(from) == 0 && len(to) == 0:
		case len(from) == 0:
			out = append(out, Change{Name: name, Change: evidence.DependencyAdded, To: strings.Join(to, ", ")})
		case len(to) == 0:
			out = append(out, Change{Name: name, Change: evidence.DependencyRemoved, From: strings.Join(from, ", ")})
		case !slices.Equal(from, to):
			out = append(out, Change{Name: name, Change: kind(from, to), From: strings.Join(from, ", "), To: strings.Join(to, ", ")})
		}
	}
	return out
}

// Change is one package whose pinned versions changed.
type Change struct {
	Name   string
	Change string
	From   string
	To     string
}

func kind(from, to []string) string {
	if len(from) != 1 || len(to) != 1 {
		return evidence.DependencyChanged
	}
	switch c, ok := Compare(from[0], to[0]); {
	case !ok || c == 0:
		return evidence.DependencyChanged
	case c < 0:
		return evidence.DependencyUpgraded
	default:
		return evidence.DependencyDowngraded
	}
}

func sortedVersions(versions []string) []string {
	out := slices.Clone(versions)
	sort.Strings(out)
	return out
}

// Compare orders two versions of the form [v]major.minor.patch[-pre][+build],
// with an optional fourth number, as Rails releases carry: negative when a is
// older. A missing number is zero. ok is false when either does not read so.
func Compare(a, b string) (int, bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa.numbers {
		if pa.numbers[i] != pb.numbers[i] {
			if pa.numbers[i] < pb.numbers[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0, true
	case pa.pre == "":
		return 1, true
	case pb.pre == "":
		return -1, true
	}
	return comparePre(pa.pre, pb.pre), true
}

type version struct {
	numbers [4]int
	pre     string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > len(version{}.numbers) {
		return version{}, false
	}
	var v version
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.numbers[i] = n
	}
	v.pre = pre
	return v, true
}

// comparePre orders two pre-release labels identifier by identifier, numbers
// numerically and below words.
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		na, ea := strconv.Atoi(as[i])
		nb, eb := strconv.Atoi(bs[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			if na < nb {
				return -1
			}
			return 1
		case ea == nil && eb != nil:
			return -1
		case ea != nil && eb == nil:
			return 1
		case as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	return len(as) - len(bs)
}
