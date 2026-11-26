// Package repositorychecks holds the checks that read this repository's files
// as text: the public-cut scan, the release rehearsal and its recorded
// verdict, the published reference the CLI cites for moved commands, the
// first-use block, the governance record, the spelling of every declared
// cache-key pattern and the shipped manifests that must not name Putnami
// Cloud.
//
// The module imports nothing from this repository, so its test task depends on
// no other project's build or tests. Its cache key is the whole candidate cut
// (`git:**`): reading every tracked file costs less than deciding which ones a
// change could reach, so the checks run on every change and cost only
// themselves.
//
// The package code is the public-cut scanner and its candidate enumeration.
// @putnami/cli-documents imports them to prove that this project's cache key
// covers exactly the files the scanner reads; nothing else imports this
// module, and nothing publishes it.
package repositorychecks

// ProjectDir is this project's path from the repository root, the form a
// cache key's file selection is relative to.
const ProjectDir = "tooling/repository-checks"

// cliModuleDir is the CLI module whose committed release plan, recorded
// rehearsal verdict, public-cut inventories and moved-commands reference these
// checks read. They stay where the documentation links point at them.
const cliModuleDir = "tooling/cli"

// CandidateCut is the candidate cut of one repository as the public-cut scan
// reads it: the tracked files and the non-ignored untracked ones that exist in
// the work tree.
type CandidateCut struct {
	files []publicCutFile
}

// ReadCandidateCut reads the candidate cut of the repository at root.
func ReadCandidateCut(root string) (CandidateCut, error) {
	files, err := publicCutTrackedFiles(root)
	if err != nil {
		return CandidateCut{}, err
	}
	return CandidateCut{files: files}, nil
}

// Paths returns the path of every file of the cut, relative to its root, in
// slash form and sorted.
func (c CandidateCut) Paths() []string {
	paths := make([]string, 0, len(c.files))
	for _, file := range c.files {
		paths = append(paths, file.path)
	}
	return paths
}

// UnreviewedFindings returns the public-cut errors of the cut against an empty
// baseline and an empty allowlist: every finding the scan makes, as the gate
// reports it.
func (c CandidateCut) UnreviewedFindings() []string {
	return evaluatePublicCut(scanPublicCut(c.files), nil, nil)
}
