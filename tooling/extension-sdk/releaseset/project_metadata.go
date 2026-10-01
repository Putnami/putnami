package releaseset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	distribution "go.putnami.dev/protocol/distribution"
)

// ProjectMetadataKey is the provider-owned workspace metadata member that
// distinguishes dependencies written into a published artifact from the wider
// dependency graph used for impact, scheduling, and cache invalidation.
const ProjectMetadataKey = "releaseSet"

// ProjectMetadata is the ecosystem-owned projection of one publishable
// project's release-set inputs. A project yields ONE MEMBER PER DECLARATION,
// so a project that publishes an npm package and an OCI image declares two —
// the member key is (ecosystem, coordinate) and the project is provenance.
// Providers emit it inside their own workspace metadata block; the release-set
// planner consumes it without learning npm or Go manifest rules.
type ProjectMetadata struct {
	// Ecosystems lists every member this project contributes to a release set.
	Ecosystems []MemberDeclaration `json:"ecosystems"`

	// publishers retains which provider metadata block declared each member.
	// It is deliberately not part of the wire shape: provenance belongs to the
	// metadata envelope's key, not to the provider-owned releaseSet payload.
	publishers map[string]string
}

// PublisherFor reports the extension whose metadata block declared a member.
// The release-set coordinator uses that provenance to plan the package and
// publish jobs that can actually emit the member. The ecosystem profile owner
// may be different when an extension uses a shared profile such as OCI.
func (m ProjectMetadata) PublisherFor(ecosystem distribution.Ecosystem, coordinate string) (string, bool) {
	publisher, found := m.publishers[MemberKey(ecosystem, coordinate)]
	return publisher, found
}

// PackagePublisherFor reports the extension whose package-command step
// produces a member. A declaration may route packaging to another installed
// extension while its metadata owner remains the publisher of record. The
// optional route defaults to PublisherFor so existing Go and TypeScript
// declarations preserve their current behavior.
func (m ProjectMetadata) PackagePublisherFor(ecosystem distribution.Ecosystem, coordinate string) (string, bool) {
	key := MemberKey(ecosystem, coordinate)
	publisher, found := m.publishers[key]
	if !found {
		return "", false
	}
	for _, declaration := range m.Ecosystems {
		if MemberKey(declaration.Ecosystem, declaration.Coordinate) != key {
			continue
		}
		if declaration.PackagePublisher != "" {
			return declaration.PackagePublisher, true
		}
		return publisher, true
	}
	return "", false
}

// PackageStepFor reports the package-command step whose execution identity is
// the member's selection fingerprint. The step belongs to
// PackagePublisherFor's extension.
func (m ProjectMetadata) PackageStepFor(ecosystem distribution.Ecosystem, coordinate string) (string, bool) {
	key := MemberKey(ecosystem, coordinate)
	for _, declaration := range m.Ecosystems {
		if MemberKey(declaration.Ecosystem, declaration.Coordinate) == key {
			return declaration.PackageStep, true
		}
	}
	return "", false
}

// PublishStepFor reports the publish-command step that performs the member's
// registry side effect. The step belongs to PublisherFor's extension and lets
// the coordinator remove unselected artifact publications before execution.
func (m ProjectMetadata) PublishStepFor(ecosystem distribution.Ecosystem, coordinate string) (string, bool) {
	key := MemberKey(ecosystem, coordinate)
	for _, declaration := range m.Ecosystems {
		if MemberKey(declaration.Ecosystem, declaration.Coordinate) == key {
			return declaration.PublishStep, true
		}
	}
	return "", false
}

// MemberDeclaration is one member a project contributes: its ecosystem, its
// coordinate in that ecosystem, the exact package and publish steps that emit
// it, and the internal coordinates of the SAME ecosystem its published
// artifact depends on.
type MemberDeclaration struct {
	// Ecosystem is the id of the profile this member belongs to.
	Ecosystem distribution.Ecosystem `json:"ecosystem"`
	// Coordinate is the member's package name in that ecosystem.
	Coordinate string `json:"coordinate"`
	// PackagePublisher optionally names the extension whose package command
	// produces this member. When omitted, the metadata owner returned by
	// PublisherFor is used. Publication ownership never moves to this extension.
	PackagePublisher string `json:"packagePublisher,omitempty"`
	// PackageStep is the exact step in PackagePublisherFor's package command
	// that produces this member. It gives the coordinator a
	// provider-neutral route to the job identity used as the member's
	// selection fingerprint; an ecosystem id is not necessarily a task name
	// (or a step name: oci is commonly packaged by a step named docker).
	PackageStep string `json:"packageStep"`
	// PublishStep is the exact step in the declaring extension's ecosystem
	// publish command that writes this member to its registry. Selection is per
	// member, so the coordinator must be able to remove a sibling artifact's
	// side effect even when both artifacts belong to one project.
	PublishStep string `json:"publishStep"`
	// Dependencies are same-ecosystem coordinates written into the published
	// artifact, sorted and unique.
	Dependencies []string `json:"dependencies,omitempty"`
}

