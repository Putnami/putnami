package inject

import (
	"context"
	"testing"
)

func childCount(c *Container) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.children)
}

// A closed child leaves its parent, so a parent that outlives many short-lived
// children (one scope per request) does not retain them.
func TestClosedChildLeavesParent(t *testing.T) {
	root := NewContainer("root", nil)
	first := root.CreateChild("first")
	second := root.CreateChild("second")
	third := root.CreateChild("third")

	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
	if got := childCount(root); got != 2 {
		t.Fatalf("children after closing one = %d, want 2", got)
	}
	root.mu.RLock()
	order := []*Container{root.children[0], root.children[1]}
	root.mu.RUnlock()
	if order[0] != first || order[1] != third {
		t.Fatal("the remaining children lost their creation order")
	}

	if err := second.Close(); err != nil {
		t.Fatalf("second close of a closed child: %v", err)
	}
	if got := childCount(root); got != 2 {
		t.Fatalf("children after a repeated close = %d, want 2", got)
	}
}

// A parent that closes still closes the children that are open, and ends with
// none.
func TestParentCloseClosesOpenChildren(t *testing.T) {
	root := NewContainer("root", nil)
	var closed []string
	for _, name := range []string{"a", "b"} {
		child := root.CreateChild(name)
		token := Named[string](name)
		if err := child.Register(Provide(token, func(Resolver) (any, error) { return name, nil }, WithOnClose(func() error {
			closed = append(closed, name)
			return nil
		}))); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if _, err := child.Get(token); err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
	}

	if err := root.Close(); err != nil {
		t.Fatalf("close root: %v", err)
	}
	if len(closed) != 2 || closed[0] != "a" || closed[1] != "b" {
		t.Fatalf("closed children = %v, want [a b]", closed)
	}
	if got := childCount(root); got != 0 {
		t.Fatalf("children after closing the root = %d, want 0", got)
	}
}

// Every scope a started context creates is gone from its root once closed.
func TestScopesDoNotAccumulateOnRoot(t *testing.T) {
	cc := NewContainerContext("app")
	if err := cc.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cc.Close() }()

	for range 1000 {
		if err := cc.Scope(context.Background(), func(context.Context, ScopeContext) error { return nil }); err != nil {
			t.Fatalf("scope: %v", err)
		}
	}
	if got := childCount(cc.root); got != 0 {
		t.Fatalf("root children after 1000 closed scopes = %d, want 0", got)
	}
}
