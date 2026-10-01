package inject

import (
	"testing"

	colla "go.putnami.dev/inject/internalcoll/a"
	collb "go.putnami.dev/inject/internalcoll/b"
)

// TestTokenKey_NoCollisionAcrossSamePackageName pins globally unique token
// keys. Two Repository interfaces declared in different subpackages that both
// have package name "internal" must not collide during registration.
func TestTokenKey_NoCollisionAcrossSamePackageName(t *testing.T) {
	tokA := TokenOf[colla.Repository]()
	tokB := TokenOf[collb.Repository]()

	if tokA.Key() == tokB.Key() {
		t.Fatalf("class tokens for distinct types collide: %q == %q", tokA.Key(), tokB.Key())
	}

	namedA := Named[colla.Repository]("primary")
	namedB := Named[collb.Repository]("primary")
	if namedA.Key() == namedB.Key() {
		t.Fatalf("named tokens for distinct types collide: %q == %q", namedA.Key(), namedB.Key())
	}

	tagA := Tagged[colla.Repository]("repo")
	tagB := Tagged[collb.Repository]("repo")
	if tagA.Key() == tagB.Key() {
		t.Fatalf("tag selectors for distinct types collide: %q == %q", tagA.Key(), tagB.Key())
	}
}

// TestContainerRegister_SameNamedSubpackages reproduces the registry
// scenario: two interfaces named Repository, each in its own package
// internal, registered in the same container.
func TestContainerRegister_SameNamedSubpackages(t *testing.T) {
	c := NewContainer("test", nil)

	tokA := TokenOf[colla.Repository]()
	tokB := TokenOf[collb.Repository]()

	implA := &fakeRepoA{id: "a"}
	implB := &fakeRepoB{name: "b"}

	if err := c.Register(ProvideValue(tokA, colla.Repository(implA))); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := c.Register(ProvideValue(tokB, collb.Repository(implB))); err != nil {
		t.Fatalf("register B: %v", err)
	}

	gotA, err := c.Get(tokA)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	gotB, err := c.Get(tokB)
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}

	if gotA.(colla.Repository).ID() != "a" {
		t.Errorf("A: expected ID=a, got %q", gotA.(colla.Repository).ID())
	}
	if gotB.(collb.Repository).Name() != "b" {
		t.Errorf("B: expected Name=b, got %q", gotB.(collb.Repository).Name())
	}
}

type fakeRepoA struct{ id string }

func (f *fakeRepoA) ID() string { return f.id }

type fakeRepoB struct{ name string }

func (f *fakeRepoB) Name() string { return f.name }

// TestTokenKey_NoCollisionOnPointerToSamePackageName pins the same uniqueness
// contract for unnamed pointer types such as *colla.Service and *collb.Service.
func TestTokenKey_NoCollisionOnPointerToSamePackageName(t *testing.T) {
	tokA := TokenOf[*colla.Service]()
	tokB := TokenOf[*collb.Service]()

	if tokA.Key() == tokB.Key() {
		t.Fatalf("class tokens for distinct pointer types collide: %q == %q", tokA.Key(), tokB.Key())
	}

	c := NewContainer("test", nil)
	implA := &colla.Service{ID: "a"}
	implB := &collb.Service{Name: "b"}

	if err := c.Register(ProvideValue(tokA, implA)); err != nil {
		t.Fatalf("register *A: %v", err)
	}
	if err := c.Register(ProvideValue(tokB, implB)); err != nil {
		t.Fatalf("register *B: %v", err)
	}

	gotA, err := c.Get(tokA)
	if err != nil {
		t.Fatalf("resolve *A: %v", err)
	}
	gotB, err := c.Get(tokB)
	if err != nil {
		t.Fatalf("resolve *B: %v", err)
	}
	if gotA.(*colla.Service).ID != "a" {
		t.Errorf("*A: expected ID=a, got %q", gotA.(*colla.Service).ID)
	}
	if gotB.(*collb.Service).Name != "b" {
		t.Errorf("*B: expected Name=b, got %q", gotB.(*collb.Service).Name)
	}
}

