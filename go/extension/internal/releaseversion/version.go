// Package releaseversion selects the version embedded in release artifacts.
package releaseversion

import pctx "go.putnami.dev/sdk/extension/context"

// Select returns the one version a release artifact must carry everywhere: the
// full version the orchestrator computed for the project's version line.
//
// There is no "stable" spelling of it any more. A version is derived from git
// (D9): a tagged commit's Full IS the tag's version, with no suffix, so the
// caller that used to ask for the base to drop a suffix would now be asking for
// a version nobody published. fallbackBase keeps direct extension invocations
// useful when the orchestrator did not provide a Version block.
func Select(version *pctx.Version, fallbackBase string) string {
	if version != nil && version.Full != "" {
		return version.Full
	}
	if version != nil && version.Base != "" {
		return version.Base
	}
	return fallbackBase
}
