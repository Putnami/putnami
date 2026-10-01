// Package lockfile provides a unified lock file for the CLI, toolchains,
// extensions, templates, and agent-workflow artifacts. The lock file (putnami.lock.json) records exact
// resolved versions and integrity hashes, ensuring deterministic installs and
// reproducible runtimes across machines.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const LockFilename = "putnami.lock.json"

// Lock file format versions.
//
// The format version is LOAD-BEARING, not decorative: a reader accepts exactly
// the window [MinSupportedVersion, MaxSupportedVersion] and rejects anything
// outside it with a versioned error, and a writer emits only the vocabulary its
// target version defines. That is what lets a file answer "which fields may I
// trust?" about itself.
//
// Since the vNext core, the window's floor is v2: v1 is READ-REJECTED, not
// silently upgraded. Silently upgrading would rewrite a
// committed lock as a side effect of an unrelated command, and reading a v1
// lock as v2 would mean inventing the one thing v2 adds — the pinned
// extension's task-contract level, which this CLI now requires to be recorded.
// `putnami migrate vnext --apply` is the one seam that performs the conversion,
// and OutdatedVersionError names it.
const (
	// FormatVersionV1 is the original format: per-entry resolved version,
	// archive integrity digests, manifest hash, and source URL. No longer read.
	FormatVersionV1 = 1
	// FormatVersionV2 adds LockEntry.TaskContract — the task-contract protocol
	// version the pinned extension's manifest declares — so a pinned
	// extension's contract level is readable from the lock alone, without
	// resolving and parsing the installed manifest.
	FormatVersionV2 = 2
	// FormatVersionV3 adds the top-level Toolchains dimension and records the
	// pinned CLI's machine-output protocol version. Both are preflight metadata:
	// a runner can decide what to materialize and whether it understands the
	// CLI stream before starting a gate.
	FormatVersionV3 = 3
	// FormatVersionV4 adds the platform-independent AgentArtifacts dimension.
	// Each entry binds both the registry archive and its file manifest; ordinary
	// reads and writes preserve v2/v3 instead of silently promoting them.
	FormatVersionV4 = 4
	// DefaultWriteVersion is the version stamped on a lock that records none.
	DefaultWriteVersion = FormatVersionV4
	// MinSupportedVersion is the oldest format version this CLI reads.
	MinSupportedVersion = FormatVersionV2
	// MaxSupportedVersion is the newest format version this CLI reads.
	MaxSupportedVersion = FormatVersionV4
)

// UnsupportedVersionError reports a lock file whose format version is newer than
// the reader understands. It is a distinct type rather than a bare string so a
// caller can name the two numbers that matter — what the file claims and what
// this binary reads — instead of silently proceeding on fields it does not know.
type UnsupportedVersionError struct {
	// Found is the format version recorded in the file.
	Found int
	// Max is the newest format version the reader supports.
	Max int
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf(
		"%s is lock format version %d, but this putnami reads at most version %d: upgrade the CLI (putnami upgrade)",
		LockFilename, e.Found, e.Max)
}

// OutdatedVersionError reports a lock file older than this reader's floor. It is
// the mirror of UnsupportedVersionError — same two numbers, opposite direction —
// and it names the command that converts the file, because the fix belongs to
// the workspace rather than to the binary.
type OutdatedVersionError struct {
	// Found is the format version recorded in the file (0 when it records
	// none, which is how a v1 lock spells v1).
	Found int
	// Min is the oldest format version the reader supports.
	Min int
}

func (e *OutdatedVersionError) Error() string {
	found := e.Found
	if found == 0 {
		found = FormatVersionV1
	}
	return fmt.Sprintf(
		"%s is lock format version %d, but this putnami requires version %d: run `putnami migrate vnext --apply` to convert it",
		LockFilename, found, e.Min)
}

