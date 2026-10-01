package api

import (
	"fmt"
	"strconv"
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// newClientGen builds a clientGen seeded with the imports every generated client
// always needs (context + the client transport). Method emission adds the rest
// based on what each body references.
func newClientGen(opts ClientGenOptions, clientName string) *clientGen {
	return &clientGen{
		opts:        opts,
		clientName:  clientName,
		structDefs:  map[string]string{},
		structOrder: []string{},
		imports:     map[string]bool{"context": true, "go.putnami.dev/client": true},
	}
}

// uniqueStructName returns base if no struct is declared under it yet, otherwise
// base with the smallest numeric suffix (base2, base3, …) that is free. Generated
// per-operation helper structs pass their intended name through this so a shared
// model that happens to own that name wins the base name and the helper takes a
// suffixed one — declareStruct/declareInputStruct are first-writer-wins, so
// without this the second declaration would silently vanish and the method would
// reference fields on the surviving (wrong) struct. Deterministic given the
// deterministic model/operation traversal order.
func (g *clientGen) uniqueStructName(base string) string {
	if _, taken := g.structDefs[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		candidate := base + strconv.Itoa(i)
		if _, taken := g.structDefs[candidate]; !taken {
			return candidate
		}
	}
}

// GenerateClientFromIR emits a typed Go client from the OpenAPI-derived [SpecIR]
// produced by [ReadOpenAPISpec]. Emission uses nested Params/Query/Body input
// sections, a method-name collision guard, conditional imports, and response
// decoding to produce a structurally consistent client for every operation.
//
// Shared models lifted into [SpecIR.NamedTypes] are emitted once as top-level
// structs and referenced by name from method bodies, responses, and other
// models, matching the OpenAPI components/$ref structure.
//
// All services in the spec are flattened onto a single client (one method per
// operation), preserving the flat Go client surface. The Go method name is
// derived from the HTTP method + path (not the operationId) so it stays
// aligned with the proto/gRPC bridge.
func GenerateClientFromIR(spec SpecIR, opts ClientGenOptions) (string, error) {
	if opts.PackageName == "" {
		return "", errors.Newf(CodeClientGenConfig, "api: ClientGenOptions.PackageName is required")
	}
	if spec.Contract != nil {
		return generateStrictClient(spec, opts)
	}
	spec, omitted, err := omitOperations(spec, opts.OmitOperations)
	if err != nil {
		return "", err
	}
	clientName := opts.ClientName
	if clientName == "" {
		clientName = "Client"
	}

	if opts.Design != nil && opts.Design.SpecHash == "" {
		design := *opts.Design
		design.SpecHash = spec.SpecHash
		opts.Design = &design
	}
	g := newClientGen(opts, clientName)
	g.omitted = omitted

	// Contract-derived closed enums and tagged unions must be registered before
	// object models so fields can reference their canonical names.
	for _, name := range sortedKeys(spec.Enums) {
		g.declareEnum(name, spec.Enums[name])
	}
	for _, name := range sortedKeys(spec.Unions) {
		g.declareUnion(name, spec.Unions[name])
	}

	// Emit shared models first so they're declared before any method references
	// them. Sorted for deterministic output.
	for _, name := range sortedKeys(spec.NamedTypes) {
		g.declareStruct(name, fieldEntriesFromIR(spec.NamedTypes[name]))
	}

	// clientcontract.RPCName is not injective — distinct operations can derive
	// the same Go method name. Guard against it so the generator never emits
	// two methods with the same receiver name.
	seen := make(map[string]MethodIR)

	var methods []methodEmit
	for _, svc := range spec.Services {
		for _, op := range svc.Methods {
			if !isClientHTTPMethod(op.HTTPMethod) {
				continue
			}
			name := clientcontract.RPCName(op.HTTPMethod, op.Path)
			if prev, ok := seen[name]; ok {
				return "", errors.Newf(CodeClientGenCollision,
					"api: operations %s %s and %s %s both derive client method %q; rename one route path so they produce distinct method names",
					prev.HTTPMethod, prev.Path, op.HTTPMethod, op.Path, name)
			}
			seen[name] = op

			paramFields := fieldEntriesFromIR(op.Params)
			queryFields := fieldEntriesFromIR(op.Query)
			hasParams := len(op.Params) > 0
			hasQuery := len(op.Query) > 0

			// Generated per-operation helper structs (Params/Query/Body/Input/Output)
			// are named "<method><suffix>". A promoted shared model can legitimately
			// carry that exact name (e.g. a struct named CreateItemsInput), and shared
			// models are declared first — so uniqueStructName suffixes a helper whose
			// name is already taken rather than letting declareStruct silently no-op,
			// which would leave the method referencing fields on the wrong struct.
			var sections []sectionEntry
			if hasParams {
				paramsName := g.uniqueStructName(name + "Params")
				g.declareStruct(paramsName, paramFields)
				sections = append(sections, sectionEntry{FieldName: "Params", JSONName: "params", TypeName: paramsName})
			}
			if hasQuery {
				queryName := g.uniqueStructName(name + "Query")
				g.declareStruct(queryName, queryFields)
				sections = append(sections, sectionEntry{FieldName: "Query", JSONName: "query", TypeName: queryName})
			}

			// Body: a $ref body references a shared model directly; an inline body
			// becomes a per-operation struct.
			hasBody := false
			switch {
			case op.BodyType != "":
				hasBody = true
				sections = append(sections, sectionEntry{FieldName: "Body", JSONName: "body", TypeName: op.BodyType})
			case len(op.Body) > 0:
				hasBody = true
				bodyName := g.uniqueStructName(name + "Body")
				g.declareStruct(bodyName, fieldEntriesFromIR(op.Body))
				sections = append(sections, sectionEntry{FieldName: "Body", JSONName: "body", TypeName: bodyName})
			}
			// Allocated last among the input helpers so it clears the Params/Query/Body
			// structs just declared above as well as the shared models.
			inputName := g.uniqueStructName(name + "Input")
			g.declareInputStruct(inputName, sections)

			// Output: a $ref response references a shared model; an inline response
			// becomes a per-operation struct; no body → void return.
			outputRef := ""
			switch {
			case op.ResponseType != "":
				outputRef = op.ResponseType
			case len(op.Response) > 0:
				outName := g.uniqueStructName(name + "Output")
				g.declareStruct(outName, fieldEntriesFromIR(op.Response))
				outputRef = outName
			}

			methods = append(methods, methodEmit{
				ClientName:  clientName,
				Name:        name,
				InputName:   inputName,
				OutputRef:   outputRef,
				HTTPMethod:  op.HTTPMethod,
				Path:        op.Path,
				OperationID: op.OperationID,
				HasParams:   hasParams,
				HasQuery:    hasQuery,
				HasBody:     hasBody,
				Params:      paramFields,
				Query:       queryFields,
			})
		}
	}

	return renderClient(g, opts, clientName, methods)
}

func (g *clientGen) declareRawType(name, source string) {
	if _, exists := g.structDefs[name]; exists {
		return
	}
	g.structDefs[name] = source
	g.structOrder = append(g.structOrder, name)
}

func (g *clientGen) declareEnum(name string, values []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s is a closed value set projected from the service contract.\n", name)
	fmt.Fprintf(&b, "type %s string\n\nconst (\n", name)
	used := map[string]bool{}
	for i, value := range values {
		member := name + exportedFieldName(value)
		if member == name+"Field" || used[member] {
			member = name + "Value" + strconv.Itoa(i+1)
		}
		used[member] = true
		fmt.Fprintf(&b, "\t%s %s = %q\n", member, name, value)
	}
	b.WriteString(")\n")
	g.declareRawType(name, b.String())
}

// declareUnion emits a JSON-friendly tagged union: the discriminator is a
// closed typed string and the union struct contains the union of variant fields.
// Variant-only fields are optional, so encoding/json can marshal and unmarshal
// the flattened wire shape without custom runtime code.
func (g *clientGen) declareUnion(name string, union UnionIR) {
	kindName := name + exportedFieldName(union.Discriminator)
	if union.Discriminator == "" {
		kindName = name + "Kind"
	}
	kindName = g.uniqueStructName(kindName)
	var kind strings.Builder
	fmt.Fprintf(&kind, "// %s selects a %s variant.\n", kindName, name)
	fmt.Fprintf(&kind, "type %s string\n\nconst (\n", kindName)
	usedTags := map[string]bool{}
	for i, variant := range union.Variants {
		member := kindName + exportedFieldName(variant.Tag)
		if member == kindName+"Field" || usedTags[member] {
			member = kindName + "Value" + strconv.Itoa(i+1)
		}
		usedTags[member] = true
		fmt.Fprintf(&kind, "\t%s %s = %q\n", member, kindName, variant.Tag)
	}
	kind.WriteString(")\n")
	g.declareRawType(kindName, kind.String())

	var b strings.Builder
	fmt.Fprintf(&b, "// %s is a tagged union projected from the service contract.\n", name)
	fmt.Fprintf(&b, "type %s struct {\n", name)
	discName := union.Discriminator
	if discName == "" {
		discName = "kind"
	}
	fmt.Fprintf(&b, "\t%s %s `json:%q`\n", exportedFieldName(discName), kindName, discName)
	fields := map[string]FieldIR{}
	order := []string{}
	for _, variant := range union.Variants {
		for _, field := range variant.Fields {
			if existing, ok := fields[field.Name]; ok {
				if existing.GoType != field.GoType || existing.Array != field.Array {
					existing.GoType, existing.Array = "any", false
				}
				existing.Optional = true
				fields[field.Name] = existing
				continue
			}
			field.Optional = true
			fields[field.Name] = field
			order = append(order, field.Name)
		}
	}
	for _, fieldName := range order {
		field := fields[fieldName]
		fmt.Fprintf(&b, "\t%s %s `json:%q`\n", exportedFieldName(field.Name), goTypeFromIR(field), field.Name+",omitempty")
	}
	b.WriteString("}\n")
	g.declareRawType(name, b.String())
}

// fieldEntriesFromIR converts IR fields into the emitter's fieldEntry shape: the
// wire name becomes the JSON tag, an exported PascalCase identifier becomes the
// Go field name, and the Go type token (with array wrapping) becomes the type.
func fieldEntriesFromIR(fields []FieldIR) []fieldEntry {
	out := make([]fieldEntry, 0, len(fields))
	for _, f := range fields {
		out = append(out, fieldEntry{
			JSONName:    f.Name,
			GoFieldName: exportedFieldName(f.Name),
			GoTypeName:  goTypeFromIR(f),
			Optional:    f.Optional,
		})
	}
	return out
}

// goTypeFromIR renders an IR field's Go source type, wrapping arrays in a slice.
func goTypeFromIR(f FieldIR) string {
	if f.Array {
		return "[]" + f.GoType
	}
	if f.Optional && shouldPointerOptionalIRType(f.GoType) {
		return "*" + f.GoType
	}
	return f.GoType
}

func shouldPointerOptionalIRType(goType string) bool {
	return goType != "any" &&
		!strings.HasPrefix(goType, "map[") &&
		!strings.HasPrefix(goType, "[]") &&
		!strings.HasPrefix(goType, "*")
}

// exportedFieldName turns a wire field name (e.g. "user_id", "page") into an
// exported Go identifier ("UserId", "Page"). The original wire name is preserved
// in the JSON tag, so casing differences never affect serialization.
func exportedFieldName(name string) string {
	if pascal := clientcontract.UpperCamel(name); pascal != "" {
		return pascal
	}
	return "Field"
}
