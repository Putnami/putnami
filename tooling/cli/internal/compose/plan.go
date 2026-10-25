// Package compose serves a workload together with the workloads it runs with.
//
// A composition is the transitive closure of `runsWith` (Project.RunsWith),
// resolved by project name then by project id, and nothing else: the
// dependency graph links a consumer to the generated client package it
// compiles against, never to the provider workload it calls. Every member binds
// an ephemeral port behind a local reverse proxy whose URL stays stable across
// restarts, receives the proxy URLs of the members it runs with and its own
// database binding through CONFIG_DATA, and is ready when its typed ready event
// arrives. A composition that dies without tearing itself down leaves a lease
// the next invocation reaps.
package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// DatabaseBinding is one datasource a member declares in its committed
// infra/requirements.json, and the physical database the composition gave it.
type DatabaseBinding struct {
	// Datasource is the requirement name, which is also the key of the
	// `database.databases` map the member's runtime reads.
	Datasource string
	// Schema is the requirement's first schema, or "public".
	Schema string
	// Database is the physical database name: compose_<id>_<slug>_<datasource>
	// on an isolating provider, the provided server's own database otherwise.
	// Empty until the databases phase ran.
	Database string

	// connection carries the credentials. Unexported so no output projection
	// can reach it; configData is its only reader.
	connection *pdb.Connection
}

// Member is one workload of a composition.
type Member struct {
	// Project is the member's workspace project.
	Project *workspace.Project
	// ServeCommand is the serve-workload command the member's serve run
	// requests; the engine plans its pipeline and starts its serve step.
	ServeCommand string
	// ProxyURL is http://127.0.0.1:<proxyPort>, stable for the composition's
	// lifetime. Empty until the proxies phase ran.
	ProxyURL string
	// BackendPort is the port the member announced in its typed ready event; 0
	// while it is starting or restarting. Read it through Composition.Status
	// once the composition is running: a live composition updates it.
	BackendPort int
	// Databases are the member's datasources, one per database requirement.
	Databases []DatabaseBinding

	// runsWith are the resolved members this one runs with, in declaration
	// order.
	runsWith []*Member
	// serviceIDs are the client-contract service ids this member declares as a
	// provider; nil when it commits no contract.
	serviceIDs []string
	// notes are non-fatal planning observations, such as a provider without a
	// committed client contract.
	notes []string
}

// Plan is a composition's members in start order.
type Plan struct {
	// Target is the member the composition was asked for.
	Target *Member
	// Members are in topological order: dependencies first, target last.
	Members []*Member
}

// Member returns the plan's member for a project id.
func (p *Plan) Member(projectID string) (*Member, bool) {
	for _, member := range p.Members {
		if member.Project.ID == projectID {
			return member, true
		}
	}
	return nil, false
}

// ProjectIDs lists the members' project ids in start order.
func (p *Plan) ProjectIDs() []string {
	ids := make([]string, 0, len(p.Members))
	for _, member := range p.Members {
		ids = append(ids, member.Project.ID)
	}
	return ids
}

// PlanFor resolves the composition of target: the transitive closure of
// runsWith in topological order, each member's database requirements, and the
// client-contract service ids of every member another one runs with.
//
// serveCommands are the loaded commands whose resolved traits declare the
// serve workload, the commands every member runs.
//
// It rejects a member that cannot serve before anything starts: a library or
// image project (compose.not_serveable), a project that disables a serve
// command (compose.serve_disabled), a runsWith name that resolves to no project
// (compose.unknown_member), and a runsWith cycle (compose.cycle).
// BindServeCommands then names the command each member's serve run requests.
func PlanFor(ws *workspace.Workspace, target *workspace.Project, serveCommands []string) (*Plan, error) {
	if ws == nil || target == nil {
		return nil, newError(CodeUnknownMember, "", PhasePlan, "no workspace or target project")
	}
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int)
	byID := make(map[string]*Member)
	var order []*Member
	var stack []string

	var visit func(project *workspace.Project) error
	visit = func(project *workspace.Project) error {
		switch state[project.ID] {
		case visited:
			return nil
		case visiting:
			cycle := append(append([]string(nil), stack[slices.Index(stack, project.ID):]...), project.ID)
			return newError(CodeCycle, project.ID, PhasePlan,
				"runsWith forms a cycle: "+strings.Join(cycle, " -> "))
		}
		if err := checkServeable(ws, project, serveCommands); err != nil {
			return err
		}
		state[project.ID] = visiting
		stack = append(stack, project.ID)
		member := &Member{Project: project}
		for _, name := range project.RunsWith {
			dependency := resolveRunsWith(ws, name)
			if dependency == nil {
				return newError(CodeUnknownMember, project.ID, PhasePlan,
					fmt.Sprintf("runsWith %q names no project of this workspace (looked up by name, then by id)", name))
			}
			if err := visit(dependency); err != nil {
				return err
			}
			resolved := byID[dependency.ID]
			if !slices.Contains(member.runsWith, resolved) {
				member.runsWith = append(member.runsWith, resolved)
			}
		}
		databases, err := databaseRequirements(ws.Root, project)
		if err != nil {
			return err
		}
		member.Databases = databases
		stack = stack[:len(stack)-1]
		state[project.ID] = visited
		byID[project.ID] = member
		order = append(order, member)
		return nil
	}
	if err := visit(target); err != nil {
		return nil, err
	}

	for _, member := range order {
		for _, provider := range member.runsWith {
			if provider.serviceIDs != nil || slices.Contains(provider.notes, noContractNote) {
				continue
			}
			ids, err := providerServiceIDs(ws.Root, provider.Project)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				provider.notes = append(provider.notes, noContractNote)
				continue
			}
			provider.serviceIDs = ids
		}
	}
	return &Plan{Target: byID[target.ID], Members: order}, nil
}

