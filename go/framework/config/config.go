// Package config provides multi-source configuration loading for the Putnami
// Go framework. It supports YAML files, environment variables, and programmatic
// defaults with source priority ordering and DI integration.
package config

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

// Error codes for configuration operations.
const (
	CodeConfigSource  errors.Code = "config.source"
	CodeConfigPath    errors.Code = "config.path"
	CodeConfigMapping errors.Code = "config.mapping"
)

// Source provides configuration values from a specific origin.
type Source interface {
	// Name identifies this source for debugging/origin tracking.
	Name() string
	// Priority determines precedence (higher wins).
	Priority() int
	// Load returns the configuration tree or nil if unavailable.
	Load() (map[string]any, error)
}

// ContextSource is an optional extension of Source for sources whose loading
// can block (network fetches, retry/backoff loops). A source that implements
// it lets a caller cancel a slow load via context — e.g. a container told to
// shut down mid-startup (SIGTERM/deadline) instead of waiting out the full
// retry budget.
//
// It is a separate interface, not a change to Source.Load, so existing Source
// implementations stay valid. LoadContext drives ContextSource when present
// and falls back to Source.Load otherwise.
type ContextSource interface {
	Source
	// LoadContext behaves like Load but honors ctx cancellation. It must return
	// ctx.Err() (or an error wrapping it) promptly once ctx is done.
	LoadContext(ctx context.Context) (map[string]any, error)
}

// loadSource loads a single source, preferring its context-aware path when the
// source implements ContextSource so cancellation propagates; otherwise it
// falls back to the plain Load.
func loadSource(ctx context.Context, src Source) (map[string]any, error) {
	if cs, ok := src.(ContextSource); ok {
		return cs.LoadContext(ctx)
	}
	return src.Load()
}

// Definition represents a typed configuration block.
type Definition[T any] struct {
	Path   string // dot-notation path into the config tree (e.g., "server.http")
	Schema T      // zero value of the config struct
}

// Config creates a typed configuration definition.
//
//	var ServerConfig = config.Config[ServerOptions]("server.http")
func Config[T any](path string) Definition[T] {
	var zero T
	return Definition[T]{Path: path, Schema: zero}
}

// Descriptor is the type-erased form of a Definition: a config path paired with
// the Go type of its schema. It lets describe-time tooling reflect a block's
// shape without the generic type parameter, so a library can contribute its
// config blocks to a workload's published schema the same way migration sources
// aggregate from deps.
type Descriptor struct {
	// Path is the dot-notation config path (e.g. "core", "server.http").
	Path string
	// Type is the schema struct type. Reflecting over it yields the block's
	// fields, matching what the source extractor derives from the declaration.
	Type reflect.Type
}

// Descriptor returns the type-erased descriptor for this definition. A plugin
// that owns config exposes its blocks to the describe phase by returning the
// descriptors of the definitions it owns:
//
//	func (p *Plugin) ConfigDefinitions() []config.Descriptor {
//	    return []config.Descriptor{coreSchema.Descriptor()}
//	}
func (d Definition[T]) Descriptor() Descriptor {
	return Descriptor{Path: d.Path, Type: reflect.TypeFor[T]()}
}

// Load loads and validates a config definition from the given sources.
// Sources are tried in priority order (highest first).
//
// When called with no sources, Load automatically discovers configuration from:
//   - conf/.env.yaml (base config)
//   - conf/.env.{APP_ENV}.yaml (environment-specific overrides)
//   - CONFIG_DATA env var (inline YAML for containers)
//   - Environment variables via `env` struct tags
//
// Load is LoadContext with a background context: sources that block (remote
// fetches with retry budgets) cannot be canceled. Use LoadContext to make a
// slow startup interruptible by a deadline or SIGTERM-derived context.
func Load[T any](def Definition[T], sources ...Source) (T, error) {
	return LoadContext(context.Background(), def, sources...)
}

