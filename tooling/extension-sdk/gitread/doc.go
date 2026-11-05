// Package gitread runs the git program for the CLI and the extensions, and
// reads what they all ask of a repository: a commit, a branch, the trunk and
// upstream refs, the commit messages of a range, and whether the clone is
// shallow or git manages a directory at all.
//
// Every call runs Executable, killed after Timeout, in the directory it
// names, with an environment that RepositoryEnvironment cleared of the
// variables that select another repository or inject configuration. Every
// revision and object ID a caller passes is validated before it reaches git,
// so a value can never be read as an option.
package gitread
