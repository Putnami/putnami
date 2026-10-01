package workspace

// AutoSelectionMode describes the project selector chosen for a bare job run.
type AutoSelectionMode string

const (
	AutoSelectionAll      AutoSelectionMode = "all"
	AutoSelectionImpacted AutoSelectionMode = "impacted"
)

// AutoSelectionReason explains why a bare job run chose its mode.
type AutoSelectionReason string

const (
	AutoSelectionReasonFirstBuild AutoSelectionReason = "first-build"
	AutoSelectionReasonLastBuild  AutoSelectionReason = "last-build"
	AutoSelectionReasonTrunk      AutoSelectionReason = "trunk"
)

// LastBuildLookup returns the last fully successful build SHA for a branch and
// command set.
type LastBuildLookup func(branch string, commands []string) (string, error)

// AutoSelection is the resolved project selection for a bare job command.
type AutoSelection struct {
	Mode     AutoSelectionMode
	Baseline string
	Branch   string
	Reason   AutoSelectionReason
}
