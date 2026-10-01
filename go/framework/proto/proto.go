// Package proto generates Protocol Buffer definitions from api.Plugin route metadata.
//
// Each endpoint produces an RPC on a single service; request/response types are derived
// from the endpoint's params/query/body/returns reflect.Types. The render is deterministic
// (messages emitted in registration order; fields in struct field order) so it's safe to
// commit the output and detect drift via diff.
package proto

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"go.putnami.dev/api"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	validation "go.putnami.dev/schema"
)

// Options configure proto document generation.
type Options struct {
	// PackageName is the proto `package` (e.g. "myapp.v1"). Defaults to "api.v1".
	PackageName string
	// GoPackage is the optional `option go_package = "..."` value.
	GoPackage string
}

// Document holds the rendered proto content and the discovered service shape.
type Document struct {
	// Content is the rendered .proto source.
	Content string
	// PackageName is the package declared in the document.
	PackageName string
	// Imports lists the proto files imported by the document (e.g. well-known
	// types), sorted for deterministic output.
	Imports []string
	// Service is the single service emitted from the discovered routes.
	Service Service
	// Messages is the ordered list of message definitions referenced by the service.
	Messages []Message
	// GenerationErr records a declaration proto3 cannot carry faithfully. The
	// document is not rendered when it is set: a .proto that silently drops a
	// declared semantic is worse than no .proto at all, because every Connect
	// client generated from it would encode a shape the provider never declared.
	GenerationErr error
}

// Service holds an emitted gRPC service.
type Service struct {
	Name string
	RPCs []RPC
}

// RPC describes a single request/response method in the service.
type RPC struct {
	Name            string
	Description     string
	Method          string
	Path            string
	RequestType     string
	ReplyType       string
	ClientStreaming bool
	ServerStreaming bool
}

// Message describes an emitted protobuf message.
type Message struct {
	Name string
	// Fields are in ascending field-number order.
	Fields []Field
	// OneOfs names the declared oneof groups, in declaration order.
	OneOfs []string
}

// Field is a single field on a message.
type Field struct {
	Name string
	// Type is the scalar name, the referenced message or enum name, or the
	// literal "map" when Kind is FieldKindMap.
	Type   string
	Number int
	// Kind classifies Type so a reader never has to guess whether a name is a
	// scalar, a message, an enum, or a map.
	Kind     FieldKind
	Repeated bool
	// Optional records explicit proto3 presence. It is set exactly when the
	// published JSON schema lets the field be absent or null, so the descriptor
	// and the schema agree on what "no value" means on both transports.
	Optional bool
	// OneOf names the containing oneof group, empty for a plain field.
	OneOf string
	// Map carries the exact key and value shape of a map field.
	Map *MapType
}

// FieldKind classifies what a [Field.Type] name refers to.
type FieldKind string

// The closed set of field kinds a first-party descriptor carries.
const (
	FieldKindScalar  FieldKind = "scalar"
	FieldKindMessage FieldKind = "message"
	FieldKindEnum    FieldKind = "enum"
	FieldKindMap     FieldKind = "map"
)

// MapType is the exact key and value shape of a proto3 map field.
type MapType struct {
	KeyType   string
	ValueKind FieldKind
	ValueType string
}

// ClientDescriptor projects the exact emitted proto wire shape into the shared
// first-party client contract. It reads assigned field numbers and streaming
// flags from Document rather than regenerating them from OpenAPI.
func ClientDescriptor(document Document) *clientcontract.ProtobufDescriptor {
	descriptor := &clientcontract.ProtobufDescriptor{
		Syntax:   "proto3",
		Package:  document.PackageName,
		Services: make([]clientcontract.ProtobufService, 1),
		Messages: make([]clientcontract.ProtobufMessage, 0, len(document.Messages)),
		Enums:    []clientcontract.ProtobufEnum{},
	}
	service := clientcontract.ProtobufService{
		Name:    document.Service.Name,
		Methods: make([]clientcontract.ProtobufMethod, 0, len(document.Service.RPCs)),
	}
	for _, rpc := range document.Service.RPCs {
		service.Methods = append(service.Methods, clientcontract.ProtobufMethod{
			Name:            rpc.Name,
			Input:           rpc.RequestType,
			Output:          rpc.ReplyType,
			ClientStreaming: rpc.ClientStreaming,
			ServerStreaming: rpc.ServerStreaming,
		})
	}
	descriptor.Services[0] = service
	for _, message := range document.Messages {
		projected := clientcontract.ProtobufMessage{
			Name:   message.Name,
			Fields: make([]clientcontract.ProtobufField, 0, len(message.Fields)),
		}
		projected.OneOfs = append([]string(nil), message.OneOfs...)
		for _, field := range message.Fields {
			projected.Fields = append(projected.Fields, clientcontract.ProtobufField{
				Name:     field.Name,
				JSONName: protobufJSONName(field.Name),
				Number:   field.Number,
				TypeKind: string(field.Kind),
				Type:     field.Type,
				Repeated: field.Repeated,
				Optional: field.Optional,
				OneOf:    field.OneOf,
				Map:      protobufMap(field.Map),
			})
		}
		descriptor.Messages = append(descriptor.Messages, projected)
	}
	return descriptor
}