// kindByStep maps a declared package or publish step to the artifact role it
// produces. It is the ONE place the step vocabulary meets the protocol's closed
// kind vocabulary: a publisher that adds a step adds its token here, and a step
// this table does not know classifies nothing rather than guessing.
var kindByStep = map[string]distribution.MemberKind{
	"npm":                     distribution.KindLibrary,
	"go":                      distribution.KindLibrary,
	"docker":                  distribution.KindImage,
	"oci":                     distribution.KindImage,
	"image":                   distribution.KindImage,
	"archives":                distribution.KindArchive,
	"cloud-publish-archives":  distribution.KindArchive,
	"cloud-publish-config":    distribution.KindConfig,
	"cloud-config-member":     distribution.KindConfig,
	"cloud-publish-migration": distribution.KindMigration,
	"site-content":            distribution.KindDoc,
	"cloud-publish-content":   distribution.KindDoc,
}

// kindByEcosystem is the fallback for an ecosystem whose profile admits exactly
// one role whatever the step that emits it: an oci member IS an image, an npm
// or go member IS a library, an archive member IS an archive. A generic
// ecosystem such as put carries several roles, so it has no entry: only its
// step can say which one, and an unknown step leaves the kind empty.
var kindByEcosystem = map[distribution.Ecosystem]distribution.MemberKind{
	"oci":     distribution.KindImage,
	"npm":     distribution.KindLibrary,
	"go":      distribution.KindLibrary,
	"archive": distribution.KindArchive,
}

// KindFor classifies one declared member for the consumers that select members
// by role — "which member is this workload's image, which is its config, which
// are its migrations" — from the publish step that writes it, then the package
// step that produces it, then its ecosystem.
//
// It is total and never fails: a member whose steps and ecosystem are all
// unknown to this build carries NO kind, which the protocol admits, rather than
// an invented one that would make a consumer bind the wrong artifact.
func KindFor(ecosystem distribution.Ecosystem, packageStep, publishStep string) distribution.MemberKind {
	for _, step := range []string{publishStep, packageStep} {
		if kind, known := kindByStep[step]; known {
			return kind
		}
	}
	return kindByEcosystem[ecosystem]
}

