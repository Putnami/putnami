package sitecontent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing, manifest validation, payload verification,
// and merge. Tooling, the cloud producer, and the site consumer key off these —
// keep them in sync with ValidErrorCodes.
const (
	ErrorCodeParseError           = "sitecontent.parse_error"
	ErrorCodeUnknownField         = "sitecontent.unknown_field"
	ErrorCodeInvalidFormatVersion = "sitecontent.invalid_format_version"
	ErrorCodeInvalidName          = "sitecontent.invalid_name"
	ErrorCodeMissingMounts        = "sitecontent.missing_mounts"
	ErrorCodeInvalidMount         = "sitecontent.invalid_mount"
	ErrorCodeInvalidSource        = "sitecontent.invalid_source"
	ErrorCodeInvalidPath          = "sitecontent.invalid_path"
	ErrorCodeInvalidDigest        = "sitecontent.invalid_digest"
	ErrorCodeMissingFiles         = "sitecontent.missing_files"
	ErrorCodeDuplicateFile        = "sitecontent.duplicate_file"
	ErrorCodePathOutsideMount     = "sitecontent.path_outside_mount"

	// Payload-verification codes (VerifyPayload).
	ErrorCodeInvalidPayloadEntry = "sitecontent.invalid_payload_entry"
	ErrorCodeInvalidSymlink      = "sitecontent.invalid_symlink"
	ErrorCodeUnlistedFile        = "sitecontent.unlisted_file"
	ErrorCodeMissingFile         = "sitecontent.missing_file"
	ErrorCodeDigestMismatch      = "sitecontent.digest_mismatch"

	// Merge code (merge.go).
	ErrorCodeMountCollision = "sitecontent.mount_collision"
)

// ValidErrorCodes enumerates the canonical sitecontent error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:           true,
	ErrorCodeUnknownField:         true,
	ErrorCodeInvalidFormatVersion: true,
	ErrorCodeInvalidName:          true,
	ErrorCodeMissingMounts:        true,
	ErrorCodeInvalidMount:         true,
	ErrorCodeInvalidSource:        true,
	ErrorCodeInvalidPath:          true,
	ErrorCodeInvalidDigest:        true,
	ErrorCodeMissingFiles:         true,
	ErrorCodeDuplicateFile:        true,
	ErrorCodePathOutsideMount:     true,
	ErrorCodeInvalidPayloadEntry:  true,
	ErrorCodeInvalidSymlink:       true,
	ErrorCodeUnlistedFile:         true,
	ErrorCodeMissingFile:          true,
	ErrorCodeDigestMismatch:       true,
	ErrorCodeMountCollision:       true,
}

// bundleNamePattern matches a canonical bundle name: lowercase letters, digits,
// '-', '_', '.'; 1–64 chars. It reuses the flat-identifier shape of the other
// protocol/* modules (a bundle name is a single token, so '/' is not allowed).
var bundleNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// mountSegmentPattern matches one URL-path segment of a mount prefix: a
// lowercase URL-safe token. It rejects "", ".", and ".." (a leading '.' is not
// in the class), so a normalized prefix can carry no traversal or empty segment.
var mountSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// formatVersionPattern matches a "major" or "major.minor" version string.
var formatVersionPattern = regexp.MustCompile(`^(\d+)(?:\.(\d+))?$`)

// FormatVersionMajor parses the major component of a "major[.minor]" version
// string. ok is false for a malformed value.
func FormatVersionMajor(v string) (major int, ok bool) {
	m := formatVersionPattern.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// CompatibleFormatVersion reports whether a consumer built against this package
// (FormatMajor) may read a bundle stamped formatVersion v. It is the runtime
// authority for the evolution rule: a bundle whose major differs from FormatMajor
// is incompatible, so a consumer seeing a newer major MUST ignore the bundle and
// fall back to its baked content. A malformed version is never compatible.
func CompatibleFormatVersion(v string) bool {
	major, ok := FormatVersionMajor(v)
	return ok && major == FormatMajor
}

// manifestFormatVersion decodes only the version envelope needed by an archive
// consumer before it attempts to decode the versioned manifest shape. Unknown
// fields are deliberately ignored here: a newer major may replace the rest of
// the manifest. Invalid JSON, a non-object document, trailing JSON, a missing
// version, and a non-string version all fail closed (ok == false).
func manifestFormatVersion(data []byte) (version string, ok bool) {
	var envelope struct {
		FormatVersion *string `json:"formatVersion"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&envelope); err != nil || envelope.FormatVersion == nil {
		return "", false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return "", false
	}
	return *envelope.FormatVersion, true
}

// ParseManifest strict-decodes bundle.json (unknown fields rejected). It returns
// a non-nil manifest only when decoding produced no errors.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"manifest must contain exactly one JSON object")}
	}
	return &m, nil
}

// decodeError maps a json decode failure to the matching diagnostic code,
// distinguishing an unknown-field rejection from a generic parse error.
func decodeError(err error) diag.Diagnostic {
	msg := err.Error()
	if field, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(field, `"`), "%s", msg)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", msg)
}

// ValidateManifest checks the structural invariants of a parsed bundle manifest:
// a compatible format-version major, a canonical name, at least one valid and
// non-overlapping mount, a complete source, and per-file a safe relative path
// (under a declared mount) and a well-formed digest. It does NOT touch the
// payload — see VerifyPayload for manifest↔payload consistency.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	var diags []diag.Diagnostic

	if !CompatibleFormatVersion(m.FormatVersion) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidFormatVersion, "formatVersion",
			"formatVersion %q is not readable by this parser (want major %d)", m.FormatVersion, FormatMajor))
	}
	diags = append(diags, validateName(m.Name)...)
	diags = append(diags, validateMounts(m.Mounts)...)
	diags = append(diags, validateSource(m.Source)...)
	diags = append(diags, validateFiles(m)...)
	return diags
}