// LockFile represents the contents of putnami.lock.json.
// It records the exact resolved versions and integrity hashes of the
// pinned CLI binary and the installed extensions and templates.
type LockFile struct {
	// Version is the lock file format version. Zero means the file recorded none,
	// which reads as v1.
	Version int `json:"version"`
	// CLI declares which putnami engine this workspace runs. It has exactly
	// three legal shapes, and a reader must distinguish all three:
	//
	//   - nil — the workspace declares nothing. The launcher is fail-OPEN:
	//     whatever binary is running continues.
	//   - a PUBLISHED PIN — Version plus Integrities["os/arch"], the SHA-256 of
	//     the CLI executable for that platform. A launcher resolves and verifies
	//     the exact CLI from the machine-global artifact store's cli/<sha>/ tree
	//     (the same blob putnamiw publishes), keyed by that binary digest.
	//   - a SOURCE WORKSPACE — Source == SourceWorkspace and nothing else. The
	//     workspace builds its own engine, so no published version exists to
	//     pin. Version and Integrities are absent BY CONSTRUCTION, not by
	//     omission: a source workspace that recorded a version would be claiming
	//     a published artifact it does not have. The launcher is fail-CLOSED
	//     here (see internal/launch): only a binary built from this tree may run.
	//
	// Reuses LockEntry to share the per-platform integrity machinery with
	// extensions; note the CLI digest is of the binary, whereas an extension's
	// is of its download archive.
	CLI *LockEntry `json:"cli,omitempty"`
	// Toolchains maps runtime names (go, node, bun) to exact, independently
	// materializable release entries. It is lock format v3 vocabulary.
	Toolchains map[string]LockEntry `json:"toolchains,omitempty"`
	// Extensions maps extension names to their resolved entries.
	Extensions map[string]LockEntry `json:"extensions"`
	// Templates maps template names to their resolved entries.
	Templates map[string]LockEntry `json:"templates"`
	// AgentArtifacts maps legacy agent-workflow artifact names to the exact,
	// platform-independent v4 pins earlier releases installed. Nothing adds
	// one: `putnami migrate agent-content` reads and removes them, and its
	// rollback restores them.
	AgentArtifacts map[string]AgentArtifactLockEntry `json:"agentArtifacts,omitempty"`
}

// AgentArtifactLockEntry is a deliberately narrower lock shape than
// LockEntry. Agent-workflow archives are platform-independent and carry no task
// contract; every pin binds the archive and its manifest.
type AgentArtifactLockEntry struct {
	Version      string `json:"version"`
	Integrity    string `json:"integrity"`
	ManifestHash string `json:"manifestHash"`
	Source       string `json:"source,omitempty"`
}

// LockEntry is a single locked CLI, extension, or template.
type LockEntry struct {
	// Version is the resolved exact version.
	//
	// It is omitempty so the one entry that legitimately has no version — a
	// source-workspace CLI entry, see SourceWorkspace — round-trips as the exact
	// bytes a human wrote. Without it the canonical writer would add
	// `"version": ""` on the first `putnami install`, dirtying a committed lock
	// as a side effect of an unrelated command. Every other entry kind requires
	// a version, so the tag is a no-op for them.
	Version string `json:"version,omitempty"`
	// Integrity is the legacy SHA-256 hash of one archive (hex-encoded).
	// Download archives are per-os/arch, so this single digest only ever
	// matches the platform that generated it; Integrities supersedes it for
	// cross-platform verification. Readers retain it for backward compatibility;
	// canonical v2 extension writes omit it once Integrities is available.
	Integrity string `json:"integrity,omitempty"`
	// Integrities maps a platform key ("os/arch") to the SHA-256 of that
	// platform's download archive. A lock generated on one platform records
	// only that platform initially; every other platform appends its digest on
	// the first verified install that writes the lock for a requested change
	// (an install never writes for this digest alone), or on
	// `extensions update`, so one committed lock can verify on each
	// supported platform without re-generation.
	Integrities map[string]string `json:"integrities,omitempty"`
	// ManifestHash is the SHA-256 hash of the installed manifest file (hex-encoded).
	// The manifest is platform-independent, so this binds a cross-platform
	// install to the locked artifact and detects post-install tampering.
	ManifestHash string `json:"manifestHash,omitempty"`
	// Source is the download URL or path used to install.
	Source string `json:"source,omitempty"`
	// TaskContract is the task-contract protocol version the pinned artifact's
	// extension manifest declares: 2 for a manifest whose tasks carry no
	// `declares` block, 3 for one that does (protocol/extension's
	// ManifestProtocolVersion is the sole authority — this field only RECORDS
	// its answer). Zero means "not recorded", which is what every v1 lock and
	// every entry whose manifest was not installed at migration time carries.
	//
	// LOCK FORMAT v2 ONLY. Reading a v1 lock discards it and writing at v1
	// omits it, so the field can never appear in a file that does not declare
	// the version defining it. It is recorded for extensions; the CLI pin and
	// template entries have no task contract and leave it zero.
	TaskContract int `json:"taskContract,omitempty"`
	// ProtocolVersion is the machine-output protocol emitted by the pinned CLI.
	// It is recorded only on the CLI entry and is lock format v3 vocabulary.
	ProtocolVersion int `json:"protocolVersion,omitempty"`
}

