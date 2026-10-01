package commandmeta

const (
	MCPToolListProjects    = "list_projects"
	MCPToolDescribeProject = "describe_project"
	MCPToolAgentContext    = "agent_context"
	MCPToolWorkspaceMap    = "workspace_map"
	MCPToolDeps            = "deps"
	MCPToolFindOwner       = "find_owner"
	MCPToolWhyImpacted     = "why_impacted"
	MCPToolTopoSort        = "topo_sort"
	MCPToolImpacted        = "impacted"
	MCPToolRunJobs         = "run_jobs"
	MCPToolGetDiagnostics  = "get_diagnostics"
)

var coreMCPToolNames = []string{
	MCPToolListProjects,
	MCPToolDescribeProject,
	MCPToolAgentContext,
	MCPToolWorkspaceMap,
	MCPToolDeps,
	MCPToolFindOwner,
	MCPToolWhyImpacted,
	MCPToolTopoSort,
	MCPToolImpacted,
	MCPToolRunJobs,
	MCPToolGetDiagnostics,
}

// CoreMCPToolNames returns the core registry names in advertisement order. The
// MCP server and generated command help both consume this inventory.
func CoreMCPToolNames() []string {
	return append([]string(nil), coreMCPToolNames...)
}
