// Package apisurface reads the public API of a TypeScript package from its
// source and tells an incompatible change from a compatible one.
//
// The public API is what the package's entry points export: the modules its
// package.json names in "exports", or in "main", "module" and "types" when it
// has no "exports". Read follows the re-exports of each entry point inside the
// package and keeps every exported declaration as it is written. Nothing is
// type-checked and no dependency is resolved: a surface is a function of the
// file texts alone.
//
// The declarations are parsed by the TypeScript parser the workspace installs,
// in one bun process that reads both sides of a comparison (extract.mjs). The
// package is found on disk, the way an import of the project finds it, and
// bun is told to install nothing.
package apisurface

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed extract.mjs
var extractScript []byte

// readTimeout bounds the bun process that parses the two sides.
const readTimeout = 2 * time.Minute

// ErrNoTypeScript says the TypeScript parser does not resolve from the
// project, so the check cannot read its API.
var ErrNoTypeScript = errors.New("the typescript package is not installed")

// File is one source file of a package, at a slash path relative to the
// project.
type File struct {
	Path string
	Data []byte
}

// Side is one of the two states of a package a check compares: its source
// files and the entry points its package.json declares.
type Side struct {
	Files   []File
	Entries []EntryPoint
	// Imports are the aliases the package.json "imports" declares, such as
	// "#src/*", each with the slash paths it may name (see ImportAliases).
	Imports map[string][]string
}

// Surface is the public API of a package in one state.
type Surface struct {
	// Entries are the entry points, in key order.
	Entries []Entry `json:"entries"`
	// Errors are the syntax errors of the files the entry points reach.
	Errors []SyntaxError `json:"errors"`
}

// SyntaxError is a syntax error in a file an entry point reaches.
type SyntaxError struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// Entry is what one entry point exports.
type Entry struct {
	// Key names the entry point: its subpath, and its conditions when they are
	// not the default one.
	Key string `json:"key"`
	// Subpath is the subpath of the entry point, such as "." or "./hooks".
	Subpath string `json:"-"`
	// Exports are the exported declarations, in name order. A name that
	// declares both a value and a type has one of each.
	Exports []Declaration `json:"exports"`
	// Referenced are the declarations of the package that the written types of
	// the exported ones name and the entry point does not export, such as the
	// interface a function returns or the base of a class, in name order.
	Referenced []Declaration `json:"referenced"`
	// Script says the target has no import and no export: it is a script or a
	// CommonJS module, and the reader knows none of its exports.
	Script bool `json:"script"`
	// Stars are the specifiers of the `export *` the reader cannot follow:
	// another package, an alias that resolves to no file, or a file the
	// package does not hold.
	Stars []string `json:"stars"`
	// Target is the slash path, relative to the project, that the entry point
	// names.
	Target string `json:"-"`
	// State says whether the target is a source file the reader holds.
	State TargetState `json:"-"`
}

// Declaration is one exported declaration.
type Declaration struct {
	Name string `json:"name"`
	// Kind is function, class, interface, type, enum, variable or namespace;
	// external for a name re-exported from another package, and unknown for a
	// name whose declaration the reader cannot find.
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
	// TypeParams are the type parameters of a class, an interface or a type.
	TypeParams []TypeParam `json:"typeParams"`
	// Signatures are the call signatures of a function: its overloads, or its
	// one implementation.
	Signatures []Signature `json:"signatures"`
	// Type is the written type of a variable or the body of a type alias that
	// is not an object type; empty when none is written.
	Type string `json:"type"`
	// Members are the members of a class, an interface, an enum, or a type
	// alias of an object type; nil for any other declaration.
	Members []Member `json:"members"`
	// ID identifies the declaration within its side: its file and position.
	ID string `json:"id"`
	// Heritage lists the extends and implements clauses, one type each.
	Heritage []string `json:"heritage"`
	// Bases holds, for each clause of Heritage, the ID of the declaration the
	// clause names, resolved in the declaring file; empty when the package
	// does not declare it.
	Bases []string `json:"bases"`
	// Abstract says a class is abstract.
	Abstract bool `json:"abstract"`
	// Const says an enum is a const enum.
	Const bool `json:"const"`
	// TypeOnly says the name is exported with `export type`, so it stands for
	// no value.
	TypeOnly bool `json:"typeOnly"`
	// Namespace lists the names a namespace of this name exports, whether the
	// declaration is the namespace or merges with it, or the names of the
	// module an `export * as` names.
	Namespace []string `json:"namespace"`
	// Unread says the namespace is a module an `export * as` names that the
	// reader cannot read whole, so Namespace is not all it exports.
	Unread bool `json:"unread"`
}

// TypeParam is one type parameter. Its name is not kept: type parameters are
// compared by position.
type TypeParam struct {
	Constraint string `json:"constraint"`
	// Default is the written default; empty when there is none.
	Default string `json:"default"`
}

