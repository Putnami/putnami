package app

import (
	"reflect"
	"testing"
)

// greeter is a test capability. It deliberately does NOT embed Plugin, to
// prove Collect works for any interface T, not just plugin capabilities.
type greeter interface {
	Greet() string
}

type fareweller interface {
	Farewell() string
}

// greeterPlugin satisfies greeter by implementing its interface.
type greeterPlugin struct {
	name string
	msg  string
}

func (p *greeterPlugin) Name() string  { return p.name }
func (p *greeterPlugin) Greet() string { return p.msg }

// greeterValue is a non-plugin greeter, registered via Contribute.
type greeterValue struct{ msg string }

func (g *greeterValue) Greet() string { return g.msg }

func greetings(gs []greeter) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Greet()
	}
	return out
}

func TestCollectInterfaceImplementers(t *testing.T) {
	root := NewModule("root").
		Use(&greeterPlugin{name: "a", msg: "a"}).
		Use(&testPlugin{name: "not-a-greeter"}).
		Use(NewModule("child").Use(&greeterPlugin{name: "b", msg: "b"}))

	got := greetings(Collect[greeter](root))
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect[greeter] = %v, want %v", got, want)
	}
}

func TestCollectContributions(t *testing.T) {
	root := NewModule("root")
	Contribute[greeter](root, &greeterValue{msg: "x"})
	Contribute[greeter](root, &greeterValue{msg: "y"})

	got := greetings(Collect[greeter](root))
	want := []string{"x", "y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect[greeter] = %v, want %v", got, want)
	}
}

type greetingFarewellValue struct {
	greeterValue
}

func (g *greetingFarewellValue) Farewell() string { return "bye" }

func TestCollectContributionsUseDeclaredCapability(t *testing.T) {
	root := NewModule("root")
	Contribute[greeter](root, &greetingFarewellValue{greeterValue: greeterValue{msg: "hello"}})

	if got := greetings(Collect[greeter](root)); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Errorf("Collect[greeter] = %v, want [hello]", got)
	}
	if got := Collect[fareweller](root); len(got) != 0 {
		t.Errorf("Collect[fareweller] = %v, want empty because contribution target was greeter", got)
	}
}

func TestCollectOrderPluginsBeforeContributionsRootBeforeChildren(t *testing.T) {
	root := NewModule("root").Use(&greeterPlugin{name: "rp", msg: "root-plugin"})
	Contribute[greeter](root, &greeterValue{msg: "root-contrib"})

	child := NewModule("child").Use(&greeterPlugin{name: "cp", msg: "child-plugin"})
	Contribute[greeter](child, &greeterValue{msg: "child-contrib"})
	root.Use(child)

	got := greetings(Collect[greeter](root))
	want := []string{"root-plugin", "root-contrib", "child-plugin", "child-contrib"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect order = %v, want %v", got, want)
	}
}

func TestCollectDedupsContributedPlugin(t *testing.T) {
	p := &greeterPlugin{name: "p", msg: "p"}
	root := NewModule("root").Use(p)
	// Same instance both used as a plugin and explicitly contributed.
	Contribute[greeter](root, p)

	got := greetings(Collect[greeter](root))
	want := []string{"p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect should dedup identical instance: got %v, want %v", got, want)
	}
}

// greeterVal is a value-typed (non-pointer) greeter, used to assert that
// Collect de-duplicates by pointer identity only — equal-but-distinct value
// capabilities must both be returned.
type greeterVal string

func (g greeterVal) Greet() string { return string(g) }

func TestCollectKeepsEqualValueCapabilities(t *testing.T) {
	root := NewModule("root")
	Contribute[greeter](root, greeterVal("dup"))
	Contribute[greeter](root, greeterVal("dup"))

	got := greetings(Collect[greeter](root))
	want := []string{"dup", "dup"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect should not dedup equal value capabilities: got %v, want %v", got, want)
	}
}

func TestCollectNoMatchesReturnsEmpty(t *testing.T) {
	root := NewModule("root").Use(&testPlugin{name: "plain"})
	if got := Collect[greeter](root); len(got) != 0 {
		t.Errorf("Collect[greeter] = %v, want empty", got)
	}
}

func TestCollectFromRootOfSubtree(t *testing.T) {
	root := NewModule("root").Use(&greeterPlugin{name: "a", msg: "a"})
	child := NewModule("child").Use(&greeterPlugin{name: "b", msg: "b"})
	root.Use(child)

	// Collecting from the child sees only the child's subtree; callers that
	// want the whole app pass owner.Root().
	if got := greetings(Collect[greeter](child)); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Collect from child = %v, want [b]", got)
	}
}

// emptyGreeterA and emptyGreeterB are distinct zero-size plugin types. Go may
// give every zero-size value the same address, so &emptyGreeterA{} and
// &emptyGreeterB{} can be equal as raw pointers.
type emptyGreeterA struct{}

func (*emptyGreeterA) Name() string  { return "a" }
func (*emptyGreeterA) Greet() string { return "a" }

type emptyGreeterB struct{}

func (*emptyGreeterB) Name() string  { return "b" }
func (*emptyGreeterB) Greet() string { return "b" }

// sameAddress reports whether a and b point at the same memory, ignoring type.
func sameAddress(a, b any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestCollectKeepsDistinctZeroSizePlugins(t *testing.T) {
	a, b := &emptyGreeterA{}, &emptyGreeterB{}
	if !sameAddress(a, b) {
		t.Skip("the runtime gave the two zero-size values distinct addresses; the shared-address case does not arise")
	}
	root := NewModule("root").Use(a).Use(b)

	got := greetings(Collect[greeter](root))
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect merged two plugin types that share an address: got %v, want %v", got, want)
	}
}

func TestCollectKeepsDistinctZeroSizeContributions(t *testing.T) {
	a, b := &emptyGreeterA{}, &emptyGreeterB{}
	if !sameAddress(a, b) {
		t.Skip("the runtime gave the two zero-size values distinct addresses; the shared-address case does not arise")
	}
	root := NewModule("root")
	Contribute[greeter](root, a)
	Contribute[greeter](root, b)

	got := greetings(Collect[greeter](root))
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect merged two contributions that share an address: got %v, want %v", got, want)
	}
}

// outerGreeter embeds a greeter as its first field, so &outer and &outer.inner
// share an address by layout on every Go implementation.
type innerGreeter struct{ msg string }

func (g *innerGreeter) Greet() string { return g.msg }

type outerGreeter struct {
	inner innerGreeter
	msg   string
}

func (g *outerGreeter) Greet() string { return g.msg }

func TestCollectKeepsValuesThatShareAnAddressByLayout(t *testing.T) {
	outer := &outerGreeter{inner: innerGreeter{msg: "inner"}, msg: "outer"}
	if !sameAddress(outer, &outer.inner) {
		t.Fatal("a struct and its first field must share an address")
	}
	root := NewModule("root")
	Contribute[greeter](root, outer)
	Contribute[greeter](root, &outer.inner)

	got := greetings(Collect[greeter](root))
	want := []string{"outer", "inner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Collect merged two values that share an address: got %v, want %v", got, want)
	}
}
