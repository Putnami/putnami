package app

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

var (
	contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errorType   = reflect.TypeOf((*error)(nil)).Elem()
)

type injectedProbeDep struct {
	token inject.Token
	typ   reflect.Type
}

type injectedProbe struct {
	name         string
	fn           reflect.Value
	deps         []injectedProbeDep
	owner        *Module
	sigErr       error
	sourceFile   string
	sourceSymbol string
}

// HealthFunc returns a named health/readiness probe backed by a function whose
// dependencies are resolved from the owning module's DI container.
//
// The function must have this shape:
//
//	func(context.Context, Dep1, Dep2, ...) error
//
// Contribute it under the capability you want exposed:
//
//	app.Contribute[app.HealthChecker](owner, app.HealthFunc("db", checkDB))
//	app.Contribute[app.ReadinessChecker](owner, app.HealthFunc("warm", checkWarm))
//
// Dependencies are resolved by type on each probe execution, so the probe always
// observes the current container for the application lifecycle.
func HealthFunc(name string, fn any) *HealthProbe {
	return &HealthProbe{probe: newInjectedProbe(name, fn)}
}

// ReadinessFunc is a readability alias for HealthFunc when registering a probe
// as app.ReadinessChecker.
func ReadinessFunc(name string, fn any) *HealthProbe {
	return HealthFunc(name, fn)
}

// HealthProbe is a named, DI-injected probe that can be contributed as either
// HealthChecker or ReadinessChecker.
type HealthProbe struct {
	probe *injectedProbe
}

func (h *HealthProbe) capabilityDeclarationHint() (string, string, bool) {
	if h == nil || h.probe == nil || h.probe.sourceFile == "" {
		return "", "", false
	}
	return h.probe.sourceFile, h.probe.sourceSymbol, true
}

// Name returns the probe identifier surfaced in health/readiness responses.
func (h *HealthProbe) Name() string { return h.probe.name }

// CheckHealth runs the injected probe as a liveness check.
func (h *HealthProbe) CheckHealth(ctx context.Context) error {
	return h.probe.call(ctx)
}

// CheckReadiness runs the injected probe as a readiness check.
func (h *HealthProbe) CheckReadiness(ctx context.Context) error {
	return h.probe.call(ctx)
}

func (h *HealthProbe) bindOwner(owner *Module) {
	h.probe.owner = owner
}

func newInjectedProbe(name string, fn any) *injectedProbe {
	p := &injectedProbe{name: name}
	if fn == nil {
		p.sigErr = errors.Newf(CodeHealth, "health probe %q must be a function", name)
		return p
	}

	rv := reflect.ValueOf(fn)
	rt := rv.Type()
	if rt.Kind() != reflect.Func {
		p.sigErr = errors.Newf(CodeHealth, "health probe %q must be a function, got %s", name, rt)
		return p
	}
	if rt.NumIn() == 0 || rt.In(0) != contextType {
		p.sigErr = errors.Newf(CodeHealth, "health probe %q must accept context.Context as its first parameter", name)
		return p
	}
	if rt.NumOut() != 1 || rt.Out(0) != errorType {
		p.sigErr = errors.Newf(CodeHealth, "health probe %q must return exactly one error", name)
		return p
	}

	p.fn = rv
	if fn := runtime.FuncForPC(rv.Pointer()); fn != nil {
		p.sourceFile, _ = fn.FileLine(rv.Pointer())
		p.sourceSymbol = fn.Name()
	}
	p.deps = make([]injectedProbeDep, 0, rt.NumIn()-1)
	for i := 1; i < rt.NumIn(); i++ {
		depType := rt.In(i)
		p.deps = append(p.deps, injectedProbeDep{
			token: inject.TokenOf2(depType),
			typ:   depType,
		})
	}
	return p
}

func (p *injectedProbe) call(ctx context.Context) error {
	if p.sigErr != nil {
		return p.sigErr
	}
	if p.owner == nil {
		return errors.Newf(CodeHealth, "health probe %q is not bound to a module; register it with app.Contribute", p.name)
	}
	cc := p.owner.Container()
	if cc == nil {
		return errors.Newf(CodeHealth, "health probe %q cannot resolve dependencies before DI is available", p.name)
	}

	args := make([]reflect.Value, 0, len(p.deps)+1)
	args = append(args, reflect.ValueOf(ctx))
	for _, dep := range p.deps {
		value, err := cc.Get(dep.token)
		if err != nil {
			// Keep the probe/dep identifiers in the rendered message: Error() renders
			// only code+message+cause and never the structured attrs, so attrs alone
			// would hide which probe/dep failed from plain Error()/%v/log sinks.
			return errors.Wrapf(err, CodeHealth,
				fmt.Sprintf("health probe %q resolve %s failed", p.name, dep.token.Name()),
				errors.String("probe", p.name), errors.String("dep", dep.token.Name()))
		}
		if value == nil {
			args = append(args, reflect.Zero(dep.typ))
			continue
		}
		rv := reflect.ValueOf(value)
		if !rv.Type().AssignableTo(dep.typ) {
			return errors.Newf(CodeHealth, "health probe %q resolve %s: got %s", p.name, dep.token.Name(), rv.Type())
		}
		args = append(args, rv)
	}

	out := p.fn.Call(args)
	if out[0].IsNil() {
		return nil
	}
	err, ok := out[0].Interface().(error)
	if !ok {
		return errors.Newf(CodeHealth, "health probe %q returned %s, want error", p.name, out[0].Type())
	}
	return err
}
