package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/template"
)

// artifactInstallOutcome is the kind-independent slice of an install result.
type artifactInstallOutcome struct {
	Version      string
	Integrity    string
	Integrities  map[string]string
	ManifestHash string
	Source       string
	FromCache    bool
	Changed      bool
}

// lockEntryFor builds the lock entry to persist for a freshly installed
// artifact, merging the per-platform integrity map. Digests are version-bound,
// so a prior platform map is COPIED forward only when the version is unchanged;
// a version bump starts a fresh map, because reusing a digest across
// versions would hard-fail installs on that platform with an integrity
// mismatch.
//
// A fresh map holds only the platform this process installed on, so on its own
// this drops every other platform a shared lock had recorded. Restoring
// them is carryPlatformIntegrities' job — it RE-RESOLVES each dropped platform
// at the new version rather than copying the stale digest, and is applied on
// the update/upgrade path where the version actually moves.
func lockEntryFor(prior lockfile.LockEntry, hadPrior bool, result *artifactInstallOutcome) lockfile.LockEntry {
	entry := lockfile.LockEntry{
		Version:      result.Version,
		Integrity:    result.Integrity,
		ManifestHash: result.ManifestHash,
		Source:       result.Source,
	}
	if hadPrior {
		// A cached (FromCache) install reports no version-pinned source; keep
		// the one already in the lock rather than wiping it.
		if entry.Source == "" {
			entry.Source = prior.Source
		}
		if prior.Version == result.Version {
			for platform, digest := range prior.Integrities {
				entry.SetPlatformIntegrity(platform, digest)
			}
			// The task-contract record (lock v2, `migrate vnext --apply`) is
			// version-bound exactly like the digests: the same version means the
			// same manifest bytes, so the record stays true and a reinstall must
			// not erase it — erasing would flap the `migrate vnext --check` CI
			// gate after every install.
			// A version BUMP deliberately resets it to "not recorded": the new
			// version's manifest may declare a different contract, and --check
			// prompting a re-record is the gate working, not flapping.
			entry.TaskContract = prior.TaskContract
		}
	}
	for platform, digest := range result.Integrities {
		entry.SetPlatformIntegrity(platform, digest)
	}
	return entry
}

// machineLocalRefresh reports whether entry, freshly installed, differs from the
// committed prior entry ONLY in what describes this machine's install of the
// same version: a per-platform digest the prior did not record (this host's
// own), and the source URL this host downloaded from. Every other member —
// the version, the manifest hash, the task-contract record, every digest the
// prior already recorded — is equal.
//
// Such an entry is not a change anybody requested. A hosted runner on
// linux/amd64 that installs from a lock recorded on darwin/arm64 used to write
// its own digest, and the loopback registry broker it downloaded through, into
// the committed lock; `--impacted` then read the rewritten lock as a change and
// selected every project that declares it as an input, so the hosted plan
// differed from the local plan for the same commit.
//
// A prior entry with no per-platform map is never a refresh: its legacy scalar
// digest migrates into the map on the next install, which is a change to the
// committed format and is written.
func machineLocalRefresh(prior lockfile.LockEntry, hadPrior bool, entry lockfile.LockEntry) bool {
	if !hadPrior || len(prior.Integrities) == 0 || prior.Version != entry.Version {
		return false
	}
	for platform, digest := range prior.Integrities {
		if entry.Integrities[platform] != digest {
			return false
		}
	}
	return entry.ManifestHash == prior.ManifestHash &&
		entry.TaskContract == prior.TaskContract &&
		entry.ProtocolVersion == prior.ProtocolVersion
}

// refuseHostedLockChange fails a hosted install whose requested lock differs
// from the committed one: a configured artifact the lock does not pin, a
// moved version or a format the lock must migrate. A hosted run installs only
// what the committed lock pins (ADR 0055), so the change must be made and
// committed by an install without the run credential.
func refuseHostedLockChange(wsRoot string, requested *lockfile.LockFile, ops artifactOps) error {
	if !runcredential.Hosted() || ops.materialize {
		return nil
	}
	changed, err := lockfile.DiffersFromDisk(wsRoot, requested)
	if err != nil || !changed {
		return err
	}
	return fmt.Errorf("%s: the committed %s does not pin the %s the workspace configures; a hosted run writes no lock, so run `putnami install` without %s and commit the lock",
		runcredential.Flag, lockfile.LockFilename, ops.plural, runcredential.Flag)
}

// writeRequestedLock persists an install's lock only when something a
// consumer requested changed: a new or removed entry, a moved version, a
// migrated format — requested, which holds every machine-local refresh at its
// committed value, differs from the file on disk. When it does, the whole
// installed lock is written, this host's newly verified digests included, so
// they ride along with a change that had to land anyway.
//
// When it does not, the committed file is left byte for byte as it is. The
// digests this host verified are not lost: it verified them against the
// registry's advertised integrity, and the next install that writes the lock,
// or `putnami extensions update`, records them.
func writeRequestedLock(wsRoot string, installed, requested *lockfile.LockFile) error {
	changed, err := lockfile.DiffersFromDisk(wsRoot, requested)
	if err != nil || !changed {
		return err
	}
	_, err = lockfile.WriteLockFileIfChanged(wsRoot, installed)
	return err
}

