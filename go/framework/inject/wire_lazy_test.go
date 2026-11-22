package inject

import "testing"

func TestWire_LazyContainerBuildsNoDependency(t *testing.T) {
	built := 0
	cc := NewContainerContext("lazy-wire")
	if err := cc.Register(AutoProvide(func() *wireDB {
		built++
		return &wireDB{DSN: "live"}
	})); err != nil {
		t.Fatal(err)
	}
	logger := &wireLogger{Name: "value"}
	if err := cc.Register(ProvideValue(TokenOf[*wireLogger](), logger)); err != nil {
		t.Fatal(err)
	}
	if err := cc.StartLazy(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	target := &wireTarget{}
	if err := Wire(cc, target); err != nil {
		t.Fatal(err)
	}
	if built != 0 {
		t.Fatalf("Wire on a lazy container ran a constructor %d time(s), want 0", built)
	}
	if target.DB != nil {
		t.Fatal("a field whose provider runs a constructor must stay nil on a lazy container")
	}
	if target.Log != logger {
		t.Fatal("a registered value must still be wired on a lazy container")
	}
	if err := WireStrict(cc, &wireTarget{}); err != nil {
		t.Fatalf("WireStrict must not report a registered field it left nil on a lazy container: %v", err)
	}
	type unregistered struct{ Missing *wireTarget }
	if err := WireStrict(cc, &unregistered{}); err == nil {
		t.Fatal("WireStrict must still report a field with no provider on a lazy container")
	}

	// Once something resolved the singleton, Wire hands out the built instance.
	db, err := cc.Get(TokenOf[*wireDB]())
	if err != nil {
		t.Fatal(err)
	}
	later := &wireTarget{}
	if err := Wire(cc, later); err != nil {
		t.Fatal(err)
	}
	if later.DB != db || built != 1 {
		t.Fatalf("Wire must reuse the built singleton: DB=%v built=%d", later.DB, built)
	}
}
