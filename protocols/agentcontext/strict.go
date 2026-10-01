package agentcontext

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic error codes for strict parsing, structural/semantic validation,
// and the publish-safety gate. Tooling keys off these — keep them in sync with
// ValidDiagnosticCodes below.
const (
	// ErrorCodeParseError marks malformed JSON or a decode failure.
	ErrorCodeParseError = "agentcontext.parse_error"
	// ErrorCodeUnknownField marks an unknown field rejected by strict parsing.
	ErrorCodeUnknownField = "agentcontext.unknown_field"
	// ErrorCodeInvalidProtocolVersion marks an unsupported protocolVersion.
	ErrorCodeInvalidProtocolVersion = "agentcontext.invalid_protocol_version"
	// ErrorCodeInvalidIdentity marks a missing required identity field.
	ErrorCodeInvalidIdentity = "agentcontext.invalid_identity"
	// ErrorCodeDuplicateID marks a repeated id in the dependency/dependent graph.
	ErrorCodeDuplicateID = "agentcontext.duplicate_id"
	// ErrorCodeInvalidRootKind marks a composition-root kind outside the enum.
	ErrorCodeInvalidRootKind = "agentcontext.invalid_root_kind"
	// ErrorCodeInvalidSourceReason marks a representative-source reason outside the enum.
	ErrorCodeInvalidSourceReason = "agentcontext.invalid_source_reason"
	// ErrorCodeInvalidDocRelationship marks a doc relationship outside the enum.
	ErrorCodeInvalidDocRelationship = "agentcontext.invalid_doc_relationship"
	// ErrorCodeInvalidTestPolicy marks a test policy outside the enum.
	ErrorCodeInvalidTestPolicy = "agentcontext.invalid_test_policy"
	// ErrorCodeInvalidAbsenceReason marks an absence reason outside the enum.
	ErrorCodeInvalidAbsenceReason = "agentcontext.invalid_absence_reason"
	// ErrorCodeMissingAbsenceReason marks an empty tests section with no absence reason.
	ErrorCodeMissingAbsenceReason = "agentcontext.missing_absence_reason"
	// ErrorCodeInvalidPack marks a tests pack entry missing its required id.
	ErrorCodeInvalidPack = "agentcontext.invalid_pack"
	// ErrorCodeInvalidTokenMethod marks a token-estimate method outside the enum.
	ErrorCodeInvalidTokenMethod = "agentcontext.invalid_token_method"
	// ErrorCodeInvalidAggregationMethod marks an aggregation method outside the enum.
	ErrorCodeInvalidAggregationMethod = "agentcontext.invalid_aggregation_method"
	// ErrorCodeInvalidDigest marks a digest not in "sha256:<64hex>" form.
	ErrorCodeInvalidDigest = "agentcontext.invalid_digest"
	// ErrorCodeInvalidRange marks a line range with a non-positive or descending bound.
	ErrorCodeInvalidRange = "agentcontext.invalid_range"
	// ErrorCodeInvalidTokenEstimate marks a negative token estimate.
	ErrorCodeInvalidTokenEstimate = "agentcontext.invalid_token_estimate"
	// ErrorCodeInvalidPath marks an empty, absolute, or "..".escaping path.
	ErrorCodeInvalidPath = "agentcontext.invalid_path"
	// ErrorCodeDuplicateRef marks the same reference path repeated within a section.
	ErrorCodeDuplicateRef = "agentcontext.duplicate_ref"
	// ErrorCodeMissingProvenance marks missing required provenance fields.
	ErrorCodeMissingProvenance = "agentcontext.missing_provenance"
	// ErrorCodeUnredactedSensitive marks a reference to a caller-flagged sensitive
	// path that does not carry the sensitivity flag (publish-safety gate).
	ErrorCodeUnredactedSensitive = "agentcontext.unredacted_sensitive"
	// ErrorCodeEmbeddedContent marks a suspiciously oversized string that looks
	// like embedded file content (publish-safety gate).
	ErrorCodeEmbeddedContent = "agentcontext.embedded_content"
)

