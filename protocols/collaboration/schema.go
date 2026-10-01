package collaboration

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

// schemaFiles are the published JSON Schemas: the common envelope document and
// one file per contract version. They are the single source of every
// operation's agent-facing input schema.
//
//go:embed schemas/*.json
var schemaFiles embed.FS

// CommonSchemaFile is the schema of the Envelope and the shared definitions.
const CommonSchemaFile = "collaboration.json"

// ContractSchemaFile names the schema file of one contract version.
func ContractSchemaFile(contract string, version int) string {
	return fmt.Sprintf("%s.v%d.json", contract, version)
}

// SchemaFile returns the bytes of one published schema file.
func SchemaFile(name string) ([]byte, error) {
	return schemaFiles.ReadFile("schemas/" + name)
}

// InputSchema returns the self-contained JSON Schema of one operation's
// request document, with every reference inlined, so an agent client that
// resolves no $ref still reads the complete shape. OperationCapabilities has
// the empty-object schema in every contract.
func InputSchema(contract string, version int, operation string) (json.RawMessage, error) {
	common, err := loadSchema(CommonSchemaFile)
	if err != nil {
		return nil, err
	}
	if operation == OperationCapabilities {
		return inlineDefinition(common, common, "capabilitiesInput")
	}
	if _, diags := lookupOperation(contract, version, operation); diags != nil {
		return nil, fmt.Errorf("%s", formatDiagnostics(diags))
	}
	document, err := loadSchema(ContractSchemaFile(contract, version))
	if err != nil {
		return nil, err
	}
	return inlineDefinition(document, common, operation+"Input")
}

type schemaDocument map[string]any

func loadSchema(name string) (schemaDocument, error) {
	data, err := SchemaFile(name)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: %w", name, err)
	}
	var document schemaDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", name, err)
	}
	return document, nil
}

func inlineDefinition(document, common schemaDocument, name string) (json.RawMessage, error) {
	definition, err := definitionOf(document, name)
	if err != nil {
		return nil, err
	}
	resolved, err := inlineRefs(definition, document, common, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resolved)
}

func definitionOf(document schemaDocument, name string) (any, error) {
	defs, _ := document["$defs"].(map[string]any)
	definition, ok := defs[name]
	if !ok {
		return nil, fmt.Errorf("schema definition %q is missing", name)
	}
	return definition, nil
}

// inlineRefs replaces every $ref with the definition it names. A reference is
// local ("#/$defs/x", resolved in the document it appears in) or points into
// the common schema ("collaboration.json#/$defs/x"). Sibling keywords of a
// $ref (a description) are kept beside the inlined definition.
func inlineRefs(node any, document, common schemaDocument, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("schema references nest deeper than 32 levels")
	}
	switch typed := node.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok {
			target, owner, err := resolveRef(ref, document, common)
			if err != nil {
				return nil, err
			}
			inlined, err := inlineRefs(target, owner, common, depth+1)
			if err != nil {
				return nil, err
			}
			merged, ok := inlined.(map[string]any)
			if !ok {
				return inlined, nil
			}
			out := make(map[string]any, len(merged)+len(typed))
			for key, value := range merged {
				out[key] = value
			}
			for key, value := range typed {
				if key != "$ref" {
					out[key] = value
				}
			}
			return out, nil
		}
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			if key == "$defs" || key == "$schema" || key == "$id" {
				continue
			}
			inlined, err := inlineRefs(value, document, common, depth)
			if err != nil {
				return nil, err
			}
			out[key] = inlined
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, value := range typed {
			inlined, err := inlineRefs(value, document, common, depth)
			if err != nil {
				return nil, err
			}
			out[i] = inlined
		}
		return out, nil
	default:
		return node, nil
	}
}

func resolveRef(ref string, document, common schemaDocument) (any, schemaDocument, error) {
	file, fragment, _ := strings.Cut(ref, "#")
	owner := document
	switch file {
	case "":
	case CommonSchemaFile:
		owner = common
	default:
		return nil, nil, fmt.Errorf("schema reference %q leaves the collaboration schemas", ref)
	}
	name, ok := strings.CutPrefix(fragment, "/$defs/")
	if !ok {
		return nil, nil, fmt.Errorf("schema reference %q is not a definition", ref)
	}
	definition, err := definitionOf(owner, name)
	if err != nil {
		return nil, nil, err
	}
	return definition, owner, nil
}
