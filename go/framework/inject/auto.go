package inject

import (
	"fmt"
	"reflect"

	"go.putnami.dev/errors"
)

// AutoProvide creates a Registration from a constructor function.
// The function's input parameters are automatically resolved by type
// from the DI container, and the return value is registered by type.
//
// This provides an fx-inspired API where types are the keys, eliminating
// the need for explicit tokens in common cases.
//
// Supported constructor signatures:
//
//	func() T
//	func() (T, error)
//	func(dep1 A, dep2 B) T
//	func(dep1 A, dep2 B) (T, error)
//
// Example:
//
//	// Register a constructor that auto-resolves dependencies
//	inject.AutoProvide(NewUserService)  // func(db *sql.DB) *UserService
//
//	// With options
//	inject.AutoProvide(NewDB, inject.WithOnClose(func() error { return db.Close() }))
func AutoProvide(constructor any, opts ...Option) Registration {
	fn := reflect.ValueOf(constructor)
	ft := fn.Type()

	if ft.Kind() != reflect.Func {
		panic(fmt.Sprintf("inject.AutoProvide: expected a function, got %T", constructor))
	}

	// Validate return values: must be (T) or (T, error)
	numOut := ft.NumOut()
	if numOut == 0 || numOut > 2 {
		panic(fmt.Sprintf("inject.AutoProvide: constructor must return (T) or (T, error), got %d return values", numOut))
	}
	if numOut == 2 {
		errType := reflect.TypeOf((*error)(nil)).Elem()
		if !ft.Out(1).Implements(errType) {
			panic(fmt.Sprintf("inject.AutoProvide: second return value must be error, got %s", ft.Out(1)))
		}
	}

	// Create token from the first return type
	outType := ft.Out(0)
	token := classToken{typ: outType}

	// Collect dependency tokens from input parameters
	deps := make([]Token, ft.NumIn())
	for i := 0; i < ft.NumIn(); i++ {
		deps[i] = classToken{typ: ft.In(i)}
	}

	factory := func(r Resolver) (any, error) {
		// Resolve all input parameters
		args := make([]reflect.Value, ft.NumIn())
		for i := 0; i < ft.NumIn(); i++ {
			depToken := classToken{typ: ft.In(i)}
			val, err := r.Resolve(depToken)
			if err != nil {
				return nil, errors.Wrapf(err, CodeNotRegistered,
					"resolving param "+fmt.Sprintf("%d", i)+" ("+ft.In(i).String()+")",
					errors.Int("param", i),
					errors.String("type", ft.In(i).String()),
				)
			}
			args[i] = reflect.ValueOf(val)
		}

		// Call the constructor
		results := fn.Call(args)

		// Handle (T, error) return
		if numOut == 2 && !results[1].IsNil() {
			if errVal, ok := results[1].Interface().(error); ok {
				return nil, errVal
			}
			return nil, errors.Newf(CodeFactoryFailed,
				"constructor returned non-nil error value of unexpected type %T", results[1].Interface())
		}

		return results[0].Interface(), nil
	}

	p := Provider{
		Token:      token,
		Factory:    factory,
		Scope:      Singleton,
		Visibility: Public,
		Deps:       deps,
	}
	for _, opt := range opts {
		opt(&p)
	}
	return Registration{provider: p}
}

// AutoProvideScoped is like AutoProvide but registers the provider with Scoped lifetime.
// Scoped providers create a new instance per scope (e.g., per HTTP request).
//
// Example:
//
//	// New repository per request, backed by a shared DB pool
//	inject.AutoProvideScoped(NewUserRepository)  // func(db *sql.DB) *UserRepository
func AutoProvideScoped(constructor any, opts ...Option) Registration {
	reg := AutoProvide(constructor, opts...)
	reg.provider.Scope = Scoped
	return reg
}

// AutoProvideNamed is like AutoProvide but registers under a named token instead of a class token.
// Use this when you need multiple providers of the same type.
//
// Example:
//
//	inject.AutoProvideNamed("primary", NewPrimaryDB)   // func(cfg Config) *sql.DB
//	inject.AutoProvideNamed("readonly", NewReadonlyDB)  // func(cfg Config) *sql.DB
func AutoProvideNamed(name string, constructor any, opts ...Option) Registration {
	fn := reflect.ValueOf(constructor)
	ft := fn.Type()

	if ft.Kind() != reflect.Func {
		panic(fmt.Sprintf("inject.AutoProvideNamed: expected a function, got %T", constructor))
	}

	numOut := ft.NumOut()
	if numOut == 0 || numOut > 2 {
		panic(fmt.Sprintf("inject.AutoProvideNamed: constructor must return (T) or (T, error), got %d return values", numOut))
	}
	if numOut == 2 {
		errType := reflect.TypeOf((*error)(nil)).Elem()
		if !ft.Out(1).Implements(errType) {
			panic(fmt.Sprintf("inject.AutoProvideNamed: second return value must be error, got %s", ft.Out(1)))
		}
	}

	outType := ft.Out(0)
	token := namedToken{name: name, typ: outType}

	deps := make([]Token, ft.NumIn())
	for i := 0; i < ft.NumIn(); i++ {
		deps[i] = classToken{typ: ft.In(i)}
	}

	factory := func(r Resolver) (any, error) {
		args := make([]reflect.Value, ft.NumIn())
		for i := 0; i < ft.NumIn(); i++ {
			depToken := classToken{typ: ft.In(i)}
			val, err := r.Resolve(depToken)
			if err != nil {
				return nil, errors.Wrapf(err, CodeNotRegistered,
					"resolving param "+fmt.Sprintf("%d", i)+" ("+ft.In(i).String()+")",
					errors.Int("param", i),
					errors.String("type", ft.In(i).String()),
				)
			}
			args[i] = reflect.ValueOf(val)
		}

		results := fn.Call(args)
		if numOut == 2 && !results[1].IsNil() {
			if errVal, ok := results[1].Interface().(error); ok {
				return nil, errVal
			}
			return nil, errors.Newf(CodeFactoryFailed,
				"constructor returned non-nil error value of unexpected type %T", results[1].Interface())
		}
		return results[0].Interface(), nil
	}

	p := Provider{
		Token:      token,
		Factory:    factory,
		Scope:      Singleton,
		Visibility: Public,
		Deps:       deps,
	}
	for _, opt := range opts {
		opt(&p)
	}
	return Registration{provider: p}
}

// ProvideInstance creates a Registration for a pre-built value, keyed by its type.
// This is the constructor-based equivalent of ProvideValue but uses auto-typing.
//
// Example:
//
//	cfg := &AppConfig{Port: 8080}
//	inject.ProvideInstance(cfg)  // registered as *AppConfig
func ProvideInstance[T any](value T, opts ...Option) Registration {
	token := TokenOf[T]()
	return ProvideValue(token, value, opts...)
}

// ProvideInstanceOf creates a Registration for a pre-built value using runtime
// type reflection. Unlike ProvideInstance, this works without generic type
// parameters, making it suitable for variadic APIs like Application.ProvideInstance().
//
// Example:
//
//	inject.ProvideInstanceOf(myLogger)  // registered by reflect.TypeOf(myLogger)
func ProvideInstanceOf(value any, opts ...Option) Registration {
	token := TokenOf2(reflect.TypeOf(value))
	return ProvideValue(token, value, opts...)
}
