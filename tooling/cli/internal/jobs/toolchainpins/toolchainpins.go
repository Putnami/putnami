// Package toolchainpins is the toolchain pins phase of an install: it asks
// each extension whose manifest lists runtime.resolves for the pins of those
// lock identities, settles the answers into the toolchains of the workspace
// lock, and writes the lock when they change and the run may write.
//
// The core knows no toolchain. The extension that answers for an identity
// knows where its releases live and which digests its vendor publishes; the
// phase only asks, checks the answers against each other, and records them.
package toolchainpins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// installNext is the next step of a failure that names no remedy of its own.
const installNext = "putnami install"

// Options says how a run settles the answers.
type Options struct {
	// Implicit marks the first-use install, which restores a checkout from its
	// committed lock: a committed lock version 5 pin stays as committed, every
	// other asked identity takes the answer, nothing is removed, and a
	// workspace without a lock gets none. Otherwise the run is explicit
	// (install, init, projects create): the toolchains become exactly the
	// answered pins, and the lock is created when it is missing.
	Implicit bool
	// Hosted marks a hosted run (runcredential.Hosted), which never writes the
	// lock: the settled toolchains stay in the Result.
	Hosted bool
	// Offline asks every extension for an answer made without the network.
	// A hosted run is offline.
	Offline bool
}

// Answer is the extension that answered an identity with a pin, and the
// runtime toolchain of that extension whose lock is the identity.
type Answer struct {
	// Extension is the answering extension's name.
	Extension string
	// Alias is the key of Toolchain in the extension's runtime.toolchains, or
	// "" when Toolchain is nil.
	Alias string
	// Toolchain is the first runtime toolchain of the extension, by alias,
	// whose lock is the identity. It is nil when the extension declares none.
	Toolchain *extensionproto.RuntimeToolchain
}

// Result is the outcome of one run of the phase.
type Result struct {
	// Toolchains are the settled toolchains of the lock, keyed by identity.
	// A run that wrote no lock holds them in memory only.
	Toolchains map[string]lockfile.LockEntry
	// Answers maps each identity an extension answered with a pin to that
	// extension. An identity the committed lock pins and no extension
	// answered has none.
	Answers map[string]Answer
	// Added, Changed and Removed are the identities whose entry the settled
	// toolchains add to, change in or remove from the committed lock, sorted.
	Added, Changed, Removed []string
	// Written reports that the lock file was replaced.
	Written bool
	// Hosted echoes Options.Hosted: the lock was not written, and the
	// toolchains are the runner's to provide.
	Hosted bool
}

// pinsProvider answers one toolchain-resolve request for one extension.
// workspace.ExecProbeProvider is the production provider.
type pinsProvider interface {
	ResolveToolchains(wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error)
}

// Resolve runs the phase for the workspace at workspaceRoot.
//
// Each extension with runtime.resolves receives one toolchain-resolve request
// through its runtime, with the workspace root as working directory and the
// probe's timeout. The request carries the extension's identities, the
// committed lock entries for them, the host platform and opts.Offline.
//
// A failed, timed-out or invalid answer fails the run and names the
// extension. A problem an extension answers, and two extensions that answer
// one identity with different pins, fail the run with one error that lists
// each identity's message and remedy, classified as invalid configuration.
// Nothing is written on failure.
func Resolve(ctx context.Context, workspaceRoot string, opts Options) (Result, error) {
	return resolve(ctx, workspaceRoot, opts, func(ext *extension.ExtensionDescription) pinsProvider {
		return &workspace.ExecProbeProvider{
			Extension:         ext.Name,
			Dir:               workspaceRoot,
			Context:           ctx,
			Resolve:           runtimeResolver(ctx, workspaceRoot, ext),
			PreparedBeforeRun: !extension.StartsRepositoryCode(workspaceRoot, ext),
		}
	})
}

