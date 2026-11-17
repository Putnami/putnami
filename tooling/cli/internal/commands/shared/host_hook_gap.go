package shared

// HostHookGap is one way an agent host would run none of the extension hooks
// a workspace's explicit install writes (CLI ADR 0098): the user's hook file
// of a host lacks them, or putnami, which they run, is not on the PATH. The
// agent-content vertical finds the gaps, and doctor reports them.
type HostHookGap struct {
	// Kind says which gap it is.
	Kind HostHookGapKind
	// Host is the agent host, or empty for a gap every host shares.
	Host string
	// File is the absolute path of the user's hook file, or empty.
	File string
	// Events lists the events the file holds no Putnami handler for.
	Events []string
	// Reason says why, in one line.
	Reason string
}

// HostHookGapKind says which way the hooks would not run.
type HostHookGapKind string

const (
	// HostHookGapFile is a user hook file that lacks hooks, or that cannot be
	// located.
	HostHookGapFile HostHookGapKind = "file"
	// HostHookGapRepository is a repository no explicit install recorded, so
	// the relay does not serve it.
	HostHookGapRepository HostHookGapKind = "repository"
	// HostHookGapPath is putnami missing from the PATH.
	HostHookGapPath HostHookGapKind = "path"
)
