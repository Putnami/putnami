package database

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and adapters key off
// these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeParseError             = "database.parse_error"
	ErrorCodeUnknownField           = "database.unknown_field"
	ErrorCodeInvalidProtocolVersion = "database.invalid_protocol_version"
	ErrorCodeInvalidName            = "database.invalid_name"
	ErrorCodeMissingEngine          = "database.missing_engine"
	ErrorCodeInvalidEngine          = "database.invalid_engine"
	ErrorCodeMissingSchema          = "database.missing_schema"
	ErrorCodeInvalidSchema          = "database.invalid_schema"
	ErrorCodeMissingConnection      = "database.missing_connection"
	ErrorCodeInvalidConnection      = "database.invalid_connection"
	ErrorCodeInvalidMode            = "database.invalid_mode"
	ErrorCodeInvalidIsolation       = "database.invalid_isolation"
	ErrorCodeInvalidReuse           = "database.invalid_reuse"
)

// ValidErrorCodes enumerates the canonical database-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidName:            true,
	ErrorCodeMissingEngine:          true,
	ErrorCodeInvalidEngine:          true,
	ErrorCodeMissingSchema:          true,
	ErrorCodeInvalidSchema:          true,
	ErrorCodeMissingConnection:      true,
	ErrorCodeInvalidConnection:      true,
	ErrorCodeInvalidMode:            true,
	ErrorCodeInvalidIsolation:       true,
	ErrorCodeInvalidReuse:           true,
}

