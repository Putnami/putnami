package inject

import (
	"sync"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestContainer_ConcurrentGet_SingletonCalledOnce(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "concurrent-first-resolution-builds-once")
	var callCount atomic.Int32

	cc := NewContainerContext("test")
	token := Named[string]("value")
	if err := cc.Register(Provide(token, func(_ Resolver) (any, error) {
		callCount.Add(1)
		return "hello", nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			val, err := cc.Get(token)
			if err != nil {
				errs <- err
				return
			}
			if val != "hello" {
				t.Errorf("got %v, want hello", val)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	if count := callCount.Load(); count != 1 {
		t.Errorf("factory called %d times, want 1 (singleton)", count)
	}
}

func TestContainer_ConcurrentGet_MultipleTokens(t *testing.T) {
	cc := NewContainerContext("test")

	tokenA := Named[string]("a")
	tokenB := Named[int]("b")
	tokenC := Named[bool]("c")

	if err := cc.Register(ProvideValue(tokenA, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(ProvideValue(tokenB, 42)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(ProvideValue(tokenC, true)); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				val, err := cc.Get(tokenA)
				if err != nil || val != "alpha" {
					t.Errorf("tokenA: got %v, err %v", val, err)
				}
			case 1:
				val, err := cc.Get(tokenB)
				if err != nil || val != 42 {
					t.Errorf("tokenB: got %v, err %v", val, err)
				}
			case 2:
				val, err := cc.Get(tokenC)
				if err != nil || val != true {
					t.Errorf("tokenC: got %v, err %v", val, err)
				}
			}
		}()
	}

	wg.Wait()
}

func TestContainer_ConcurrentList(t *testing.T) {
	cc := NewContainerContext("test")

	tag := Tagged[string]("items")

	if err := cc.Register(Provide(Named[string]("x"), func(_ Resolver) (any, error) {
		return "x-val", nil
	}, WithTags("items"))); err != nil {
		t.Fatal(err)
	}
	if err := cc.Register(Provide(Named[string]("y"), func(_ Resolver) (any, error) {
		return "y-val", nil
	}, WithTags("items"))); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			results, err := cc.List(tag)
			if err != nil {
				t.Errorf("List error: %v", err)
				return
			}
			if len(results) != 2 {
				t.Errorf("List returned %d items, want 2", len(results))
			}
		}()
	}

	wg.Wait()
}

func TestContainer_ConcurrentChildGet(t *testing.T) {
	root := NewContainer("root", nil)
	rootToken := Named[string]("shared")
	if err := root.Register(ProvideValue(rootToken, "from-root")); err != nil {
		t.Fatal(err)
	}

	child := root.CreateChild("child")
	childToken := Named[int]("local")
	if err := child.Register(ProvideValue(childToken, 99)); err != nil {
		t.Fatal(err)
	}

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				val, err := child.Get(rootToken)
				if err != nil || val != "from-root" {
					t.Errorf("child.Get(rootToken): got %v, err %v", val, err)
				}
			} else {
				val, err := child.Get(childToken)
				if err != nil || val != 99 {
					t.Errorf("child.Get(childToken): got %v, err %v", val, err)
				}
			}
		}()
	}

	wg.Wait()
}
