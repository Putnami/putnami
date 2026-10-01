package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// `putnami migrate vnext` migrates putnami.lock.json to the current lock format.
//
// For v1 inputs it also records v2's per-extension TaskContract field from the
// already-installed manifest. It then performs the explicit, non-mutating
// v2/v3 projection onto v4, which adds empty current-format dimensions without
// inventing pins. --check is the CI gate (exit 0 clean, exit 2 when a migration
// is pending) and --apply is the idempotent, atomic conversion.
//
// It is deliberately mechanical. It records what the INSTALLED manifests
// already declare; it never resolves versions, contacts a registry, or changes
// a pin. An extension whose manifest is not installed is reported rather than
// guessed at, because a wrong contract record is worse than a missing one: the
// slice that requires contract v3 reads this field to fail closed.

// migrate vnext outcomes (also VNextReport.Outcome).
const (
	// VNextOutcomeClean means the lock is already at the target format and
	// every resolvable manifest's contract is recorded correctly.
	VNextOutcomeClean = "clean"
	// VNextOutcomeMigrate means --check found pending changes.
	VNextOutcomeMigrate = "migrate"
	// VNextOutcomeApplied means --apply wrote the pending changes.
	VNextOutcomeApplied = "applied"
)

// migrate vnext change kinds (VNextChange.Kind).
const (
	// VNextChangeLockVersion is the format version bump itself.
	VNextChangeLockVersion = "lockVersion"
	// VNextChangeTaskContract records or corrects one extension's
	// task-contract version.
	VNextChangeTaskContract = "taskContract"
)

// migrate vnext drift reasons (VNextDrift.Reason).
const (
	// VNextDriftManifestUnavailable: the locked extension is not installed, so
	// its contract level cannot be read.
	VNextDriftManifestUnavailable = "manifestUnavailable"
	// VNextDriftManifestUnreadable: the manifest exists but did not load.
	VNextDriftManifestUnreadable = "manifestUnreadable"
)

// VNextReport is the machine-readable result of `putnami migrate vnext`.
type VNextReport struct {
	// Lock is the lock file name this report describes.
	Lock string `json:"lock"`
	// LockVersion is the format version the lock carries on disk (0 when the
	// file records none, which reads as v1).
	LockVersion int `json:"lockVersion"`
	// TargetVersion is the format version the migration targets.
	TargetVersion int `json:"targetVersion"`
	// Outcome is VNextOutcomeClean, VNextOutcomeMigrate, or
	// VNextOutcomeApplied.
	Outcome string `json:"outcome"`
	// Changes are the mechanical conversions --apply performs (or performed),
	// in a deterministic order: the version bump first, then one entry per
	// extension in name order.
	Changes []VNextChange `json:"changes,omitempty"`
	// Drift lists locked extensions whose contract level could not be read, in
	// name order. These are reported, never guessed, and do not fail --check:
	// they are fixed by installing the extension, not by migrating the lock.
	Drift []VNextDrift `json:"drift,omitempty"`
}

// VNextChange is one mechanical conversion.
type VNextChange struct {
	// Kind is VNextChangeLockVersion or VNextChangeTaskContract.
	Kind string `json:"kind"`
	// Extension names the lock entry a taskContract change applies to, and is
	// empty for the lock-wide version bump.
	Extension string `json:"extension,omitempty"`
	// From is the current value (0 = not recorded).
	From int `json:"from"`
	// To is the value the migration writes.
	To int `json:"to"`
}

// VNextDrift is one locked extension whose declared contract could not be read.
type VNextDrift struct {
	// Extension is the locked extension name.
	Extension string `json:"extension"`
	// Reason is VNextDriftManifestUnavailable or VNextDriftManifestUnreadable.
	Reason string `json:"reason"`
	// Detail is the underlying load error, when there was one.
	Detail string `json:"detail,omitempty"`
}