func protobufMap(value *MapType) *clientcontract.ProtobufMap {
	if value == nil {
		return nil
	}
	return &clientcontract.ProtobufMap{
		KeyType:   value.KeyType,
		ValueKind: string(value.ValueKind),
		ValueType: value.ValueType,
	}
}

func protobufJSONName(name string) string {
	var out strings.Builder
	upper := false
	for _, char := range name {
		if char == '_' {
			upper = true
			continue
		}
		if upper && char >= 'a' && char <= 'z' {
			char -= 'a' - 'A'
		}
		upper = false
		out.WriteRune(char)
	}
	return out.String()
}

// Generate renders proto content from the given DiscoveredRoutes.
//
// Each route maps to one RPC. Request messages are synthesized by merging the route's
// path params, query params, and request body fields. Reply messages come from the route's
// Returns schema; void responses produce a generated empty message.
func Generate(routes []api.DiscoveredRoute, opts Options) Document {
	pkg := opts.PackageName
	if pkg == "" {
		pkg = "api.v1"
	}

	gen := &generator{
		pkg:       pkg,
		goPackage: opts.GoPackage,
		messages:  map[string]Message{},
		ordered:   []string{},
		imports:   map[string]struct{}{},
		typeNames: map[string]string{},
	}

	service := Service{Name: "ApiService"}
	usedRPCNames := map[string]bool{}
	for _, r := range routes {
		if isStreamMethod(r.Method) {
			continue
		}
		// Document-only routes are intentionally not bridged to Connect-style
		// RPC URLs (the gRPC bridge skips them too — see grpc/api_bridge.go),
		// so a generated proto RPC would name an unreachable endpoint. The
		// underlying REST URL is still exposed via OpenAPI and the typed REST
		// client; only the proto/Connect surface skips them.
		if r.DocumentOnly {
			continue
		}
		// A route an external authority owns (api.ClientOperationOptions
		// External) speaks the standard's wire format, not a proto3 message:
		// it has no RPC, so neither the descriptor nor the bridge offers it.
		if r.Meta.ClientOptions.IsExternal() {
			continue
		}
		// A route that carries opaque JSON has no lossless proto3 form:
		// google.protobuf.Value holds every number as a double. It keeps its
		// REST, SSE and WebSocket transports and is left out of the descriptor,
		// so neither the bridge nor the contract offers a Connect wire for it —
		// the rule a raw octet payload follows (clientcontract ADR 0008).
		if routeCarriesOpaqueJSON(r) {
			continue
		}
		rpcName := uniqueRPCName(clientcontract.RPCName(r.Method, r.Path), r.Path, usedRPCNames)
		req := gen.requestMessage(rpcName, r)
		reply := gen.replyMessage(rpcName, r)
		service.RPCs = append(service.RPCs, RPC{
			Name:            rpcName,
			Description:     r.Description,
			Method:          r.Method,
			Path:            r.Path,
			RequestType:     req,
			ReplyType:       reply,
			ClientStreaming: isClientStream(r.StreamMode),
			ServerStreaming: isServerStream(r.StreamMode),
		})
	}

	doc := Document{
		PackageName:   pkg,
		Imports:       gen.sortedImports(),
		Service:       service,
		Messages:      gen.orderedMessages(),
		GenerationErr: gen.generationErr,
	}
	if doc.GenerationErr != nil {
		return doc
	}
	doc.Content = render(doc, opts.GoPackage)
	return doc
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// routeCarriesOpaqueJSON reports whether any declared section of a route
// reaches a json.RawMessage or an empty interface, at any depth.
func routeCarriesOpaqueJSON(r api.DiscoveredRoute) bool {
	seen := map[reflect.Type]bool{}
	for _, t := range []reflect.Type{r.ParamsSchema, r.QuerySchema, r.BodySchema, r.ReturnsSchema} {
		if carriesOpaqueJSON(t, seen) {
			return true
		}
	}
	return false
}

// carriesOpaqueJSON walks t with the same field selector the JSON schema uses.
// A type already seen is either in progress (a cycle, which adds nothing) or
// finished without finding one, so it is never walked twice.
func carriesOpaqueJSON(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == nil {
		return false
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType || (t.Kind() == reflect.Interface && t.NumMethod() == 0) {
		return true
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return !isByteSequence(t) && carriesOpaqueJSON(t.Elem(), seen)
	case reflect.Struct:
		for _, selected := range validation.JSONFields(t) {
			if carriesOpaqueJSON(selected.Field.Type, seen) {
				return true
			}
		}
	}
	return false
}

// RouteMethods maps each bridged route to the canonical protobuf method identity
// the descriptor declares for it, keyed by "<METHOD> <path>". It is the join a
// Connect transport needs: without it the contract would have to recompute the
// RPC name a third time and could disagree with both the .proto document and the
// URL the bridge serves.
func (d Document) RouteMethods() map[string]string {
	methods := make(map[string]string, len(d.Service.RPCs))
	for _, rpc := range d.Service.RPCs {
		methods[RouteKey(rpc.Method, rpc.Path)] = "/" + d.PackageName + "." + d.Service.Name + "/" + rpc.Name
	}
	return methods
}

// RouteKey is the shared route identity used by [Document.RouteMethods].
func RouteKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// generator walks routes and accumulates messages, guaranteeing each unique message is
// emitted once (keyed by message name).
type generator struct {
	pkg       string
	goPackage string
	messages  map[string]Message
	ordered   []string
	imports   map[string]struct{}
	// generationErr holds the first declaration proto3 cannot carry faithfully.
	// Generation continues so the error names one cause rather than the last one.
	generationErr error
	// typeNames maps a Go type's fully-qualified identity (pkgpath.Name, or the
	// structural name for anonymous structs) to the proto message name emitted
	// for it. Keying by identity — not the simple name — keeps two distinct
	// types that share a simple name (pkga.User vs pkgb.User) from clobbering
	// each other in the message registry, and lets the simple-name collision be
	// disambiguated into distinct proto messages.
	typeNames map[string]string
}

// requestMessage emits one or three messages: a top-level <rpc>Request whose fields
// reference per-section sub-messages (Params / Query / Body). Sections are nested so a
// path-param "id" and a body-field "id" never produce duplicate fields on the wire.
//
// Empty sections are omitted from the request message; when no sections are declared at
// all, the request message is generated empty (callers can still reference it).
func (g *generator) requestMessage(rpcName string, r api.DiscoveredRoute) string {
	name := rpcName + "Request"
	var fields []Field
	num := 1

	// Each section field references a sub-message, so its kind is stated like any
	// other. Leaving it empty published a descriptor the strict reader refuses
	// with `protobuf typeKind "" is unsupported` — the request message of every
	// route that declares params, query or a body.
	if section := g.requestSection(rpcName+"Params", r.ParamsSchema); section != "" {
		fields = append(fields, Field{Name: "params", Type: section, Kind: FieldKindMessage, Number: num})
		num++
	}
	if section := g.requestSection(rpcName+"Query", r.QuerySchema); section != "" {
		fields = append(fields, Field{Name: "query", Type: section, Kind: FieldKindMessage, Number: num})
		num++
	}
	if section := g.requestSection(rpcName+"Body", r.BodySchema); section != "" {
		fields = append(fields, Field{Name: "body", Type: section, Kind: FieldKindMessage, Number: num})
	}

	g.add(Message{Name: name, Fields: fields})
	return name
}

// requestSection registers a sub-message for a request section (Params, Query, Body)
// and returns the message name. Returns "" only when the section is undeclared.
//
// A non-struct root — a []Item body, a map body, a scalar body — is wrapped in a
// single "value" field exactly like a non-struct reply. Dropping the section
// instead (the previous behavior) published a Connect method whose request
// message silently lost the entire declared body, so a generated client sent an
// empty request and the endpoint's validation pipeline rejected every call.
func (g *generator) requestSection(name string, t reflect.Type) string {
	if t == nil {
		return ""
	}
	root := derefType(t)
	if root.Kind() == reflect.Struct {
		var fields []Field
		g.appendStructFields(&fields, root, 1)
		g.add(Message{Name: name, Fields: fields})
		return name
	}
	g.add(Message{Name: name, Fields: []Field{g.rootValueField(name, t, 1)}})
	return name
}

// rootValueField wraps a non-struct root type in the single "value" field a
// proto message needs. Presence follows the declaration: a pointer root can be
// absent, so the wrapper field carries explicit presence.
func (g *generator) rootValueField(owner string, t reflect.Type, number int) Field {
	optional := false
	for t.Kind() == reflect.Pointer {
		optional = true
		t = t.Elem()
	}
	repeated := false
	if isByteSequence(t) {
		return Field{Name: "value", Type: "bytes", Kind: FieldKindScalar, Number: number, Optional: optional}
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		repeated = true
		t = derefType(t.Elem())
	}
	resolved := g.protoType(owner+".value", t)
	field := Field{
		Name:     "value",
		Type:     resolved.Name,
		Kind:     resolved.Kind,
		Number:   number,
		Repeated: repeated,
		Map:      resolved.Map,
	}
	if repeated && resolved.Kind == FieldKindMap {
		g.refuse("proto: %s declares a repeated map; proto3 has no repeated map field", owner)
		return field
	}
	if !repeated && resolved.Kind != FieldKindMap {
		field.Optional = optional
	}
	return field
}

func (g *generator) replyMessage(rpcName string, r api.DiscoveredRoute) string {
	name := rpcName + "Reply"
	if r.ReturnsSchema == nil {
		g.add(Message{Name: name})
		return name
	}
	if root := derefType(r.ReturnsSchema); root.Kind() != reflect.Struct || isByteSequence(root) {
		// A non-struct return (a []User list, a map, a scalar, a []byte payload)
		// is wrapped in a single "value" field, exactly like a non-struct request
		// section, so both directions of an RPC describe the same shape the JSON
		// schema does.
		g.add(Message{Name: name, Fields: []Field{g.rootValueField(name, r.ReturnsSchema, 1)}})
		return name
	}
	var fields []Field
	g.appendStructFields(&fields, derefType(r.ReturnsSchema), 1)
	g.add(Message{Name: name, Fields: fields})
	return name
}

// appendStructFields walks t with the SAME field selector the JSON schema and
// the body validator use (validation.JSONFields), so an embedded struct is
// flattened in the descriptor exactly as encoding/json flattens it on the wire.
// The previous NumField loop nested promoted fields, which made the descriptor
// describe a message shape no JSON payload ever had.
func (g *generator) appendStructFields(out *[]Field, t reflect.Type, start int) {
	if t == nil {
		return
	}
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return
	}
	num := start
	for _, selected := range validation.JSONFields(t) {
		*out = append(*out, g.fieldFromStructField(selected, num))
		num++
	}
}

// fieldFromStructField projects one selected JSON field. Presence is declared,
// never inferred from the transport: a field the schema lets be absent (no
// `validate:"required"`) or null (a pointer) carries explicit proto3 presence,
// so "no value" means the same thing on REST JSON and on Connect.
func (g *generator) fieldFromStructField(selected validation.JSONField, num int) Field {
	name := sanitizeIdent(selected.Name)
	structField := selected.Field
	nullable := false
	t := structField.Type
	for t.Kind() == reflect.Pointer {
		nullable = true
		t = t.Elem()
	}
	absent := !hasValidateConstraint(structField, "required")

	if isByteSequence(t) {
		// []byte is base64 JSON text, not a list of numbers: proto3 `bytes`
		// carries exactly the same value.
		return Field{Name: name, Type: "bytes", Kind: FieldKindScalar, Number: num, Optional: nullable || absent}
	}
	repeated := false
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		repeated = true
		t = derefType(t.Elem())
	}
	resolved := g.protoType(name, t)
	field := Field{
		Name:     name,
		Type:     resolved.Name,
		Kind:     resolved.Kind,
		Number:   num,
		Repeated: repeated,
		Map:      resolved.Map,
	}
	if repeated && resolved.Kind == FieldKindMap {
		g.refuse("proto: field %q declares a repeated map; proto3 has no repeated map field", name)
		return field
	}
	if repeated || resolved.Kind == FieldKindMap {
		// A repeated field and a map are already absent-as-empty on both wires;
		// proto3 forbids marking either optional.
		return field
	}
	field.Optional = nullable || absent
	return field
}

