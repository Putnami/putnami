package app

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

type healthFuncDep struct {
	calls int
}

func TestHealthFuncResolvesDependenciesFromOwnerContainer(t *testing.T) {
	root := NewModule("root")
	dep := &healthFuncDep{}
	cc := inject.NewContainerContext("test")
	if err := cc.Register(inject.ProvideInstance(dep)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close() //nolint:errcheck
	root.cc = cc

	Contribute[HealthChecker](root, HealthFunc("dep", func(_ context.Context, got *healthFuncDep) error {
		got.calls++
		return nil
	}))

	checkers := Collect[HealthChecker](root)
	if len(checkers) != 1 {
		t.Fatalf("Collect[HealthChecker] = %d, want 1", len(checkers))
	}
	if err := checkers[0].CheckHealth(context.Background()); err != nil {
		t.Fatalf("CheckHealth() error = %v", err)
	}
	if dep.calls != 1 {
		t.Fatalf("dep.calls = %d, want 1", dep.calls)
	}
}

func TestHealthFuncContributedAsReadinessDoesNotRegisterAsHealth(t *testing.T) {
	root := NewModule("root")
	Contribute[ReadinessChecker](root, HealthFunc("warm", func(context.Context) error {
		return nil
	}))

	if got := Collect[HealthChecker](root); len(got) != 0 {
		t.Fatalf("Collect[HealthChecker] = %d, want 0", len(got))
	}
	if got := Collect[ReadinessChecker](root); len(got) != 1 {
		t.Fatalf("Collect[ReadinessChecker] = %d, want 1", len(got))
	}
}

func TestHealthFuncReportsMissingDependency(t *testing.T) {
	root := NewModule("root")
	cc := inject.NewContainerContext("test")
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close() //nolint:errcheck
	root.cc = cc

	Contribute[HealthChecker](root, HealthFunc("dep", func(_ context.Context, _ *healthFuncDep) error {
		return nil
	}))

	err := Collect[HealthChecker](root)[0].CheckHealth(context.Background())
	if err == nil {
		t.Fatal("CheckHealth() error = nil, want missing dependency error")
	}
	// The migration to structured errors must let callers match by a stable Code
	// (app.health) instead of scraping message text.
	if !errors.Is(err, CodeHealth) {
		t.Fatalf("CheckHealth() error code = %v, want %v", errors.GetCode(err), CodeHealth)
	}
	// The wrapped cause detail (which dependency failed to resolve) must survive
	// the wrap so the diagnostic is not lost.
	if !strings.Contains(err.Error(), "*app.healthFuncDep") {
		t.Fatalf("CheckHealth() error = %v, want missing dependency detail", err)
	}
	// The probe name must survive in the rendered message, not only in structured
	// attrs that plain Error()/%v sinks never surface.
	if !strings.Contains(err.Error(), `health probe "dep"`) {
		t.Fatalf("CheckHealth() error = %v, want probe name in rendered message", err)
	}
}
