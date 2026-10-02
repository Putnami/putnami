//go:build !unix

package credentialcustody

// Outside Unix there is no /dev/fd to list, and every scenario that starts a
// hostile process needs a shell, so it skips there.

// descriptorSearchProbes names no probe outside Unix.
func descriptorSearchProbes() []string { return nil }

// descriptorProbes contributes no finding outside Unix.
func descriptorProbes(string) []finding { return nil }
