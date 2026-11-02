// Package i18n holds the file formats of a localized Putnami web project: the
// translation catalog of one namespace in one locale, and the glossary of one
// locale. @putnami/web reads and writes both; the schemas let an editor, a
// translator's tool or an agent check a file without running Putnami.
package i18n

import _ "embed"

// CatalogSchemaID is where putnami.dev publishes the catalog schema, and the
// `$schema` every catalog file names.
const CatalogSchemaID = "https://putnami.dev/schemas/i18n/catalog.json"

// GlossarySchemaID is where putnami.dev publishes the glossary schema.
const GlossarySchemaID = "https://putnami.dev/schemas/i18n/glossary.json"

//go:embed schemas/catalog.json
var catalogSchema []byte

//go:embed schemas/glossary.json
var glossarySchema []byte

// CatalogSchema returns the JSON Schema of a catalog file.
func CatalogSchema() []byte { return append([]byte(nil), catalogSchema...) }

// GlossarySchema returns the JSON Schema of a glossary file.
func GlossarySchema() []byte { return append([]byte(nil), glossarySchema...) }