// ValidDiagnosticCodes enumerates the canonical agent-context parse/validate and
// publish-safety error taxonomy.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError:               true,
	ErrorCodeUnknownField:             true,
	ErrorCodeInvalidProtocolVersion:   true,
	ErrorCodeInvalidIdentity:          true,
	ErrorCodeDuplicateID:              true,
	ErrorCodeInvalidRootKind:          true,
	ErrorCodeInvalidSourceReason:      true,
	ErrorCodeInvalidDocRelationship:   true,
	ErrorCodeInvalidTestPolicy:        true,
	ErrorCodeInvalidAbsenceReason:     true,
	ErrorCodeMissingAbsenceReason:     true,
	ErrorCodeInvalidPack:              true,
	ErrorCodeInvalidTokenMethod:       true,
	ErrorCodeInvalidAggregationMethod: true,
	ErrorCodeInvalidDigest:            true,
	ErrorCodeInvalidRange:             true,
	ErrorCodeInvalidTokenEstimate:     true,
	ErrorCodeInvalidPath:              true,
	ErrorCodeDuplicateRef:             true,
	ErrorCodeMissingProvenance:        true,
	ErrorCodeUnredactedSensitive:      true,
	ErrorCodeEmbeddedContent:          true,
}

// digestPattern matches the canonical "sha256:<64 lowercase hex>" content address.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// defaultMaxStringBytes is the publish-safety threshold above which a string
// field is treated as suspected embedded content. Every legitimate wire string
// (a path, a digest, a name, a short tag) is far below it, so it flags content
// smuggling without false positives.
const defaultMaxStringBytes = 8192

// ParseDocument decodes an agent-context Document from JSON in strict mode
// (unknown fields rejected). It returns a non-nil document only when decoding
// produced no errors.
func ParseDocument(data []byte) (*Document, []diag.Diagnostic) {
	var d Document
	if ds := strictDecode(data, &d); ds != nil {
		return nil, ds
	}
	return &d, nil
}

// ParseOverrides decodes an agent-context OverridesFile from JSON in strict mode
// (unknown fields rejected). It returns a non-nil file only when decoding
// produced no errors.
func ParseOverrides(data []byte) (*OverridesFile, []diag.Diagnostic) {
	var o OverridesFile
	if ds := strictDecode(data, &o); ds != nil {
		return nil, ds
	}
	return &o, nil
}

// strictDecode runs a DisallowUnknownFields decode and maps a decode failure to
// a coded diagnostic. It returns nil on success.
func strictDecode(data []byte, v any) []diag.Diagnostic {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		code := ErrorCodeParseError
		field := ""
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			code = ErrorCodeUnknownField
			field = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		}
		return []diag.Diagnostic{diag.Errorf(code, field, "%s", msg)}
	}
	// Require EOF after the first value: a document followed by trailing JSON is
	// not a single valid document and must fail closed rather than silently
	// dropping the trailer.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"trailing data after the JSON document; the input must be exactly one JSON value")}
	}
	return nil
}

// ValidateDocument checks structural and semantic invariants on a parsed
// document. It returns one diagnostic per finding in a stable, source-order
// sequence: protocol version, identity, each section in field order, then
// provenance. Path hygiene and duplicate-reference detection run here so the
// normal validate path rejects an unsafe path without a caller opting in; the
// fail-closed publish-safety gate (ValidatePublishSafety) re-checks them and
// adds redaction rules.
func ValidateDocument(d *Document) []diag.Diagnostic {
	if d == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "document is nil")}
	}

	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(d.ProtocolVersion)...)
	diags = append(diags, validateIdentity(d.Identity)...)

	for i, r := range d.CompositionRoots {
		field := fmt.Sprintf("compositionRoots[%d]", i)
		diags = append(diags, validatePath(field+".path", r.Path)...)
		if !ValidRootKinds[r.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRootKind, field+".kind",
				"composition-root kind %q is not in the v1 set: applicationMain, describeEntrypoint", r.Kind))
		}
	}

	diags = append(diags, validateRefs("capabilities", d.Capabilities)...)
	diags = append(diags, validateRefs("contracts", d.Contracts)...)
	diags = append(diags, validateRefs("infra", d.Infra)...)
	diags = append(diags, validateRefs("migrations", d.Migrations)...)

	for i, s := range d.RepresentativeSources {
		diags = append(diags, validateSourceRange(fmt.Sprintf("representativeSources[%d]", i), s)...)
	}

	diags = append(diags, validateTests(d.Tests)...)

	for i, doc := range d.Docs {
		field := fmt.Sprintf("docs[%d]", i)
		diags = append(diags, validatePath(field+".path", doc.Path)...)
		if !ValidDocRelationships[doc.Relationship] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDocRelationship, field+".relationship",
				"doc relationship %q is not in the v1 set: checked, unchecked", doc.Relationship))
		}
	}

	diags = append(diags, validateConfig(d.Config)...)
	diags = append(diags, validateProvenance(d.Provenance)...)
	return diags
}