// TestTokenKey_NoCollisionOnSliceOfSamePackageName covers the slice variant
// of the unnamed-composite collision. Slices are unnamed, so []colla.Service
// and []collb.Service both stringified to "[]internal.Service" pre-round-2.
func TestTokenKey_NoCollisionOnSliceOfSamePackageName(t *testing.T) {
	tokA := TokenOf[[]colla.Service]()
	tokB := TokenOf[[]collb.Service]()

	if tokA.Key() == tokB.Key() {
		t.Fatalf("class tokens for distinct slice types collide: %q == %q", tokA.Key(), tokB.Key())
	}

	c := NewContainer("test", nil)
	if err := c.Register(ProvideValue(tokA, []colla.Service{{ID: "a1"}, {ID: "a2"}})); err != nil {
		t.Fatalf("register []A: %v", err)
	}
	if err := c.Register(ProvideValue(tokB, []collb.Service{{Name: "b1"}})); err != nil {
		t.Fatalf("register []B: %v", err)
	}

	gotA, err := c.Get(tokA)
	if err != nil {
		t.Fatalf("resolve []A: %v", err)
	}
	gotB, err := c.Get(tokB)
	if err != nil {
		t.Fatalf("resolve []B: %v", err)
	}
	if got := gotA.([]colla.Service); len(got) != 2 || got[0].ID != "a1" {
		t.Errorf("[]A: unexpected value %+v", got)
	}
	if got := gotB.([]collb.Service); len(got) != 1 || got[0].Name != "b1" {
		t.Errorf("[]B: unexpected value %+v", got)
	}
}

// TestQualifiedName_CompositeBranchesAreDistinct exercises the map, array,
// channel, and func branches of qualifiedName (which produces every token's
// identity key). The colla/collb fixtures share the short package name
// "internal", so reflect.Type.String() renders colla.Service and collb.Service
// identically — any branch that fell back to String() instead of recursing
// would collide, silently mis-injecting one type's provider for the other.
func TestQualifiedName_CompositeBranchesAreDistinct(t *testing.T) {
	distinct := func(name string, a, b Token) {
		t.Helper()
		if a.Key() == b.Key() {
			t.Errorf("%s: keys collide: %q == %q", name, a.Key(), b.Key())
		}
	}

	// Map: the colliding short name may appear as the value or the key type.
	distinct("map value", TokenOf[map[string]colla.Service](), TokenOf[map[string]collb.Service]())
	distinct("map key", TokenOf[map[colla.Service]int](), TokenOf[map[collb.Service]int]())

	// Array: distinct element type, and array vs slice of the same element.
	distinct("array element", TokenOf[[2]colla.Service](), TokenOf[[2]collb.Service]())
	distinct("array vs slice", TokenOf[[2]colla.Service](), TokenOf[[]colla.Service]())
	distinct("array length", TokenOf[[2]colla.Service](), TokenOf[[3]colla.Service]())

	// Channel: distinct element type and distinct direction.
	distinct("chan element", TokenOf[chan colla.Service](), TokenOf[chan collb.Service]())
	distinct("chan vs recv-only", TokenOf[chan colla.Service](), TokenOf[<-chan colla.Service]())
	distinct("chan vs send-only", TokenOf[chan colla.Service](), TokenOf[chan<- colla.Service]())
	distinct("recv-only vs send-only", TokenOf[<-chan colla.Service](), TokenOf[chan<- colla.Service]())

	// Func: distinct parameter type, result type, and variadic vs slice param.
	distinct("func param", TokenOf[func(colla.Service)](), TokenOf[func(collb.Service)]())
	distinct("func result", TokenOf[func() colla.Service](), TokenOf[func() collb.Service]())
	distinct("variadic param", TokenOf[func(...colla.Service)](), TokenOf[func(...collb.Service)]())
	distinct("variadic vs slice param", TokenOf[func(...colla.Service)](), TokenOf[func([]colla.Service)]())
}
