// Package structurizr reads a Structurizr workspace, as a DSL file or as a
// JSON document, into one neutral model of elements and relationships.
//
// It reads only what describes the model: people, software systems,
// containers, components, groups, relationships, properties and tags. Views,
// styles, deployment environments, documentation and decision directives are
// skipped. Any other DSL statement stops the read at its line: a statement the
// reader does not understand could declare an element or a relationship, and
// silently dropping one would change the architecture the caller imports.
//
// The package uses the standard library only (ADR 0005 of @putnami/sdd).
package structurizr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Kind is the C4 level of an element.
type Kind string

const (
	KindPerson         Kind = "person"
	KindSoftwareSystem Kind = "softwareSystem"
	KindContainer      Kind = "container"
	KindComponent      Kind = "component"
	// KindOther is a custom element, which has no C4 level.
	KindOther Kind = "element"
)

// Element is one model element.
type Element struct {
	// Ref is how the source names the element: the DSL identifier (qualified by
	// its parents) or the JSON id. Empty for an unassigned DSL element.
	Ref         string
	Kind        Kind
	Name        string
	Description string
	Technology  string
	Tags        []string
	Properties  map[string]string
	Parent      *Element
	Children    []*Element
	// Line is the DSL line that declares the element, 0 for JSON.
	Line int
}

// Property returns the value of a property, or "".
func (e *Element) Property(name string) string {
	if e == nil || e.Properties == nil {
		return ""
	}
	return strings.TrimSpace(e.Properties[name])
}

// Path is the element's names from its outermost parent, joined by "/", so a
// report can name an element unambiguously.
func (e *Element) Path() string {
	if e == nil {
		return ""
	}
	if e.Parent == nil {
		return e.Name
	}
	return e.Parent.Path() + "/" + e.Name
}

// Relationship is one directed relationship between two elements.
type Relationship struct {
	Source      *Element
	Destination *Element
	Description string
	Technology  string
	Line        int
}

// Workspace is the model of one Structurizr workspace.
type Workspace struct {
	Name string
	// Elements lists every element in declaration order, nested ones included.
	Elements      []*Element
	Relationships []Relationship
}

// Read reads a workspace file: a .json file is a JSON workspace, anything
// else is read as DSL.
func Read(path string) (*Workspace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return ParseJSON(data, filepath.Base(path))
	}
	return ParseDSL(data, filepath.Base(path))
}

// Error is a read failure at a source position.
type Error struct {
	File    string
	Line    int
	Message string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.File, e.Message)
}

func splitTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return tags
}
