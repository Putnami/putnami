package extension

import (
	"errors"
	"fmt"
	"sort"
)

// Reserved-command provider resolution.
//
// Core delegates a few capabilities to an out-of-process extension — today the
// remote build cache (protocol/cache.ProviderCommandName). The extension that
// serves one is found by the RESERVED COMMAND NAME the protocol owns, exactly
// as `cache clean`/`cache gc` fan out to ReservedCacheCommands: core asks "does
// this extension declare the command the protocol names?" and nothing else.
//
// An earlier design deleted what used to sit around that question. The
// old gate also knew a PRODUCT NAME ("@putnami/cloud") and an extension-VERSION
// floor, and it turned "the extension I expected is loaded but does not declare
// the command" into a bespoke upgrade error. Both were provider knowledge in a
// neutral core, and both were unsound as compatibility signals: the cloud
// extension publishes SHA-stamped prereleases whose identifiers admit no semver
// order, so a floor rejected NEWER builds as older, and the name made a
// third-party provider a second-class citizen for no reason. What actually
// establishes compatibility is the extension contract version (rejected at
// discovery) plus the provider RPC version negotiated at the initialize
// handshake — neither of which needs to know who ships the provider.
//
// The publish gate is gone outright rather than made neutral: @putnami/cloud
// retired its `publish-provider` marker command, so nothing declares one and
// advanced publishing is selected by the tasks a manifest declares.

// ErrProviderAmbiguous is the sentinel for the one resolution failure that
// remains: two loaded extensions both declaring the same reserved provider
// command. Core cannot pick between them, and picking silently would make the
// active provider a function of discovery order.
var ErrProviderAmbiguous = errors.New("provider command is ambiguous")

// ResolvedProvider identifies the extension that serves a reserved provider
// command.
type ResolvedProvider struct {
	// ExtensionName is the extension that declares the command.
	ExtensionName string
	// Version is that extension's resolved version, carried for diagnostics
	// only — it is never compared against a floor.
	Version string
	// Command is the reserved command name that selected it.
	Command string
}

// ResolveReservedProvider finds the single loaded extension declaring command,
// which must be a protocol-owned reserved command name. It returns:
//
//   - (provider, nil) — exactly one extension declares it; core may delegate.
//   - (nil, nil)      — none does. This is the ordinary "the capability is not
//     installed" answer, NOT an error: the caller decides whether to degrade
//     silently or say so once. skipped feeds SkippedProviderCause so a caller
//     that wants to explain the absence can.
//   - (nil, err)      — several do, wrapping ErrProviderAmbiguous.
//
// An empty command disables resolution and reports absence, so a caller that
// has no reserved name to look for never accidentally matches an unnamed
// command.
func ResolveReservedProvider(exts []*ExtensionDescription, command string) (*ResolvedProvider, error) {
	if command == "" {
		return nil, nil
	}

	var matches []*ExtensionDescription
	for _, ext := range exts {
		if ext != nil && declaresCommand(ext, command) {
			matches = append(matches, ext)
		}
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &ResolvedProvider{
			ExtensionName: matches[0].Name,
			Version:       matches[0].Version,
			Command:       command,
		}, nil
	default:
		names := make([]string, 0, len(matches))
		for _, ext := range matches {
			names = append(names, ext.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%w: extensions %v all declare the %q command; only one may serve it",
			ErrProviderAmbiguous, names, command)
	}
}

// SkippedProviderCause returns a one-line explanation when a provider is absent
// only because discovery could not LOAD an extension (an unparseable manifest,
// a contract newer than this CLI). Without it, an extension that is installed
// but unloadable is indistinguishable from one that was never installed, and
// the run degrades silently on a fixable fault.
//
// It names no product: whichever extensions were skipped are reported, because
// core cannot know which of them would have declared the command — the manifest
// that would have said so is the one that failed to parse.
func SkippedProviderCause(skipped []SkippedExtension) string {
	if len(skipped) == 0 {
		return ""
	}
	names := make([]string, 0, len(skipped))
	for _, skip := range skipped {
		name := skip.Name
		if name == "" {
			name = skip.Ref
		}
		if skip.Reason != nil {
			name += " (" + skip.Reason.Error() + ")"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d extension(s) could not be loaded and may have provided it: %v; %s",
		len(names), names, InstalledRemediation)
}

// declaresCommand reports whether the extension exposes a command of the given
// name.
func declaresCommand(ext *ExtensionDescription, name string) bool {
	if ext == nil || ext.Commands == nil {
		return false
	}
	_, ok := ext.Commands[name]
	return ok
}