// hasValidateConstraint reports whether a struct field declares one exact
// validate constraint. It matches the tokenizer the OpenAPI projection uses so
// the descriptor and the schema read the same declaration.
func hasValidateConstraint(field reflect.StructField, constraint string) bool {
	tag := field.Tag.Get("validate")
	if tag == "" {
		return false
	}
	for _, part := range strings.Split(tag, ",") {
		if strings.TrimSpace(part) == constraint {
			return true
		}
	}
	return false
}

// isByteSequence reports whether t is the Go form of a proto3 `bytes` value.
func isByteSequence(t reflect.Type) bool {
	return (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && t.Elem().Kind() == reflect.Uint8
}

// protoType is the resolved proto3 shape of one Go type: the name a field
// references, what that name is, and — for a map — its exact key and value
// shape. owner names the declaration under repair in a refusal message.
type protoType struct {
	Name string
	Kind FieldKind
	Map  *MapType
}

// protoType resolves a Go type to its proto3 shape. Integer widths follow the
// declaration, never the transport: Go `int` and `uint` are 64-bit values in the
// published JSON schema, so they are int64 and uint64 here too. Narrowing them
// to 32 bits was a transport side effect that truncated every identifier and
// cursor above 2^31 the REST transport carried intact.
func (g *generator) protoType(owner string, t reflect.Type) protoType {
	t = derefType(t)
	// Known opaque types (time.Time, time.Duration) map to well-known proto types
	// rather than recursing into their unexported fields.
	if wk, ok := wellKnownType(t); ok {
		g.imports[wk.protoImport] = struct{}{}
		return protoType{Name: wk.protoType, Kind: FieldKindMessage}
	}
	if isByteSequence(t) {
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	}
	switch t.Kind() {
	case reflect.String:
		return protoType{Name: "string", Kind: FieldKindScalar}
	case reflect.Bool:
		return protoType{Name: "bool", Kind: FieldKindScalar}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return protoType{Name: "int32", Kind: FieldKindScalar}
	case reflect.Int, reflect.Int64:
		return protoType{Name: "int64", Kind: FieldKindScalar}
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return protoType{Name: "uint32", Kind: FieldKindScalar}
	case reflect.Uint, reflect.Uint64:
		return protoType{Name: "uint64", Kind: FieldKindScalar}
	case reflect.Float32:
		return protoType{Name: "float", Kind: FieldKindScalar}
	case reflect.Float64:
		return protoType{Name: "double", Kind: FieldKindScalar}
	case reflect.Map:
		return g.mapType(owner, t)
	case reflect.Slice, reflect.Array:
		// A nested list has no proto3 form: `repeated repeated T` does not exist.
		g.refuse("proto: %s declares a nested list; proto3 has no repeated repeated field, so wrap the inner list in a message", owner)
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	case reflect.Struct:
		// Resolve the type by its fully-qualified identity, not its simple name,
		// so distinct same-simple-name types each get their own message. The
		// emitted proto name is disambiguated with a numeric suffix on collision,
		// mirroring uniqueRPCName.
		identity := typeIdentity(t)
		if name, ok := g.typeNames[identity]; ok {
			return protoType{Name: name, Kind: FieldKindMessage} // already emitted or in progress (also breaks cycles)
		}
		name := g.uniqueMessageName(nameFromType(t))
		g.typeNames[identity] = name
		fields := []Field{}
		g.add(Message{Name: name}) // placeholder to break cycles
		g.appendStructFields(&fields, t, 1)
		m := g.messages[name]
		m.Fields = fields
		g.messages[name] = m
		return protoType{Name: name, Kind: FieldKindMessage}
	default:
		// Channels, interfaces, and other shapes with no proto3 form. Emitting
		// bytes here would publish a field whose content no client can decode.
		g.refuse("proto: %s declares %s, which has no proto3 representation", owner, t.Kind())
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	}
}

// mapType resolves a Go map to a proto3 map field. proto3 restricts map keys to
// integral and string scalars, and the JSON schema restricts them further to
// strings, so a non-string key is refused rather than degraded to an opaque
// value the JSON projection also cannot describe.
func (g *generator) mapType(owner string, t reflect.Type) protoType {
	if t.Key().Kind() != reflect.String {
		g.refuse("proto: %s declares a map keyed by %s; a first-party map key is a string on both transports", owner, t.Key().Kind())
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	}
	value := derefType(t.Elem())
	if value.Kind() == reflect.Map {
		g.refuse("proto: %s declares a map of maps; proto3 map values cannot be maps, so wrap the inner map in a message", owner)
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	}
	if !isByteSequence(value) && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
		g.refuse("proto: %s declares a map of lists; proto3 map values cannot be repeated, so wrap the inner list in a message", owner)
		return protoType{Name: "bytes", Kind: FieldKindScalar}
	}
	resolved := g.protoType(owner+" value", value)
	return protoType{
		Name: "map",
		Kind: FieldKindMap,
		Map:  &MapType{KeyType: "string", ValueKind: resolved.Kind, ValueType: resolved.Name},
	}
}

