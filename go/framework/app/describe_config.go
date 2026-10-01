package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/config"
	"go.putnami.dev/errors"
	protocfg "go.putnami.dev/protocol/config"
)

// describerNameConfig is the built-in describer that aggregates library-owned
// config blocks. Filtered by "config" / "all" via DescribeContext.Wants.
const describerNameConfig = "config"

// configDepsFragment is the file describeConfig writes dependency-owned config
// blocks to, under DescribeContext.OutputDir. The Go build's config merge step
// (extension codegen) unions these with the workload's own blocks — extracted
// from source — into the published schema/config.json. It is written outside
// the schema/ subtree so the generic artifact collector never commits the
// fragment itself; only the merged manifest is committed.
const configDepsFragment = "config-deps.json"

// configDepsDocument is the on-disk shape of the fragment: a wrapper object so
// the format can grow (metadata, version) without breaking the reader.
type configDepsDocument struct {
	Blocks []protocfg.Block `json:"blocks"`
}

// describeConfig aggregates config blocks from every ConfigContributor in the
// module tree and writes them to <OutputDir>/config-deps.json. These are the
// library-owned blocks a workload publishes transitively — the same way
// migration sources aggregate from deps. No file is written when no contributor
// declares a block, so apps that use no config-owning libraries stay untouched.
func (a *Application) describeConfig(dctx *DescribeContext) error {
	contributors := Collect[ConfigContributor](a.Module)
	descriptors := make([]config.Descriptor, 0, len(contributors))
	for _, c := range contributors {
		descriptors = append(descriptors, c.ConfigDefinitions()...)
	}
	if len(descriptors) == 0 {
		// Nothing contributed → remove any stale fragment so a later build
		// that dropped its last config-owning dependency doesn't keep
		// republishing the block.
		if err := os.Remove(filepath.Join(dctx.OutputDir, configDepsFragment)); err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, CodeConfigure, "remove stale config-deps fragment")
		}
		return nil
	}

	blocks, err := buildConfigBlocks(descriptors)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(configDepsDocument{Blocks: blocks}, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(dctx.OutputDir, configDepsFragment)
	if err := os.WriteFile(out, data, 0o644); err != nil { //nolint:gosec // OutputDir is framework-controlled
		return errors.Wrapf(err, CodeConfigure, "write config-deps fragment")
	}
	return nil
}

// buildConfigBlocks reflects each descriptor's schema type into a protocfg.Block.
// Paths must be unique across all contributors; a duplicate path (two plugins
// claiming the same block) is a build error, mirroring the source extractor's
// duplicate-path guard. Resolver errors (e.g. non-primitive map keys) are
// aggregated so the build fails atomically rather than emitting a partial
// manifest.
func buildConfigBlocks(descriptors []config.Descriptor) ([]protocfg.Block, error) {
	blocks := make([]protocfg.Block, 0, len(descriptors))
	errs := make([]string, 0, len(descriptors))
	counts := make(map[string]int, len(descriptors))
	for _, d := range descriptors {
		counts[d.Path]++
		r := &reflectResolver{stack: map[reflect.Type]bool{}}
		field := r.resolve(d.Type)
		errs = append(errs, r.errs...)
		var fields []protocfg.FieldSchema
		if field.Type == protocfg.FieldTypeObject {
			fields = field.Fields
		}
		blocks = append(blocks, protocfg.Block{Path: d.Path, Fields: fields})
	}

	dups := make([]string, 0)
	for path, n := range counts {
		if n > 1 {
			dups = append(dups, path)
		}
	}
	sort.Strings(dups)
	for _, path := range dups {
		errs = append(errs, fmt.Sprintf("duplicate config path %q contributed by multiple plugins", path))
	}

	if len(errs) > 0 {
		return nil, errors.Newf(CodeConfigure, "config describe failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return blocks, nil
}

// reflectResolver turns a reflect.Type into a protocfg.FieldSchema, mirroring
// the AST-based source extractor (go/extension/.../configextract) so a block
// reflected from a library's schema type matches the block that library
// publishes from its own source. It owns a cycle guard keyed by reflect.Type:
// a type that reappears while being expanded yields an opaque object, the same
// terminating shape the source extractor uses for recursive and unresolvable
// types.
type reflectResolver struct {
	stack map[reflect.Type]bool
	errs  []string
}

// durationType is time.Duration's reflect.Type. A Duration is an int64 under
// the hood, so it must be matched before the integer kinds.
var durationType = reflect.TypeFor[time.Duration]()

func (r *reflectResolver) resolve(t reflect.Type) protocfg.FieldSchema {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
	if t == durationType {
		return protocfg.FieldSchema{Type: protocfg.FieldTypeDuration}
	}

	switch t.Kind() {
	case reflect.String:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeString}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeInt}
	case reflect.Float32, reflect.Float64:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeFloat}
	case reflect.Bool:
		return protocfg.FieldSchema{Type: protocfg.FieldTypeBool}
	case reflect.Struct:
		return r.resolveStruct(t)
	case reflect.Slice, reflect.Array:
		items := r.resolve(t.Elem())
		return protocfg.FieldSchema{Type: protocfg.FieldTypeArray, Items: &items}
	case reflect.Map:
		key := r.resolve(t.Key())
		val := r.resolve(t.Elem())
		if !protocfg.ValidMapKeyTypes[key.Type] {
			r.errs = append(r.errs, fmt.Sprintf("map key type %q is not a primitive; map keys must be string, int, or bool", key.Type))
			return protocfg.FieldSchema{Type: protocfg.FieldTypeMap, Keys: protocfg.FieldTypeString, Values: &val}
		}
		return protocfg.FieldSchema{Type: protocfg.FieldTypeMap, Keys: key.Type, Values: &val}
	default:
		// Interfaces and any other shape the schema vocabulary can't express
		// serialize as an opaque object, matching the source extractor.
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
}