// SourceWorkspace is the reserved LockEntry.Source literal that marks a
// workspace as the SOURCE of its own CLI: the engine is built from the tree in
// the working copy, not downloaded from a registry.
//
// It reuses Source rather than adding a field because Source already answers
// exactly this question — "where do these bytes come from" — and a workspace
// that builds its own engine has no published version and no per-platform
// archive digest to record. It is a sentinel VALUE, not a new format version:
// a lock carrying it stays at whatever format version it already declares.
//
// The literal is deliberately not a URL, so it can never collide with a real
// download source: LockEntry.Source for a published pin is always an absolute
// registry URL.
const SourceWorkspace = "workspace"

// IsWorkspaceSource reports whether this entry is the source-workspace sentinel
// rather than a published pin. Callers that read the CLI entry must branch on it
// instead of reading Version, which is empty by construction here.
func (e LockEntry) IsWorkspaceSource() bool {
	return strings.TrimSpace(e.Source) == SourceWorkspace
}

// PlatformKey builds the "os/arch" key used to index LockEntry.Integrities.
func PlatformKey(os, arch string) string {
	return os + "/" + arch
}

// SplitPlatformKey is the inverse of PlatformKey: it splits an "os/arch" key
// back into its components so a recorded platform can be turned into registry
// download parameters. It reports ok=false for anything that is not exactly one
// non-empty os and one non-empty arch, so a malformed key from a hand-edited
// lock can never be silently turned into a bogus query.
func SplitPlatformKey(key string) (os, arch string, ok bool) {
	o, a, found := strings.Cut(key, "/")
	if !found || o == "" || a == "" || strings.Contains(a, "/") {
		return "", "", false
	}
	return o, a, true
}

// IntegrityFor returns the recorded archive digest for the given platform, or
// "" when the lock has no per-platform digest for it.
func (e LockEntry) IntegrityFor(os, arch string) string {
	if e.Integrities == nil {
		return ""
	}
	return e.Integrities[PlatformKey(os, arch)]
}

// SetPlatformIntegrity records the archive digest for a platform, allocating
// the map on first use. Empty platform or digest values are ignored.
func (e *LockEntry) SetPlatformIntegrity(platform, digest string) {
	if platform == "" || digest == "" {
		return
	}
	if e.Integrities == nil {
		e.Integrities = make(map[string]string)
	}
	e.Integrities[platform] = digest
}