// refuse records the first declaration proto3 cannot carry faithfully.
func (g *generator) refuse(format string, args ...any) {
	if g.generationErr == nil {
		g.generationErr = fmt.Errorf(format, args...)
	}
}

func (g *generator) add(m Message) {
	if _, ok := g.messages[m.Name]; !ok {
		g.ordered = append(g.ordered, m.Name)
	}
	g.messages[m.Name] = m
}

func (g *generator) orderedMessages() []Message {
	out := make([]Message, 0, len(g.ordered))
	for _, name := range g.ordered {
		out = append(out, g.messages[name])
	}
	return out
}

func (g *generator) sortedImports() []string {
	if len(g.imports) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.imports))
	for imp := range g.imports {
		out = append(out, imp)
	}
	sort.Strings(out)
	return out
}

// wellKnown maps a Go type to a protobuf well-known type and the import its use requires.
type wellKnown struct {
	protoType   string
	protoImport string
}

// wellKnownType returns the protobuf well-known mapping for opaque Go types whose
// internals must not be reflected into messages (e.g. time.Time has only unexported
// fields). Returns ok=false for types without a special mapping.
func wellKnownType(t reflect.Type) (wellKnown, bool) {
	switch t.PkgPath() + "." + t.Name() {
	case "time.Time":
		return wellKnown{protoType: "google.protobuf.Timestamp", protoImport: "google/protobuf/timestamp.proto"}, true
	case "time.Duration":
		return wellKnown{protoType: "google.protobuf.Duration", protoImport: "google/protobuf/duration.proto"}, true
	}
	return wellKnown{}, false
}