// validateIdentity checks the required identity fields and rejects a duplicate
// id in either neighbor list.
func validateIdentity(id Identity) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(id.ID) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIdentity, "identity.id",
			"identity must carry a project id"))
	}
	if strings.TrimSpace(id.Name) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIdentity, "identity.name",
			"identity must carry a project name"))
	}
	if strings.TrimSpace(id.Path) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIdentity, "identity.path",
			"identity must carry a project path"))
	} else {
		// Consumers use identity.path as the project directory, so it gets the
		// same workspace-relative hygiene as every other wire path.
		diags = append(diags, validatePath("identity.path", id.Path)...)
	}
	diags = append(diags, validateUniqueIDs("identity.dependencies", id.Dependencies)...)
	diags = append(diags, validateUniqueIDs("identity.dependents", id.Dependents)...)
	return diags
}

// validateUniqueIDs rejects a repeated id in an ordered id list; the second
// occurrence is reported so the message is stable.
func validateUniqueIDs(field string, ids []string) []diag.Diagnostic {
	seen := make(map[string]bool, len(ids))
	var diags []diag.Diagnostic
	for i, id := range ids {
		if seen[id] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateID, fmt.Sprintf("%s[%d]", field, i),
				"id %q is listed more than once; keep a single entry", id))
			continue
		}
		seen[id] = true
	}
	return diags
}

// validateRefs validates a section of artifact references: each path is
// workspace-relative, each digest is well-formed, and no path repeats within the
// section.
func validateRefs(section string, refs []ArtifactRef) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]bool, len(refs))
	for i, r := range refs {
		field := fmt.Sprintf("%s[%d]", section, i)
		diags = append(diags, validatePath(field+".path", r.Path)...)
		diags = append(diags, validateDigest(field+".digest", r.Digest)...)
		if r.Path != "" {
			if seen[r.Path] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicateRef, field+".path",
					"reference path %q is listed more than once in %s; keep a single entry", r.Path, section))
			}
			seen[r.Path] = true
		}
	}
	return diags
}

// validateSourceRange validates one representative-source entry: path hygiene,
// a 1-based non-descending line range, a known reason, and a well-formed token
// estimate.
func validateSourceRange(field string, s SourceRange) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validatePath(field+".path", s.Path)...)
	if s.StartLine < 1 || s.EndLine < s.StartLine {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRange, field,
			"line range [%d,%d] is invalid; require 1 <= startLine <= endLine", s.StartLine, s.EndLine))
	}
	if !ValidSourceReasons[s.Why] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceReason, field+".why",
			"source reason %q is not in the v1 set: capability-evidence, main, override", s.Why))
	}
	diags = append(diags, validateTokenEstimate(field+".tokens", s.Tokens)...)
	return diags
}

// validateTokenEstimate rejects a negative estimate or an unknown method.
func validateTokenEstimate(field string, e TokenEstimate) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Estimated < 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTokenEstimate, field+".estimated",
			"token estimate %d is negative", e.Estimated))
	}
	if !ValidTokenMethods[e.Method] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTokenMethod, field+".method",
			"token method %q is not in the v1 set: bytes/4", e.Method))
	}
	return diags
}

// validateTests validates the optional tests section. A present section must
// name a known policy, and its packs and absence reason are mutually exclusive:
// an empty section MUST carry an absence reason (so the emptiness is explained),
// and a non-empty section MUST NOT.
func validateTests(t *TestsSection) []diag.Diagnostic {
	if t == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if !ValidTestPolicies[t.Policy] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTestPolicy, "tests.policy",
			"test policy %q is not in the v1 set: auto, require, skip", t.Policy))
	}
	if len(t.Packs) == 0 {
		if t.AbsenceReason == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAbsenceReason, "tests.absenceReason",
				"tests section lists no packs; set a machine-readable absenceReason (not-collected, no-packs, unsupported-project-type)"))
		} else if !ValidAbsenceReasons[t.AbsenceReason] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidAbsenceReason, "tests.absenceReason",
				"absence reason %q is not in the v1 set: not-collected, no-packs, unsupported-project-type", t.AbsenceReason))
		}
	} else if t.AbsenceReason != "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidAbsenceReason, "tests.absenceReason",
			"tests section lists packs, so absenceReason %q must be empty", t.AbsenceReason))
	}
	for i, p := range t.Packs {
		if strings.TrimSpace(p.ID) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPack, fmt.Sprintf("tests.packs[%d].id", i),
				"pack reference must carry its stable conformance-pack id"))
		}
	}
	diags = append(diags, validateRefs("tests.fixtureDigests", t.FixtureDigests)...)
	return diags
}