// LoadContext is Load with cancellation: sources implementing ContextSource
// (the remote config/secrets sources) honor ctx, so a caller can abort a slow
// load — e.g. a required remote source retrying through its budget — when ctx
// is canceled instead of blocking for the full budget. Plain Sources, which
// load synchronously from memory/disk, are unaffected.
func LoadContext[T any](ctx context.Context, def Definition[T], sources ...Source) (T, error) {
	if len(sources) == 0 {
		sources = DiscoverSources()
	}

	// Sort sources by priority (highest first)
	sorted := make([]Source, len(sources))
	copy(sorted, sources)
	sortSources(sorted)

	// Merge all sources
	merged := make(map[string]any)
	for i := len(sorted) - 1; i >= 0; i-- {
		data, err := loadSource(ctx, sorted[i])
		if err != nil {
			var zero T
			return zero, errors.Wrapf(err, CodeConfigSource, "config source failed", errors.String("source", sorted[i].Name()))
		}
		if data != nil {
			deepMerge(merged, data)
		}
	}

	// Navigate to the path
	value := navigatePath(merged, def.Path)
	if value == nil {
		value = make(map[string]any)
	}

	valueMap, ok := value.(map[string]any)
	if !ok {
		var zero T
		return zero, errors.New(CodeConfigPath, "config path is not an object", errors.String("path", def.Path))
	}

	// Map to struct
	var result T
	if err := mapToStruct(valueMap, &result); err != nil {
		var zero T
		return zero, errors.Wrapf(err, CodeConfigMapping, "config mapping failed", errors.String("path", def.Path))
	}

	return result, nil
}

// Token creates a DI token for a config definition.
func Token[T any](def Definition[T]) inject.Token {
	return inject.Named[T]("config:" + def.Path)
}

// Provide creates a DI registration that loads config at container start.
//
// The load uses a background context, so a blocking remote source cannot be
// canceled during container startup. Use ProvideContext with the app's
// startup/root context when startup must be interruptible (e.g. SIGTERM while a
// required config server is still warming up).
func Provide[T any](def Definition[T], sources ...Source) inject.Registration {
	return ProvideContext(context.Background(), def, sources...)
}

// ProvideContext is Provide with a caller-supplied context threaded into the
// load. The inject factory signature exposes no per-resolution context, so the
// context is captured at registration time; pass the application's
// startup/root context so a canceled startup aborts a slow required remote
// source instead of blocking for its full retry budget.
func ProvideContext[T any](ctx context.Context, def Definition[T], sources ...Source) inject.Registration {
	token := Token(def)
	return inject.Provide(token, func(_ inject.Resolver) (any, error) {
		return LoadContext(ctx, def, sources...)
	})
}

// --- Built-in Sources ---

// EnvSource reads configuration from environment variables.
// Keys are mapped to a nested config tree by stripping the prefix, lowercasing,
// and splitting on every "_" — each underscore introduces one nesting level:
//
//	EnvSource with prefix "APP" maps APP_SERVER_PORT → server.port
//
// Because every "_" is a path separator, each path segment must be a single
// lowercase word. A camelCase JSON key cannot be reached through EnvSource:
// APP_MAX_CONNS becomes the path max.conns, never the key "maxConns". For fields
// whose JSON key is camelCase (or otherwise multi-word), bind them with a
// per-field `env:"..."` struct tag instead; that tag is read directly by Load and
// takes precedence over values supplied by any Source, including EnvSource.
type EnvSource struct {
	prefix string
}

// NewEnvSource creates an environment variable source with an optional prefix.
func NewEnvSource(prefix string) *EnvSource {
	return &EnvSource{prefix: prefix}
}

// Name returns the source name.
func (s *EnvSource) Name() string { return "env" }

// Priority returns the source priority.
func (s *EnvSource) Priority() int { return 80 }

