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
//
// A process can answer the calls whose answer it kept (UseHeld): the CLI
// answers a run's reads of an unchanged repository from its input record. A
// process that installs none runs git for every call.
package gitread
