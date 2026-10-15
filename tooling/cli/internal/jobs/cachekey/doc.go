// Package cachekey computes a task's cache key and the inputs it hashes: the
// task's declared files, params and environment, its project's dependency
// closure and the workspace membership, its extension's implementation, its
// runtime toolchains, the host platform and the release baseline. It also
// holds what reads the same declarations: key movement, the key precompute of
// a whole plan, the portable inputs a remote run binds, and the cache
// verification contract.
//
// It imports neither the scheduler nor the runner, so a change there does not
// rerun its tests, and the bytes a key hashes stay checkable in one package.
package cachekey