func render(doc Document, goPackage string) string {
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n\n")
	fmt.Fprintf(&b, "package %s;\n\n", doc.PackageName)
	for _, imp := range doc.Imports {
		fmt.Fprintf(&b, "import %q;\n", imp)
	}
	if len(doc.Imports) > 0 {
		b.WriteString("\n")
	}
	if goPackage != "" {
		fmt.Fprintf(&b, "option go_package = %q;\n\n", goPackage)
	}

	for _, m := range doc.Messages {
		fmt.Fprintf(&b, "message %s {\n", m.Name)
		renderFields(&b, m)
		b.WriteString("}\n\n")
	}

	if len(doc.Service.RPCs) > 0 {
		fmt.Fprintf(&b, "service %s {\n", doc.Service.Name)
		for _, rpc := range doc.Service.RPCs {
			if rpc.Description != "" {
				// Emit one comment line per source line so a description
				// containing a newline can't break out of its `//` comment and
				// inject a stray token into the service body.
				for _, line := range commentLines(rpc.Description) {
					fmt.Fprintf(&b, "  // %s\n", line)
				}
			}
			req := rpc.RequestType
			if rpc.ClientStreaming {
				req = "stream " + req
			}
			reply := rpc.ReplyType
			if rpc.ServerStreaming {
				reply = "stream " + reply
			}
			fmt.Fprintf(&b, "  rpc %s(%s) returns (%s);\n", rpc.Name, req, reply)
		}
		b.WriteString("}\n")
	}

	return b.String()
}

