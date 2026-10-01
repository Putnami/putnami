package app

import "reflect"

type contribution struct {
	target reflect.Type
	value  any
}

type ownerBinder interface {
	bindOwner(owner *Module)
}

type collectedValue[T any] struct {
	Value T
	Owner *Module
}

func collectTarget[T any]() reflect.Type {
	var zero T
	return reflect.TypeOf(&zero).Elem()
}

// Contribute records a capability value on a module so that Collect[T] can
// discover it across the module tree, alongside plugins that satisfy T by
// implementing its interface.
//
// Use this when a single plugin owns more than one instance of a capability
// — e.g. a plugin managing both a connection pool and a cache that each want
// their own named HealthChecker probe. A plugin can only implement a given
// capability interface once (one method set, one Name); Contribute lifts that
// limit by letting it register additional, independently-keyed instances:
//
//	app.Contribute[app.HealthChecker](owner, poolProbe)
//	app.Contribute[app.HealthChecker](owner, cacheProbe)
//
// Contributions are additive to interface discovery, not a replacement: a
// plugin that implements HealthChecker is still collected without any
// Contribute call.
func Contribute[T any](owner *Module, v T) {
	if binder, ok := any(v).(ownerBinder); ok {
		binder.bindOwner(owner)
	}
	owner.contributions = append(owner.contributions, contribution{
		target: collectTarget[T](),
		value:  v,
	})
}

// Collect walks the module tree rooted at root once and returns every
// capability of type T it finds, from two sources:
//
//   - plugins that satisfy T by implementing its interface, and
//   - values explicitly registered with Contribute[T].
//
// Results are returned in a deterministic order: modules in depth-first
// pre-order (root before descendants), and within each module its plugins
// before its contributions, each in registration order. Pointer values are
// de-duplicated by pointer identity, so contributing a plugin that is also
// registered via Use does not yield it twice; non-pointer values are never
// de-duplicated (two distinct but equal values are both returned).
//
// Collect replaces the hand-written "walk CollectPlugins, type-assert against
// one capability" loops: a new capability needs only its interface and a
// Collect[Cap] call at the point of use, with no new walker.
func Collect[T any](root *Module) []T {
	owned := collectWithOwner[T](root)
	result := make([]T, 0, len(owned))
	for _, value := range owned {
		result = append(result, value.Value)
	}
	return result
}

// collectWithOwner retains the module that supplied each value while matching
// Collect's ordering, contribution support, and pointer de-duplication exactly.
// Build-time projections use the owner to attribute native facts to the right
// feature scope without changing the public Collect API.
func collectWithOwner[T any](root *Module) []collectedValue[T] {
	var result []collectedValue[T]
	seen := make(map[uintptr]struct{})
	target := collectTarget[T]()
	add := func(owner *Module, v T) {
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && !rv.IsNil() {
			ptr := rv.Pointer()
			if _, dup := seen[ptr]; dup {
				return
			}
			seen[ptr] = struct{}{}
		}
		result = append(result, collectedValue[T]{Value: v, Owner: owner})
	}
	for _, m := range root.CollectModules() {
		for _, p := range m.plugins {
			if t, ok := any(p).(T); ok {
				add(m, t)
			}
		}
		for _, c := range m.contributions {
			if !c.target.AssignableTo(target) {
				continue
			}
			if t, ok := c.value.(T); ok {
				add(m, t)
			}
		}
	}
	return result
}
