//go:build !unix

package parent

// supported reports whether Watch watches the parent on this platform. A
// Windows process keeps the parent pid it started with after its parent
// exits, and putnami's Job Object ends the command with putnami.
const supported = false

// ignoreBrokenPipes does nothing: only Unix raises SIGPIPE.
func ignoreBrokenPipes() {}
