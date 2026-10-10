// Package authoredmember defines the authored config member: the immutable,
// secret-free artifact a config producer publishes and the config store
// accepts. It fixes the wire types, the canonical encoding, the digests, and
// the validation rules both sides share. It reads no secret and grants no
// runtime access.
package authoredmember

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	// FormatVersion identifies the canonical authored-member wire format.
	FormatVersion = "config.authored-member.v1"
	// MediaType identifies authored-member artifact bytes.
	MediaType = "application/vnd.putnami.config.authored-member+json"
	// MaxMemberSize bounds the canonical artifact size.
	MaxMemberSize = 4 << 20
)

// ErrInvalidMember reports a member that is incomplete, non-canonical, or unsafe.
var ErrInvalidMember = errors.New("invalid authored member")

// Publisher builds and validates members. It has no method that reads a
// secret.
type Publisher struct{}

// PublisherContract builds and validates canonical secret-free members.
type PublisherContract interface {
	Build(context.Context, BuildAuthoredMemberRequest) (AuthoredMember, error)
	Validate(context.Context, AuthoredMember) (Descriptor, error)
}

// SourceProvenance pins the repository and revision that authored the member.
type SourceProvenance struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// SchemaField is a normalized, canonical configuration path. Supported types
// are string, integer, number, boolean, object, and array.
type SchemaField struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	Required  bool   `json:"required,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

// NormalizedSchema is the ordered, closed schema embedded in the member.
type NormalizedSchema struct {
	Fields []SchemaField `json:"fields"`
}

// AuthoredLayer is repository intent. Pin is the immutable repository pin that
// selected this layer; values are keyed by canonical schema path.
type AuthoredLayer struct {
	Name   string         `json:"name"`
	Pin    string         `json:"pin"`
	Values map[string]any `json:"values"`
}

// SecretReference names a secret by reference: a path never carries a secret
// value.
//
// Path names a sensitive schema field, and that field says whether the boot
// needs the secret. A reference to a required field is required: the config
// store refuses to prepare the member until the secret is set. A reference to
// a field that is not required is optional: a boot receives the secret when it
// is set and boots without it when it is not.
//
// The shape is exactly path and reference. Validate re-encodes the member and
// compares bytes, so a reader that predates an added field refuses the member
// as not canonical.
type SecretReference struct {
	Path      string `json:"path"`
	Reference string `json:"reference"`
}

// BuildAuthoredMemberRequest is the complete input to canonical publication.
type BuildAuthoredMemberRequest struct {
	WorkspaceID      string
	Project          string
	Environment      string
	Schema           NormalizedSchema
	AuthoredLayers   []AuthoredLayer
	SecretReferences []SecretReference
	SourceProvenance SourceProvenance
}

// Descriptor is the complete immutable public member identity.
type Descriptor struct {
	FormatVersion          string           `json:"formatVersion"`
	MediaType              string           `json:"mediaType"`
	WorkspaceID            string           `json:"workspaceID"`
	Project                string           `json:"project"`
	Environment            string           `json:"environment"`
	SchemaDigest           string           `json:"schemaDigest"`
	AuthoredDigest         string           `json:"authoredDigest"`
	SecretReferencesDigest string           `json:"secretReferencesDigest"`
	ContentDigest          string           `json:"contentDigest"`
	SelectionFingerprint   string           `json:"selectionFingerprint"`
	Size                   int64            `json:"size"`
	SourceProvenance       SourceProvenance `json:"sourceProvenance"`
}

// AuthoredMember pairs canonical artifact bytes with their verified descriptor.
type AuthoredMember struct {
	Descriptor Descriptor
	Bytes      []byte
}

// Build validates and canonicalizes one secret-free authored member.
func (Publisher) Build(_ context.Context, request BuildAuthoredMemberRequest) (AuthoredMember, error) {
	w, err := canonicalize(request)
	if err != nil {
		return AuthoredMember{}, err
	}
	bytes, err := json.Marshal(w)
	if err != nil {
		return AuthoredMember{}, fmt.Errorf("%w: encode canonical member", ErrInvalidMember)
	}
	if len(bytes) > MaxMemberSize {
		return AuthoredMember{}, invalid("canonical member exceeds size bound")
	}
	d := descriptorFor(w, bytes)
	return AuthoredMember{Descriptor: d, Bytes: bytes}, nil
}

// Validate recomputes and returns the descriptor of canonical member bytes.
func (Publisher) Validate(_ context.Context, member AuthoredMember) (Descriptor, error) {
	if len(member.Bytes) == 0 || len(member.Bytes) > MaxMemberSize {
		return Descriptor{}, invalid("member bytes are required")
	}
	var wire memberWire
	decoder := json.NewDecoder(bytes.NewReader(member.Bytes))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return Descriptor{}, invalid("member bytes are not JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Descriptor{}, invalid("member bytes contain trailing JSON")
	}
	request := BuildAuthoredMemberRequest{wire.WorkspaceID, wire.Project, wire.Environment, wire.Schema, wire.AuthoredLayers, wire.SecretReferences, wire.SourceProvenance}
	canonical, err := canonicalize(request)
	if err != nil {
		return Descriptor{}, err
	}
	expected, err := json.Marshal(canonical)
	if err != nil {
		return Descriptor{}, invalid("canonical member cannot be encoded")
	}
	if string(expected) != string(member.Bytes) {
		return Descriptor{}, invalid("member bytes are not canonical")
	}
	d := descriptorFor(canonical, expected)
	if member.Descriptor != (Descriptor{}) && !sameDescriptor(member.Descriptor, d) {
		return Descriptor{}, invalid("declared descriptor does not match bytes")
	}
	return d, nil
}

type memberWire struct {
	FormatVersion    string            `json:"formatVersion"`
	MediaType        string            `json:"mediaType"`
	WorkspaceID      string            `json:"workspaceID"`
	Project          string            `json:"project"`
	Environment      string            `json:"environment"`
	Schema           NormalizedSchema  `json:"schema"`
	AuthoredLayers   []AuthoredLayer   `json:"authoredLayers"`
	SecretReferences []SecretReference `json:"secretReferences"`
	SourceProvenance SourceProvenance  `json:"sourceProvenance"`
}

func canonicalize(r BuildAuthoredMemberRequest) (memberWire, error) {
	if !boundedID(r.WorkspaceID) || !boundedID(r.Project) || !boundedID(r.Environment) {
		return memberWire{}, invalid("workspace, project, and environment are required bounded identifiers")
	}
	if !boundedID(r.SourceProvenance.Repository) || !boundedID(r.SourceProvenance.Revision) {
		return memberWire{}, invalid("repository provenance is required")
	}
	fields := append([]SchemaField{}, r.Schema.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Path < fields[j].Path })
	fieldByPath := make(map[string]SchemaField, len(fields))
	for _, f := range fields {
		if !canonicalPath(f.Path) || !supportedType(f.Type) || fieldByPath[f.Path].Path != "" {
			return memberWire{}, invalid("schema has an invalid or duplicate canonical path")
		}
		fieldByPath[f.Path] = f
	}
	layers := append([]AuthoredLayer(nil), r.AuthoredLayers...)
	sort.Slice(layers, func(i, j int) bool { return layers[i].Name < layers[j].Name })
	seenLayer := map[string]bool{}
	values := map[string]bool{}
	authoredPaths := map[string]bool{}
	for i := range layers {
		l := &layers[i]
		if !boundedID(l.Name) || !boundedID(l.Pin) || seenLayer[l.Name] {
			return memberWire{}, invalid("authored layers require unique names and pins")
		}
		seenLayer[l.Name] = true
		if l.Values == nil {
			l.Values = map[string]any{}
		}
		for path, value := range l.Values {
			f, ok := fieldByPath[path]
			if !ok || f.Sensitive || !canonicalPath(path) || placeholder(value) || !valueMatches(f.Type, value) {
				return memberWire{}, invalid("authored values do not match the normalized schema")
			}
			if authoredPaths[path] {
				return memberWire{}, invalid("duplicate canonical authored path")
			}
			authoredPaths[path] = true
			if err := validateAuthoredDescendants(path, value, fieldByPath, values); err != nil {
				return memberWire{}, err
			}
		}
	}
	for path := range authoredPaths {
		if hasPathAncestor(authoredPaths, path) {
			return memberWire{}, invalid("authored paths overlap")
		}
	}
	for _, f := range fields {
		if f.Required && !f.Sensitive && !values[f.Path] {
			return memberWire{}, invalid("required authored value is missing")
		}
	}
	references := append([]SecretReference(nil), r.SecretReferences...)
	sort.Slice(references, func(i, j int) bool { return references[i].Path < references[j].Path })
	seenRef := map[string]bool{}
	for _, ref := range references {
		f, ok := fieldByPath[ref.Path]
		_, referenceErr := SecretReferencePath(ref.Reference)
		if !ok || !f.Sensitive || !canonicalPath(ref.Path) || seenRef[ref.Path] || referenceErr != nil {
			return memberWire{}, invalid("secret reference is invalid")
		}
		for authoredPath := range authoredPaths {
			if pathsOverlap(authoredPath, ref.Path) {
				return memberWire{}, invalid("authored and secret-reference paths overlap")
			}
		}
		if hasPathAncestor(seenRef, ref.Path) {
			return memberWire{}, invalid("secret-reference paths overlap")
		}
		for prior := range seenRef {
			if hasPathPrefix(prior, ref.Path) {
				return memberWire{}, invalid("secret-reference paths overlap")
			}
		}
		seenRef[ref.Path] = true
	}
	for _, f := range fields {
		if f.Sensitive && f.Required && !seenRef[f.Path] {
			return memberWire{}, invalid("required secret reference is missing")
		}
	}
	return memberWire{FormatVersion, MediaType, r.WorkspaceID, r.Project, r.Environment, NormalizedSchema{fields}, layers, references, r.SourceProvenance}, nil
}

func descriptorFor(w memberWire, bytes []byte) Descriptor {
	schema := digest(w.Schema)
	authored := digest(w.AuthoredLayers)
	refs := digest(w.SecretReferences)
	selection := digest([]string{w.WorkspaceID, w.Project, w.Environment, schema, authored, refs, w.SourceProvenance.Repository, w.SourceProvenance.Revision})
	return Descriptor{w.FormatVersion, w.MediaType, w.WorkspaceID, w.Project, w.Environment, schema, authored, refs, digestBytes(bytes), selection, int64(len(bytes)), w.SourceProvenance}
}

func digest(v any) string                 { b, _ := json.Marshal(v); return digestBytes(b) }
func digestBytes(b []byte) string         { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func sameDescriptor(a, b Descriptor) bool { return a == b }
func invalid(reason string) error         { return fmt.Errorf("%w: %s", ErrInvalidMember, reason) }
func boundedID(s string) bool {
	return len(s) > 0 && len(s) <= 256 && s == strings.TrimSpace(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func canonicalPath(s string) bool {
	if !boundedID(s) || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
		for _, r := range p {
			if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}
func supportedType(t string) bool {
	switch t {
	case "string", "integer", "number", "boolean", "object", "array":
		return true
	}
	return false
}

// SecretReferencePath validates a canonical config-secret URI and returns the
// exact dotted key it selects from the secrets the config store holds.
func SecretReferencePath(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || len(s) == 0 || len(s) > 512 || u.Scheme != "config-secret" || u.Opaque != "" || u.Host == "" || u.Host != u.Hostname() || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.RawPath != "" {
		return "", invalid("secret reference URI is not canonical")
	}
	segments := append([]string{u.Host}, strings.Split(strings.TrimPrefix(u.Path, "/"), "/")...)
	if u.Path == "" {
		segments = segments[:1]
	}
	for _, segment := range segments {
		if !canonicalPath(segment) {
			return "", invalid("secret reference URI has an invalid path")
		}
	}
	if "config-secret://"+strings.Join(segments, "/") != s {
		return "", invalid("secret reference URI is not canonical")
	}
	return strings.Join(segments, "."), nil
}

func validateAuthoredDescendants(path string, value any, schema map[string]SchemaField, materialized map[string]bool) error {
	materialized[path] = true
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if !canonicalPath(key) || strings.Contains(key, ".") {
				return invalid("authored object has an invalid child path")
			}
			childPath := path + "." + key
			field, ok := schema[childPath]
			if !ok || field.Sensitive || placeholder(child) || !valueMatches(field.Type, child) {
				return invalid("authored descendants do not match the normalized schema")
			}
			if err := validateAuthoredDescendants(childPath, child, schema, materialized); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			switch child.(type) {
			case map[string]any, []any:
				if err := validateAuthoredArrayElement(path, child, schema, materialized); err != nil {
					return err
				}
			default:
				if placeholder(child) || hasSchemaDescendant(schema, path) {
					return invalid("authored array does not match the normalized schema")
				}
			}
		}
	}
	return nil
}

func validateAuthoredArrayElement(path string, value any, schema map[string]SchemaField, materialized map[string]bool) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if !canonicalPath(key) || strings.Contains(key, ".") {
				return invalid("authored array object has an invalid child path")
			}
			childPath := path + "." + key
			field, ok := schema[childPath]
			if !ok || field.Sensitive || placeholder(child) || !valueMatches(field.Type, child) {
				return invalid("authored array descendants do not match the normalized schema")
			}
			if err := validateAuthoredDescendants(childPath, child, schema, materialized); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateAuthoredArrayElement(path, child, schema, materialized); err != nil {
				return err
			}
		}
	default:
		if placeholder(value) || hasSchemaDescendant(schema, path) {
			return invalid("authored array does not match the normalized schema")
		}
	}
	return nil
}

func hasSchemaDescendant(schema map[string]SchemaField, path string) bool {
	for candidate := range schema {
		if hasPathPrefix(path, candidate) {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	return left == right || hasPathPrefix(left, right) || hasPathPrefix(right, left)
}

func hasPathPrefix(parent, child string) bool {
	return strings.HasPrefix(child, parent+".")
}

func hasPathAncestor(paths map[string]bool, path string) bool {
	for index := strings.LastIndexByte(path, '.'); index >= 0; index = strings.LastIndexByte(path[:index], '.') {
		if paths[path[:index]] {
			return true
		}
	}
	return false
}
func placeholder(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "todo" || s == "tbd" || s == "changeme" || (strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}")) || (strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}"))
}
func valueMatches(kind string, v any) bool {
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch n := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case json.Number:
			_, e := strconv.ParseInt(string(n), 10, 64)
			return e == nil
		}
	case "number":
		switch v.(type) {
		case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
			return true
		}
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	}
	return false
}
