package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// --- no-bag no-op ------------------------------------------------------------

func TestFieldBag_NoBagIsNoOp(t *testing.T) {
	ctx := context.Background()
	if fieldBagFrom(ctx) != nil {
		t.Fatal("background context should carry no field bag")
	}
	// None of these may panic or error when no bag is installed.
	Set(ctx, "http.method", "GET")
	Append(ctx, "event.publishes", "orders")
	Increment(ctx, "event.publishCount", 1)
	SetRequestError(ctx, perrors.New(perrors.CodeConnection, "unused"))
	if RequestError(ctx) != nil {
		t.Error("RequestError should be nil without a bag")
	}
}

// --- append semantics --------------------------------------------------------

func TestFieldBag_AppendFreshAndArray(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Append(ctx, "event.publishes", "a")
	snap := bag.Snapshot()
	pubs := fieldAs[[]any](t, snap, "event.publishes")
	if len(pubs) != 1 || pubs[0] != "a" {
		t.Fatalf("fresh append = %v, want [a]", pubs)
	}

	Append(ctx, "event.publishes", "b")
	pubs = fieldAs[[]any](t, bag.Snapshot(), "event.publishes")
	if len(pubs) != 2 || pubs[0] != "a" || pubs[1] != "b" {
		t.Fatalf("array append = %v, want [a b]", pubs)
	}
}

func TestFieldBag_AppendTypedSlice(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Set(ctx, "items", []string{"a"})
	Append(ctx, "items", "b")

	items := bag.Snapshot()["items"]
	arr, ok := items.([]any)
	if !ok || len(arr) != 2 || arr[0] != "a" || arr[1] != "b" {
		t.Fatalf("typed-slice append = %#v, want [a b]", items)
	}
}

func TestFieldBag_AppendScalarPromotion(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Set(ctx, "count", "first")
	Append(ctx, "count", "second")

	got := bag.Snapshot()["count"]
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != "first" || arr[1] != "second" {
		t.Fatalf("scalar promotion = %#v, want [first second]", got)
	}
}

func TestFieldBag_AppendCapDropsBeyondLimit(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	// Append MaxAppendedFieldValues+1 distinct values.
	for i := 0; i <= MaxAppendedFieldValues; i++ {
		Append(ctx, "items", i)
	}

	arr := bag.Snapshot()["items"].([]any)
	if len(arr) != MaxAppendedFieldValues {
		t.Fatalf("len = %d, want %d (101st dropped)", len(arr), MaxAppendedFieldValues)
	}
	// The first 100 are kept; the 101st (index 100) is the one dropped.
	if arr[0] != 0 || arr[MaxAppendedFieldValues-1] != MaxAppendedFieldValues-1 {
		t.Errorf("cap kept the wrong window: first=%v last=%v", arr[0], arr[len(arr)-1])
	}
}

// --- increment semantics -----------------------------------------------------

func TestFieldBag_Increment(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Increment(ctx, "event.publishCount", 1) // fresh
	if got := fieldAs[int64](t, bag.Snapshot(), "event.publishCount"); got != 1 {
		t.Fatalf("fresh increment = %d, want 1", got)
	}

	Increment(ctx, "event.publishCount", 4) // existing
	if got := fieldAs[int64](t, bag.Snapshot(), "event.publishCount"); got != 5 {
		t.Fatalf("existing increment = %d, want 5", got)
	}

	Increment(ctx, "event.publishCount", -2) // negative delta
	if got := fieldAs[int64](t, bag.Snapshot(), "event.publishCount"); got != 3 {
		t.Fatalf("negative delta = %d, want 3", got)
	}
}

func TestFieldBag_IncrementNonNumberCoercion(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Set(ctx, "count", "not-a-number")
	Increment(ctx, "count", 7)

	if got := bag.Snapshot()["count"]; got != int64(7) {
		t.Fatalf("non-number coercion = %#v, want int64(7)", got)
	}
}

// --- Set shallow-merge -------------------------------------------------------

func TestFieldBag_SetShallowMergesMaps(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Set(ctx, "http", map[string]any{"method": "GET", "status": 200})
	Set(ctx, "http", map[string]any{"status": 500, "route": "/x"})

	m := bag.Snapshot()["http"].(map[string]any)
	if m["method"] != "GET" || m["status"] != 500 || m["route"] != "/x" {
		t.Fatalf("shallow merge = %#v, want method=GET status=500 route=/x", m)
	}

	// Non-map value overwrites a map.
	Set(ctx, "http", "scalar")
	if got := bag.Snapshot()["http"]; got != "scalar" {
		t.Fatalf("overwrite = %#v, want scalar", got)
	}
}

// --- dot-path nesting + scalar-intermediate replacement (rendered bytes) -----

func TestFieldBag_DotPathRendersNestedJSON(t *testing.T) {
	var buf bytes.Buffer
	log := New("http", LevelDebug, NewJSONSinkWriter(&buf))
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Set(ctx, "http.method", "GET")
	Increment(ctx, "http.statusCount", 2)
	// "http" was created as a group above; a scalar-rooted path must be replaced
	// by a fresh map when a nested child is written.
	Set(ctx, "scalarRoot", "leaf")
	Append(ctx, "scalarRoot.child", "x")

	log.InfoCtx(ctx, "request handled")

	got := decodeLine(t, buf.Bytes())
	http, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("http did not render as a nested object: %#v", got["http"])
	}
	if http["method"] != "GET" {
		t.Errorf("http.method = %v, want GET", http["method"])
	}
	if http["statusCount"] != float64(2) { // JSON numbers decode to float64
		t.Errorf("http.statusCount = %v, want 2", http["statusCount"])
	}
	sr, ok := got["scalarRoot"].(map[string]any)
	if !ok {
		t.Fatalf("scalar intermediate not replaced by a map: %#v", got["scalarRoot"])
	}
	if child, ok := sr["child"].([]any); !ok || len(child) != 1 || child[0] != "x" {
		t.Errorf("scalarRoot.child = %#v, want [x]", sr["child"])
	}
}