// renderFields writes a message body: plain fields in field-number order, then
// one block per declared oneof group holding its members. proto3 requires oneof
// members to be written inside their group, so the group is the only place a
// member appears.
func renderFields(b *strings.Builder, m Message) {
	for _, f := range m.Fields {
		if f.OneOf != "" {
			continue
		}
		b.WriteString("  ")
		renderField(b, f)
	}
	for _, group := range m.OneOfs {
		fmt.Fprintf(b, "  oneof %s {\n", group)
		for _, f := range m.Fields {
			if f.OneOf != group {
				continue
			}
			b.WriteString("    ")
			renderField(b, f)
		}
		b.WriteString("  }\n")
	}
}

func renderField(b *strings.Builder, f Field) {
	if f.Kind == FieldKindMap && f.Map != nil {
		fmt.Fprintf(b, "map<%s, %s> %s = %d;\n", f.Map.KeyType, f.Map.ValueType, f.Name, f.Number)
		return
	}
	prefix := ""
	switch {
	case f.Repeated:
		prefix = "repeated "
	case f.Optional:
		prefix = "optional "
	}
	fmt.Fprintf(b, "%s%s %s = %d;\n", prefix, f.Type, f.Name, f.Number)
}

// uniqueRPCName guarantees a collision-free RPC name (and, by extension,
// collision-free request/reply message names, which are derived from it).
//
// Two distinct routes can reduce to the same base name — e.g. POST /users and
// POST /users/{id} both yield CreateUsers because path params are dropped from
// the subject. Without disambiguation the rendered proto would contain duplicate
// `rpc` lines (which protoc rejects) and one route's body message would silently
// overwrite the other. On collision we first fold the path params into the name
// (CreateUsers → CreateUsersById), then fall back to a stable numeric suffix.
func uniqueRPCName(base, path string, used map[string]bool) string {
	if !used[base] {
		used[base] = true
		return base
	}
	if suffix := pathParamSuffix(path); suffix != "" {
		if candidate := base + suffix; !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s%d", base, i)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// pathParamSuffix derives a "By<Param>" suffix from a path's parameters, e.g.
// /users/{id} → "ById" and /orgs/{org}/repos/{repo} → "ByOrgAndRepo". Returns ""
// when the path has no parameters.
func pathParamSuffix(path string) string {
	var names []string
	for _, p := range strings.Split(strings.Trim(path, "/"), "/") {
		if !strings.HasPrefix(p, "{") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "{"), "}")
		name = strings.TrimSuffix(name, "...") // catch-all marker
		if name != "" {
			names = append(names, clientcontract.UpperCamel(name))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "By" + strings.Join(names, "And")
}

func isStreamMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return false
	default:
		return true
	}
}

