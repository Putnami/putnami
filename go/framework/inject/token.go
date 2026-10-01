// Package inject provides a hierarchical dependency injection container
// with support for named tokens, tag-based multi-resolution, scoped providers,
// and circular dependency detection.
package inject

import (
	"fmt"
	"reflect"
	"strings"
)

// Token identifies a dependency in the container. Tokens are used to register
// and resolve providers. Two tokens are equal if they have the same Key().
type Token interface {
	// Key returns a unique string identifier for this token.
	Key() string
	// Name returns a human-readable name for error messages.
	Name() string
}

// classToken identifies a dependency by its Go type using reflect.Type.
type classToken struct {
	typ reflect.Type
}

// namedToken identifies a dependency by a string name and Go type.
type namedToken struct {
	name string
	typ  reflect.Type
}

// tagSelector selects multiple providers that have a matching tag.
type tagSelector struct {
	tag string
	typ reflect.Type
}

// TokenOf creates a class token for the given type parameter.
// Class tokens use the reflect.Type as their identity.
//
//	token := inject.TokenOf[MyService]()
func TokenOf[T any]() Token {
	var zero T
	return classToken{typ: reflect.TypeOf(&zero).Elem()}
}

// Named creates a named token with a string key and type parameter.
// Named tokens allow multiple providers of the same type with different names.
//
//	dbToken := inject.Named[*sql.DB]("primary")
func Named[T any](name string) Token {
	var zero T
	return namedToken{name: name, typ: reflect.TypeOf(&zero).Elem()}
}

// Tagged creates a tag selector for multi-resolution.
// Use with Container.List() to resolve all providers matching a tag.
//
//	plugins := container.List(inject.Tagged[Plugin]("http"))
func Tagged[T any](tag string) FilterOptions {
	var zero T
	return tagSelector{tag: tag, typ: reflect.TypeOf(&zero).Elem()}
}

// FilterOptions is used with Container.List() to select multiple providers.
type FilterOptions interface {
	Token
	// Tag returns the tag string to filter by.
	Tag() string
}

// qualifiedName returns a string that is unique per Go type identity,
// suitable for use as a map key. reflect.Type.String() uses the short
// package name (e.g. "internal.Repository") and is explicitly documented
// as not unique, so we cannot use it directly — two named types from
// different packages whose final path segment is the same would collide.
//
// For named types we use PkgPath() + "." + Name(). For composite unnamed
// types (pointer, slice, array, map, chan, func) we recurse into element
// types so that, for example, *pkg/internal.Service and *version/internal.Service
// produce distinct keys. For anonymous interfaces and structs we fall
// back to String(): those embed package qualifiers via their methods and
// fields, so collisions are only theoretical, and recursing into them
// would balloon the implementation.
func qualifiedName(t reflect.Type) string {
	if t.Name() != "" {
		// PkgPath is "" for predeclared types like int — yields ".int", which
		// is still unique across all named types because user-defined types
		// always have a non-empty PkgPath.
		return t.PkgPath() + "." + t.Name()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + qualifiedName(t.Elem())
	case reflect.Slice:
		return "[]" + qualifiedName(t.Elem())
	case reflect.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), qualifiedName(t.Elem()))
	case reflect.Map:
		return "map[" + qualifiedName(t.Key()) + "]" + qualifiedName(t.Elem())
	case reflect.Chan:
		switch t.ChanDir() {
		case reflect.SendDir:
			return "chan<- " + qualifiedName(t.Elem())
		case reflect.RecvDir:
			return "<-chan " + qualifiedName(t.Elem())
		default:
			return "chan " + qualifiedName(t.Elem())
		}
	case reflect.Func:
		var sb strings.Builder
		sb.WriteString("func(")
		for i := range t.NumIn() {
			if i > 0 {
				sb.WriteString(",")
			}
			if t.IsVariadic() && i == t.NumIn()-1 {
				sb.WriteString("...")
				sb.WriteString(qualifiedName(t.In(i).Elem()))
			} else {
				sb.WriteString(qualifiedName(t.In(i)))
			}
		}
		sb.WriteString(")(")
		for i := range t.NumOut() {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(qualifiedName(t.Out(i)))
		}
		sb.WriteString(")")
		return sb.String()
	default:
		return t.String()
	}
}

// --- classToken implementation ---

func (t classToken) Key() string {
	return "type:" + qualifiedName(t.typ)
}

func (t classToken) Name() string {
	return t.typ.String()
}

// --- namedToken implementation ---

func (t namedToken) Key() string {
	return "named:" + t.name + ":" + qualifiedName(t.typ)
}

func (t namedToken) Name() string {
	return fmt.Sprintf("%s(%s)", t.name, t.typ.String())
}

// --- tagSelector implementation ---

func (t tagSelector) Key() string {
	return "tag:" + t.tag + ":" + qualifiedName(t.typ)
}

func (t tagSelector) Name() string {
	return fmt.Sprintf("tagged(%s, %s)", t.tag, t.typ.String())
}

func (t tagSelector) Tag() string {
	return t.tag
}

// isNamedToken reports whether the token is a named token.
func isNamedToken(t Token) bool {
	_, ok := t.(namedToken)
	return ok
}

// isClassToken reports whether the token is a class token.
func isClassToken(t Token) bool {
	_, ok := t.(classToken)
	return ok
}

// isTagSelector reports whether the token is a tag selector.
func isTagSelector(t Token) bool {
	_, ok := t.(tagSelector)
	return ok
}

// TokenOf2 creates a class token from a reflect.Type.
// This is the non-generic equivalent of TokenOf, useful when the type
// is only known at runtime (e.g., when resolving invoker parameters).
func TokenOf2(typ reflect.Type) Token {
	return classToken{typ: typ}
}

// TokenName returns a human-readable name for a token, suitable for error messages.
func TokenName(t Token) string {
	if t == nil {
		return "<nil>"
	}
	return t.Name()
}
