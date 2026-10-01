// Package dirlink creates and reads the directory links Putnami writes: a
// symbolic link on Unix and a directory junction on Windows, which a standard
// user creates without Developer Mode.
//
// A junction stores an absolute path. Create and Replace therefore resolve a
// relative target against the link's directory on Windows, and os.Readlink
// answers that absolute path there, while Unix keeps the target verbatim.
//
// Go reports a junction differently from a symbolic link: since Go 1.23,
// os.Lstat gives a junction fs.ModeIrregular without fs.ModeSymlink or
// fs.ModeDir, and filepath.EvalSymlinks does not follow it. A reader of a link
// this package writes uses IsLink instead of fs.ModeSymlink and Resolve instead
// of filepath.EvalSymlinks. os.Readlink, os.Stat and os.Remove already treat a
// junction as a link: os.Remove and os.RemoveAll delete the junction, never
// what it points at.
//
// Replace swaps a link atomically on Unix, with a rename over the old link.
// Windows cannot rename over a directory link, so Replace removes the old link
// and creates the new one while it holds an exclusive filelock lock on
// link+".lock". Concurrent writers never interleave; a reader may find no link
// for the duration of the swap. The lock file stays next to the link.
//
// A platform without an implementation does not compile.
package dirlink

import "io/fs"

// Create makes link a directory link to target. Like os.Symlink, it fails with
// an error matching fs.ErrExist when link already exists. On Windows a junction
// is an empty directory, then its reparse point: a failed create removes the
// directory, and its error names that removal when it fails too.
func Create(target, link string) error {
	return create(target, link)
}

// Replace points link at target, replacing a link or a file already at link,
// and refuses a directory there. On Windows it replaces an empty directory,
// which is what a junction create stopped between its two steps leaves. On
// Unix the new link is created at tmp and renamed over link; a caller whose
// concurrent writers can collide passes a process-unique tmp. Windows does not
// use tmp.
func Replace(target, link, tmp string) error {
	return replace(target, link, tmp)
}

// IsLink reports whether info, which os.Lstat returned for path, describes a
// symbolic link or, on Windows, a directory junction.
func IsLink(path string, info fs.FileInfo) bool {
	return isLink(path, info)
}

// Resolve is filepath.EvalSymlinks that also follows directory junctions. On
// Windows its result is absolute.
func Resolve(path string) (string, error) {
	return resolve(path)
}