func isClientStream(mode api.StreamMode) bool {
	return mode == api.StreamModeClient || mode == api.StreamModeBidirectional
}

func isServerStream(mode api.StreamMode) bool {
	return mode == api.StreamModeServer || mode == api.StreamModeBidirectional
}

// typeIdentity returns a key that uniquely identifies a Go type for the message
// registry. Named types use pkgpath.Name so two types sharing a simple name in
// different packages stay distinct; anonymous structs fall back to their
// structural name (nameFromType), so structurally identical anonymous structs
// collapse to one message while differing ones do not.
func typeIdentity(t reflect.Type) string {
	if name := t.Name(); name != "" {
		if pkg := t.PkgPath(); pkg != "" {
			return pkg + "." + name
		}
		return name
	}
	return nameFromType(t)
}

// uniqueMessageName returns base if no message with that name has been emitted
// yet, otherwise base with a numeric suffix (base2, base3, ...) — mirroring
// uniqueRPCName so two distinct types with the same simple name produce two
// distinct proto messages instead of silently clobbering.
func (g *generator) uniqueMessageName(base string) string {
	if _, taken := g.messages[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s%d", base, i)
		if _, taken := g.messages[candidate]; !taken {
			return candidate
		}
	}
}

func nameFromType(t reflect.Type) string {
	if name := t.Name(); name != "" {
		return sanitizeIdent(name)
	}
	// Anonymous struct — synthesize a stable name from its field set.
	var fields []string
	for i := 0; i < t.NumField(); i++ {
		fields = append(fields, t.Field(i).Name)
	}
	sort.Strings(fields)
	return sanitizeIdent("Anon_" + strings.Join(fields, "_"))
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// sanitizeIdent maps an arbitrary string (e.g. a `json` tag or a Go type name)
// onto the proto identifier grammar [A-Za-z_][A-Za-z0-9_]*. Invalid characters
// become '_'; a leading digit is prefixed with '_'. Already-valid identifiers are
// returned unchanged, so this never perturbs existing well-formed output.
func sanitizeIdent(s string) string {
	var b strings.Builder
	first := true
	for _, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if first {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		first = false
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// commentLines splits a description into individual lines (normalizing CRLF/CR)
// so each can be emitted as its own `//` comment.
func commentLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}
