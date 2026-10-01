package inject

import (
	"strings"
	"testing"

	"go.putnami.dev/errors"
)

type wireDB struct{ DSN string }

func newWireDB() *wireDB { return &wireDB{DSN: "localhost"} }

type wireLogger struct{ Name string }

func newWireLogger() *wireLogger { return &wireLogger{Name: "test"} }

type wireTarget struct {
	DB  *wireDB
	Log *wireLogger
}

func TestWire_BasicFields(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(AutoProvide(newWireLogger)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}

	if target.DB == nil {
		t.Fatal("expected DB to be wired")
	}
	if target.DB.DSN != "localhost" {
		t.Errorf("expected DSN 'localhost', got %q", target.DB.DSN)
	}
	if target.Log == nil {
		t.Fatal("expected Log to be wired")
	}
}

func TestWire_SkipsNonNilFields(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	existing := &wireDB{DSN: "custom"}
	target := &wireTarget{DB: existing}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}

	if target.DB != existing {
		t.Error("should not overwrite existing value")
	}
	if target.DB.DSN != "custom" {
		t.Errorf("expected DSN 'custom', got %q", target.DB.DSN)
	}
}

func TestWire_SkipsUnregisteredTypes(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}

	if target.DB == nil {
		t.Fatal("expected DB to be wired")
	}
	if target.Log != nil {
		t.Error("expected Log to be nil (not registered)")
	}
}

type wireExcluded struct {
	DB     *wireDB
	Ignore *wireLogger `inject:"-"`
}

func TestWire_RespectsExcludeTag(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(AutoProvide(newWireLogger)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireExcluded{}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}

	if target.DB == nil {
		t.Fatal("expected DB to be wired")
	}
	if target.Ignore != nil {
		t.Error("expected Ignore to be nil (inject:\"-\")")
	}
}

type wireUnexported struct {
	db *wireDB //lint:ignore U1000 testing unexported field wiring
}

func TestWire_SkipsUnexportedFields(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireUnexported{}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}
}

type wireValueTypes struct {
	Name string
	Port int
	DB   *wireDB
}

func TestWire_SkipsValueTypes(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireValueTypes{Name: "hello"}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}

	if target.Name != "hello" {
		t.Error("should not touch value types")
	}
	if target.Port != 0 {
		t.Error("should not touch value types")
	}
	if target.DB == nil {
		t.Fatal("expected DB to be wired")
	}
}

func TestWire_NilContainer(t *testing.T) {
	target := &wireTarget{}
	if err := Wire(nil, target); err != nil {
		t.Fatal(err)
	}
	if target.DB != nil || target.Log != nil {
		t.Error("nil container should not wire anything")
	}
}

func TestWire_NonStructTarget(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	str := "hello"
	if err := Wire(cc, &str); err != nil {
		t.Fatal(err)
	}

	num := 42
	if err := Wire(cc, num); err != nil {
		t.Fatal(err)
	}
}

func TestWireStrict_WiresAllRegisteredFields(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(AutoProvide(newWireLogger)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{}
	if err := WireStrict(cc, target); err != nil {
		t.Fatalf("WireStrict should succeed when every field resolves, got %v", err)
	}
	if target.DB == nil || target.Log == nil {
		t.Fatal("WireStrict should populate all registered fields")
	}
}

func TestWireStrict_ReportsUnresolvedFields(t *testing.T) {
	cc := NewContainerContext("test")
	// DB is registered; Log is intentionally NOT.
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{}
	err := WireStrict(cc, target)
	if err == nil {
		t.Fatal("WireStrict should return an error when a required field is unresolved")
	}
	if !errors.Is(err, CodeWireIncomplete) {
		t.Errorf("expected CodeWireIncomplete, got %v", err)
	}
	// The error must name the offending field so misconfiguration is diagnosable.
	if !strings.Contains(err.Error(), "Log") {
		t.Errorf("error should name the unresolved field 'Log', got: %s", err.Error())
	}
	// The resolvable field must still have been wired (best-effort population).
	if target.DB == nil {
		t.Error("resolvable field DB should still be wired by WireStrict")
	}
	// The unresolved field stays nil.
	if target.Log != nil {
		t.Error("unresolved field Log should remain nil")
	}
}

// TestWireStrict_IgnoresNonCandidateFields asserts that excluded (inject:"-"),
// already-set, unexported, and value-type fields never count as "unresolved" —
// only eligible nil pointer/interface fields can trigger an error.
func TestWireStrict_IgnoresNonCandidateFields(t *testing.T) {
	type mixed struct {
		DB       *wireDB     // registered -> wired
		Ignore   *wireLogger `inject:"-"` // excluded, must not error
		Name     string      // value type, must not error
		Existing *wireDB     // pre-set, must not error
		hidden   *wireDB     //lint:ignore U1000 unexported, must not error
	}

	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	preset := &wireDB{DSN: "preset"}
	target := &mixed{Name: "x", Existing: preset}
	if err := WireStrict(cc, target); err != nil {
		t.Fatalf("WireStrict should not error on non-candidate fields, got %v", err)
	}
	if target.DB == nil {
		t.Error("DB should be wired")
	}
	if target.Ignore != nil {
		t.Error("inject:\"-\" field must stay nil")
	}
	if target.Existing != preset {
		t.Error("pre-set field must not be overwritten")
	}
	_ = target.hidden
}

func TestWireStrict_NilContainerAndNonStructAreLenient(t *testing.T) {
	// A nil container satisfies trivially (mirrors Wire).
	if err := WireStrict(nil, &wireTarget{}); err != nil {
		t.Errorf("WireStrict(nil, ...) should return nil, got %v", err)
	}

	cc := NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	// Non-struct targets are not wireable and must not error.
	str := "hello"
	if err := WireStrict(cc, &str); err != nil {
		t.Errorf("WireStrict on *string should return nil, got %v", err)
	}
	num := 42
	if err := WireStrict(cc, num); err != nil {
		t.Errorf("WireStrict on non-pointer should return nil, got %v", err)
	}
}

// TestWire_RemainsLenientWhenStrictWouldError asserts the default Wire path is
// unchanged by the strict addition: an unresolved field is silently skipped and
// Wire still returns nil, so existing callers (e.g. plugin auto-wire) keep
// booting.
func TestWire_RemainsLenientWhenStrictWouldError(t *testing.T) {
	cc := NewContainerContext("test")
	if err := cc.Register(AutoProvide(newWireDB)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{} // Log unregistered
	if err := Wire(cc, target); err != nil {
		t.Fatalf("Wire must stay lenient and return nil, got %v", err)
	}
	if target.DB == nil {
		t.Error("DB should be wired by lenient Wire")
	}
	if target.Log != nil {
		t.Error("unresolved Log should be nil under lenient Wire")
	}
}