// runtimeResolver prepares the runtime of ext on first use and returns its
// executable, as a workspace probe's provider does. The prepare toolchains of
// a local extension resolve as a provisioning run's do
// (toolchain.ResolveRuntimeToolchainsForPreparation): the phase is what pins
// them, so a missing pin must not stop the extension that answers for it.
func runtimeResolver(ctx context.Context, root string, ext *extension.ExtensionDescription) func() (string, error) {
	return func() (string, error) {
		if ext.RuntimeExecutable != "" {
			return ext.RuntimeExecutable, nil
		}
		ws, err := workspace.Load(root)
		if err != nil {
			return "", err
		}
		exts := []*extension.ExtensionDescription{ext}
		if !extension.FromSourceBuild(root, ext) {
			if err := toolchain.ResolveRuntimeToolchainsForPreparation(ctx, root, exts, os.Environ(), true); err != nil {
				return "", err
			}
		}
		if err := scheduler.SynchronizeExtensionRuntimes(ctx, ws, exts, nil); err != nil {
			return "", err
		}
		return ext.RuntimeExecutable, nil
	}
}

// answered is one extension's pin for one identity.
type answered struct {
	extension *extension.ExtensionDescription
	pin       wsproto.LockToolchainEntry
}

// problem is one identity the run cannot pin: an extension's problem or a
// disagreement between two extensions.
type problem struct {
	identity string
	message  string
	remedy   string
}

func resolve(ctx context.Context, root string, opts Options, provider func(*extension.ExtensionDescription) pinsProvider) (Result, error) {
	exts, err := resolvingExtensions(root)
	if err != nil {
		return Result{}, protocolcli.WithNext(fmt.Errorf("toolchain pins: %w", err), installNext)
	}
	committed, err := lockfile.ReadLockFile(root)
	if err != nil {
		return Result{}, protocolcli.WithNext(fmt.Errorf("toolchain pins: %w", err), installNext)
	}
	var committedToolchains map[string]lockfile.LockEntry
	if committed != nil {
		committedToolchains = committed.Toolchains
	}

	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	pins := map[string][]answered{}
	var problems []problem
	var failures []error
	for _, ext := range exts {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		request := requestFor(ext, committedToolchains, platform, opts.Offline)
		answer, err := provider(ext).ResolveToolchains(request)
		if err != nil {
			failures = append(failures, answerFailure(ext.Name, err))
			continue
		}
		for _, identity := range sortedKeys(answer.Toolchains) {
			resolution := answer.Toolchains[identity]
			switch {
			case resolution.Problem != nil:
				problems = append(problems, problem{
					identity: identity,
					message:  fmt.Sprintf("%s: %s", ext.Name, resolution.Problem.Message),
					remedy:   resolution.Problem.Remedy,
				})
			case resolution.Pin != nil:
				pins[identity] = append(pins[identity], answered{extension: ext, pin: *resolution.Pin})
			}
		}
	}
	if len(failures) > 0 {
		return Result{}, protocolcli.WithNext(fmt.Errorf("toolchain pins: %w", errors.Join(failures...)), installNext)
	}

	answers := make(map[string]Answer, len(pins))
	answeredPins := make(map[string]lockfile.LockEntry, len(pins))
	for _, identity := range sortedKeys(pins) {
		first := pins[identity][0]
		if other, disagrees := disagreement(pins[identity]); disagrees {
			problems = append(problems, problem{
				identity: identity,
				message: fmt.Sprintf("%s and %s answer different pins (%s and %s); one identity has one pin, so the workspace keeps one of the two extensions",
					first.extension.Name, other.extension.Name, first.pin.Version, other.pin.Version),
			})
			continue
		}
		answeredPins[identity] = lockfile.ToolchainEntryFromProtocol(first.pin)
		answers[identity] = answerOf(first.extension, identity)
	}
	if len(problems) > 0 {
		return Result{}, problemsError(problems)
	}

	next := settle(committed, answeredPins, opts.Implicit)
	added, changed, removed := diff(committedToolchains, next)
	result := Result{
		Toolchains: next,
		Answers:    answers,
		Added:      added,
		Changed:    changed,
		Removed:    removed,
		Hosted:     opts.Hosted,
	}
	if len(added)+len(changed)+len(removed) == 0 || opts.Hosted {
		return result, nil
	}
	if committed == nil {
		if opts.Implicit {
			return result, nil
		}
		committed = lockfile.NewLockFile()
	}
	if opts.Implicit && len(unwritable(next)) > 0 {
		// A pin committed below lock version 5 that no extension answered
		// for records no archives, and a version 5 lock cannot hold it. The
		// implicit run removes nothing, so it writes nothing.
		return result, nil
	}
	written, err := writeToolchains(root, committed, next)
	if err != nil {
		return Result{}, protocolcli.WithNext(fmt.Errorf("toolchain pins: %w", err), installNext)
	}
	result.Written = written
	return result, nil
}

