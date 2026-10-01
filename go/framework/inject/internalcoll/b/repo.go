// Package internal is a test fixture for the inject package.
// Its package name intentionally collides with the sibling fixture in ../a
// so that reflect.Type.String() produces "internal.Repository" for both.
package internal

// Repository is a test-only interface used to verify that classToken keys
// are unique per Go type identity even when reflect.Type.String() collides
// across packages with the same final path segment.
type Repository interface {
	Name() string
}

// Service is a test-only struct used to verify that classToken keys remain
// unique for composite unnamed types like *Service or []Service across
// packages whose short names collide.
type Service struct {
	Name string
}
