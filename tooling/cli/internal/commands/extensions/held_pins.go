package extensions

import (
	"context"
	"fmt"
	"os"

	"go.putnami.dev/sdk/extension/dirlink"
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
	if pinned == served || !channelHoldsPin(request, pinned) {
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

// channelHoldsPin reports whether a fresh resolution of request can hold
// pinned (holdPin): request is a channel and pinned is a release.
func channelHoldsPin(request, pinned string) bool {
	return pinned != "" && pinned != "0.0.0" && !extension.IsRangeConstraint(request)
}

// isUnnamedAnswer reports whether version is what a resolve-only lookup
// answers when the registry does not name the build a channel serves.
func isUnnamedAnswer(version string) bool {
	return version == "" || version == "0.0.0"
}

// installHoldingPin installs item. An item that holds its pin and resolves
// again on a channel first asks the registry which build the channel serves,
// without downloading it. A held answer installs from the lock entry, so the
// older build is never fetched. A newer answer, one equal to the pin, which
// refreshes the lock entry from the registry, one that names no build, and a
// lookup that fails while ctx is live all download the channel's build and
// compare that build's own version: a held build installs the pin again, and
// the stable link is restored when the pin cannot be installed. A held item
// records the answer in item.held and reads as a lock-pinned install.
func installHoldingPin(ctx context.Context, ops artifactOps, item *artifactInstallWork) {
	if item.lockEntry != nil || !item.holdsPin || !channelHoldsPin(item.constraint, item.prior.Version) {
		item.result, item.err = ops.install(ctx, item.name, item.constraint, item.lockEntry)
		return
	}
	if ops.resolve != nil {
		answer, err := ops.resolve(ctx, item.name, item.constraint)
		switch {
		case err != nil && ctx.Err() != nil:
			item.err = err
			return
		case err == nil && !isUnnamedAnswer(answer.Version):
			if held := holdPin(item.constraint, item.prior.Version, answer.Version); held != nil {
				installPin(ctx, ops, item, held, nil)
				return
			}
		}
	}
	link := snapshotLink(ops, item.name)
	item.result, item.err = ops.install(ctx, item.name, item.constraint, nil)
	if item.err != nil {
		return
	}
	if held := holdPin(item.constraint, item.prior.Version, item.result.Version); held != nil {
		installPin(ctx, ops, item, held, link)
	}
}

// installPin installs the build item pins from its lock entry and records
// held. When that install fails, link, the stable link before an install of
// another build, is restored, and the error of a held pin ends with the
// exact-version install that refreshes its lock entry.
func installPin(ctx context.Context, ops artifactOps, item *artifactInstallWork, held *heldPin, link *linkSnapshot) {
	locked := item.prior
	result, err := ops.install(ctx, item.name, item.constraint, &locked)
	if err != nil {
		if restoreErr := link.restore(); restoreErr != nil {
			err = fmt.Errorf("%w; the stable link could not be restored: %w", err, restoreErr)
		}
		if held != nil {
			// An update resolves the channel again and holds the pin, so it
			// cannot refresh this entry; an exact version installs with no lock
			// entry and is never held.
			err = fmt.Errorf("%s, and installing the pinned %s failed: %w; `update` keeps this pin, so run `%s %s@%s` to refresh its lock entry",
				held.note(), locked.Version, err, ops.installCmd(), item.name, locked.Version)
		}
		item.result, item.err = nil, err
		return
	}
	item.result, item.lockEntry, item.held = result, &locked, held
}

// linkSnapshot is the stable link of an artifact before an install, so the
// install can be undone.
type linkSnapshot struct {
	path, target string
	linked       bool
}

// snapshotLink records the stable link of name, nil when ops names no stable
// link.
func snapshotLink(ops artifactOps, name string) *linkSnapshot {
	if ops.stableLink == nil {
		return nil
	}
	path := ops.stableLink(name)
	target, err := os.Readlink(path)
	return &linkSnapshot{path: path, target: target, linked: err == nil}
}

// restore points the stable link at its recorded target again. When there was
// no link, it removes the link. When the link cannot be pointed back, it
// removes the link, as restoreUserScopePin unlinks a build it cannot link
// again, and returns the reason.
func (s *linkSnapshot) restore() error {
	if s == nil {
		return nil
	}
	var reason error
	if s.linked {
		reason = dirlink.Replace(s.target, s.path, fmt.Sprintf("%s.tmp.%d", s.path, os.Getpid()))
		if reason == nil {
			return nil
		}
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return reason
}

// moveMark is the mark of a status line that moves a pin from old to next:
// ↑ for a newer version, ↓ for an older one, and → when the two versions do
// not compare. A 0.0.0 pin is not a release, so a move from it is →.
func moveMark(old, next string) string {
	a, errA := extension.ParseVersion(old)
	b, errB := extension.ParseVersion(next)
	switch {
	case errA != nil || errB != nil || old == "0.0.0":
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