// resolvingExtensions returns the extensions of the workspace whose runtime
// resolves at least one lock identity, sorted by name. Extensions are
// discovered as a job run discovers them: from the workspace projects, the
// workspace `extensions` list and the root package declarations.
func resolvingExtensions(root string) ([]*extension.ExtensionDescription, error) {
	ws, err := workspace.Load(root)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		projectPaths[i] = project.Path
	}
	discovered, err := extension.DiscoverExtensions(root, wsproto.Load(root), projectPaths)
	if err != nil {
		return nil, fmt.Errorf("discover extensions: %w", err)
	}
	var exts []*extension.ExtensionDescription
	for _, ext := range discovered {
		if ext != nil && ext.Runtime != nil && len(ext.Runtime.Resolves) > 0 {
			exts = append(exts, ext)
		}
	}
	sort.SliceStable(exts, func(i, j int) bool { return exts[i].Name < exts[j].Name })
	return exts, nil
}

// requestFor is the one request ext receives: its identities, the committed
// entries for them as committed, the host platform and offline.
func requestFor(ext *extension.ExtensionDescription, committed map[string]lockfile.LockEntry, platform string, offline bool) wsproto.ToolchainResolveRequest {
	identities := slices.Clone(ext.Runtime.Resolves)
	sort.Strings(identities)
	identities = slices.Compact(identities)
	var locked map[string]wsproto.LockToolchainEntry
	for _, identity := range identities {
		entry, ok := committed[identity]
		if !ok {
			continue
		}
		if locked == nil {
			locked = map[string]wsproto.LockToolchainEntry{}
		}
		locked[identity] = entry.ProtocolToolchainEntry()
	}
	return wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  ext.Name,
		Identities: identities,
		Locked:     locked,
		Platform:   platform,
		Offline:    offline,
	}
}

// answerFailure names the extension whose toolchain-resolve call failed, with
// the diagnostics of an answer the contract rejected.
func answerFailure(name string, err error) error {
	var failure *wsproto.ProbeFailure
	if errors.As(err, &failure) && len(failure.Diagnostics) > 0 {
		details := make([]string, 0, len(failure.Diagnostics))
		for _, d := range diag.Errors(failure.Diagnostics) {
			details = append(details, d.Field+": "+d.Message)
		}
		return fmt.Errorf("extension %s did not answer for its toolchains: %w (%s)", name, err, strings.Join(details, "; "))
	}
	return fmt.Errorf("extension %s did not answer for its toolchains: %w", name, err)
}

// disagreement returns the first answer whose pin differs from the first one.
func disagreement(answers []answered) (answered, bool) {
	first := canonicalPin(answers[0].pin)
	for _, other := range answers[1:] {
		if !bytes.Equal(canonicalPin(other.pin), first) {
			return other, true
		}
	}
	return answered{}, false
}

// canonicalPin is the JSON form of pin, which encoding/json renders with
// sorted map keys, so two equal pins have equal bytes.
func canonicalPin(pin wsproto.LockToolchainEntry) []byte {
	data, _ := json.Marshal(pin)
	return data
}

