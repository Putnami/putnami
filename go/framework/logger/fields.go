package logger

import (
	"context"
	"reflect"
	"strings"
	"sync"
)

// MaxAppendedFieldValues caps how many values a single Append target may hold.
// Once the target slice reaches this length, further appends are dropped
// silently — pair every Append with an Increment on a sibling counter so the
// true total survives the cap. The constant is kept in sync across the three
// definitions of the logging contract: this package, the TypeScript
// @putnami/runtime logger, and protocols/logging/conformance.
const MaxAppendedFieldValues = 100

// FieldBag is a request-scoped, concurrency-safe accumulator of structured log
// fields. Work performed anywhere inside one request/event boundary contributes
// fields to the boundary's single terminal record through the bag, instead of
// last-write-losing on a shared logger. The bag also carries a dedicated
// request-error slot (see SetRequestError) so the terminal record can emit the
// real failure error without it being forgeable through the fields map.
type FieldBag struct {
	mu         sync.Mutex
	fields     map[string]any
	requestErr error
}

// NewFieldBag returns an empty, ready-to-use FieldBag.
func NewFieldBag() *FieldBag {
	return &FieldBag{fields: make(map[string]any)}
}

type fieldBagKey struct{}

// ContextWithFieldBag installs b on the context, always shadowing any parent
// bag: each request/event boundary owns its terminal record, so a nested
// boundary must accumulate into its own bag, not its parent's.
func ContextWithFieldBag(ctx context.Context, b *FieldBag) context.Context {
	return context.WithValue(ctx, fieldBagKey{}, b)
}

// fieldBagFrom returns the FieldBag attached to ctx, or nil when none is present.
func fieldBagFrom(ctx context.Context) *FieldBag {
	if ctx == nil {
		return nil
	}
	b, ok := ctx.Value(fieldBagKey{}).(*FieldBag)
	if !ok {
		return nil
	}
	return b
}

// Set writes value at the dot-separated path in ctx's field bag. It is a no-op
// (never panics or errors) when ctx carries no bag. When both the existing value
// and value are map[string]any, they shallow-merge (value wins per field);
// otherwise value overwrites.
func Set(ctx context.Context, path string, value any) {
	if b := fieldBagFrom(ctx); b != nil {
		b.set(path, value)
	}
}

// Append pushes value onto the slice at the dot-separated path in ctx's field
// bag. It is a no-op when ctx carries no bag. An absent leaf becomes
// []any{value}; an existing slice is appended to; any other existing value is
// promoted to []any{existing, value} so earlier values are never lost. Once the
// slice reaches MaxAppendedFieldValues, further appends are dropped silently.
func Append(ctx context.Context, path string, value any) {
	if b := fieldBagFrom(ctx); b != nil {
		b.append(path, value)
	}
}

// Increment adds delta to the integer counter at the dot-separated path in ctx's
// field bag. It is a no-op when ctx carries no bag. An existing integer is
// incremented; any other (or absent) value is replaced by delta.
func Increment(ctx context.Context, path string, delta int64) {
	if b := fieldBagFrom(ctx); b != nil {
		b.increment(path, delta)
	}
}

// SetRequestError records err in ctx's field bag's dedicated error slot. It is a
// no-op when ctx carries no bag. The slot is separate from the fields map so the
// terminal record can emit the real error through buildErrorInfo and it can
// never be forged via Set("error", …).
func SetRequestError(ctx context.Context, err error) {
	if b := fieldBagFrom(ctx); b != nil {
		b.mu.Lock()
		b.requestErr = err
		b.mu.Unlock()
	}
}

// RequestError returns the error recorded via SetRequestError, or nil when ctx
// carries no bag or no error was set.
func RequestError(ctx context.Context) error {
	b := fieldBagFrom(ctx)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requestErr
}

// Snapshot returns a deep copy of the bag's fields. Bag-owned maps and slices
// are copied so a concurrent Append/Set after the snapshot cannot race a sink
// marshaling the returned map. Values that the bag does not own (scalars and
// caller-supplied objects) are shared by reference.
func (b *FieldBag) Snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return deepCopyMap(b.fields)
}

// walk descends fields along keys, creating fresh map[string]any intermediates
// and replacing any non-map intermediate with a fresh map. Callers must hold
// b.mu.
func (b *FieldBag) walk(keys []string) map[string]any {
	node := b.fields
	for _, k := range keys {
		next, ok := node[k].(map[string]any)
		if !ok {
			next = make(map[string]any)
			node[k] = next
		}
		node = next
	}
	return node
}

func (b *FieldBag) set(path string, value any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := strings.Split(path, ".")
	node := b.walk(keys[:len(keys)-1])
	leaf := keys[len(keys)-1]
	if existing, ok := node[leaf].(map[string]any); ok {
		if incoming, ok := value.(map[string]any); ok {
			for k, v := range incoming {
				existing[k] = v
			}
			return
		}
	}
	node[leaf] = value
}

func (b *FieldBag) append(path string, value any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := strings.Split(path, ".")
	node := b.walk(keys[:len(keys)-1])
	leaf := keys[len(keys)-1]
	existing, ok := node[leaf]
	switch {
	case !ok:
		node[leaf] = []any{value}
	default:
		if arr, isArr := asAnySlice(existing); isArr {
			if len(arr) >= MaxAppendedFieldValues {
				return // cap reached: drop silently
			}
			node[leaf] = append(arr, value)
			return
		}
		// Scalar promotion: never lose the earlier value.
		node[leaf] = []any{existing, value}
	}
}

// asAnySlice recognizes any Go slice, not only []any, and returns a mutable
// []any copy suitable for the FieldBag's uniform append representation.
func asAnySlice(v any) ([]any, bool) {
	if arr, ok := v.([]any); ok {
		return arr, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range rv.Len() {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func (b *FieldBag) increment(path string, delta int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := strings.Split(path, ".")
	node := b.walk(keys[:len(keys)-1])
	leaf := keys[len(keys)-1]
	switch n := node[leaf].(type) {
	case int64:
		node[leaf] = n + delta
	case int:
		node[leaf] = int64(n) + delta
	default:
		node[leaf] = delta
	}
}

// deepCopyMap returns a deep copy of m, cloning nested map[string]any and []any
// containers so the returned structure shares no mutable container with m.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