// --- precedence: With < bag < attrs < reserved -------------------------------

func TestFieldBag_Precedence(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "field-precedence", "field-precedence-order")
	var buf bytes.Buffer
	base := New("test", LevelDebug, NewJSONSinkWriter(&buf))
	log := base.With("shared", "from-with").With("onlyWith", "with-only")

	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)
	Set(ctx, "shared", "from-bag")    // beats With
	Set(ctx, "onlyBag", "bag-only")   // no collision
	Set(ctx, "message", "forged-msg") // reserved: must lose

	log.InfoCtx(ctx, "real message",
		slog.String("shared", "from-attr"), // beats bag and With
		slog.String("onlyAttr", "attr-only"),
	)

	got := decodeLine(t, buf.Bytes())
	if got["shared"] != "from-attr" {
		t.Errorf("shared = %v, want from-attr (attr > bag > With)", got["shared"])
	}
	if got["onlyWith"] != "with-only" {
		t.Errorf("onlyWith = %v, want with-only", got["onlyWith"])
	}
	if got["onlyBag"] != "bag-only" {
		t.Errorf("onlyBag = %v, want bag-only", got["onlyBag"])
	}
	if got["onlyAttr"] != "attr-only" {
		t.Errorf("onlyAttr = %v, want attr-only", got["onlyAttr"])
	}
	if got["message"] != "real message" {
		t.Errorf("message = %v, want real message (reserved wins)", got["message"])
	}
}

// --- concurrency (run under -race) -------------------------------------------

func TestFieldBag_ConcurrentAccumulation(t *testing.T) {
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)
	// A JSON sink to io.Discard actually marshals entry.Context on every
	// InfoCtx, so the race detector can catch a marshal racing a concurrent
	// Append if Snapshot did not isolate the captured map.
	log := New("test", LevelDebug, NewJSONSinkWriter(discardWriter{}))

	const goroutines = 50
	const perGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				Append(ctx, "event.publishes", "x")
				Increment(ctx, "event.publishCount", 1)
				log.InfoCtx(ctx, "concurrent")
			}
		}()
	}
	wg.Wait()

	snap := bag.Snapshot()
	if got := fieldAs[int64](t, snap, "event.publishCount"); got != int64(goroutines*perGoroutine) {
		t.Errorf("publishCount = %d, want %d", got, goroutines*perGoroutine)
	}
	if pubs := fieldAs[[]any](t, snap, "event.publishes"); len(pubs) != MaxAppendedFieldValues {
		t.Errorf("publishes len = %d, want %d (capped)", len(pubs), MaxAppendedFieldValues)
	}
}

// --- snapshot isolation ------------------------------------------------------

func TestFieldBag_SnapshotIsolation(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	Append(ctx, "items", "a")
	log.InfoCtx(ctx, "captured")

	// Mutating the bag after the entry was emitted must not touch the captured
	// entry's context (Snapshot deep-copied the bag-owned slice).
	Append(ctx, "items", "b")

	items, ok := sink.Last().Context["items"].([]any)
	if !ok || len(items) != 1 || items[0] != "a" {
		t.Fatalf("captured entry mutated by later Append: %#v", sink.Last().Context["items"])
	}
}

// --- SetRequestError → ErrorCtx renders structured error ---------------------

func TestFieldBag_RequestErrorRendersStructured(t *testing.T) {
	var buf bytes.Buffer
	log := New("test", LevelDebug, NewJSONSinkWriter(&buf))
	bag := NewFieldBag()
	ctx := ContextWithFieldBag(context.Background(), bag)

	SetRequestError(ctx, perrors.New(perrors.CodeConnection, "database unreachable"))
	err := RequestError(ctx)
	if err == nil {
		t.Fatal("expected a request error from the slot")
	}
	log.ErrorCtx(ctx, "request failed", err)

	got := decodeLine(t, buf.Bytes())
	if got["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", got["severity"])
	}
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error did not render as a structured object: %#v", got["error"])
	}
	if errObj["message"] != err.Error() {
		t.Errorf("error.message = %v, want %v", errObj["message"], err.Error())
	}
	if errObj["name"] != string(perrors.CodeConnection) {
		t.Errorf("error.name = %v, want %v", errObj["name"], perrors.CodeConnection)
	}
}

// --- helpers -----------------------------------------------------------------

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func decodeLine(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal rendered line: %v (line=%s)", err, b)
	}
	return got
}

// fieldAs navigates m along a dot-separated path (descending through
// map[string]any groups) and returns the leaf value asserted to type T, failing
// the test if any segment is not a group or the leaf is not a T.
func fieldAs[T any](t *testing.T, m map[string]any, path string) T {
	t.Helper()
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		group, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%q: %#v is not a group", path, cur)
		}
		cur = group[k]
	}
	v, ok := cur.(T)
	if !ok {
		t.Fatalf("%q is not %T: %#v", path, v, cur)
	}
	return v
}
