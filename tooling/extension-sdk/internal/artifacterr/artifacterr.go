// Package artifacterr marks the two upload failures a publication uploader
// tells apart from any other: an artifact whose bytes are not the ones its
// descriptor names, and a registry that already holds another artifact at the
// version. A marked error keeps its text, and errors.Is matches it to its mark
// and to every error it wraps.
package artifacterr

import "errors"

var (
	// ErrMismatch marks an artifact whose size or digest is not the one its
	// descriptor names.
	ErrMismatch = errors.New("the artifact is not the one its descriptor names")
	// ErrConflict marks a registry that already holds another artifact at
	// the version.
	ErrConflict = errors.New("the registry holds another artifact at this version")
)

// Mismatch returns err marked with ErrMismatch.
func Mismatch(err error) error { return &marked{err: err, mark: ErrMismatch} }

// Conflict returns err marked with ErrConflict.
func Conflict(err error) error { return &marked{err: err, mark: ErrConflict} }

// marked has the text of err, and unwraps to err and to mark.
type marked struct{ err, mark error }

func (m *marked) Error() string   { return m.err.Error() }
func (m *marked) Unwrap() []error { return []error{m.err, m.mark} }
