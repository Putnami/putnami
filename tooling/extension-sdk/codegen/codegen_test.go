package codegen

import (
	"errors"
	"testing"
)

type stubVisitor struct {
	name   string
	result *Result
	err    error
}

func (s *stubVisitor) Name() string                         { return s.name }
func (s *stubVisitor) Visit(_ *Generation) (*Result, error) { return s.result, s.err }

func TestRegisterAndVisitorsPreservesOrder(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	a := &stubVisitor{name: "a"}
	b := &stubVisitor{name: "b"}
	c := &stubVisitor{name: "c"}

	Register(a)
	Register(b)
	Register(c)

	got := Visitors()
	if len(got) != 3 {
		t.Fatalf("expected 3 visitors, got %d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Name() != want {
			t.Errorf("position %d: want %q, got %q", i, want, got[i].Name())
		}
	}
}

func TestRegisterNilIsNoOp(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	Register(nil)

	if got := Visitors(); len(got) != 0 {
		t.Fatalf("expected empty registry, got %d entries", len(got))
	}
}

func TestVisitorsReturnsCopy(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	Register(&stubVisitor{name: "a"})

	snapshot := Visitors()
	snapshot[0] = &stubVisitor{name: "mutated"}

	if got := Visitors(); got[0].Name() != "a" {
		t.Errorf("registry mutated through returned slice: got %q", got[0].Name())
	}
}

func TestResultIsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		result *Result
		want   bool
	}{
		{"nil result", nil, true},
		{"zero result", &Result{}, true},
		{"with schema file", &Result{SchemaFiles: []SchemaFile{{RelPath: "x"}}}, false},
		{"with export", &Result{Exports: map[string]string{"k": "v"}}, false},
		{"with asset", &Result{Assets: map[string]string{"k": "v"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVisitorErrorPropagates(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	want := errors.New("boom")
	Register(&stubVisitor{name: "fails", err: want})

	visitors := Visitors()
	_, got := visitors[0].Visit(nil)
	if !errors.Is(got, want) {
		t.Fatalf("expected error %v, got %v", want, got)
	}
}
