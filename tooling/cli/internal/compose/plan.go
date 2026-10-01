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
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// serveCommand is the job command every member runs.
const serveCommand = "serve"

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
	// ServeJob is the extension's serve step for this project, bound after the
	// engine prepared the composition (ADR 0001 §3: only the engine plans).
	ServeJob *jobs.ScheduledJob
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
// It rejects a member that cannot serve before anything starts: a library or
// image project (compose.not_serveable), a project whose serve job is disabled
// (compose.serve_disabled), a runsWith name that resolves to no project
// (compose.unknown_member), and a runsWith cycle (compose.cycle). Whether an
// extension provides a serve step is only known once the engine planned it:
// BindServeJobs reports compose.no_serve_command.
func PlanFor(ws *workspace.Workspace, target *workspace.Project) (*Plan, error) {
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
		if err := checkServeable(ws, project); err != nil {
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

// BindServeJobs attaches each member's serve step from the jobs the engine
// withheld while preparing the composition. A member without one has no
// extension that serves it (compose.no_serve_command).
func (p *Plan) BindServeJobs(withheld []*jobs.ScheduledJob) error {
	for _, member := range p.Members {
		member.ServeJob = nil
		for _, job := range withheld {
			if job != nil && job.Project != nil && job.Project.ID == member.Project.ID {
				member.ServeJob = job
				break
			}
		}
		if member.ServeJob == nil {
			return newError(CodeNoServeCommand, member.Project.ID, PhasePlan,
				"no extension provides a serve step for this project")
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
func checkServeable(ws *workspace.Workspace, project *workspace.Project) error {
	switch project.Type {
	case "library", "image":
		return newError(CodeNotServeable, project.ID, PhasePlan,
			fmt.Sprintf("a %s project has no workload to serve", project.Type))
	}
	if project.Config != nil && project.Config.Disable != nil && disablesServe(project.Config.Disable.Jobs) {
		return newError(CodeServeDisabled, project.ID, PhasePlan,
			"the project's putnami.json disables the serve job")
	}
	if ws.Config != nil && ws.Config.Disable != nil && disablesServe(ws.Config.Disable.Jobs) {
		return newError(CodeServeDisabled, project.ID, PhasePlan,
			"the workspace configuration disables the serve job")
	}
	return nil
}

// disablesServe reports whether a disable.jobs list turns off `serve`, either
// for every extension or for one ("@putnami/go:serve").
func disablesServe(disabled []string) bool {
	for _, entry := range disabled {
		if entry == serveCommand || strings.HasSuffix(entry, ":"+serveCommand) {
			return true
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
