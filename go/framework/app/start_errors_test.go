package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// startFailPlugin is a Starter whose Start always fails with a fixed error.
type startFailPlugin struct {
	name string
	err  error
}

func (p *startFailPlugin) Name() string                             { return p.name }
func (p *startFailPlugin) Start(_ context.Context, _ *Module) error { return p.err }

// When several plugins fail to start together, every failure must be reported,
// rather than whichever result happens to arrive first.
func TestStartPlugins_AggregatesAllStartErrors(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "startup-failure", "start-errors-aggregated")
	a := New("agg-start-errors")
	a.Use(&startFailPlugin{name: "alpha", err: fmt.Errorf("boom-alpha")})
	a.Use(&startFailPlugin{name: "beta", err: fmt.Errorf("boom-beta")})

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail when plugins fail to start")
	}

	msg := err.Error()
	if !strings.Contains(msg, "boom-alpha") {
		t.Errorf("aggregate error missing first plugin failure: %q", msg)
	}
	if !strings.Contains(msg, "boom-beta") {
		t.Errorf("aggregate error missing sibling plugin failure (it was dropped): %q", msg)
	}
}
