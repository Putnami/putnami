package api

import "reflect"

// Type returns the reflect.Type for the given Go type. It is a thin wrapper around
// `reflect.TypeFor[T]()` so callers can write `Type[User]()` consistently alongside the
// other api/* helpers without importing reflect themselves.
//
//	api.Endpoint("GET", "/users").
//	    Params(api.Type[UserParams]()).
//	    Returns(api.Type[User]()).
//	    Handle(getUser)
func Type[T any]() reflect.Type {
	return reflect.TypeFor[T]()
}