func validateName(name string) []diag.Diagnostic {
	if name == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, "name", "name is required")}
	}
	if !bundleNamePattern.MatchString(name) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, "name",
			"name %q does not match canonical pattern %q", name, bundleNamePattern.String())}
	}
	return nil
}

// validateMounts checks each mount prefix is well-formed and that no two mounts
// within the bundle overlap. Overlap is a hard error (see PrefixesConflict).
func validateMounts(mounts []Mount) []diag.Diagnostic {
	if len(mounts) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingMounts, "mounts",
			"a bundle must declare at least one mount")}
	}
	var diags []diag.Diagnostic
	for i, mt := range mounts {
		if !ValidMountPrefix(mt.URLPrefix) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMount, fmt.Sprintf("mounts[%d].urlPrefix", i),
				"urlPrefix %q must be an absolute, normalized, lowercase site path with no '..' segment, no trailing slash, and not the root '/'", mt.URLPrefix))
		}
	}
	// Intra-bundle mount overlap.
	for i := 0; i < len(mounts); i++ {
		for j := i + 1; j < len(mounts); j++ {
			if PrefixesConflict(mounts[i].URLPrefix, mounts[j].URLPrefix) {
				diags = append(diags, diag.Errorf(ErrorCodeMountCollision, fmt.Sprintf("mounts[%d].urlPrefix", j),
					"mount %q overlaps mount %q; a bundle's mounts may not overlap", mounts[j].URLPrefix, mounts[i].URLPrefix))
			}
		}
	}
	return diags
}

func validateSource(s Source) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(s.Repo) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSource, "source.repo",
			"source.repo is required"))
	}
	if s.Commit == "" || strings.IndexFunc(s.Commit, isSpace) >= 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSource, "source.commit",
			"source.commit is required and must not contain whitespace"))
	}
	return diags
}

// validateFiles checks every listed file has a safe relative path that falls
// under a declared mount, and a well-formed digest. A bundle must list at least
// one file.
func validateFiles(m *Manifest) []diag.Diagnostic {
	if len(m.Files) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingFiles, "files",
			"a bundle must list at least one file")}
	}
	var diags []diag.Diagnostic
	seen := map[string]int{}
	for i, f := range m.Files {
		field := fmt.Sprintf("files[%d]", i)
		if !ValidRelPath(f.Path) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".path",
				"path %q must be a forward-slash relative path with no leading '/', '.' or '..' segment, or backslash", f.Path))
		} else if _, ok := MountForPath(m.Mounts, f.Path); !ok {
			diags = append(diags, diag.Errorf(ErrorCodePathOutsideMount, field+".path",
				"path %q (served at %q) falls under no declared mount", f.Path, URLPath(f.Path)))
		}
		if !ValidDigest(f.Digest) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, field+".digest",
				"digest %q must be 64 lowercase hex chars (sha256 bare-hex)", f.Digest))
		}
		if first, ok := seen[f.Path]; ok {
			diags = append(diags, duplicateManifestPath(f.Path, first, i))
		} else {
			seen[f.Path] = i
		}
	}
	return diags
}

// ParseAndValidateManifest strict-parses then validates a bundle manifest.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifest(m)...)
}

const digestHexLen = 64

// ValidDigest reports whether s is a sha256 bare-hex digest: exactly 64
// lowercase hex chars, with no "sha256:" prefix.
func ValidDigest(s string) bool {
	if len(s) != digestHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidRelPath reports whether p is a safe payload-relative path: non-empty,
// forward-slash separated, with no leading '/', no backslash, no NUL, and no
// empty, '.' or '..' segment. This rejects absolute paths and "../" traversal.
func ValidRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ValidMountPrefix reports whether p is a well-formed mount URL prefix: it
// starts with "/", is not the root "/", has no trailing slash, and every path
// segment is a lowercase URL-safe token (which excludes "", "." and "..").
func ValidMountPrefix(p string) bool {
	if !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if !mountSegmentPattern.MatchString(seg) {
			return false
		}
	}
	return true
}

// isSpace reports whether r is an ASCII whitespace rune. It matches the bytes a
// commit ref must never contain when carried in JSON.
func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}