func (r *reflectResolver) resolveStruct(t reflect.Type) protocfg.FieldSchema {
	if r.stack[t] {
		return protocfg.FieldSchema{Type: protocfg.FieldTypeObject}
	}
	r.stack[t] = true
	defer delete(r.stack, t)
	return protocfg.FieldSchema{Type: protocfg.FieldTypeObject, Fields: r.structFields(t)}
}

// structFields walks a struct's fields. Named exported fields come first; then
// embedded fields are inlined unless a name collides — Go's JSON marshaling
// resolves shadowing by preferring the outer field, and the source extractor
// applies the same rule.
func (r *reflectResolver) structFields(t reflect.Type) []protocfg.FieldSchema {
	fields := make([]protocfg.FieldSchema, 0, t.NumField())
	seen := map[string]bool{}

	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Anonymous || !sf.IsExported() {
			continue
		}
		f := r.buildField(sf)
		if f == nil || seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		fields = append(fields, *f)
	}

	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.Anonymous {
			continue
		}
		inlined := r.resolve(sf.Type)
		if inlined.Type != protocfg.FieldTypeObject {
			continue
		}
		for _, nested := range inlined.Fields {
			if seen[nested.Name] {
				continue
			}
			seen[nested.Name] = true
			fields = append(fields, nested)
		}
	}

	return fields
}

// buildField resolves a single named field's type and overlays its struct tags,
// reading the same tags the source extractor reads: json (name / "-" skip),
// default, env, validate (constraints, with "required" lifted), sensitive, and
// desc. Returns nil when the field opts out via json:"-".
func (r *reflectResolver) buildField(sf reflect.StructField) *protocfg.FieldSchema {
	resolved := r.resolve(sf.Type)
	resolved.Name = sf.Name

	if jsonTag := sf.Tag.Get("json"); jsonTag != "" {
		name, _, _ := strings.Cut(jsonTag, ",")
		if name == "-" {
			return nil
		}
		if name != "" {
			resolved.Name = name
		}
	}
	if def := sf.Tag.Get("default"); def != "" {
		resolved.Default = def
	}
	if env := sf.Tag.Get("env"); env != "" {
		resolved.Env = env
	}
	if validate := sf.Tag.Get("validate"); validate != "" {
		resolved.Constraints = strings.Split(validate, ",")
		for _, c := range resolved.Constraints {
			if c == "required" {
				resolved.Required = true
			}
		}
	}
	if sf.Tag.Get("sensitive") == "true" {
		resolved.Sensitive = true
	}
	if desc := sf.Tag.Get("desc"); desc != "" {
		resolved.Description = desc
	}

	return &resolved
}