// carryPlatformIntegrities restores the per-platform digests a version bump
// drops from entry, so one committed lock keeps verifying on every platform it
// already listed instead of shrinking to whichever machine ran the upgrade.
//
// It never copies a prior digest: digests are bound to (name, version,
// platform), so each dropped platform is RE-RESOLVED at entry.Version through
// the registry. The host's own platform is skipped — its digest comes only from
// bytes this process downloaded, hashed and verified.
//
// It is fail-soft by design. A platform that cannot be resolved at the new
// version (404 for that os/arch, network failure, no advertised integrity) is
// omitted with a warning, which degrades that platform to the pre-existing
// unverified per-worktree install path; failing the upgrade instead would break
// every framework bump. quiet suppresses the human warning on the structured
// (--output=jsonl) path.
func carryPlatformIntegrities(ctx context.Context, ops artifactOps, name string, prior lockfile.LockEntry, hadPrior bool, entry *lockfile.LockEntry, quiet bool) {
	if !hadPrior || len(prior.Integrities) == 0 {
		return
	}

	var missing []string
	for platform := range prior.Integrities {
		if platform == ops.localPlatform || entry.Integrities[platform] != "" {
			continue
		}
		missing = append(missing, platform)
	}
	// No version change (lockEntryFor already copied the map) means no missing
	// platforms and therefore no extra registry round-trips at all.
	if len(missing) == 0 {
		return
	}
	// prior.Integrities is a map, so fix a deterministic order: repeated runs
	// must issue the same requests and print the same warnings in the same
	// sequence. The resulting map is order-independent either way.
	sort.Strings(missing)

	warn := func(platform string, reason error) {
		if quiet {
			return
		}
		iox.Fprintf(os.Stderr, "  ! %s: dropping %s from the lock — no integrity at %s: %v\n",
			name, platform, entry.Version, reason)
	}

	if ops.resolvePlatformIntegrity == nil {
		for _, platform := range missing {
			warn(platform, errors.New("no cross-platform resolver for this artifact kind"))
		}
		return
	}
	for _, platform := range missing {
		goos, goarch, ok := lockfile.SplitPlatformKey(platform)
		if !ok {
			warn(platform, errors.New("malformed platform key"))
			continue
		}
		digest, err := ops.resolvePlatformIntegrity(ctx, name, entry.Version, goos, goarch)
		if err != nil {
			warn(platform, err)
			continue
		}
		entry.SetPlatformIntegrity(platform, digest)
	}
}

type ArtifactUpdateOptions struct {
	ConstraintOverride string
	DryRun             bool
}

// InstallAction is one completed artifact-install operation. Install callbacks
// receive these values in stable artifact-name order after every concurrent
// worker has stopped and the lock update (when any) has been published.
type InstallAction struct {
	Kind      InstallActionKind
	Action    InstallActionVerb
	Name      string
	Version   string
	Source    string
	Status    InstallActionStatus
	FromCache bool
	Error     string
}

type InstallActionKind string

const (
	InstallKindExtension InstallActionKind = "extension"
	InstallKindTemplate  InstallActionKind = "template"
)

type InstallActionVerb string

const InstallActionInstall InstallActionVerb = "install"

type InstallActionStatus string

const (
	InstallStatusInstalled InstallActionStatus = "installed"
	InstallStatusUpdated   InstallActionStatus = "updated"
	InstallStatusRestored  InstallActionStatus = "restored"
	InstallStatusCached    InstallActionStatus = "cached"
	InstallStatusLocal     InstallActionStatus = "local"
	InstallStatusFailed    InstallActionStatus = "failed"
)

// InstallOptions controls artifact install reporting. Out is the human stream;
// nil suppresses human lines. OnAction is never called concurrently.
type InstallOptions struct {
	OutputFormat string
	Out          io.Writer
	OnAction     func(InstallAction)
	// DeferInstallHooks installs the extension artifacts and runs no onInstall
	// hook. A hosted install runs its workspace-fetch between the two, then
	// the hooks (RunExtensionInstallHooksTo). Templates have no hooks.
	DeferInstallHooks bool
}

const maxParallelArtifactInstalls = 4