// Signature is one call signature. An unwritten type is empty.
type Signature struct {
	TypeParams []TypeParam `json:"typeParams"`
	// This is the written type of a `this` parameter; empty without one.
	This    string  `json:"this"`
	Params  []Param `json:"params"`
	Returns string  `json:"returns"`
}

// Param is one parameter. Its name is not kept.
type Param struct {
	Type     string `json:"type"`
	Optional bool   `json:"optional"`
	Rest     bool   `json:"rest"`
}

// Member is one member of a class, an interface, an object type or an enum.
// A static member's name starts with "static ".
type Member struct {
	Name string `json:"name"`
	// Kind is property, method, constructor, call signature, construct
	// signature, index signature or enum member.
	Kind     string `json:"kind"`
	Optional bool   `json:"optional"`
	Abstract bool   `json:"abstract"`
	// Readonly says a property cannot be written: it is declared readonly, or
	// it has a getter and no setter.
	Readonly bool `json:"readonly"`
	// Access is "protected", or "private" for a member kept by its name alone;
	// empty for a public member.
	Access string `json:"access"`
	// Type is the written type of a property, or the value of an enum member.
	Type       string      `json:"type"`
	Signatures []Signature `json:"signatures"`
	Line       int         `json:"line"`
}

// request is the document the script reads.
type request struct {
	// TypeScript is the directory of the typescript package to load.
	TypeScript string        `json:"typescript"`
	Sides      []requestSide `json:"sides"`
}

type requestSide struct {
	Files   map[string]string   `json:"files"`
	Entries []requestEntry      `json:"entries"`
	Imports map[string][]string `json:"imports"`
}

type requestEntry struct {
	Key  string `json:"key"`
	File string `json:"file"`
}

// response is the document the script writes.
type response struct {
	Sides []*Surface `json:"sides"`
}

// Read returns the surface of each side, in order. bun is the bun binary to
// run, and projectDir is the directory the TypeScript parser resolves from,
// the way an import of the project would. It returns ErrNoTypeScript when the
// parser does not resolve.
func Read(bun, projectDir string, sides ...Side) ([]*Surface, error) {
	parser, ok := installedTypeScript(projectDir)
	if !ok {
		return nil, fmt.Errorf("%w for %s", ErrNoTypeScript, projectDir)
	}
	input := request{TypeScript: parser, Sides: make([]requestSide, len(sides))}
	for i, side := range sides {
		files := make(map[string]string, len(side.Files))
		for _, file := range side.Files {
			files[file.Path] = string(file.Data)
		}
		entries := make([]requestEntry, 0, len(side.Entries))
		for _, entry := range side.Entries {
			if entry.State == TargetRead {
				entries = append(entries, requestEntry{Key: entry.Key, File: entry.Target})
			}
		}
		input.Sides[i] = requestSide{Files: files, Entries: entries, Imports: side.Imports}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode the sources: %w", err)
	}
	output, err := runScript(bun, payload)
	if err != nil {
		return nil, err
	}
	var answer response
	if err := json.Unmarshal(output, &answer); err != nil {
		return nil, fmt.Errorf("read the API: decode the answer of the reader: %w", err)
	}
	if len(answer.Sides) != len(sides) {
		return nil, fmt.Errorf("read the API: the reader answered %d sides for %d", len(answer.Sides), len(sides))
	}
	for i, surface := range answer.Sides {
		surface.Entries = mergeEntries(sides[i].Entries, surface.Entries)
	}
	return answer.Sides, nil
}

// installedTypeScript returns the directory of the typescript package that
// resolves from dir: the nearest node_modules/typescript, walking up to the
// filesystem root the way package resolution does.
func installedTypeScript(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, "node_modules", "typescript")
		if _, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// mergeEntries returns one entry per declared entry point, in key order: the
// one the reader answered for a target it read, and an entry without exports
// for any other.
func mergeEntries(declared []EntryPoint, read []Entry) []Entry {
	byKey := make(map[string]Entry, len(read))
	for _, entry := range read {
		byKey[entry.Key] = entry
	}
	entries := make([]Entry, 0, len(declared))
	for _, point := range declared {
		entry := byKey[point.Key]
		entry.Key, entry.Subpath, entry.Target, entry.State = point.Key, point.Subpath, point.Target, point.State
		entries = append(entries, entry)
	}
	return entries
}

// runScript runs the embedded script with bun and returns its stdout.
func runScript(bun string, input []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "putnami-ts-api-")
	if err != nil {
		return nil, fmt.Errorf("read the API: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, "extract.mjs")
	if err := os.WriteFile(script, extractScript, 0o600); err != nil {
		return nil, fmt.Errorf("read the API: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	// --no-install: bun must load the parser it is given and fetch nothing.
	cmd := exec.CommandContext(ctx, bun, "--no-install", script) //nolint:gosec // the resolved bun and a script this binary embeds
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("read the API: bun timed out after %s", readTimeout)
		}
		return nil, fmt.Errorf("read the API: bun: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