// BindServeCommands names the serve-workload command each member's serve run
// requests: the one command of serveCommands, the loaded commands whose
// resolved traits declare the serve workload (PlanFor already refused a member
// that disables one). Without one there is no command to serve a member
// (compose.no_serve_command); with two there is no one workload to start
// (compose.ambiguous_serve). Whether an extension serves a member at all is the
// serve run's answer: it fails before its serve step starts
// (compose.prepare_failed).
func (p *Plan) BindServeCommands(serveCommands []string) error {
	commands := slices.Clone(serveCommands)
	slices.Sort(commands)
	commands = slices.Compact(commands)
	for _, member := range p.Members {
		member.ServeCommand = ""
		switch len(commands) {
		case 0:
			return newError(CodeNoServeCommand, member.Project.ID, PhasePlan,
				"no loaded extension declares a command whose traits declare the serve workload")
		case 1:
			member.ServeCommand = commands[0]
		default:
			return newError(CodeAmbiguousServe, member.Project.ID, PhasePlan,
				"the commands "+strings.Join(commands, ", ")+" each declare the serve workload; compose starts one serve run per member")
		}
	}
	return nil
}

// resolveRunsWith resolves a runsWith entry by project name, then by id.
func resolveRunsWith(ws *workspace.Workspace, name string) *workspace.Project {
	if project := ws.ProjectByName(name); project != nil {
		return project
	}
	return ws.ProjectByID(name)
}

// checkServeable rejects the members compose can never start.
func checkServeable(ws *workspace.Workspace, project *workspace.Project, serveCommands []string) error {
	switch project.Type {
	case "library", "image":
		return newError(CodeNotServeable, project.ID, PhasePlan,
			fmt.Sprintf("a %s project has no workload to serve", project.Type))
	}
	if project.Config != nil && project.Config.Disable != nil && disablesServe(project.Config.Disable.Jobs, serveCommands) {
		return newError(CodeServeDisabled, project.ID, PhasePlan,
			"the project's putnami.json disables a command whose traits declare the serve workload")
	}
	if ws.Config != nil && ws.Config.Disable != nil && disablesServe(ws.Config.Disable.Jobs, serveCommands) {
		return newError(CodeServeDisabled, project.ID, PhasePlan,
			"the workspace configuration disables a command whose traits declare the serve workload")
	}
	return nil
}

// disablesServe reports whether a disable.jobs list turns off one of
// serveCommands, either for every extension or for one ("@putnami/go:serve").
func disablesServe(disabled, serveCommands []string) bool {
	for _, entry := range disabled {
		for _, command := range serveCommands {
			if entry == command || strings.HasSuffix(entry, ":"+command) {
				return true
			}
		}
	}
	return false
}

// databaseRequirements reads the member's committed, generator-owned
// infra/requirements.json. A project without one declares no database.
func databaseRequirements(workspaceRoot string, project *workspace.Project) ([]DatabaseBinding, error) {
	path := infra.ProjectRequirementsPath(filepath.Join(workspaceRoot, project.Path))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	manifest, diags := infra.LoadGeneratedPerProjectManifest(path)
	if diag.HasErrors(diags) || manifest == nil {
		return nil, newError(CodeInvalidRequirement, project.ID, PhasePlan,
			"infra/requirements.json is invalid: "+firstErrorMessage(diags))
	}
	bindings := make([]DatabaseBinding, 0, len(manifest.Databases))
	for _, database := range manifest.Databases {
		if database.Engine != infra.EnginePostgres {
			return nil, newError(CodeInvalidRequirement, project.ID, PhasePlan,
				fmt.Sprintf("datasource %q needs engine %q; compose provisions postgres only", database.Name, database.Engine))
		}
		schema := "public"
		if len(database.Schemas) > 0 && database.Schemas[0] != "" {
			schema = database.Schemas[0]
		}
		bindings = append(bindings, DatabaseBinding{Datasource: database.Name, Schema: schema})
	}
	return bindings, nil
}

func firstErrorMessage(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if d.Severity == diag.Error {
			return d.String()
		}
	}
	return "unreadable manifest"
}