// artifactOps parameterizes the install/update/remove command family over the
// artifact kind (extensions vs templates): naming, config map, lockfile
// accessors, and installer.
type artifactOps struct {
	// label is the singular kind name used in messages ("extension").
	label string
	// plural is the plural kind name used in messages ("extensions").
	plural string

	parseArg  func(arg string) (name, version string)
	configMap func(cfg *wsproto.Config) map[string]string

	lockGet    func(lf *lockfile.LockFile, name string) (lockfile.LockEntry, bool)
	lockSet    func(lf *lockfile.LockFile, name string, entry lockfile.LockEntry)
	lockRemove func(lf *lockfile.LockFile, name string)

	install func(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*artifactInstallOutcome, error)
	resolve func(ctx context.Context, name, constraint string) (*artifactInstallOutcome, error)
	remove  func(name string) error

	// localPlatform is the "os/arch" lock key this process installs for. Its
	// digest may only ever come from locally downloaded, hashed and verified
	// bytes, so the cross-platform carry-forward never re-resolves it.
	localPlatform string
	// resolvePlatformIntegrity returns the archive digest the registry
	// advertises for name@version on a platform other than localPlatform. It is
	// how a version bump keeps the foreign platforms a shared lock already
	// recorded. Nil disables the carry-forward.
	resolvePlatformIntegrity func(ctx context.Context, name, version, goos, goarch string) (string, error)

	// installLocal handles workspace-local references and reports handled=true
	// when name is one; out receives the human-readable status line and a nil out
	// suppresses it. Nil when the kind has no local form.
	installLocal func(name string, out io.Writer) (handled bool, err error)

	// workspacePins returns the exact release each name pins in the workspace
	// file, and pinLiteral the string that pins name to version. Nil when the
	// kind keeps no pin in the workspace file.
	workspacePins func(config []byte) (map[string]workspacePin, error)
	pinLiteral    func(name, version string) string

	// materialize marks the install as producing a packageable tree for another
	// platform and/or another store root (`--platform` / `--dest`)
	// rather than installing into this workspace. The committed lock is then
	// NOT written: the digests came out of it in the first place, so there is
	// nothing new to record, and writing would risk shrinking a shared lock to
	// whatever the materialization happened to touch.
	materialize bool
}

// persistsLock reports whether an install through these ops should write the
// committed lock file. It is a method rather than an inline conjunction so the
// two independent reasons NOT to write it stay stated with their rationale.
func (ops artifactOps) persistsLock(ctx context.Context) bool {
	// A materialization only READ the lock's per-platform digests; there is
	// nothing new to record, and a write could shrink a shared lock.
	if ops.materialize {
		return false
	}
	// A hosted run installs from the committed lock and writes none of it
	// (ADR 0055); refuseHostedLockChange fails the install that would need to.
	if runcredential.Hosted() {
		return false
	}
	// An implicit first-use bootstrap restores workspace state ahead of
	// a read-only gate (build/test/lint). Writing this host's freshly-resolved
	// per-platform integrity into the committed putnami.lock.json would dirty
	// the tree on every fresh worktree/CI host whose os/arch the committed lock
	// does not already list. The artifacts are installed on disk
	// regardless; only an explicit install/upgrade writes the lock — and an
	// explicit install writes it only when a requested fact changed, never for
	// this host's digest alone (writeRequestedLock).
	return !shared.IsImplicitInstall(ctx)
}

func extensionOps(wsRoot string) artifactOps {
	return extensionOpsForTarget(wsRoot, extension.ArtifactTarget{})
}

// extensionOpsForTarget is extensionOps for a `--platform`/`--dest`
// materialization. A zero target yields exactly extensionOps.
func extensionOpsForTarget(wsRoot string, target extension.ArtifactTarget) artifactOps {
	installer := extension.NewInstaller(wsRoot)
	installer.Target = target
	ops := extensionOpsFor(wsRoot, installer)
	ops.materialize = target.Materializes()
	return ops
}

func extensionOpsFor(wsRoot string, installer *extension.Installer) artifactOps {
	return artifactOps{
		label:     "extension",
		plural:    "extensions",
		parseArg:  parseExtensionArg,
		configMap: shared.BuildExtensionMap,

		workspacePins: extensionWorkspacePins,
		pinLiteral:    extensionPinLiteral,
		lockGet: func(lf *lockfile.LockFile, name string) (lockfile.LockEntry, bool) {
			return lf.GetExtension(name)
		},
		lockSet: func(lf *lockfile.LockFile, name string, entry lockfile.LockEntry) {
			lf.SetExtension(name, entry)
		},
		lockRemove: func(lf *lockfile.LockFile, name string) {
			lf.RemoveExtension(name)
		},
		install: func(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			result, err := installer.Install(ctx, name, constraint, lockEntry)
			if err != nil {
				return nil, err
			}
			return &artifactInstallOutcome{
				Version:      result.Version,
				Integrity:    result.Integrity,
				Integrities:  result.Integrities,
				ManifestHash: result.ManifestHash,
				Source:       result.Source,
				FromCache:    result.FromCache,
				Changed:      result.Changed,
			}, nil
		},
		resolve: func(ctx context.Context, name, constraint string) (*artifactInstallOutcome, error) {
			result, err := installer.Resolve(ctx, name, constraint)
			if err != nil {
				return nil, err
			}
			return &artifactInstallOutcome{
				Version:   result.Version,
				Integrity: result.Integrity,
				Source:    result.Source,
			}, nil
		},
		remove: func(name string) error {
			return installer.Remove(name, "")
		},
		localPlatform: installer.Platform(),
		resolvePlatformIntegrity: func(ctx context.Context, name, version, goos, goarch string) (string, error) {
			return installer.ResolveIntegrityForPlatform(ctx, name, version, goos, goarch)
		},
		installLocal: func(name string, out io.Writer) (bool, error) {
			localExt, err := resolveLocalExtensionRef(wsRoot, name)
			if err != nil {
				return false, err
			}
			if localExt == nil {
				return false, nil
			}
			if out != nil {
				printLocalExtension(out, name, localExt)
			}
			return true, nil
		},
	}
}