// Load scans environment variables matching the prefix and builds a nested config
// tree. Variables are expected in UPPER_SNAKE_CASE and every "_" becomes a nesting
// level: prefix "APP" maps APP_SERVER_PORT → server.port. See the EnvSource type
// doc for the single-word-segment limitation and the `env:` tag alternative for
// camelCase keys.
func (s *EnvSource) Load() (map[string]any, error) {
	prefix := s.prefix + "_"
	result := make(map[string]any)
	for _, env := range os.Environ() {
		key, value, ok := strings.Cut(env, "=")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		// Strip prefix and convert UPPER_SNAKE_CASE to dot-separated lowercase path
		path := strings.ToLower(strings.TrimPrefix(key, prefix))
		parts := strings.Split(path, "_")
		setNestedValue(result, parts, value)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// setNestedValue sets a value in a nested map at the given path.
func setNestedValue(m map[string]any, parts []string, value string) {
	for i, part := range parts {
		if i == len(parts)-1 {
			m[part] = value
			return
		}
		next, ok := m[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			m[part] = next
		}
		m = next
	}
}

// MapSource provides configuration from a static map.
type MapSource struct {
	name     string
	priority int
	data     map[string]any
}

// NewMapSource creates a source from a pre-built map.
func NewMapSource(name string, priority int, data map[string]any) *MapSource {
	return &MapSource{name: name, priority: priority, data: data}
}

// Name returns the source name.
func (s *MapSource) Name() string { return s.name }

// Priority returns the source priority.
func (s *MapSource) Priority() int { return s.priority }

// Load returns the configuration tree from the static map.
func (s *MapSource) Load() (map[string]any, error) { return s.data, nil }

// --- Helpers ---

func navigatePath(data map[string]any, path string) any {
	if path == "" {
		return data
	}
	parts := strings.Split(path, ".")
	var current any = data
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = m[part]
		if !ok {
			return nil
		}
	}
	return current
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
				continue
			}
		}
		// Never alias a source-owned value into the merge result. deepMerge
		// writes into dst maps in place, so a nested source map assigned by
		// reference would be mutated by a higher-priority merge — corrupting
		// the source (MapSource.Load returns its retained map) and racing
		// concurrent Loads that share a source. Install an owned deep copy.
		dst[k] = cloneValue(v)
	}
}

// cloneValue deep-copies a decoded config value (maps and slices recursively;
// scalars are immutable and returned as-is) so the merge result owns its data
// and can be mutated without touching any source's tree.
func cloneValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = cloneValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = cloneValue(vv)
		}
		return out
	default:
		return v
	}
}

func sortSources(sources []Source) {
	for i := 1; i < len(sources); i++ {
		for j := i; j > 0 && sources[j].Priority() > sources[j-1].Priority(); j-- {
			sources[j], sources[j-1] = sources[j-1], sources[j]
		}
	}
}

// mapToStruct maps a flat map to a struct using json tags and env tags.
func mapToStruct(data map[string]any, target any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return errors.New(CodeConfigMapping, "target must be a pointer to a struct")
	}
	rv = rv.Elem()
	rt := rv.Type()

	for i := range rt.NumField() {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}

		// Determine field name
		name := field.Name
		if jsonTag := field.Tag.Get("json"); jsonTag != "" {
			n, _, _ := strings.Cut(jsonTag, ",")
			if n != "" {
				name = n
			}
		}

		fv := rv.Field(i)

		// A field tagged sensitive:"true" never has its raw env/default value
		// echoed into an error attr, even on a parse failure (see valueAttr). The
		// flag is threaded into setFieldFromString so the inner parse errors
		// redact at the source too, not just the outer wrap.
		sensitive := field.Tag.Get("sensitive") == "true"

		// Try environment variable first
		if envKey := field.Tag.Get("env"); envKey != "" {
			if envVal := os.Getenv(envKey); envVal != "" {
				if err := setFieldFromString(fv, envVal, sensitive); err != nil {
					return errors.Wrap(err, CodeConfigMapping,
						errors.String("field", name), errors.String("env", envKey),
						valueAttr("value", envVal, sensitive))
				}
				continue
			}
		}

		// Try map value
		value, ok := data[name]
		if !ok {
			// Apply default tag for scalar fields.
			if defaultVal := field.Tag.Get("default"); defaultVal != "" {
				if err := setFieldFromString(fv, defaultVal, sensitive); err != nil {
					return errors.Wrap(err, CodeConfigMapping,
						errors.String("field", name),
						valueAttr("default", defaultVal, sensitive))
				}
			}
			// Process nested structs even when the key is missing so that
			// their own default and env tags are applied.
			if fv.Kind() == reflect.Struct {
				if err := mapToStruct(make(map[string]any), fv.Addr().Interface()); err != nil {
					return err
				}
			}
			continue
		}

		if err := setFieldFromAny(fv, value, sensitive); err != nil {
			return errors.Wrap(err, CodeConfigMapping, errors.String("field", name))
		}
	}

	return nil
}