// ReadLockFile reads and parses the lock file from the workspace root.
// Returns nil, nil if the file does not exist.
//
// It accepts exactly [MinSupportedVersion, MaxSupportedVersion]: a newer file
// is an *UnsupportedVersionError, an older one an *OutdatedVersionError naming
// `putnami migrate vnext --apply`.
func ReadLockFile(workspaceRoot string) (*LockFile, error) {
	path := filepath.Join(workspaceRoot, LockFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lock file: %w", err)
	}
	return ParseLockFile(data)
}

// ParseLockFile parses lock file bytes at this CLI's supported version range.
func ParseLockFile(data []byte) (*LockFile, error) {
	return parseLockFile(data, MinSupportedVersion, MaxSupportedVersion)
}

// ReadMigratableLockFile reads a lock file for the migration path, accepting
// formats BELOW this CLI's floor. It is the single exception to the read window
// and exists because `putnami migrate vnext --apply` is what moves a workspace
// onto that floor: a migration that could not read the file it converts would
// leave the workspace with no way forward.
//
// Everything else must go through ReadLockFile. The distinction is a function
// name rather than a flag so a reviewer can see, at each call site, whether the
// caller is the migration or a consumer that requires a migrated lock.
func ReadMigratableLockFile(workspaceRoot string) (*LockFile, error) {
	path := filepath.Join(workspaceRoot, LockFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lock file: %w", err)
	}
	return parseLockFile(data, FormatVersionV1, MaxSupportedVersion)
}

// parseLockFile is the single reader, parameterized by the format-version
// window the reader accepts. The bounds are parameters rather than constants so
// the compatibility matrix can exercise a NARROWER reader (a v1-only client)
// against a newer file, and the migration a WIDER one, on exactly this code
// path.
//
// Three rules make the version field mean something:
//   - a version above maxVersion is refused, so a reader never acts on a file
//     whose vocabulary it does not know;
//   - a version below minVersion is refused too, so a reader never acts on a
//     file that is MISSING vocabulary it requires — the symmetric failure, and
//     the one a silent upgrade would hide;
//   - vocabulary a version does not define is discarded, so a hand-edited v1
//     lock carrying a v2 field cannot smuggle a contract record into a v1 read.
func parseLockFile(data []byte, minVersion, maxVersion int) (*LockFile, error) {
	var lf LockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parse lock file: %w", err)
	}
	if lf.Version > maxVersion {
		return nil, &UnsupportedVersionError{Found: lf.Version, Max: maxVersion}
	}
	// A file that records no version is v1: that absence is how v1 spells
	// itself, so it is compared like any other version rather than defaulted.
	if effectiveVersion(lf.Version) < minVersion {
		return nil, &OutdatedVersionError{Found: lf.Version, Min: minVersion}
	}

	if lf.Extensions == nil {
		lf.Extensions = make(map[string]LockEntry)
	}
	if lf.Templates == nil {
		lf.Templates = make(map[string]LockEntry)
	}
	if lf.Version >= FormatVersionV3 && lf.Toolchains == nil {
		lf.Toolchains = make(map[string]LockEntry)
	}
	if lf.Version >= FormatVersionV4 && lf.AgentArtifacts == nil {
		lf.AgentArtifacts = make(map[string]AgentArtifactLockEntry)
	}
	lf.dropFieldsAbove(lf.Version)

	return &lf, nil
}

// WriteLockFile writes the lock file to the workspace root at the format
// version it carries (DefaultWriteVersion when it carries none), so an existing
// lock keeps its own version instead of being silently up- or down-graded by an
// unrelated install.
//
// Existing v2 locks remain v2 under ordinary extension/template updates; the
// explicit install/pin metadata refresh is what promotes one to v3. New locks
// start at DefaultWriteVersion.
//
// The output is deterministic: entries are sorted by name within each section
// (encoding/json sorts map keys), and vocabulary above the written version is
// dropped.
func WriteLockFile(workspaceRoot string, lf *LockFile) error {
	prepareLockFileForWrite(lf)
	return writeLockFile(workspaceRoot, lf)
}

