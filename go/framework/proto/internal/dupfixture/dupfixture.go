// Package dupfixture provides a fixture type whose simple name (DupKind)
// deliberately collides with a same-named type in the proto test package. It
// exists so tests can exercise proto message-name disambiguation across two
// distinct packages — a case that cannot be expressed within a single package.
package dupfixture

// DupKind shares its simple name with a type in the proto test package but has a
// different package path and a distinct field, so a correct generator must emit
// two separate proto messages rather than clobbering one with the other.
type DupKind struct {
	FromDup string `json:"fromDup"`
}