// validateConfig validates the optional config section's references.
func validateConfig(c *ConfigSection) []diag.Diagnostic {
	if c == nil || c.SchemaRef == nil {
		return nil
	}
	diags := validatePath("config.schemaRef.path", c.SchemaRef.Path)
	return append(diags, validateDigest("config.schemaRef.digest", c.SchemaRef.Digest)...)
}

// validateProvenance checks the required provenance fields: a workspace
// revision, a generator identity/version, and a known aggregation method.
func validateProvenance(p Provenance) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(p.WorkspaceRevision) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, "provenance.workspaceRevision",
			"provenance must carry a workspace revision"))
	}
	if strings.TrimSpace(p.Generator.Name) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, "provenance.generator.name",
			"provenance must carry a generator name"))
	}
	if strings.TrimSpace(p.Generator.Version) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, "provenance.generator.version",
			"provenance must carry a generator version"))
	}
	if !ValidAggregationMethods[p.AggregationMethod] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidAggregationMethod, "provenance.aggregationMethod",
			"aggregation method %q is not in the v1 set: by-reference", p.AggregationMethod))
	}
	return diags
}

// validateDigest checks that a digest is in canonical "sha256:<64hex>" form.
func validateDigest(field, digest string) []diag.Diagnostic {
	if !digestPattern.MatchString(digest) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDigest, field,
			"digest %q is not in sha256:<64 lowercase hex> form", digest)}
	}
	return nil
}

// validatePath checks that a path is workspace-relative: non-empty, not
// absolute, and free of any ".." segment that would escape the workspace.
func validatePath(field, p string) []diag.Diagnostic {
	if !isWorkspaceRelative(p) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field,
			"path %q must be a non-empty, canonical workspace-relative path (no leading '/' or './', no '..' segment, no doubled or trailing slash)", p)}
	}
	return nil
}

// isWorkspaceRelative reports whether p is a non-empty, CANONICAL relative path
// with no ".." segment. Canonical means p equals path.Clean(p): spellings like
// "./x", "a//b", "a/./b", or a trailing slash are rejected rather than
// normalized, so two references to the same file can never differ in spelling —
// which is what makes the sensitive-path lookup in the publish-safety gate
// sound (an equivalent-but-different spelling cannot bypass an exact-match
// sensitive set). Forward-slash semantics: wire paths are always
// workspace-relative POSIX paths regardless of the host OS.
func isWorkspaceRelative(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	// Reject Windows-style absolute/backslash paths explicitly; wire paths are POSIX.
	if strings.ContainsRune(p, '\\') {
		return false
	}
	if path.IsAbs(p) {
		return false
	}
	if p != path.Clean(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// ValidateOverrides checks structural and semantic invariants on a parsed
// overrides file: protocol version, each added source and doc in source order,
// and path hygiene on every referenced path.
func ValidateOverrides(o *OverridesFile) []diag.Diagnostic {
	if o == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "overrides file is nil")}
	}
	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(o.ProtocolVersion)...)
	for i, s := range o.AddSources {
		diags = append(diags, validateSourceRange(fmt.Sprintf("addSources[%d]", i), s)...)
	}
	for i, p := range o.RemoveSources {
		diags = append(diags, validatePath(fmt.Sprintf("removeSources[%d]", i), p)...)
	}
	for i, d := range o.AddDocs {
		field := fmt.Sprintf("addDocs[%d]", i)
		diags = append(diags, validatePath(field+".path", d.Path)...)
		if !ValidDocRelationships[d.Relationship] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDocRelationship, field+".relationship",
				"doc relationship %q is not in the v1 set: checked, unchecked", d.Relationship))
		}
	}
	for i, p := range o.RemoveDocs {
		diags = append(diags, validatePath(fmt.Sprintf("removeDocs[%d]", i), p)...)
	}
	for i, p := range o.Sensitive {
		diags = append(diags, validatePath(fmt.Sprintf("sensitive[%d]", i), p)...)
	}
	return diags
}

// PublishSafetyOptions parameterizes the fail-closed publish-safety gate.
type PublishSafetyOptions struct {
	// SensitivePaths is the caller-supplied set of workspace-relative paths that
	// are sensitive: gitignored paths, files backing `sensitive` config fields,
	// infra `secret`-kind entries, and keyring material. A reference to any of
	// these must carry the sensitivity flag, or the gate fails closed.
	SensitivePaths map[string]bool
	// MaxStringBytes is the length above which a string field is treated as
	// suspected embedded content. Zero selects the built-in default (8192).
	MaxStringBytes int
}