func templateOps(wsRoot string) artifactOps {
	installer := template.NewInstaller(wsRoot)
	return artifactOps{
		label:     "template",
		plural:    "templates",
		parseArg:  parseTemplateArg,
		configMap: shared.BuildTemplateMap,

		workspacePins: templateWorkspacePins,
		pinLiteral:    templatePinLiteral,
		lockGet: func(lf *lockfile.LockFile, name string) (lockfile.LockEntry, bool) {
			return lf.GetTemplate(name)
		},
		lockSet: func(lf *lockfile.LockFile, name string, entry lockfile.LockEntry) {
			lf.SetTemplate(name, entry)
		},
		lockRemove: func(lf *lockfile.LockFile, name string) {
			lf.RemoveTemplate(name)
		},
		install: func(ctx context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*artifactInstallOutcome, error) {
			result, err := installer.Install(ctx, name, constraint, lockEntry)
			if err != nil {
				return nil, err
			}
			return &artifactInstallOutcome{
				Version:      result.Version,
				Integrity:    result.Integrity,
				Integrities:  result.Integrities,
				ManifestHash: result.ManifestHash,
				Source:       result.Source,
				FromCache:    result.FromCache,
				Changed:      result.Changed,
			}, nil
		},
		resolve: func(ctx context.Context, name, constraint string) (*artifactInstallOutcome, error) {
			result, err := installer.Resolve(ctx, name, constraint)
			if err != nil {
				return nil, err
			}
			return &artifactInstallOutcome{
				Version:   result.Version,
				Integrity: result.Integrity,
				Source:    result.Source,
			}, nil
		},
		remove: func(name string) error {
			return installer.Remove(name, "")
		},
		localPlatform: installer.Platform(),
		resolvePlatformIntegrity: func(ctx context.Context, name, version, goos, goarch string) (string, error) {
			return installer.ResolveIntegrityForPlatform(ctx, name, version, goos, goarch)
		},
	}
}

// localArtifactWriter returns the human status stream for a workspace-local
// artifact on the process-stdout paths: nil when a JSONL stream owns stdout.
func localArtifactWriter(jsonlOut bool) io.Writer {
	if jsonlOut {
		return nil
	}
	return os.Stdout
}

// noteUnreadableLock explains a lock read the LISTING commands deliberately
// tolerate. Those commands keep working without a lock — they still show what
// config declares — but the INSTALLED column comes from the lock alone, so a
// read failure renders it blank, which is indistinguishable from "nothing is
// installed". Since the format floor moved to v2, a v1 lock
// makes that the DEFAULT rendering in an unmigrated workspace, so the reason is
// printed rather than inferred.
//
// It writes to stderr so a `--output=jsonl` stream on stdout stays machine-clean,
// and it never changes control flow: the caller carries on with a nil lock.
func noteUnreadableLock(err error) {
	if err == nil {
		return
	}
	iox.Fprintf(os.Stderr, "putnami: warning: %s could not be read, so installed versions are unknown: %v\n",
		lockfile.LockFilename, err)
}

// installArtifacts installs artifacts from the workspace config and lock file.
// If a specific name is given in args, only that artifact is installed/added.
// Pass --latest in args to ignore the lock file and resolve the latest versions.
// When outputFormat is "jsonl", each artifact's outcome is emitted as one JSON
// object per line instead of the human-readable progress lines.
//
// out is the human progress stream. It is a parameter because the first-use
// bootstrap installs with it pointed at stderr under --output=json|jsonl, which
// an earlier design required doing by reassigning the process-global os.Stdout.
// The JSONL stream is deliberately NOT routed through it: that one is the
// caller's own machine contract on the real stdout.
func installArtifacts(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, ops artifactOps, outputFormat string, out io.Writer) error {
	return installArtifactsWithOptions(ctx, wsRoot, cfg, args, ops, InstallOptions{
		OutputFormat: outputFormat,
		Out:          out,
	})
}

type artifactInstallWork struct {
	name       string
	constraint string
	prior      lockfile.LockEntry
	hadPrior   bool
	lockEntry  *lockfile.LockEntry
	local      bool
	localLine  string
	result     *artifactInstallOutcome
	err        error
}