// MigrateVNext runs the explicit lock migration to the current format in check
// mode (apply=false) or apply mode. root is the workspace root, or the current
// directory when the command runs outside one.
//
// Exit-code contract (protocols/cli taxonomy): a pending migration in check
// mode is an ErrInvalidConfig, which maps to exit 2 — the same code
// `contracts check` uses for drift, so CI can gate on it. --apply exits 0 when
// it converts and 0 again when there is nothing left to do.
func MigrateVNext(root string, apply bool, outputFormat string) error {
	// The one reader allowed below the format floor: this command IS how a
	// workspace reaches it (lockfile.ReadMigratableLockFile).
	lf, err := lockfile.ReadMigratableLockFile(root)
	if err != nil {
		// A version this CLI cannot read, or a corrupt file: both are the
		// lock's problem, not the invocation's.
		return protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
	}
	if lf == nil {
		return protocolcli.WithNext(
			protocolcli.NotFoundf("no %s in %s: nothing to migrate", lockfile.LockFilename, root),
			"putnami install")
	}

	report := planVNextMigration(root, lf)
	if !apply {
		return renderVNextCheck(outputFormat, report)
	}

	if len(report.Changes) > 0 {
		applyVNextMigration(lf, report)
		// MigrateLockFileToCurrent intentionally accepts only the ordinary read
		// window (v2+). v1 reaches that floor here, after its task-contract
		// records have been derived, and never through an ordinary reader.
		if lf.Version < lockfile.FormatVersionV2 {
			lf.Version = lockfile.FormatVersionV2
		}
		current, err := lockfile.MigrateLockFileToCurrent(lf)
		if err != nil {
			return err
		}
		if err := lockfile.WriteLockFile(root, current); err != nil {
			return err
		}
		report.Outcome = VNextOutcomeApplied
	}
	return renderVNextApply(outputFormat, report)
}

// planVNextMigration computes the conversion without touching anything. The
// same plan drives --check and --apply, so what --check reports is exactly what
// --apply writes.
func planVNextMigration(root string, lf *lockfile.LockFile) VNextReport {
	report := VNextReport{
		Lock:          lockfile.LockFilename,
		LockVersion:   lf.Version,
		TargetVersion: lockfile.MaxSupportedVersion,
		Outcome:       VNextOutcomeClean,
	}

	if lf.Version < lockfile.MaxSupportedVersion {
		report.Changes = append(report.Changes, VNextChange{
			Kind: VNextChangeLockVersion,
			From: lf.Version,
			To:   lockfile.MaxSupportedVersion,
		})
	}

	names := make([]string, 0, len(lf.Extensions))
	for name := range lf.Extensions {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := lf.Extensions[name]
		contract, drift := installedTaskContract(root, name, entry.Version)
		if drift != nil {
			// Nothing to record and nothing recorded: say so. When the lock
			// already carries a record, an uninstalled manifest is not drift —
			// there is simply nothing to re-verify against.
			if entry.TaskContract == 0 {
				report.Drift = append(report.Drift, *drift)
			}
			continue
		}
		if entry.TaskContract == contract {
			continue
		}
		report.Changes = append(report.Changes, VNextChange{
			Kind:      VNextChangeTaskContract,
			Extension: name,
			From:      entry.TaskContract,
			To:        contract,
		})
	}

	if len(report.Changes) > 0 {
		report.Outcome = VNextOutcomeMigrate
	}
	return report
}

// applyVNextMigration folds the planned contract records into lf. The version
// projection is MigrateLockFileToCurrent's job, so it is not replayed here.
func applyVNextMigration(lf *lockfile.LockFile, report VNextReport) {
	for _, change := range report.Changes {
		if change.Kind != VNextChangeTaskContract {
			continue
		}
		entry, ok := lf.GetExtension(change.Extension)
		if !ok {
			continue
		}
		entry.TaskContract = change.To
		lf.SetExtension(change.Extension, entry)
	}
}

// installedTaskContract reads the task-contract version an installed
// extension's manifest declares, or returns the drift record explaining why it
// could not. Probe order is fixed (stable symlink, versioned artifact
// directory, node_modules) so the answer does not depend on the environment
// beyond which of them exists.
func installedTaskContract(root, name, version string) (int, *VNextDrift) {
	for _, dir := range installedExtensionDirs(root, name, version) {
		path := filepath.Join(dir, extension.ManifestFilename)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		manifest, err := extension.LoadManifest(path)
		if err != nil {
			return 0, &VNextDrift{
				Extension: name,
				Reason:    VNextDriftManifestUnreadable,
				Detail:    err.Error(),
			}
		}
		return extension.ManifestProtocolVersion(manifest), nil
	}
	return 0, &VNextDrift{Extension: name, Reason: VNextDriftManifestUnavailable}
}