// ValidatePublishSafety is the fail-closed redaction gate. It is the
// acceptance-critical check a producer runs before a document leaves the
// workspace for an authorized index. It runs independently of ValidateDocument
// (it does not assume that pass ran) and reports, as hard errors:
//
//   - every reference to a caller-flagged sensitive path that is not marked
//     Sensitive (agentcontext.unredacted_sensitive);
//   - any string field long enough to look like embedded file content
//     (agentcontext.embedded_content);
//   - any non-workspace-relative path (agentcontext.invalid_path); and
//   - any duplicated reference (agentcontext.duplicate_ref).
//
// The wire types have no slot for file content, so structurally a document can
// only ever reference — the oversized-string check is defense in depth against a
// producer stuffing content into a name or tag. A nil document is itself an
// error, so the gate never passes vacuously.
func ValidatePublishSafety(d *Document, opts PublishSafetyOptions) []diag.Diagnostic {
	if d == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "document is nil")}
	}
	maxLen := opts.MaxStringBytes
	if maxLen <= 0 {
		maxLen = defaultMaxStringBytes
	}
	sensitive := opts.SensitivePaths

	var diags []diag.Diagnostic
	// Path hygiene + duplicate refs (defense in depth: the gate re-derives these
	// so it is safe to run alone). identity.path is included: consumers use it
	// as the project directory, so an escaping value is a publish hazard too.
	if strings.TrimSpace(d.Identity.Path) != "" {
		diags = append(diags, validatePath("identity.path", d.Identity.Path)...)
	}
	for i, r := range d.CompositionRoots {
		diags = append(diags, validatePath(fmt.Sprintf("compositionRoots[%d].path", i), r.Path)...)
		diags = append(diags, checkSensitive(fmt.Sprintf("compositionRoots[%d]", i), r.Path, r.Sensitive, sensitive)...)
	}
	diags = append(diags, publishRefs("capabilities", d.Capabilities, sensitive)...)
	diags = append(diags, publishRefs("contracts", d.Contracts, sensitive)...)
	diags = append(diags, publishRefs("infra", d.Infra, sensitive)...)
	diags = append(diags, publishRefs("migrations", d.Migrations, sensitive)...)
	for i, s := range d.RepresentativeSources {
		field := fmt.Sprintf("representativeSources[%d]", i)
		diags = append(diags, validatePath(field+".path", s.Path)...)
		diags = append(diags, checkSensitive(field, s.Path, s.Sensitive, sensitive)...)
	}
	if d.Tests != nil {
		diags = append(diags, publishRefs("tests.fixtureDigests", d.Tests.FixtureDigests, sensitive)...)
	}
	for i, doc := range d.Docs {
		field := fmt.Sprintf("docs[%d]", i)
		diags = append(diags, validatePath(field+".path", doc.Path)...)
		diags = append(diags, checkSensitive(field, doc.Path, doc.Sensitive, sensitive)...)
	}
	if d.Config != nil && d.Config.SchemaRef != nil {
		diags = append(diags, validatePath("config.schemaRef.path", d.Config.SchemaRef.Path)...)
		diags = append(diags, checkSensitive("config.schemaRef", d.Config.SchemaRef.Path, d.Config.SchemaRef.Sensitive, sensitive)...)
	}

	// Oversized-string scan: no legitimate wire string approaches the threshold.
	for _, sf := range documentStrings(d) {
		if len(sf.value) > maxLen {
			diags = append(diags, diag.Errorf(ErrorCodeEmbeddedContent, sf.field,
				"string field is %d bytes, over the %d-byte limit; the document must reference content, never embed it", len(sf.value), maxLen))
		}
	}
	return diags
}

// publishRefs runs path hygiene, duplicate detection, and the sensitivity-flag
// rule over a section of artifact references.
func publishRefs(section string, refs []ArtifactRef, sensitive map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]bool, len(refs))
	for i, r := range refs {
		field := fmt.Sprintf("%s[%d]", section, i)
		diags = append(diags, validatePath(field+".path", r.Path)...)
		if r.Path != "" {
			if seen[r.Path] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicateRef, field+".path",
					"reference path %q is listed more than once in %s; keep a single entry", r.Path, section))
			}
			seen[r.Path] = true
		}
		diags = append(diags, checkSensitive(field, r.Path, r.Sensitive, sensitive)...)
	}
	return diags
}