// installArtifactsWithOptions restores independent artifacts concurrently, but
// deliberately keeps every shared mutation outside the workers. Workers only
// install one artifact's bytes and stable link. The caller-owned lock and every
// output stream are merged by this goroutine in sorted name order afterwards.
func installArtifactsWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, ops artifactOps, opts InstallOptions) error {
	jsonlOut := opts.OutputFormat == "jsonl"
	humanOut := opts.Out
	if jsonlOut {
		humanOut = nil
	}

	latest, filteredArgs := splitLatestInstallArg(args)
	var specificName, specificVersion string
	if len(filteredArgs) > 0 {
		specificName, specificVersion = ops.parseArg(filteredArgs[0])
	}

	lockFile, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	if lockFile == nil {
		lockFile = lockfile.NewLockFile()
	}

	artMap := ops.configMap(cfg)
	if specificName != "" {
		constraint := specificVersion
		if constraint == "" {
			constraint = "latest"
		}
		artMap[specificName] = constraint
	}
	if len(artMap) == 0 {
		if !jsonlOut && opts.Out != nil {
			iox.Fprintf(opts.Out, "  No %s configured.\n", ops.plural)
		}
		return nil
	}

	work := prepareArtifactInstallWork(lockFile, artMap, specificName, specificVersion, latest, ops, humanOut)
	runArtifactInstallWorkers(ctx, work, ops)

	persistLock := ops.persistsLock(ctx)
	requested := lockFile.Clone()
	actions, installed, cached, local, failed := mergeArtifactInstallWork(ctx, lockFile, requested, work, ops, persistLock, jsonlOut)
	if err := refuseHostedLockChange(wsRoot, requested, ops); err != nil {
		publishInstallActions(actions, work, opts, false)
		return err
	}
	if persistLock {
		if err := writeRequestedLock(wsRoot, lockFile, requested); err != nil {
			publishInstallActions(actions, work, opts, false)
			return fmt.Errorf("write lock file: %w", err)
		}
	}
	// Publishing after the lock write ensures a successful action is never
	// exposed when the operation's final commit failed.
	publishInstallActions(actions, work, opts, true)

	if !jsonlOut && opts.Out != nil {
		if local > 0 {
			iox.Fprintf(opts.Out, "\n  %d installed, %d cached, %d local, %d failed\n", installed, cached, local, failed)
		} else {
			iox.Fprintf(opts.Out, "\n  %d installed, %d cached, %d failed\n", installed, cached, failed)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d %s(s) failed to install", failed, ops.label)
	}
	return nil
}

func splitLatestInstallArg(args []string) (bool, []string) {
	var latest bool
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--latest" {
			latest = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return latest, filtered
}

func prepareArtifactInstallWork(lockFile *lockfile.LockFile, artMap map[string]string, specificName, specificVersion string, latest bool, ops artifactOps, humanOut io.Writer) []artifactInstallWork {
	names := make([]string, 0, len(artMap))
	for name := range artMap {
		if specificName == "" || name == specificName {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	work := make([]artifactInstallWork, 0, len(names))
	for _, name := range names {
		item := artifactInstallWork{name: name, constraint: artMap[name]}
		if ops.installLocal != nil {
			var localOutput bytes.Buffer
			localWriter := io.Writer(&localOutput)
			if humanOut == nil {
				localWriter = nil
			}
			handled, err := ops.installLocal(name, localWriter)
			if err != nil {
				item.err = err
				work = append(work, item)
				continue
			}
			if handled {
				item.local = true
				item.localLine = localOutput.String()
				work = append(work, item)
				continue
			}
		}

		item.prior, item.hadPrior = ops.lockGet(lockFile, name)
		explicitVersion := name == specificName && specificVersion != ""
		if !latest && !explicitVersion && item.hadPrior {
			locked := item.prior
			item.lockEntry = &locked
		}
		work = append(work, item)
	}
	return work
}

func runArtifactInstallWorkers(ctx context.Context, work []artifactInstallWork, ops artifactOps) {
	workerCount := len(work)
	if workerCount > maxParallelArtifactInstalls {
		workerCount = maxParallelArtifactInstalls
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for i := range jobs {
				item := &work[i]
				if item.local || item.err != nil {
					continue
				}
				item.result, item.err = ops.install(ctx, item.name, item.constraint, item.lockEntry)
			}
		}()
	}
	for i := range work {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
}

// mergeArtifactInstallWork records every installed entry into lockFile, and
// into requested every entry except a machine-local refresh, which requested
// keeps at its prior value. The caller writes lockFile only when requested
// alone would change the file (writeRequestedLock).
func mergeArtifactInstallWork(ctx context.Context, lockFile, requested *lockfile.LockFile, work []artifactInstallWork, ops artifactOps, persistLock, quiet bool) ([]InstallAction, int, int, int, int) {
	actions := make([]InstallAction, 0, len(work))
	var installed, cached, local, failed int
	for i := range work {
		item := &work[i]
		switch {
		case item.err != nil:
			failed++
			actions = append(actions, installActionFor(ops, item, InstallStatusFailed))
		case item.local:
			local++
			actions = append(actions, installActionFor(ops, item, InstallStatusLocal))
		default:
			entry := lockEntryFor(item.prior, item.hadPrior, item.result)
			if persistLock {
				carryPlatformIntegrities(ctx, ops, item.name, item.prior, item.hadPrior, &entry, quiet)
			}
			if machineLocalRefresh(item.prior, item.hadPrior, entry) {
				// Where this host downloaded the bytes from is not a fact about
				// the pin: keep the committed source whatever the transport.
				entry.Source = item.prior.Source
				ops.lockSet(requested, item.name, item.prior)
			} else {
				ops.lockSet(requested, item.name, entry)
			}
			ops.lockSet(lockFile, item.name, entry)
			status := InstallStatusInstalled
			if item.result.FromCache {
				cached++
				status = InstallStatusCached
				if item.result.Changed {
					status = InstallStatusRestored
				}
			} else {
				installed++
				if item.hadPrior && item.prior.Version != "" && item.prior.Version != item.result.Version {
					status = InstallStatusUpdated
				}
			}
			actions = append(actions, installActionFor(ops, item, status))
		}
	}
	return actions, installed, cached, local, failed
}

func installActionFor(ops artifactOps, item *artifactInstallWork, status InstallActionStatus) InstallAction {
	action := InstallAction{
		Kind:   InstallActionKind(ops.label),
		Action: InstallActionInstall,
		Name:   item.name,
		Status: status,
	}
	if item.result != nil {
		action.Version = item.result.Version
		action.Source = item.result.Source
		action.FromCache = item.result.FromCache
	}
	if item.err != nil {
		action.Error = item.err.Error()
	}
	return action
}

func publishInstallActions(actions []InstallAction, work []artifactInstallWork, opts InstallOptions, includeSuccess bool) {
	jsonlOut := opts.OutputFormat == "jsonl"
	for i, action := range actions {
		if !includeSuccess && action.Status != InstallStatusFailed {
			continue
		}
		if opts.OnAction != nil {
			opts.OnAction(action)
		}
		if jsonlOut {
			emitArtifactActionJSONL(artifactActionEntry{
				Kind:      string(action.Kind),
				Action:    string(action.Action),
				Name:      action.Name,
				Version:   action.Version,
				Source:    action.Source,
				FromCache: action.FromCache,
				Status:    legacyInstallStatus(action.Status),
				Error:     action.Error,
			})
			continue
		}
		if opts.Out == nil && action.Status != InstallStatusFailed {
			continue
		}
		switch action.Status {
		case InstallStatusFailed:
			iox.Fprintf(os.Stderr, "  ✗ %s: %s\n", action.Name, action.Error)
		case InstallStatusLocal:
			iox.Fprintf(opts.Out, "%s", work[i].localLine)
		case InstallStatusCached, InstallStatusRestored:
			iox.Fprintf(opts.Out, "  ✓ %s@%s (cached)\n", action.Name, action.Version)
		default:
			iox.Fprintf(opts.Out, "  ✓ %s@%s\n", action.Name, action.Version)
		}
	}
}

func legacyInstallStatus(status InstallActionStatus) string {
	switch status {
	case InstallStatusRestored:
		return "cached"
	case InstallStatusUpdated:
		return "installed"
	default:
		return string(status)
	}
}

// updateArtifacts updates one or all artifacts to the latest compatible version.
// When outputFormat is "jsonl", each artifact's outcome is emitted as one JSON
// object per line instead of the human-readable progress lines.
func updateArtifacts(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, ops artifactOps, opts ArtifactUpdateOptions, outputFormat string) error {
	jsonlOut := outputFormat == "jsonl"
	var specificName string
	if len(args) > 0 {
		specificName, _ = ops.parseArg(args[0])
	}

	pins, err := readWorkspacePins(wsRoot, ops)
	if err != nil {
		return err
	}

	if opts.DryRun {
		return previewArtifactUpdates(ctx, wsRoot, cfg, specificName, ops, opts, pins, outputFormat)
	}

	lockFile, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	if lockFile == nil {
		lockFile = lockfile.NewLockFile()
	}

	artMap := ops.configMap(cfg)
	if len(artMap) == 0 {
		if !jsonlOut {
			iox.Fprintf(os.Stdout, "  No %s configured.\n", ops.plural)
		}
		return nil
	}

	var updated int
	// moved maps each name whose workspace pin moves to its new release, and
	// replaced to the release it replaces.
	moved, replaced := map[string]string{}, map[string]string{}
	for name, constraint := range artMap {
		if specificName != "" && name != specificName {
			continue
		}
		declared := constraint
		if opts.ConstraintOverride != "" {
			constraint = opts.ConstraintOverride
		}

		if ops.installLocal != nil {
			// These two paths (`update`, its --dry-run preview) still render on the
			// process stdout; a nil writer is their "jsonl suppresses human output".
			handled, localErr := ops.installLocal(name, localArtifactWriter(jsonlOut))
			if localErr != nil {
				if jsonlOut {
					emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "failed", Error: localErr.Error()})
				} else {
					iox.Fprintf(os.Stderr, "  ✗ %s: %v\n", name, localErr)
				}
				continue
			}
			if handled {
				if jsonlOut {
					emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "local"})
				}
				continue
			}
		}

		// Force re-download by not passing the lock entry
		result, err := ops.install(ctx, name, constraint, nil)
		if err != nil {
			if jsonlOut {
				emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "failed", Error: err.Error()})
			} else {
				iox.Fprintf(os.Stderr, "  ✗ %s: %v\n", name, err)
			}
			continue
		}

		prior, hadPrior := ops.lockGet(lockFile, name)
		oldVersion := prior.Version

		entry := lockEntryFor(prior, hadPrior, result)
		// A version bump starts a fresh per-platform map, which would otherwise
		// leave the lock pinning only the platform that ran the upgrade.
		carryPlatformIntegrities(ctx, ops, name, prior, hadPrior, &entry, jsonlOut)
		ops.lockSet(lockFile, name, entry)

		replacesPin := ""
		if movesWorkspacePin(pins, name, declared, result.Version) {
			moved[name] = result.Version
			replaced[name] = declared
			replacesPin = declared
		}
		globalPin := staleGlobalPin(pins, name, declared, result.Version)
		if globalPin != "" && !jsonlOut {
			iox.Fprintf(os.Stderr, "  ! %s is pinned to %s outside %s (in the global putnami config); the lock now records %s, so update that pin by hand\n",
				name, globalPin, wsproto.WorkspaceConfigFilename, result.Version)
		}

		if oldVersion != "" && oldVersion != result.Version {
			if jsonlOut {
				emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Version: result.Version, OldVersion: oldVersion, Source: result.Source, Status: "updated", ReplacesPin: replacesPin, GlobalPin: globalPin})
			} else {
				iox.Fprintf(os.Stdout, "  ↑ %s: %s → %s\n", name, oldVersion, result.Version)
			}
			updated++
		} else if jsonlOut {
			emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Version: result.Version, OldVersion: oldVersion, Source: result.Source, FromCache: result.FromCache, Status: "up-to-date", ReplacesPin: replacesPin, GlobalPin: globalPin})
		} else {
			iox.Fprintf(os.Stdout, "  ✓ %s@%s (up to date)\n", name, result.Version)
		}
	}

	// A workspace file that cannot be rendered stops the update before the
	// lock is written.
	renderedPins, err := renderWorkspacePins(wsRoot, ops, moved)
	if err != nil {
		return err
	}
	if err := lockfile.WriteLockFile(wsRoot, lockFile); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := writeWorkspacePins(wsRoot, renderedPins, replaced, moved); err != nil {
		return err
	}

	if !jsonlOut {
		for _, name := range sortedPinNames(moved) {
			iox.Fprintf(os.Stdout, "  ↑ %s pin %s: %s → %s\n", wsproto.WorkspaceConfigFilename, name, replaced[name], moved[name])
		}
		if len(moved) > 0 {
			iox.Fprintf(os.Stdout, "\n  %d updated, %d pins moved in %s\n", updated, len(moved), wsproto.WorkspaceConfigFilename)
		} else {
			iox.Fprintf(os.Stdout, "\n  %d updated\n", updated)
		}
	}
	return nil
}