// valueAttr builds the "value"/"default" error attr that carries the raw
// string being parsed. When the field is sensitive the raw value is replaced
// with a fixed "[redacted]" marker so a malformed secret never reaches an
// attr-iterating logger (errors.GetAttrs), while the attr key is kept so the
// diagnostic still records which input failed. Field name and kind, surfaced
// separately, remain intact for debugging.
func valueAttr(key, value string, sensitive bool) errors.Attr {
	if sensitive {
		return errors.String(key, "[redacted]")
	}
	return errors.String(key, value)
}

// setFieldFromString parses s into fv according to fv's kind. A value that
// cannot be parsed returns a CodeConfigMapping error rather than being
// silently discarded: a typo like PORT=abc must surface at Load time, not show
// up later as a zero-valued field bound to a random port.
//
// sensitive redacts the raw value from every error attr this function emits:
// because errors.GetAttrs walks the whole chain, the inner parse error (not
// just the outer wrap in mapToStruct) must omit a sensitive field's value, or
// a malformed numeric/duration/bool secret leaks to attr-iterating loggers.
func setFieldFromString(fv reflect.Value, s string, sensitive bool) error {
	if fv.Kind() == reflect.Ptr {
		elem := reflect.New(fv.Type().Elem())
		if err := setFieldFromString(elem.Elem(), s, sensitive); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}

	// Handle time.Duration before general int case.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(s)
		if err != nil {
			return errors.New(CodeConfigMapping, "invalid duration value", valueAttr("value", s, sensitive))
		}
		fv.SetInt(int64(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Int, reflect.Int64:
		n, err := parseInt(s)
		if err != nil {
			return errors.New(CodeConfigMapping, "invalid integer value", valueAttr("value", s, sensitive))
		}
		fv.SetInt(n)
	case reflect.Float64:
		n, err := parseFloat(s)
		if err != nil {
			return errors.New(CodeConfigMapping, "invalid number value", valueAttr("value", s, sensitive))
		}
		fv.SetFloat(n)
	case reflect.Bool:
		b, err := parseBool(s, sensitive)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	default:
		// Fail loud: an env/default tag on a field whose kind cannot be parsed
		// from a string (uint, int32, slice, map, …) is a misuse that must
		// surface at Load time, not silently leave the field at its zero value.
		return errors.New(CodeConfigMapping,
			"unsupported field kind for a string-tagged value; supported kinds are string, int, int64, float64, bool, and time.Duration",
			errors.String("kind", fv.Kind().String()), errors.String("type", fv.Type().String()))
	}
	return nil
}

func setFieldFromAny(fv reflect.Value, value any, sensitive bool) error {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	if rv.Type().AssignableTo(fv.Type()) {
		fv.Set(rv)
		return nil
	}
	if fv.Kind() == reflect.Ptr {
		elem := reflect.New(fv.Type().Elem())
		if err := setFieldFromAny(elem.Elem(), value, sensitive); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}
	if fv.Kind() == reflect.String {
		fv.SetString(fmt.Sprint(value))
		return nil
	}
	if rv.Type().ConvertibleTo(fv.Type()) && fv.Kind() != reflect.String {
		// Don't Convert into a string field: an int Converted to string yields
		// a rune (80 -> "P"), not its decimal text. String fields use fmt.Sprint
		// above so values are stringified, not reinterpreted.
		fv.Set(rv.Convert(fv.Type()))
		return nil
	}
	// Try string conversion. The map/YAML/remote value path carries the field's
	// sensitive tag from mapToStruct so malformed string-valued secrets redact
	// the same way env/default values do.
	if s, ok := value.(string); ok {
		return setFieldFromString(fv, s, sensitive)
	}
	// Handle nested structs from map[string]any
	if m, ok := value.(map[string]any); ok && fv.Kind() == reflect.Struct {
		return mapToStruct(m, fv.Addr().Interface())
	}
	// Handle slices (e.g., []CustomType from []any parsed by YAML). Each
	// element recurses through setFieldFromAny, so a mismatched element kind
	// surfaces as an error rather than being silently zeroed.
	if fv.Kind() == reflect.Slice && rv.Kind() == reflect.Slice {
		elemType := fv.Type().Elem()
		newSlice := reflect.MakeSlice(fv.Type(), rv.Len(), rv.Len())
		for i := range rv.Len() {
			elem := reflect.New(elemType).Elem()
			if err := setFieldFromAny(elem, rv.Index(i).Interface(), sensitive); err != nil {
				return err
			}
			newSlice.Index(i).Set(elem)
		}
		fv.Set(newSlice)
		return nil
	}
	// Handle maps (e.g., map[string]CustomType from map[string]any parsed by
	// YAML). Convert both keys and values recursively so typed maps follow the
	// same mapping rules as slices and nested structs.
	if fv.Kind() == reflect.Map && rv.Kind() == reflect.Map {
		keyType := fv.Type().Key()
		elemType := fv.Type().Elem()
		newMap := reflect.MakeMapWithSize(fv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := reflect.New(keyType).Elem()
			if err := setFieldFromAny(key, iter.Key().Interface(), sensitive); err != nil {
				return err
			}

			elem := reflect.New(elemType).Elem()
			if err := setFieldFromAny(elem, iter.Value().Interface(), sensitive); err != nil {
				return err
			}
			newMap.SetMapIndex(key, elem)
		}
		fv.Set(newMap)
		return nil
	}
	// Fail loud: a value that matched none of the branches above would
	// otherwise be silently left at its zero value, contradicting the module's
	// fail-loud mapping contract (see setFieldFromString's default case).
	return errors.New(CodeConfigMapping,
		"cannot map value into field; the value type is not assignable, convertible, or string-parseable into the field kind",
		errors.String("kind", fv.Kind().String()),
		errors.String("type", fv.Type().String()),
		errors.String("valueType", rv.Type().String()))
}

// parseInt parses a base-10 integer, rejecting trailing garbage. strconv is
// used instead of fmt.Sscanf("%d") because Sscanf accepts "80x" as 80.
func parseInt(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// parseFloat parses a float, rejecting trailing garbage (see parseInt).
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// parseBool accepts the historical truthy tokens (true/1/yes) plus their
// falsy counterparts (false/0/no) and strconv.ParseBool's set, case-insensitively.
// Anything else is an error rather than being silently coerced to false.
//
// sensitive redacts the raw value from the error attr (see valueAttr) so a
// malformed boolean secret cannot leak through errors.GetAttrs.
func parseBool(s string, sensitive bool) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes", "y", "t", "on":
		return true, nil
	case "false", "0", "no", "n", "f", "off":
		return false, nil
	default:
		return false, errors.New(CodeConfigMapping, "invalid boolean value", valueAttr("value", s, sensitive))
	}
}