// datasourceNamePattern matches a canonical logical datasource identifier. We
// reuse the same shape as the other protocol/* modules (lowercase letters,
// digits, '-', '_', '.', '/'; 1–64 chars) so names look uniform across
// protocols.
var datasourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]{0,63}$`)

// schemaPattern matches an unquoted SQL identifier used as a Postgres schema /
// search_path entry.
var schemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// ParseRequirementManifest decodes a RequirementManifest from JSON in strict
// mode (unknown fields rejected). A non-nil manifest is returned only when
// parsing produced no errors.
func ParseRequirementManifest(data []byte) (*RequirementManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m RequirementManifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &m, nil
}

// ParseBinding decodes a Binding from JSON in strict mode.
func ParseBinding(data []byte) (*Binding, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Binding
	if err := dec.Decode(&b); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &b, nil
}

// ParseTestBinding decodes a TestBinding from JSON in strict mode.
func ParseTestBinding(data []byte) (*TestBinding, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t TestBinding
	if err := dec.Decode(&t); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &t, nil
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

// ValidateRequirementManifest checks structural invariants: a supported protocol
// version and, per datasource, a canonical name, an in-enum engine, and a schema
// when the engine requires one. A requirement carries no connection by
// construction.
func ValidateRequirementManifest(m *RequirementManifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "requirement manifest is nil")}
	}
	diags := validateProtocolVersion(m.ProtocolVersion)
	for _, name := range sortedKeys(m.Databases) {
		field := fmt.Sprintf("databases[%q]", name)
		r := m.Databases[name]
		diags = append(diags, validateDatasourceName(field, name)...)
		diags = append(diags, validateEngine(field+".engine", r.Engine)...)
		diags = append(diags, validateSchema(field+".schema", r.Engine, r.Schema)...)
	}
	return diags
}

// ValidateBinding checks structural invariants: a supported protocol version
// and, per datasource, a canonical name, an in-enum engine, a schema when
// required, and a connection that declares exactly one transport strategy.
func ValidateBinding(b *Binding) []diag.Diagnostic {
	if b == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "binding is nil")}
	}
	diags := validateProtocolVersion(b.ProtocolVersion)
	for _, name := range sortedKeys(b.Databases) {
		field := fmt.Sprintf("databases[%q]", name)
		diags = append(diags, validateDatasourceName(field, name)...)
		diags = append(diags, validateDatabaseEntry(field, b.Databases[name])...)
	}
	return diags
}

// ValidateTestBinding checks the Binding invariants plus the in-enum test policy
// fields (mode, isolation, reuse) when they are present.
func ValidateTestBinding(t *TestBinding) []diag.Diagnostic {
	if t == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "test binding is nil")}
	}
	diags := validateProtocolVersion(t.ProtocolVersion)
	if t.Mode != "" && !t.Mode.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidMode, "mode",
			"mode %q is not in the v1 set: skip, require, auto", t.Mode))
	}
	if t.Isolation != "" && !t.Isolation.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIsolation, "isolation",
			"isolation %q is not in the v1 set: database, schema", t.Isolation))
	}
	if t.Reuse != "" && !t.Reuse.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidReuse, "reuse",
			"reuse %q is not in the v1 set: none, bundle-template", t.Reuse))
	}
	for _, name := range sortedKeys(t.Databases) {
		field := fmt.Sprintf("databases[%q]", name)
		diags = append(diags, validateDatasourceName(field, name)...)
		diags = append(diags, validateDatabaseEntry(field, t.Databases[name])...)
	}
	return diags
}

// validateDatabaseEntry validates one resolved datasource binding entry.
func validateDatabaseEntry(field string, d Database) []diag.Diagnostic {
	diags := validateEngine(field+".engine", d.Engine)
	diags = append(diags, validateSchema(field+".schema", d.Engine, d.Schema)...)
	diags = append(diags, validateConnection(field+".connection", d.Connection)...)
	return diags
}

// validateConnection enforces the exactly-one-transport rule: a connection
// declares its transport via dsn, host (TCP), or instance (cloud socket), and
// never more than one. A dsn is self-contained, so no structured field may
// accompany it.
func validateConnection(field string, c *Connection) []diag.Diagnostic {
	if c == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingConnection, field,
			"binding datasource must declare a connection")}
	}
	var transports []string
	if strings.TrimSpace(c.DSN) != "" {
		transports = append(transports, "dsn")
	}
	if strings.TrimSpace(c.Host) != "" {
		transports = append(transports, "host")
	}
	if strings.TrimSpace(c.Instance) != "" {
		transports = append(transports, "instance")
	}
	switch len(transports) {
	case 0:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidConnection, field,
			"connection must declare exactly one transport: dsn, host, or instance")}
	case 1:
		// ok — fall through to the dsn self-containment check.
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidConnection, field,
			"connection must declare exactly one transport but found %s", strings.Join(transports, ", "))}
	}
	if transports[0] == "dsn" {
		if extras := dsnExtras(c); len(extras) > 0 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidConnection, field,
				"dsn is self-contained; remove %s", strings.Join(extras, ", "))}
		}
	}
	if transports[0] == "instance" {
		if extras := instanceExtras(c); len(extras) > 0 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidConnection, field,
				"instance connections do not use %s", strings.Join(extras, ", "))}
		}
	}
	return nil
}

// dsnExtras returns the sorted structured fields that must not accompany a
// self-contained dsn transport.
func dsnExtras(c *Connection) []string {
	var extras []string
	if c.Port != 0 {
		extras = append(extras, "port")
	}
	if strings.TrimSpace(c.Database) != "" {
		extras = append(extras, "database")
	}
	if strings.TrimSpace(c.User) != "" {
		extras = append(extras, "user")
	}
	if strings.TrimSpace(c.Password) != "" {
		extras = append(extras, "password")
	}
	if c.SSL != nil {
		extras = append(extras, "ssl")
	}
	if len(c.Params) > 0 {
		extras = append(extras, "params")
	}
	sort.Strings(extras)
	return extras
}

// instanceExtras returns the sorted fields that an instance (cloud socket)
// transport does not use.
func instanceExtras(c *Connection) []string {
	var extras []string
	if c.Port != 0 {
		extras = append(extras, "port")
	}
	if c.SSL != nil {
		extras = append(extras, "ssl")
	}
	sort.Strings(extras)
	return extras
}

// validateEngine validates a datasource engine: required and in the closed enum.
func validateEngine(field string, e Engine) []diag.Diagnostic {
	if e == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingEngine, field, "engine is required")}
	}
	if !e.Valid() {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidEngine, field,
			"engine %q is not in the v1 set: postgres", e)}
	}
	return nil
}

// validateSchema validates a datasource schema: required for postgres, and a
// well-formed identifier when present.
func validateSchema(field string, e Engine, schema string) []diag.Diagnostic {
	if e == EnginePostgres && strings.TrimSpace(schema) == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingSchema, field,
			"schema is required for postgres datasources")}
	}
	if schema != "" && !schemaPattern.MatchString(schema) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidSchema, field,
			"schema %q does not match the identifier pattern %q", schema, schemaPattern.String())}
	}
	return nil
}

// validateDatasourceName validates a logical datasource name (a map key) against
// the canonical pattern.
func validateDatasourceName(field, name string) []diag.Diagnostic {
	if name == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field, "datasource name is required")}
	}
	if !datasourceNamePattern.MatchString(name) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field,
			"datasource name %q does not match canonical pattern %q", name, datasourceNamePattern.String())}
	}
	return nil
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// sortedKeys returns the keys of m sorted lexicographically, so validation
// emits diagnostics in a deterministic order regardless of map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseAndValidateRequirementManifest runs strict parsing followed by structural
// validation.
func ParseAndValidateRequirementManifest(data []byte) (*RequirementManifest, []diag.Diagnostic) {
	m, diags := ParseRequirementManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateRequirementManifest(m)...)
}

// ParseAndValidateBinding runs strict parsing followed by structural validation.
func ParseAndValidateBinding(data []byte) (*Binding, []diag.Diagnostic) {
	b, diags := ParseBinding(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return b, append(diags, ValidateBinding(b)...)
}

// ParseAndValidateTestBinding runs strict parsing followed by structural
// validation.
func ParseAndValidateTestBinding(data []byte) (*TestBinding, []diag.Diagnostic) {
	t, diags := ParseTestBinding(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return t, append(diags, ValidateTestBinding(t)...)
}