// ProjectReleaseMetadata reads the union of every provider's release-set
// metadata for one project. The bool is false when no provider declared one.
// Metadata blocks may contain unrelated provider-owned fields, but the
// releaseSet member itself is closed, bounded, sorted, and unique, and two
// providers may not declare the same (ecosystem, coordinate).
func ProjectReleaseMetadata(metadata map[string]json.RawMessage) (ProjectMetadata, bool, error) {
	extensions := make([]string, 0, len(metadata))
	for extension := range metadata {
		extensions = append(extensions, extension)
	}
	sort.Strings(extensions)

	answer := ProjectMetadata{publishers: make(map[string]string)}
	claimed := make(map[string]string)
	found := false
	for _, extension := range extensions {
		raw := metadata[extension]
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var block map[string]json.RawMessage
		if err := json.Unmarshal(raw, &block); err != nil {
			return ProjectMetadata{}, false, fmt.Errorf("decode project metadata from %q: %w", extension, err)
		}
		releaseRaw, declared := block[ProjectMetadataKey]
		if !declared {
			continue
		}
		if err := refuseLegacyProjectMetadata(extension, releaseRaw); err != nil {
			return ProjectMetadata{}, false, err
		}
		var projection ProjectMetadata
		decoder := json.NewDecoder(bytes.NewReader(releaseRaw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&projection); err != nil {
			return ProjectMetadata{}, false, fmt.Errorf("decode %s project metadata from %q: %w", ProjectMetadataKey, extension, err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return ProjectMetadata{}, false, fmt.Errorf("decode %s project metadata from %q: %w", ProjectMetadataKey, extension, err)
		}
		for index, declaration := range projection.Ecosystems {
			if err := validateMemberDeclaration(extension, index, declaration); err != nil {
				return ProjectMetadata{}, false, err
			}
			key := MemberKey(declaration.Ecosystem, declaration.Coordinate)
			if previous, duplicate := claimed[key]; duplicate {
				return ProjectMetadata{}, false, fmt.Errorf("%s project metadata declares member %s twice, from %q and %q",
					ProjectMetadataKey, printableKey(key), previous, extension)
			}
			claimed[key] = extension
			answer.publishers[key] = extension
			answer.Ecosystems = append(answer.Ecosystems, MemberDeclaration{
				Ecosystem:        declaration.Ecosystem,
				Coordinate:       declaration.Coordinate,
				PackagePublisher: declaration.PackagePublisher,
				PackageStep:      declaration.PackageStep,
				PublishStep:      declaration.PublishStep,
				Dependencies:     append([]string(nil), declaration.Dependencies...),
			})
		}
		found = true
	}
	sort.Slice(answer.Ecosystems, func(i, j int) bool {
		return MemberKey(answer.Ecosystems[i].Ecosystem, answer.Ecosystems[i].Coordinate) <
			MemberKey(answer.Ecosystems[j].Ecosystem, answer.Ecosystems[j].Coordinate)
	})
	return answer, found, nil
}

// refuseLegacyProjectMetadata names the exact remedy for a workspace index
// written by an older CLI. The previous shape carried one ecosystem at the top
// level of the releaseSet block; decoding it against the new shape would report
// "unknown field", which says nothing about how to fix a stale index.
func refuseLegacyProjectMetadata(extension string, raw json.RawMessage) error {
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil
	}
	if _, legacy := block["ecosystem"]; !legacy {
		return nil
	}
	return fmt.Errorf("%s project metadata from %q declares one top-level ecosystem, the shape a release set no longer uses; run `putnami projects sync` to re-probe the workspace",
		ProjectMetadataKey, extension)
}

func validateMemberDeclaration(extension string, index int, declaration MemberDeclaration) error {
	field := fmt.Sprintf("%s project metadata from %q ecosystems[%d]", ProjectMetadataKey, extension, index)
	if !ecosystemPattern.MatchString(string(declaration.Ecosystem)) {
		return fmt.Errorf("%s has invalid ecosystem %q", field, declaration.Ecosystem)
	}
	if !validCoordinate(declaration.Coordinate) {
		return fmt.Errorf("%s has invalid coordinate %q", field, declaration.Coordinate)
	}
	if declaration.PackagePublisher != "" && !validPackagePublisher(declaration.PackagePublisher) {
		return fmt.Errorf("%s has invalid package publisher %q", field, declaration.PackagePublisher)
	}
	if !validPackageStep(declaration.PackageStep) {
		return fmt.Errorf("%s has invalid package step %q; run `putnami projects sync` to refresh release-set metadata", field, declaration.PackageStep)
	}
	if !validPackageStep(declaration.PublishStep) {
		return fmt.Errorf("%s has invalid publish step %q; run `putnami projects sync` to refresh release-set metadata", field, declaration.PublishStep)
	}
	if len(declaration.Dependencies) > distribution.MaxDependenciesPerMember {
		return fmt.Errorf("%s has %d dependencies, exceeding the limit of %d", field, len(declaration.Dependencies), distribution.MaxDependenciesPerMember)
	}
	previous := ""
	for position, coordinate := range declaration.Dependencies {
		if !validCoordinate(coordinate) {
			return fmt.Errorf("%s has invalid dependency at index %d", field, position)
		}
		if position > 0 && coordinate <= previous {
			return fmt.Errorf("%s dependencies are not sorted and unique", field)
		}
		previous = coordinate
	}
	return nil
}

// ecosystemPattern is the protocol's open ecosystem grammar. The metadata
// reader admits any identifier that matches it; whether an extension owns that
// ecosystem is a question only the CLI's profile registry can answer.
var ecosystemPattern = regexp.MustCompile(distribution.EcosystemPattern)

func validCoordinate(coordinate string) bool {
	return coordinate != "" && strings.TrimSpace(coordinate) == coordinate &&
		len(coordinate) <= distribution.MaxCoordinateBytes
}

func validPackageStep(step string) bool {
	return step != "" && strings.TrimSpace(step) == step && len(step) <= distribution.MaxCoordinateBytes
}

func validPackagePublisher(publisher string) bool {
	return strings.TrimSpace(publisher) == publisher &&
		len(publisher) <= distribution.MaxCoordinateBytes &&
		!strings.ContainsRune(publisher, '\x00')
}