// staleGlobalPin returns the exact release declared pins outside the
// workspace file — in the global putnami config — when version moves away
// from it, and "" otherwise. Only the workspace file is rewritten, so that pin
// disagrees with the lock until someone updates it by hand.
func staleGlobalPin(pins map[string]string, name, declared, version string) string {
	if _, inWorkspace := pins[name]; inWorkspace || !movesPin(declared, version) {
		return ""
	}
	return declared
}

func previewArtifactUpdates(ctx context.Context, wsRoot string, cfg *wsproto.Config, specificName string, ops artifactOps, opts ArtifactUpdateOptions, pins map[string]string, outputFormat string) error {
	jsonlOut := outputFormat == "jsonl"
	artMap := ops.configMap(cfg)
	if len(artMap) == 0 {
		if !jsonlOut {
			iox.Fprintf(os.Stdout, "  No %s configured.\n", ops.plural)
		}
		return nil
	}

	var resolved int
	moved := map[string]string{}
	for name, constraint := range artMap {
		if specificName != "" && name != specificName {
			continue
		}
		declared := constraint
		if opts.ConstraintOverride != "" {
			constraint = opts.ConstraintOverride
		}
		if ops.installLocal != nil {
			// These two paths (`update`, its --dry-run preview) still render on the
			// process stdout; a nil writer is their "jsonl suppresses human output".
			handled, localErr := ops.installLocal(name, localArtifactWriter(jsonlOut))
			if localErr != nil {
				if jsonlOut {
					emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "failed", Error: localErr.Error()})
				} else {
					iox.Fprintf(os.Stderr, "  ✗ %s: %v\n", name, localErr)
				}
				continue
			}
			if handled {
				if jsonlOut {
					emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "local"})
				}
				continue
			}
		}
		if ops.resolve == nil {
			if jsonlOut {
				emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Version: constraint, Status: "resolved"})
			} else {
				iox.Fprintf(os.Stdout, "  Would resolve %s@%s\n", name, constraint)
			}
			resolved++
			continue
		}
		result, err := ops.resolve(ctx, name, constraint)
		if err != nil {
			if jsonlOut {
				emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Status: "failed", Error: err.Error()})
			} else {
				iox.Fprintf(os.Stderr, "  ✗ %s: %v\n", name, err)
			}
			continue
		}
		replacesPin := ""
		if movesWorkspacePin(pins, name, declared, result.Version) {
			moved[name] = result.Version
			replacesPin = declared
		}
		globalPin := staleGlobalPin(pins, name, declared, result.Version)
		if globalPin != "" && !jsonlOut {
			iox.Fprintf(os.Stderr, "  ! %s is pinned to %s outside %s (in the global putnami config); the update would record %s, so that pin would need updating by hand\n",
				name, globalPin, wsproto.WorkspaceConfigFilename, result.Version)
		}
		if jsonlOut {
			emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: name, Version: result.Version, Source: result.Source, Status: "resolved", ReplacesPin: replacesPin, GlobalPin: globalPin})
			resolved++
			continue
		}
		iox.Fprintf(os.Stdout, "  Would use %s@%s\n", name, result.Version)
		if replacesPin != "" {
			iox.Fprintf(os.Stdout, "  Would update %s pin %s: %s → %s\n", wsproto.WorkspaceConfigFilename, name, replacesPin, result.Version)
		}
		resolved++
	}
	if !jsonlOut {
		iox.Fprintf(os.Stdout, "\n  %d resolved\n", resolved)
	}
	// The preview fails where the update would.
	_, err := renderWorkspacePins(wsRoot, ops, moved)
	return err
}

