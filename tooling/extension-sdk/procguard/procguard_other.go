//go:build !linux

package procguard

// DenyInspection does nothing outside Linux and returns nil; see the package
// documentation.
func DenyInspection() error {
	return nil
}