// installedExtensionDirs lists the directories an installed extension's
// manifest can live in, most authoritative first.
func installedExtensionDirs(root, name, version string) []string {
	dirs := []string{layout.StableDir(root, layout.Extensions, name)}
	if version != "" {
		dirs = append(dirs, layout.ArtifactDir(root, layout.Extensions, name, version))
	}
	return append(dirs, filepath.Join(root, "node_modules", filepath.FromSlash(name)))
}

// renderVNextCheck writes the check result and returns the error that carries
// the exit code. In a structured output mode a clean report is a success
// envelope and a pending migration emits nothing here — the dispatcher writes
// the failure envelope with the report attached via WithResultData, exactly as
// `contracts check` does.
func renderVNextCheck(outputFormat string, report VNextReport) error {
	err := vnextCheckError(report)
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		if err == nil {
			_, _ = protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
				protocolcli.NewResultV2("migrate vnext", report, nil))
		}
		return err
	}
	printVNextReport(os.Stdout, report)
	return err
}

// renderVNextApply writes the apply result. Applying is a success either way:
// a lock that needed nothing is as migrated as one that was just converted.
func renderVNextApply(outputFormat string, report VNextReport) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("migrate vnext", report, nil))
		return err
	}
	printVNextReport(os.Stdout, report)
	return nil
}

// vnextCheckError classifies a pending migration as an invalid-config failure
// (exit 2) carrying the report, and suggests the command that fixes it.
func vnextCheckError(report VNextReport) error {
	if report.Outcome != VNextOutcomeMigrate {
		return nil
	}
	err := protocolcli.Classify(
		fmt.Errorf("%s needs migration to lock format v%d: %d pending change(s)",
			report.Lock, report.TargetVersion, len(report.Changes)),
		protocolcli.ErrInvalidConfig)
	return shared.WithResultData(protocolcli.WithNext(err, "putnami migrate vnext --apply"), report)
}

// printVNextReport prints the human-readable summary.
func printVNextReport(w *os.File, report VNextReport) {
	iox.Fprintf(w, "\n  Lock migration (%s)\n\n", report.Lock)
	iox.Fprintf(w, "    format version: %d → %d\n", report.LockVersion, report.TargetVersion)

	switch report.Outcome {
	case VNextOutcomeClean:
		iox.Fprintln(w, "    already at the target format — nothing to do")
	case VNextOutcomeApplied:
		iox.Fprintf(w, "\n  Applied %d change(s):\n", len(report.Changes))
		printVNextChanges(w, report.Changes)
	default:
		iox.Fprintf(w, "\n  Pending %d change(s):\n", len(report.Changes))
		printVNextChanges(w, report.Changes)
	}

	if len(report.Drift) > 0 {
		iox.Fprintln(w, "\n  Not recorded:")
		for _, drift := range report.Drift {
			iox.Fprintf(w, "    ? %s: %s\n", drift.Extension, vnextDriftMessage(drift))
		}
	}

	if report.Outcome == VNextOutcomeMigrate {
		iox.Fprintln(w, "\n  Run `putnami migrate vnext --apply` to migrate.")
	}
	iox.Fprintln(w)
}

func printVNextChanges(w *os.File, changes []VNextChange) {
	for _, change := range changes {
		switch change.Kind {
		case VNextChangeLockVersion:
			iox.Fprintf(w, "    ✓ lock format v%d → v%d\n", change.From, change.To)
		default:
			iox.Fprintf(w, "    ✓ %s: task contract %s → v%d\n",
				change.Extension, vnextContractLabel(change.From), change.To)
		}
	}
}

// vnextContractLabel renders a recorded contract version, spelling the absence
// of a record rather than printing "v0".
func vnextContractLabel(version int) string {
	if version == 0 {
		return "unrecorded"
	}
	return fmt.Sprintf("v%d", version)
}

func vnextDriftMessage(drift VNextDrift) string {
	switch drift.Reason {
	case VNextDriftManifestUnreadable:
		return "manifest did not load (" + drift.Detail + ")"
	default:
		return "not installed, so its task contract cannot be recorded — run `putnami install`"
	}
}