// removeArtifact removes an artifact from the lock file and disk.
//
// The lock read is CHECKED, like install's and update's, and it is checked
// BEFORE anything is deleted. Removal is a two-sided edit — drop the lock entry,
// then drop the bytes — and swallowing the read error performed only the second
// side: the artifact vanished from disk while the lock kept pinning it, and the
// command printed success. Since the lock floor moved to v2,
// that is not a corner case, because every v1 lock now fails this read with an
// *OutdatedVersionError.
func removeArtifact(wsRoot string, args []string, ops artifactOps) error {
	if len(args) == 0 {
		return fmt.Errorf("%s name required", ops.label)
	}
	name := args[0]

	lockFile, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	if lockFile != nil {
		ops.lockRemove(lockFile, name)
		if err := lockfile.WriteLockFile(wsRoot, lockFile); err != nil {
			return fmt.Errorf("write lock file: %w", err)
		}
	}

	if err := ops.remove(name); err != nil {
		return fmt.Errorf("remove %s: %w", ops.label, err)
	}

	iox.Fprintf(os.Stdout, "  ✓ Removed %s\n", name)
	iox.Fprintf(os.Stdout, "  Note: update putnami.workspace.json %s field manually\n", ops.plural)
	return nil
}

// artifactListEntry is one row of a list --output=jsonl stream.
type artifactListEntry struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint,omitempty"`
	Installed  string `json:"installed,omitempty"`
	Source     string `json:"source"`
}

