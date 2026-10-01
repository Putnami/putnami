package workspace

// ResolveLinks is filepath.EvalSymlinks that also follows a Windows directory
// junction. Since Go 1.23, filepath.EvalSymlinks leaves a junction in place, so
// a workspace entered through one would keep the junction's spelling while Git
// and the file system report the directory it points at. On Windows the result
// is absolute.
//
// go.putnami.dev/sdk/extension/dirlink.Resolve is the reference: the CLI
// resolves every other directory link through it. This module may require only
// protocol modules (spec cli/workspace-model#model-purity, ADR 0006), so it
// keeps this standard-library copy, and a CLI test pins the two to the same
// answer.
func ResolveLinks(path string) (string, error) {
	return resolveLinks(path)
}
