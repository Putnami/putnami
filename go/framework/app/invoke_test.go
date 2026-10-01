package app

import (
	"testing"

	"go.putnami.dev/inject"
)

// invokerNilDep is an interface so it can be registered as an untyped nil:
// ProvideInstance[invokerNilDep](nil) stores a nil `any`, which cc.Get returns
// as (nil, nil).
type invokerNilDep interface{ marker() }

// TestRunInvokers_NilResolvedValueDoesNotPanic registers a token that resolves
// to untyped nil and asserts the invoker receives a typed nil argument instead
// of panicking on reflect.ValueOf(nil) in fn.Call. The health-probe invoker
// (health_func.go) guards the identical case.
func TestRunInvokers_NilResolvedValueDoesNotPanic(t *testing.T) {
	cc := inject.NewContainerContext("invoke-nil")
	if err := cc.Register(inject.ProvideInstance[invokerNilDep](nil)); err != nil {
		t.Fatalf("register nil dep: %v", err)
	}
	if err := cc.Start(); err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer cc.Close() //nolint:errcheck

	called := false
	gotNil := false
	a := New("invoke-nil")
	a.InvokeFunc(func(dep invokerNilDep) {
		called = true
		gotNil = dep == nil
	})

	if err := a.runInvokers(cc); err != nil {
		t.Fatalf("runInvokers() error = %v", err)
	}
	if !called {
		t.Fatal("invoker was not called")
	}
	if !gotNil {
		t.Fatal("invoker received a non-nil dep, want the typed zero (nil)")
	}
}