func printArtifactListJSONL(entries []artifactListEntry) error {
	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		iox.Fprintln(os.Stdout, string(data))
	}
	return nil
}

// artifactActionEntry is one event of an install/update --output=jsonl stream.
// Status is one of: installed, cached, updated, "up-to-date", local, resolved,
// failed. Action is "install" or "update".
type artifactActionEntry struct {
	Kind       string `json:"kind"`   // "extension" | "template"
	Action     string `json:"action"` // "install" | "update"
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	OldVersion string `json:"oldVersion,omitempty"`
	Source     string `json:"source,omitempty"`
	FromCache  bool   `json:"fromCache,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	// ReplacesPin is the exact release putnami.workspace.json pinned for Name
	// that Version replaces there (or, on a dry run, would replace).
	ReplacesPin string `json:"replacesPin,omitempty"`
	// GlobalPin is the exact release the global putnami config pins for Name
	// that Version moves away from. That file is not edited, so the pin
	// disagrees with the lock until someone updates it.
	GlobalPin string `json:"globalPin,omitempty"`
}

// emitArtifactActionJSONL writes one artifactActionEntry as a JSON line.
func emitArtifactActionJSONL(entry artifactActionEntry) {
	data, _ := json.Marshal(entry)
	iox.Fprintln(os.Stdout, string(data))
}
