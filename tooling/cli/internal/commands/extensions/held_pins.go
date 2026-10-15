package extensions

import (
	"context"
	"fmt"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// heldPin is a channel answer that did not replace the locked pin.
type heldPin struct {
	// channel is the channel the answer came from, and served the build it
	// named.
	channel, served string
	// older reports a build older than the pin. It is false when the two
	// versions do not compare.
	older bool
}

// note states which channel answered and which build it serves.
func (h heldPin) note() string {
	if h.older {
		return fmt.Sprintf("channel %s serves the older build %s", h.channel, h.served)
	}
	return fmt.Sprintf("channel %s serves %s, which does not compare with the pin", h.channel, h.served)
}

// holdPin returns the hold of pinned when request, resolved again, answered
// served, and nil when served replaces pinned. Only a channel answer is held:
// latest, an empty constraint, * or a channel name. An exact version or a
// range replaces the pin with whatever it resolves. A channel never replaces
// a pin with an older build, or with a build whose version does not compare
// with the pin, the 0.0.0 a registry answer without a version falls back to
// included. A 0.0.0 pin is not a release, so any answer replaces it.
func holdPin(request, pinned, served string) *heldPin {
	if pinned == "" || pinned == "0.0.0" || pinned == served || extension.IsRangeConstraint(request) {
		return nil
	}
	channel := request
	if constraint, err := extension.ParseConstraint(request); err == nil && constraint.IsLatest {
		channel = "latest"
	}
	pin, pinErr := extension.ParseVersion(pinned)
	build, buildErr := extension.ParseVersion(served)
	if pinErr != nil || buildErr != nil || served == "0.0.0" {
		return &heldPin{channel: channel, served: served}
	}
	if build.LessThan(pin) {
		return &heldPin{channel: channel, served: served, older: true}
	}
	return nil
}

// installHoldingPin installs item. When item holds its pin and resolves
// again on a channel whose answer holdPin keeps out, it installs the pinned
// build again from the lock entry and records the answer in item.held, so
// the item reads as a lock-pinned install.
func installHoldingPin(ctx context.Context, ops artifactOps, item *artifactInstallWork) {
	item.result, item.err = ops.install(ctx, item.name, item.constraint, item.lockEntry)
	if item.err != nil || item.lockEntry != nil || !item.holdsPin {
		return
	}
	held := holdPin(item.constraint, item.prior.Version, item.result.Version)
	if held == nil {
		return
	}
	locked := item.prior
	result, err := ops.install(ctx, item.name, item.constraint, &locked)
	if err != nil {
		item.result, item.err = nil, fmt.Errorf("%s, and installing the pinned %s again failed: %w", held.note(), locked.Version, err)
		return
	}
	item.result, item.lockEntry, item.held = result, &locked, held
}

// moveMark is the mark of a status line that moves a pin from old to next:
// ↑ for a newer version, ↓ for an older one, and → when the two versions do
// not compare.
func moveMark(old, next string) string {
	a, errA := extension.ParseVersion(old)
	b, errB := extension.ParseVersion(next)
	switch {
	case errA != nil || errB != nil:
		return "→"
	case b.GreaterThan(a):
		return "↑"
	case b.LessThan(a):
		return "↓"
	}
	return "→"
}

// lockedEntry returns the entry lf pins for name; a nil lf pins nothing.
func lockedEntry(lf *lockfile.LockFile, ops artifactOps, name string) (lockfile.LockEntry, bool) {
	if lf == nil {
		return lockfile.LockEntry{}, false
	}
	return ops.lockGet(lf, name)
}