// answerOf is the Answer of ext for identity.
func answerOf(ext *extension.ExtensionDescription, identity string) Answer {
	answer := Answer{Extension: ext.Name}
	for _, alias := range sortedKeys(ext.Runtime.Toolchains) {
		declared := ext.Runtime.Toolchains[alias]
		if declared.Lock != identity {
			continue
		}
		answer.Alias = alias
		answer.Toolchain = &declared
		break
	}
	return answer
}

// problemsError is the one error of a run whose answers hold problems: each
// identity's message and remedy, in identity order. Its next step is the
// first remedy, or `putnami install` when no problem names one.
func problemsError(problems []problem) error {
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].identity < problems[j].identity })
	lines := make([]string, 0, len(problems))
	next := ""
	for _, p := range problems {
		line := fmt.Sprintf("toolchain %q: %s", p.identity, p.message)
		if p.remedy != "" {
			line += " (remedy: " + p.remedy + ")"
			if next == "" {
				next = p.remedy
			}
		}
		lines = append(lines, line)
	}
	if next == "" {
		next = installNext
	}
	return protocolcli.WithNext(
		protocolcli.InvalidConfigf("toolchain pins: the extensions cannot pin every toolchain:\n  %s", strings.Join(lines, "\n  ")),
		next)
}

// settle returns the lock's next toolchains. An explicit run records exactly
// the answered pins. An implicit run keeps every committed entry, takes the
// answer for an identity whose committed entry is not a lock version 5 pin,
// and removes nothing.
func settle(committed *lockfile.LockFile, answered map[string]lockfile.LockEntry, implicit bool) map[string]lockfile.LockEntry {
	next := map[string]lockfile.LockEntry{}
	if !implicit || committed == nil {
		for identity, entry := range answered {
			next[identity] = entry
		}
		return next
	}
	for identity, entry := range committed.Toolchains {
		next[identity] = entry
	}
	keepsPins := committed.Version >= lockfile.FormatVersionV5
	for identity, entry := range answered {
		if pinned, ok := committed.Toolchains[identity]; ok && keepsPins && strings.TrimSpace(pinned.Version) != "" {
			continue
		}
		next[identity] = entry
	}
	return next
}

// diff returns the identities next adds to, changes in and removes from
// committed, each sorted.
func diff(committed, next map[string]lockfile.LockEntry) (added, changed, removed []string) {
	for _, identity := range sortedKeys(next) {
		before, ok := committed[identity]
		switch {
		case !ok:
			added = append(added, identity)
		case !sameEntry(before, next[identity]):
			changed = append(changed, identity)
		}
	}
	for _, identity := range sortedKeys(committed) {
		if _, ok := next[identity]; !ok {
			removed = append(removed, identity)
		}
	}
	return added, changed, removed
}

func sameEntry(a, b lockfile.LockEntry) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// unwritable returns the identities of entries that record no archives, which
// a lock version 5 toolchain entry requires.
func unwritable(toolchains map[string]lockfile.LockEntry) []string {
	var identities []string
	for _, identity := range sortedKeys(toolchains) {
		if len(toolchains[identity].Archives) == 0 {
			identities = append(identities, identity)
		}
	}
	return identities
}

// writeToolchains writes committed with next as its toolchains, at lock
// version 5. Every other entry keeps its content; vocabulary the committed
// version lacks is added empty (lockfile.MigrateLockFileToCurrent).
func writeToolchains(root string, committed *lockfile.LockFile, next map[string]lockfile.LockEntry) (bool, error) {
	withoutToolchains := committed.Clone()
	withoutToolchains.Toolchains = nil
	lf, err := lockfile.MigrateLockFileToCurrent(withoutToolchains)
	if err != nil {
		return false, err
	}
	lf.Toolchains = make(map[string]lockfile.LockEntry, len(next))
	for identity, entry := range next {
		lf.Toolchains[identity] = entry
	}
	return lockfile.WriteLockFileIfChanged(root, lf)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
