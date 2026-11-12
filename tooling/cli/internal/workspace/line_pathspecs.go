package workspace

import (
	"sort"
	"strings"
)

// LinePathspecs are the paths whose commits advance a version line: the line's
// scope path, then the committed contract of each generated client on the line
// whose provider's contract lies outside it, sorted. A generated client commits
// no generated code, so a commit that changes only its provider's contract is
// the change that moves the client (CLI ADR 0087). The root line covers the
// whole tree and has no pathspec.
func LinePathspecs(ws *Workspace, line string) []string {
	if line == "" {
		return nil
	}
	seen := map[string]bool{}
	var contracts []string
	if ws != nil {
		for _, project := range ws.Projects {
			if project.Line != line || project.GeneratedClient == nil {
				continue
			}
			contract := project.GeneratedClient.ContractPath
			if contract == "" || pathWithin(contract, line) || seen[contract] {
				continue
			}
			seen[contract] = true
			contracts = append(contracts, contract)
		}
	}
	sort.Strings(contracts)
	return append([]string{line}, contracts...)
}

// pathWithin reports whether the slash-form path lies inside dir.
func pathWithin(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}
