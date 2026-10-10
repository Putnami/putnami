// Package hometest points the user home of a test's injected environment at a
// directory the test owns. It is test support: import it only from _test.go
// files.
package hometest

// Env sets dir as the user home in env and returns env. A nil env starts a new
// map. It sets HOME, which os.UserHomeDir and clicore.UserHomeDirFor read on
// Unix, and USERPROFILE, which they read on Windows (decision D-W7). An env
// that carries only HOME resolves to the real %USERPROFILE% on Windows, so the
// test would read and write the real user's files.
func Env(dir string, env map[string]string) map[string]string {
	if env == nil {
		env = make(map[string]string, 2)
	}
	env["HOME"] = dir
	env["USERPROFILE"] = dir
	return env
}
