package configcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/protocol/config/authoredmember"
)

// extendAuthoredSchema admits only explicitly authored descendants of opaque
// generated objects. It never infers a schema from values or replaces a field
// (in particular, its sensitivity) already declared by the source generator.
func extendAuthoredSchema(projectDir string, schema authoredmember.NormalizedSchema) (authoredmember.NormalizedSchema, error) {
	path := filepath.Join(projectDir, "schema", "config-authored-fields.json")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return schema, nil
	}
	if err != nil {
		return schema, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return schema, fmt.Errorf("read bounded authored Config field declarations")
	}
	var additional authoredmember.NormalizedSchema
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&additional); err != nil {
		return schema, fmt.Errorf("decode authored Config field declarations: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return schema, fmt.Errorf("authored Config field declarations contain trailing data")
	}
	opaque := map[string]bool{}
	known := map[string]bool{}
	for _, field := range schema.Fields {
		known[field.Path] = true
		if field.Type == "object" && !field.Sensitive {
			opaque[field.Path] = true
		}
	}
	for parent := range opaque {
		for path := range known {
			if strings.HasPrefix(path, parent+".") {
				delete(opaque, parent)
			}
		}
	}
	for _, field := range additional.Fields {
		allowed := false
		for parent := range opaque {
			allowed = allowed || strings.HasPrefix(field.Path, parent+".")
		}
		if !allowed || known[field.Path] {
			return schema, fmt.Errorf("authored Config field must uniquely extend an opaque generated object: %s", field.Path)
		}
		known[field.Path] = true
	}
	schema.Fields = append(append([]authoredmember.SchemaField{}, schema.Fields...), additional.Fields...)
	return schema, nil
}