// checkSensitive fails closed when an entry references a caller-flagged
// sensitive path without carrying the sensitivity flag.
func checkSensitive(field, p string, flagged bool, sensitive map[string]bool) []diag.Diagnostic {
	if sensitive[p] && !flagged {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnredactedSensitive, field,
			"entry references sensitive path %q but is not marked sensitive; flag it or drop the reference", p)}
	}
	return nil
}

// stringField pairs a document field path with its string value for the
// oversized-string scan.
type stringField struct {
	field string
	value string
}

// documentStrings collects every free-form string value in a document, in a
// stable order, for the publish-safety oversized-string scan. Paths and digests
// are covered by the path/digest checks but are included here too so no string
// escapes the content-embedding guard.
func documentStrings(d *Document) []stringField {
	var out []stringField
	add := func(field, value string) {
		if value != "" {
			out = append(out, stringField{field, value})
		}
	}
	add("$schema", d.Schema)
	add("identity.id", d.Identity.ID)
	add("identity.name", d.Identity.Name)
	add("identity.path", d.Identity.Path)
	add("identity.type", d.Identity.Type)
	for i, t := range d.Identity.Tags {
		add(fmt.Sprintf("identity.tags[%d]", i), t)
	}
	for i, l := range d.Identity.Languages {
		add(fmt.Sprintf("identity.languages[%d]", i), l)
	}
	for i, v := range d.Identity.Dependencies {
		add(fmt.Sprintf("identity.dependencies[%d]", i), v)
	}
	for i, v := range d.Identity.Dependents {
		add(fmt.Sprintf("identity.dependents[%d]", i), v)
	}
	for i, r := range d.CompositionRoots {
		add(fmt.Sprintf("compositionRoots[%d].path", i), r.Path)
		add(fmt.Sprintf("compositionRoots[%d].provenance", i), r.Provenance)
	}
	out = append(out, refStrings("capabilities", d.Capabilities)...)
	out = append(out, refStrings("contracts", d.Contracts)...)
	out = append(out, refStrings("infra", d.Infra)...)
	out = append(out, refStrings("migrations", d.Migrations)...)
	for i, s := range d.RepresentativeSources {
		add(fmt.Sprintf("representativeSources[%d].path", i), s.Path)
	}
	if d.Tests != nil {
		for i, p := range d.Tests.Packs {
			add(fmt.Sprintf("tests.packs[%d].id", i), p.ID)
		}
		out = append(out, refStrings("tests.fixtureDigests", d.Tests.FixtureDigests)...)
	}
	for i, doc := range d.Docs {
		add(fmt.Sprintf("docs[%d].path", i), doc.Path)
	}
	if d.Config != nil && d.Config.SchemaRef != nil {
		add("config.schemaRef.path", d.Config.SchemaRef.Path)
		add("config.schemaRef.digest", d.Config.SchemaRef.Digest)
	}
	add("provenance.workspaceRevision", d.Provenance.WorkspaceRevision)
	add("provenance.generator.name", d.Provenance.Generator.Name)
	add("provenance.generator.version", d.Provenance.Generator.Version)
	return out
}

// refStrings collects the path, digest, and kind of every reference in a
// section. Kind is an unrestricted string, so it MUST be in the
// embedded-content sweep — otherwise it is a smuggling slot.
func refStrings(section string, refs []ArtifactRef) []stringField {
	var out []stringField
	for i, r := range refs {
		if r.Path != "" {
			out = append(out, stringField{fmt.Sprintf("%s[%d].path", section, i), r.Path})
		}
		if r.Digest != "" {
			out = append(out, stringField{fmt.Sprintf("%s[%d].digest", section, i), r.Digest})
		}
		if r.Kind != "" {
			out = append(out, stringField{fmt.Sprintf("%s[%d].kind", section, i), r.Kind})
		}
	}
	return out
}

// ParseAndValidateDocument is a convenience that runs strict parsing followed by
// structural and semantic validation.
func ParseAndValidateDocument(data []byte) (*Document, []diag.Diagnostic) {
	d, diags := ParseDocument(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return d, append(diags, ValidateDocument(d)...)
}

// ParseAndValidateOverrides is a convenience that runs strict parsing followed
// by structural and semantic validation.
func ParseAndValidateOverrides(data []byte) (*OverridesFile, []diag.Diagnostic) {
	o, diags := ParseOverrides(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return o, append(diags, ValidateOverrides(o)...)
}