// WriteLockFileIfChanged atomically writes the canonical lock only when its
// bytes differ from the file on disk. It returns true only after a replacement
// was published.
func WriteLockFileIfChanged(workspaceRoot string, lf *LockFile) (bool, error) {
	data, changed, err := canonicalBytesDifferFromDisk(workspaceRoot, lf)
	if err != nil || !changed {
		return false, err
	}
	if err := writeFileAtomic(filepath.Join(workspaceRoot, LockFilename), data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// DiffersFromDisk reports whether WriteLockFileIfChanged would replace the file
// on disk with lf, without writing anything. It applies the same write-time
// defaults to lf that WriteLockFileIfChanged does.
func DiffersFromDisk(workspaceRoot string, lf *LockFile) (bool, error) {
	_, changed, err := canonicalBytesDifferFromDisk(workspaceRoot, lf)
	return changed, err
}

// canonicalBytesDifferFromDisk renders lf's canonical bytes and compares them
// with the committed file. An absent file always differs.
func canonicalBytesDifferFromDisk(workspaceRoot string, lf *LockFile) ([]byte, bool, error) {
	prepareLockFileForWrite(lf)
	data, err := MarshalLockFile(lf)
	if err != nil {
		return nil, false, err
	}
	current, err := os.ReadFile(filepath.Join(workspaceRoot, LockFilename))
	if err == nil && bytes.Equal(current, data) {
		return data, false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	return data, true, nil
}

// Clone returns a deep copy of lf: every entry map and every per-platform
// integrity map is fresh, so a caller can edit the copy without the original
// observing it. A nil lf clones to nil.
func (lf *LockFile) Clone() *LockFile {
	if lf == nil {
		return nil
	}
	out := &LockFile{
		Version:        lf.Version,
		Toolchains:     copyEntries(lf.Toolchains),
		Extensions:     copyEntries(lf.Extensions),
		Templates:      copyEntries(lf.Templates),
		AgentArtifacts: copyAgentArtifactEntries(lf.AgentArtifacts),
	}
	if lf.CLI != nil {
		cli := copyEntries(map[string]LockEntry{"": *lf.CLI})[""]
		out.CLI = &cli
	}
	return out
}

func prepareLockFileForWrite(lf *LockFile) {
	if lf.Version == 0 {
		lf.Version = DefaultWriteVersion
	}
	if lf.Extensions == nil {
		lf.Extensions = make(map[string]LockEntry)
	}
	if lf.Templates == nil {
		lf.Templates = make(map[string]LockEntry)
	}
	if lf.Version >= FormatVersionV3 && lf.Toolchains == nil {
		lf.Toolchains = make(map[string]LockEntry)
	}
	if lf.Version >= FormatVersionV4 && lf.AgentArtifacts == nil {
		lf.AgentArtifacts = make(map[string]AgentArtifactLockEntry)
	}
}

// MigrateLockFileToCurrent explicitly projects a supported v2/v3 lock onto
// the current format. Ordinary reads and writes never call this function, so
// they preserve the version recorded by the workspace. The returned value is a
// deep copy and can be updated without aliasing the source lock.
func MigrateLockFileToCurrent(lf *LockFile) (*LockFile, error) {
	if lf == nil {
		return nil, fmt.Errorf("migrate lock file: lock file is nil")
	}
	version := effectiveVersion(lf.Version)
	if version < MinSupportedVersion {
		return nil, &OutdatedVersionError{Found: lf.Version, Min: MinSupportedVersion}
	}
	if version > MaxSupportedVersion {
		return nil, &UnsupportedVersionError{Found: lf.Version, Max: MaxSupportedVersion}
	}
	out := lf.canonicalFor(lf.Version)
	out.Version = FormatVersionV4
	if out.Toolchains == nil {
		out.Toolchains = make(map[string]LockEntry)
	}
	if out.AgentArtifacts == nil {
		out.AgentArtifacts = make(map[string]AgentArtifactLockEntry)
	}
	return &out, nil
}

// WriteLockFileV2 writes the lock file at format v2, upgrading a lock the
// migration read below the floor.
//
// This is the seam `putnami migrate vnext --apply` writes through. It writes
// atomically (temp file + rename in the destination directory) so an
// interrupted migration leaves the previous lock intact rather than a truncated
// one; WriteLockFile inherits that, because every write is at v2 or later now.
func WriteLockFileV2(workspaceRoot string, lf *LockFile) error {
	lf.Version = FormatVersionV2
	if lf.Extensions == nil {
		lf.Extensions = make(map[string]LockEntry)
	}
	if lf.Templates == nil {
		lf.Templates = make(map[string]LockEntry)
	}
	return writeLockFile(workspaceRoot, lf)
}

// writeLockFile renders and writes the canonical bytes. Every write is atomic
// (temp file + rename in the destination directory): the historical in-place v1
// write went out with the v1 read, so a reader never observes a partially
// written lock regardless of which caller produced it.
func writeLockFile(workspaceRoot string, lf *LockFile) error {
	data, err := MarshalLockFile(lf)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(workspaceRoot, LockFilename), data, 0o644)
}

// MarshalLockFile renders the canonical on-disk bytes of a lock file: two-space
// indentation, one trailing newline, sections ordered by name, and only the
// vocabulary the file's own format version defines.
//
// Determinism is structural, not incidental: every collection in the file is a
// map that encoding/json emits in sorted key order, so the bytes do not depend
// on map iteration order and are safe to digest.
//
// HTML escaping is off. json.Marshal's default escapes the three bytes '&',
// '<', and '>' to the six-byte sequences \u0026, \u003c, and \u003e, a
// setting meant for JSON embedded in an HTML script tag — never this
// on-disk file's situation. Left on, a lock whose CLI or extension source URL
// joins query parameters with '&' (every download URL this CLI issues) gets
// that '&' rewritten to \u0026 on the very next `putnami install`, even when
// nothing about the install resolved anything new. The resulting diff makes
// the lock look modified, which makes the working tree look dirty, which
// appends a `-<dirtyhash>` suffix to the version stamp — the failure this
// function exists to prevent. A disabled escaper is the only setting under
// which "read, then write back unchanged" is actually a no-op for every byte
// a URL can contain.
func MarshalLockFile(lf *LockFile) ([]byte, error) {
	out := lf.canonicalFor(lf.Version)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&out); err != nil {
		return nil, fmt.Errorf("marshal lock file: %w", err)
	}
	// json.Encoder.Encode already terminates with '\n'; the buffer's bytes are
	// the final on-disk form.
	return buf.Bytes(), nil
}

// canonicalFor returns a copy of lf carrying only the canonical representation
// defined at or below version. The copy makes every projection non-destructive:
// writing a v2 lock at v1 must not erase the caller's in-memory contract
// records, and omitting a legacy extension scalar must not erase the
// backward-compatible value a reader loaded.
func (lf *LockFile) canonicalFor(version int) LockFile {
	out := LockFile{
		Version:        lf.Version,
		Toolchains:     copyEntries(lf.Toolchains),
		Extensions:     copyEntries(lf.Extensions),
		Templates:      copyEntries(lf.Templates),
		AgentArtifacts: copyAgentArtifactEntries(lf.AgentArtifacts),
	}
	if lf.CLI != nil {
		cli := *lf.CLI
		if lf.CLI.Integrities != nil {
			cli.Integrities = make(map[string]string, len(lf.CLI.Integrities))
			for platform, digest := range lf.CLI.Integrities {
				cli.Integrities[platform] = digest
			}
		}
		out.CLI = &cli
	}
	if out.Extensions == nil {
		out.Extensions = make(map[string]LockEntry)
	}
	if out.Templates == nil {
		out.Templates = make(map[string]LockEntry)
	}
	if version >= FormatVersionV4 && out.AgentArtifacts == nil {
		out.AgentArtifacts = make(map[string]AgentArtifactLockEntry)
	}
	if version >= FormatVersionV2 {
		// Extension archives vary by os/arch. Persisting the last installer's
		// scalar makes otherwise complete locks platform-dependent; the
		// per-platform map is the sole canonical v2 representation. A scalar-only
		// legacy entry keeps its sole verification datum until a verified install
		// supplies the map. Keep v1 rendering unchanged for the migration bridge,
		// and leave CLI/template entries alone because this compatibility cleanup
		// is extension-only.
		for name, entry := range out.Extensions {
			if len(entry.Integrities) == 0 {
				continue
			}
			entry.Integrity = ""
			out.Extensions[name] = entry
		}
	}
	out.dropFieldsAbove(version)
	return out
}

// dropFieldsAbove clears every field introduced after version, in place. It is
// the one place the version→vocabulary mapping is stated, so a v3 field has
// exactly one line to add here.
func (lf *LockFile) dropFieldsAbove(version int) {
	if version >= FormatVersionV4 {
		return
	}
	// v4 vocabulary: platform-independent agent-workflow artifact pins.
	lf.AgentArtifacts = nil
	if version >= FormatVersionV3 {
		return
	}
	// v3 vocabulary: the toolchain dimension and the CLI output protocol.
	lf.Toolchains = nil
	if lf.CLI != nil {
		lf.CLI.ProtocolVersion = (LockEntry{}).ProtocolVersion
	}
	if version >= FormatVersionV2 {
		return
	}
	// v2 vocabulary: LockEntry.TaskContract.
	if lf.CLI != nil {
		lf.CLI.TaskContract = 0
	}
	for _, entries := range []map[string]LockEntry{lf.Extensions, lf.Templates} {
		for name, entry := range entries {
			if entry.TaskContract == 0 {
				continue
			}
			entry.TaskContract = 0
			entries[name] = entry
		}
	}
}

// effectiveVersion resolves the version a file's `version` member MEANS: an
// absent member (0) is v1, which is the only way a v1 file identifies itself.
func effectiveVersion(recorded int) int {
	if recorded == 0 {
		return FormatVersionV1
	}
	return recorded
}

// copyEntries deeply copies an entry map so canonical projections and explicit
// migrations never alias a caller's per-platform integrity maps.
func copyEntries(entries map[string]LockEntry) map[string]LockEntry {
	if entries == nil {
		return nil
	}
	out := make(map[string]LockEntry, len(entries))
	for name, entry := range entries {
		if entry.Integrities != nil {
			integrities := make(map[string]string, len(entry.Integrities))
			for platform, digest := range entry.Integrities {
				integrities[platform] = digest
			}
			entry.Integrities = integrities
		}
		out[name] = entry
	}
	return out
}

func copyAgentArtifactEntries(entries map[string]AgentArtifactLockEntry) map[string]AgentArtifactLockEntry {
	if entries == nil {
		return nil
	}
	out := make(map[string]AgentArtifactLockEntry, len(entries))
	for name, entry := range entries {
		out[name] = entry
	}
	return out
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory followed by a rename, so a reader never observes a partially
// written lock and an interrupted write leaves the previous file intact.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup of an already-renamed or failed temp

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // write error is the one being reported
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close() //nolint:errcheck // chmod error is the one being reported
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	return nil
}

// NewLockFile creates a new empty lock file at the default write version.
func NewLockFile() *LockFile {
	return &LockFile{
		Version:        DefaultWriteVersion,
		Toolchains:     make(map[string]LockEntry),
		Extensions:     make(map[string]LockEntry),
		Templates:      make(map[string]LockEntry),
		AgentArtifacts: make(map[string]AgentArtifactLockEntry),
	}
}

// Toolchain accessors

// GetToolchain returns the locked runtime entry for name, or false.
func (lf *LockFile) GetToolchain(name string) (LockEntry, bool) {
	e, ok := lf.Toolchains[name]
	return e, ok
}

// SetToolchain adds or updates a runtime lock entry.
func (lf *LockFile) SetToolchain(name string, entry LockEntry) {
	if lf.Toolchains == nil {
		lf.Toolchains = make(map[string]LockEntry)
	}
	lf.Toolchains[name] = entry
}

// RemoveToolchain removes a runtime from the lock file.
func (lf *LockFile) RemoveToolchain(name string) {
	delete(lf.Toolchains, name)
}

// CLI accessors

// GetCLI returns the locked CLI entry and true, or a zero entry and false when
// the workspace declares no CLI at all.
//
// true does NOT mean "a published version is pinned": it means the workspace has
// an opinion. Check IsWorkspaceSource on the returned entry before reading
// Version, or a source workspace degrades into an empty version string — the
// exact silent fallback the sentinel exists to prevent.
func (lf *LockFile) GetCLI() (LockEntry, bool) {
	if lf.CLI == nil {
		return LockEntry{}, false
	}
	return *lf.CLI, true
}

// SetCLI pins the CLI to the given entry. The entry is copied so later mutation
// of the caller's value does not alias the stored pin.
func (lf *LockFile) SetCLI(entry LockEntry) {
	e := entry
	lf.CLI = &e
}

// RemoveCLI clears the CLI pin.
func (lf *LockFile) RemoveCLI() {
	lf.CLI = nil
}

// Extension accessors

// GetExtension returns the lock entry for the given extension, or false.
func (lf *LockFile) GetExtension(name string) (LockEntry, bool) {
	e, ok := lf.Extensions[name]
	return e, ok
}

// SetExtension adds or updates an extension lock entry.
func (lf *LockFile) SetExtension(name string, entry LockEntry) {
	if lf.Extensions == nil {
		lf.Extensions = make(map[string]LockEntry)
	}
	lf.Extensions[name] = entry
}

// RemoveExtension removes an extension from the lock file.
func (lf *LockFile) RemoveExtension(name string) {
	delete(lf.Extensions, name)
}

// Template accessors

// GetTemplate returns the lock entry for the given template, or false.
func (lf *LockFile) GetTemplate(name string) (LockEntry, bool) {
	e, ok := lf.Templates[name]
	return e, ok
}

// SetTemplate adds or updates a template lock entry.
func (lf *LockFile) SetTemplate(name string, entry LockEntry) {
	if lf.Templates == nil {
		lf.Templates = make(map[string]LockEntry)
	}
	lf.Templates[name] = entry
}

// RemoveTemplate removes a template from the lock file.
func (lf *LockFile) RemoveTemplate(name string) {
	delete(lf.Templates, name)
}

// GetAgentArtifact returns the lock entry for the given agent-workflow
// artifact, or false.
func (lf *LockFile) GetAgentArtifact(name string) (AgentArtifactLockEntry, bool) {
	entry, ok := lf.AgentArtifacts[name]
	return entry, ok
}

// SetAgentArtifact adds or updates a legacy agent-workflow artifact pin: the
// rollback of an agent-content migration restores one. Callers migrating an
// older lock must invoke MigrateLockFileToCurrent first; an older format's
// canonical projection deliberately omits this v4-only vocabulary.
func (lf *LockFile) SetAgentArtifact(name string, entry AgentArtifactLockEntry) {
	if lf.AgentArtifacts == nil {
		lf.AgentArtifacts = make(map[string]AgentArtifactLockEntry)
	}
	lf.AgentArtifacts[name] = entry
}

// RemoveAgentArtifact removes an agent-workflow artifact pin.
func (lf *LockFile) RemoveAgentArtifact(name string) {
	delete(lf.AgentArtifacts, name)
}

// HashFile computes the SHA-256 hash of a file and returns it hex-encoded.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file for hashing: %w", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// HashBytes computes the SHA-256 hash of raw bytes and returns it hex-encoded.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
