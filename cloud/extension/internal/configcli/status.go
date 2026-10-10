package configcli

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// `putnami cloud config status` and `putnami cloud secrets status`
// compare what each project declares in its published config schema with what
// each of its environments sets. Both list the projects the same way: the
// config apps config-api serves, with the environments each has values or
// secrets in. Both only read, and neither reads a secret value: the secret
// side asks whether a key is set, never what it holds.

const (
	// statusParallelism bounds the calls one status has in flight.
	statusParallelism = 8
	// statusCallBudget bounds the calls a whole-workspace status makes after
	// it lists the projects, so `putnami cloud status` answers within its
	// budget. Config spends one call per project (its schema) and one per
	// environment (its resolved values); secrets spends one per environment.
	// The projects past the budget become one unknown check.
	statusCallBudget = 240
)

// statusProject is one project a status reads, with the environments it
// reads it in.
type statusProject struct {
	Name         string
	Environments []string
}

// listStatusProjects lists the workspace's config apps. The "*" layer, which
// applies to every environment, is not an environment of its own: a project
// with values only there is read in the linked environment.
func listStatusProjects(base *secretsCtx) ([]statusProject, error) {
	answer, err := base.configAPI().listConfigApps(base.workspaceID)
	if err != nil {
		return nil, err
	}
	var projects []statusProject
	for _, app := range clicore.Deref(answer.Apps) {
		name := strings.TrimSpace(clicore.Deref(app.Name))
		if name == "" || name == "*" {
			continue
		}
		var environments []string
		for _, environment := range clicore.Deref(app.Environments) {
			if environment = strings.TrimSpace(environment); environment != "" && environment != "*" {
				environments = append(environments, environment)
			}
		}
		if len(environments) == 0 {
			environments = []string{base.environment}
		}
		sort.Strings(environments)
		projects = append(projects, statusProject{Name: name, Environments: environments})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

// namedStatusProject is the project a named status reads: the environments
// config-api lists for it, else the linked environment.
func namedStatusProject(base *secretsCtx, name string) (statusProject, error) {
	projects, err := listStatusProjects(base)
	if err != nil {
		return statusProject{}, err
	}
	for _, project := range projects {
		if project.Name == name {
			return project, nil
		}
	}
	return statusProject{Name: name, Environments: []string{base.environment}}, nil
}

// withinBudget splits projects into the ones a status reads and the names of
// the ones past its call budget. cost is what reading one project spends.
func withinBudget(projects []statusProject, cost func(statusProject) int) ([]statusProject, []string) {
	spent := 0
	for index, project := range projects {
		spent += cost(project)
		if spent > statusCallBudget {
			unread := make([]string, 0, len(projects)-index)
			for _, rest := range projects[index:] {
				unread = append(unread, rest.Name)
			}
			return projects[:index], unread
		}
	}
	return projects, nil
}

// runBounded calls read once per index, with at most statusParallelism calls
// in flight.
func runBounded(count int, read func(index int)) {
	slots := make(chan struct{}, statusParallelism)
	var wg sync.WaitGroup
	for index := range count {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			read(index)
		}()
	}
	wg.Wait()
}

// target is a copy of ctx that reads one project environment.
func (ctx *secretsCtx) target(project, environment string) *secretsCtx {
	copied := *ctx
	copied.app, copied.environment = project, environment
	return &copied
}

// statusPositional reads the optional project name of `<entry> status
// [<project>]`.
func statusPositional(args []string, entry string) (string, error) {
	names := clicore.Positionals(args)
	if len(names) > 0 && names[0] == "status" {
		names = names[1:]
	}
	switch len(names) {
	case 0:
		return "", nil
	case 1:
		return names[0], nil
	default:
		return "", clicore.NewError("cloud "+entry+" status takes at most one project", clicore.ExitUsage)
	}
}

// ---- secrets ---------------------------------------------------------------

// SecretKeyState is one secret key a project's published schema declares, and
// whether one environment sets it. It never carries a value.
type SecretKeyState struct {
	Key      string
	Required bool
	Set      bool
}

// SecretsEnvironment is what one project environment answered to a secrets
// status: its declared secret keys, or that the project has no published
// schema, or why it could not be read.
type SecretsEnvironment struct {
	Project     string
	Environment string
	Keys        []SecretKeyState
	NoSchema    bool
	Err         error
}

// SecretsStatusNode is the secrets line of `putnami cloud status`: every
// project's declared secrets against what each environment sets.
func SecretsStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	base, err := newSecretsWorkspaceCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("secrets", "secrets", err, "")
	}
	return secretsWorkspaceStatus(base, workspaceRoot)
}

// secretsRunStatus serves `putnami cloud secrets status [<project>]`.
func secretsRunStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	name, err := statusPositional(args, "secrets")
	if err != nil {
		return err
	}
	base, err := newSecretsWorkspaceCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	if name == "" {
		return clicore.WriteStatus(params, ioctx, secretsWorkspaceStatus(base, workspaceRoot))
	}
	project, err := namedStatusProject(base, name)
	if err != nil {
		return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("secrets."+name, "secrets "+name, err, ""))
	}
	return clicore.WriteStatus(params, ioctx, SecretsProjectStatusFrom(name, readSecretsEnvironments(base, []statusProject{project})))
}

func secretsWorkspaceStatus(base *secretsCtx, workspaceRoot string) clicore.StatusNode {
	projects, err := listStatusProjects(base)
	if err != nil {
		return clicore.UnknownStatus("secrets", "secrets", err, "")
	}
	projects, leftover, sharedNames, sharedErr := splitWorkspaceApps(base, projects, clicore.WorkspaceProjectKeys(workspaceRoot))
	read, unread := withinBudget(projects, func(project statusProject) int { return len(project.Environments) })
	node := SecretsStatusFrom(readSecretsEnvironments(base, read), unread)
	return withLeftoverApps(node, "secrets", leftover, sharedNames, sharedErr)
}

// readSecretsEnvironments reads every environment of projects, in order.
func readSecretsEnvironments(base *secretsCtx, projects []statusProject) []SecretsEnvironment {
	var out []SecretsEnvironment
	for _, project := range projects {
		for _, environment := range project.Environments {
			out = append(out, SecretsEnvironment{Project: project.Name, Environment: environment})
		}
	}
	runBounded(len(out), func(index int) {
		out[index] = readSecretsEnvironment(base.target(out[index].Project, out[index].Environment))
	})
	return out
}

// readSecretsEnvironment asks config-api which declared secret keys one
// project environment sets: GET /api/secrets/status, one call. config-api
// answers 404 when the project has no published schema, so a 404 is
// NoSchema.
func readSecretsEnvironment(ctx *secretsCtx) SecretsEnvironment {
	report := SecretsEnvironment{Project: ctx.app, Environment: ctx.environment}
	answer, err := ctx.configAPI().secretStatus(ctx.app, ctx.environment)
	switch {
	case configStatus(err) == http.StatusNotFound:
		report.NoSchema = true
		return report
	case err != nil:
		report.Err = err
		return report
	}
	for _, row := range secretStatusRows(answer, ctx) {
		report.Keys = append(report.Keys, SecretKeyState{Key: row.Key, Required: row.Required, Set: row.Status == "set"})
	}
	return report
}

// secretTally counts the declared secret keys of a set of environments.
type secretTally struct {
	declared, set, requiredMissing, optionalMissing int
	unread                                          int
}

func (t *secretTally) add(environment SecretsEnvironment) {
	if environment.Err != nil {
		t.unread++
		return
	}
	for _, key := range environment.Keys {
		t.declared++
		switch {
		case key.Set:
			t.set++
		case key.Required:
			t.requiredMissing++
		default:
			t.optionalMissing++
		}
	}
}

func (t secretTally) metrics() []clicore.StatusMetric {
	return []clicore.StatusMetric{
		clicore.CountMetric("secrets_set", "secrets set", t.set, clicore.MetricUsage).Of(float64(t.declared)),
		clicore.CountMetric("required_missing", "required missing", t.requiredMissing, clicore.MetricHealth),
		clicore.CountMetric("optional_missing", "optional missing", t.optionalMissing, clicore.MetricHealth),
	}
}

// SecretsStatusFrom folds what each project environment answered into the
// secrets status of the whole workspace: one check per project, and under it
// one per environment. unread names the projects left unread because the
// status reached its call budget. The state is the worst of the checks.
func SecretsStatusFrom(environments []SecretsEnvironment, unread []string) clicore.StatusNode {
	node := clicore.StatusNode{ID: "secrets", Title: "secrets"}
	var total secretTally
	projectsMissing := 0
	for _, group := range groupByProject(environments, func(environment SecretsEnvironment) string { return environment.Project }) {
		var tally secretTally
		for _, environment := range group {
			tally.add(environment)
			total.add(environment)
		}
		if tally.requiredMissing > 0 {
			projectsMissing++
		}
		child := clicore.StatusNode{ID: "secrets." + group[0].Project, Title: group[0].Project}
		for _, environment := range group {
			child.Children = append(child.Children, secretsEnvironmentNode(child.ID, environment, false))
		}
		child.State = clicore.WorstStatus(child.ChildStates()...)
		child.Detail = secretsSummary(tally, group)
		node.Children = append(node.Children, child)
	}
	if len(unread) > 0 {
		node.Children = append(node.Children, unreadNode("secrets", unread))
	}
	node.Metrics = total.metrics()
	node.State = clicore.WorstStatus(node.ChildStates()...)
	projects := len(node.Children)
	if len(unread) > 0 {
		projects += len(unread) - 1
	}
	switch {
	case projects == 0:
		node.Detail = "no project has config or secrets"
	case total.requiredMissing > 0:
		node.Detail = fmt.Sprintf("%s missing in %s", countOf(total.requiredMissing, "required secret", "required secrets"), countOf(projectsMissing, "project", "projects"))
	case total.unread > 0 || len(unread) > 0:
		node.Detail = fmt.Sprintf("%s, %s not read", countOf(projects, "project", "projects"), countOf(total.unread+len(unread), "check", "checks"))
	default:
		node.Detail = countOf(projects, "project", "projects") + ", every required secret set"
		if total.optionalMissing > 0 {
			node.Detail += fmt.Sprintf(", %d optional missing", total.optionalMissing)
		}
	}
	return node
}

// SecretsProjectStatusFrom folds one project's environments into the status
// `putnami cloud secrets status <project>` prints: one check per environment
// and, under it, one per secret key that is not set.
func SecretsProjectStatusFrom(project string, environments []SecretsEnvironment) clicore.StatusNode {
	node := clicore.StatusNode{ID: "secrets." + project, Title: "secrets " + project}
	var tally secretTally
	for _, environment := range environments {
		tally.add(environment)
		node.Children = append(node.Children, secretsEnvironmentNode(node.ID, environment, true))
	}
	node.Metrics = tally.metrics()
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail = secretsSummary(tally, environments)
	return node
}

// secretsEnvironmentNode is the check of one project environment. With keys,
// each secret that is not set is a check of its own and carries its fix;
// without, the environment names the fix of its first missing required key.
func secretsEnvironmentNode(parentID string, environment SecretsEnvironment, keys bool) clicore.StatusNode {
	node := clicore.StatusNode{ID: parentID + "." + environment.Environment, Title: environment.Environment}
	if environment.Err != nil {
		return clicore.UnknownStatus(node.ID, node.Title, environment.Err, "")
	}
	if environment.NoSchema {
		node.State, node.Detail = clicore.StatusDegraded, "no published schema, so no secret can be checked"
		node.Fix = configPublishFix(environment.Project, environment.Environment)
		return node
	}
	var tally secretTally
	tally.add(environment)
	var required []string
	for _, key := range environment.Keys {
		if !key.Set && key.Required {
			required = append(required, key.Key)
		}
	}
	switch {
	case tally.declared == 0:
		node.State, node.Detail = clicore.StatusOK, "no secret declared"
	case len(required) > 0:
		node.State = clicore.StatusFailing
		node.Detail = fmt.Sprintf("%s missing: %s", countOf(len(required), "required secret", "required secrets"), listNames(required))
		if !keys {
			node.Fix = secretsSetFix(environment.Project, required[0], environment.Environment)
		}
	default:
		node.State = clicore.StatusOK
		node.Detail = fmt.Sprintf("%d of %d secrets set", tally.set, tally.declared)
		if tally.optionalMissing > 0 {
			node.Detail += fmt.Sprintf(", %d optional missing", tally.optionalMissing)
		}
	}
	if keys {
		for _, key := range environment.Keys {
			if key.Set {
				continue
			}
			child := clicore.StatusNode{ID: node.ID + "." + key.Key, Title: key.Key, State: clicore.StatusOK, Detail: "optional, not set"}
			if key.Required {
				child.State, child.Detail = clicore.StatusFailing, "required, not set"
				child.Fix = secretsSetFix(environment.Project, key.Key, environment.Environment)
			}
			node.Children = append(node.Children, child)
		}
	}
	return node
}

// secretsSummary is the detail of a project: what is missing first, then
// what could not be read, then how many secrets are set.
func secretsSummary(tally secretTally, environments []SecretsEnvironment) string {
	var missingIn []string
	noSchema := 0
	for _, environment := range environments {
		if environment.NoSchema {
			noSchema++
		}
		for _, key := range environment.Keys {
			if !key.Set && key.Required {
				missingIn = append(missingIn, environment.Environment)
				break
			}
		}
	}
	switch {
	case tally.requiredMissing > 0:
		return fmt.Sprintf("%s missing in %s", countOf(tally.requiredMissing, "required secret", "required secrets"), strings.Join(missingIn, ", "))
	case tally.unread > 0:
		return fmt.Sprintf("%d of %d environments not read", tally.unread, len(environments))
	case noSchema > 0:
		return "no published schema"
	case tally.declared == 0:
		return "no secret declared"
	}
	detail := fmt.Sprintf("%d of %d secrets set", tally.set, tally.declared)
	if len(environments) > 1 {
		detail += fmt.Sprintf(" in %d environments", len(environments))
	}
	if tally.optionalMissing > 0 {
		detail += fmt.Sprintf(", %d optional missing", tally.optionalMissing)
	}
	return detail
}

// secretsSetFix is the command that sets one missing secret. The value comes
// from stdin, so it never lands in argv or the shell history.
func secretsSetFix(project, key, environment string) string {
	return fmt.Sprintf("putnami cloud secrets set %s %s --env %s --from-stdin", project, key, environment)
}

// configPublishFix registers the project's schema and publishes the values its
// committed conf/env*.yaml files hold for environment.
func configPublishFix(project, environment string) string {
	return fmt.Sprintf("putnami cloud config publish %s --env %s", project, environment)
}

// ---- config ----------------------------------------------------------------

// ConfigKeyState is one non-secret key a project's published schema declares,
// and how one environment resolves it: set, default (no value, the schema's
// default applies) or missing (no value and no default).
type ConfigKeyState struct {
	Key      string
	Required bool
	Status   string
}

// ConfigEnvironment is what one project environment answered to a config
// status: its declared keys, or that the project has no published schema, or
// why it could not be read.
type ConfigEnvironment struct {
	Project     string
	Environment string
	Keys        []ConfigKeyState
	NoSchema    bool
	Err         error
}

// ConfigStatusNode is the config line of `putnami cloud status`: every
// project's declared config keys against what each environment resolves.
func ConfigStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	base, err := newSecretsWorkspaceCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("config", "config", err, "")
	}
	return configWorkspaceStatus(base, workspaceRoot)
}

// configRunStatus serves `putnami cloud config status [<project>]`.
func configRunStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	name, err := statusPositional(args, "config")
	if err != nil {
		return err
	}
	base, err := newSecretsWorkspaceCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	if name == "" {
		return clicore.WriteStatus(params, ioctx, configWorkspaceStatus(base, workspaceRoot))
	}
	project, err := namedStatusProject(base, name)
	if err != nil {
		return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("config."+name, "config "+name, err, ""))
	}
	return clicore.WriteStatus(params, ioctx, ConfigProjectStatusFrom(name, readConfigEnvironments(base, []statusProject{project})))
}

func configWorkspaceStatus(base *secretsCtx, workspaceRoot string) clicore.StatusNode {
	projects, err := listStatusProjects(base)
	if err != nil {
		return clicore.UnknownStatus("config", "config", err, "")
	}
	projects, leftover, sharedNames, sharedErr := splitWorkspaceApps(base, projects, clicore.WorkspaceProjectKeys(workspaceRoot))
	read, unread := withinBudget(projects, func(project statusProject) int { return 1 + len(project.Environments) })
	node := ConfigStatusFrom(readConfigEnvironments(base, read), unread)
	return withLeftoverApps(node, "config", leftover, sharedNames, sharedErr)
}

// readConfigEnvironments reads each project's schema once, then resolves each
// of its environments, with every call bounded by statusParallelism. The
// resolve sends no secret mode, so config-api answers no secret value.
func readConfigEnvironments(base *secretsCtx, projects []statusProject) []ConfigEnvironment {
	type schemaAnswer struct {
		specs    []schemaKeySpec
		noSchema bool
		err      error
	}
	schemas := make([]schemaAnswer, len(projects))
	runBounded(len(projects), func(index int) {
		schema, err := base.configAPI().fetchSchema(projects[index].Name)
		switch {
		case configStatus(err) == http.StatusNotFound:
			schemas[index].noSchema = true
		case err != nil:
			schemas[index].err = err
		default:
			schemas[index].specs = schemaKeySpecs(schema)
		}
	})
	var out []ConfigEnvironment
	var specs [][]schemaKeySpec
	var pending []int
	for index, project := range projects {
		for _, environment := range project.Environments {
			report := ConfigEnvironment{Project: project.Name, Environment: environment, NoSchema: schemas[index].noSchema, Err: schemas[index].err}
			if !report.NoSchema && report.Err == nil {
				pending = append(pending, len(out))
			}
			out = append(out, report)
			specs = append(specs, schemas[index].specs)
		}
	}
	runBounded(len(pending), func(index int) {
		at := pending[index]
		ctx := base.target(out[at].Project, out[at].Environment)
		resolved, err := ctx.configAPI().resolveConfigs(map[string]any{"appName": ctx.app, "environment": ctx.environment}, ctx.workspaceID)
		if err != nil {
			out[at].Err = err
			return
		}
		for _, spec := range specs[at] {
			if spec.Sensitive {
				continue
			}
			status := configStatusForSpec(spec, resolved.Config)
			out[at].Keys = append(out[at].Keys, ConfigKeyState{Key: spec.Key, Required: spec.Required, Status: status.Status})
		}
	})
	return out
}

// configTally counts the declared config keys of a set of environments. A key
// that resolves to its schema default counts as set.
type configTally struct {
	declared, set, requiredMissing, optionalMissing int
	unread, noSchema                                int
}

func (t *configTally) add(environment ConfigEnvironment) {
	switch {
	case environment.Err != nil:
		t.unread++
		return
	case environment.NoSchema:
		t.noSchema++
		return
	}
	for _, key := range environment.Keys {
		t.declared++
		switch {
		case key.Status != "missing":
			t.set++
		case key.Required:
			t.requiredMissing++
		default:
			t.optionalMissing++
		}
	}
}

// ConfigStatusFrom folds what each project environment answered into the
// config status of the whole workspace: one check per project, and under it
// one per environment. unread names the projects left unread because the
// status reached its call budget. The state is the worst of the checks.
func ConfigStatusFrom(environments []ConfigEnvironment, unread []string) clicore.StatusNode {
	node := clicore.StatusNode{ID: "config", Title: "config"}
	var total configTally
	projectsMissing := 0
	for _, group := range groupByProject(environments, func(environment ConfigEnvironment) string { return environment.Project }) {
		var tally configTally
		for _, environment := range group {
			tally.add(environment)
			total.add(environment)
		}
		if tally.requiredMissing > 0 {
			projectsMissing++
		}
		child := clicore.StatusNode{ID: "config." + group[0].Project, Title: group[0].Project}
		for _, environment := range group {
			child.Children = append(child.Children, configEnvironmentNode(child.ID, environment, false))
		}
		child.State = clicore.WorstStatus(child.ChildStates()...)
		child.Detail = configSummary(tally, group)
		node.Children = append(node.Children, child)
	}
	projects := len(node.Children)
	if len(unread) > 0 {
		node.Children = append(node.Children, unreadNode("config", unread))
		projects += len(unread)
	}
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("projects", "projects", projects, clicore.MetricUsage),
		clicore.CountMetric("keys_declared", "keys declared", total.declared, clicore.MetricUsage),
		clicore.CountMetric("keys_set", "keys set", total.set, clicore.MetricUsage).Of(float64(total.declared)),
		clicore.CountMetric("required_missing", "required missing", total.requiredMissing, clicore.MetricHealth),
		clicore.CountMetric("optional_missing", "optional missing", total.optionalMissing, clicore.MetricHealth),
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	switch {
	case projects == 0:
		node.Detail = "no project has config or secrets"
	case total.requiredMissing > 0:
		node.Detail = fmt.Sprintf("%s missing in %s", countOf(total.requiredMissing, "required key", "required keys"), countOf(projectsMissing, "project", "projects"))
	case total.unread > 0 || len(unread) > 0:
		node.Detail = fmt.Sprintf("%s, %s not read", countOf(projects, "project", "projects"), countOf(total.unread+len(unread), "check", "checks"))
	default:
		node.Detail = countOf(projects, "project", "projects") + ", every required key set"
		if total.optionalMissing > 0 {
			node.Detail += fmt.Sprintf(", %d optional missing", total.optionalMissing)
		}
	}
	return node
}

// ConfigProjectStatusFrom folds one project's environments into the status
// `putnami cloud config status <project>` prints: one check per environment
// and, under it, one per key with no value and no default.
func ConfigProjectStatusFrom(project string, environments []ConfigEnvironment) clicore.StatusNode {
	node := clicore.StatusNode{ID: "config." + project, Title: "config " + project}
	var tally configTally
	for _, environment := range environments {
		tally.add(environment)
		node.Children = append(node.Children, configEnvironmentNode(node.ID, environment, true))
	}
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("keys_declared", "keys declared", tally.declared, clicore.MetricUsage),
		clicore.CountMetric("keys_set", "keys set", tally.set, clicore.MetricUsage).Of(float64(tally.declared)),
		clicore.CountMetric("required_missing", "required missing", tally.requiredMissing, clicore.MetricHealth),
		clicore.CountMetric("optional_missing", "optional missing", tally.optionalMissing, clicore.MetricHealth),
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail = configSummary(tally, environments)
	return node
}

// configEnvironmentNode is the check of one project environment. A required
// key with no value and no default fails it; an optional one does not change
// its state, because the workload runs without it, and the detail names it. The
// fix is `config publish`, not `config put`: publish writes the values the
// committed conf/env*.yaml files hold, which `config drift` and every later
// release compare against, while put writes a value the repository does not
// hold. With keys, each key without a value is a check of its own.
func configEnvironmentNode(parentID string, environment ConfigEnvironment, keys bool) clicore.StatusNode {
	node := clicore.StatusNode{ID: parentID + "." + environment.Environment, Title: environment.Environment}
	if environment.Err != nil {
		return clicore.UnknownStatus(node.ID, node.Title, environment.Err, "")
	}
	if environment.NoSchema {
		node.State, node.Detail = clicore.StatusDegraded, "no published schema, so no key can be checked"
		node.Fix = configPublishFix(environment.Project, environment.Environment)
		return node
	}
	var required, optional []string
	for _, key := range environment.Keys {
		if key.Status != "missing" {
			continue
		}
		if key.Required {
			required = append(required, key.Key)
		} else {
			optional = append(optional, key.Key)
		}
	}
	set := len(environment.Keys) - len(required) - len(optional)
	switch {
	case len(environment.Keys) == 0:
		node.State, node.Detail = clicore.StatusOK, "no config key declared"
	case len(required) > 0:
		node.State = clicore.StatusFailing
		node.Detail = fmt.Sprintf("%s missing: %s", countOf(len(required), "required key", "required keys"), listNames(required))
		node.Fix = configPublishFix(environment.Project, environment.Environment)
	case len(optional) > 0:
		node.State = clicore.StatusOK
		node.Detail = fmt.Sprintf("%d of %d keys set, %d optional missing", set, len(environment.Keys), len(optional))
	default:
		node.State = clicore.StatusOK
		node.Detail = fmt.Sprintf("%d of %d keys set", set, len(environment.Keys))
	}
	if keys {
		for _, key := range environment.Keys {
			if key.Status != "missing" {
				continue
			}
			child := clicore.StatusNode{ID: node.ID + "." + key.Key, Title: key.Key, State: clicore.StatusOK, Detail: "optional, no value and no default"}
			if key.Required {
				child.State, child.Detail = clicore.StatusFailing, "required, no value and no default"
			}
			node.Children = append(node.Children, child)
		}
	}
	return node
}

// configSummary is the detail of a project: what is missing first, then what
// could not be read, then how many keys are set.
func configSummary(tally configTally, environments []ConfigEnvironment) string {
	switch {
	case tally.requiredMissing > 0:
		return countOf(tally.requiredMissing, "required key", "required keys") + " missing"
	case tally.unread > 0:
		return fmt.Sprintf("%d of %d environments not read", tally.unread, len(environments))
	case tally.noSchema > 0:
		return "no published schema"
	case tally.declared == 0:
		return "no config key declared"
	}
	detail := fmt.Sprintf("%d of %d keys set", tally.set, tally.declared)
	if len(environments) > 1 {
		detail += fmt.Sprintf(" in %d environments", len(environments))
	}
	if tally.optionalMissing > 0 {
		detail += fmt.Sprintf(", %d optional missing", tally.optionalMissing)
	}
	return detail
}

// ---- shared ----------------------------------------------------------------

// groupByProject splits environments, already ordered by project, into one
// group per project.
func groupByProject[T any](environments []T, project func(T) string) [][]T {
	var groups [][]T
	for _, environment := range environments {
		if n := len(groups); n > 0 && project(groups[n-1][0]) == project(environment) {
			groups[n-1] = append(groups[n-1], environment)
			continue
		}
		groups = append(groups, []T{environment})
	}
	return groups
}

// unreadNode is the check of the projects a status left unread because it
// reached its call budget. Each one reads with a named status.
func unreadNode(entry string, unread []string) clicore.StatusNode {
	return clicore.StatusNode{
		ID: entry + ".unread", Title: "not read", State: clicore.StatusUnknown,
		Detail: fmt.Sprintf("%s past the %d-call budget of a status: %s", countOf(len(unread), "project", "projects"), statusCallBudget, listNames(unread)),
		Fix:    fmt.Sprintf("putnami cloud %s status %s", entry, unread[0]),
	}
}

// sharedApps names the config apps a service account reads through an active
// secret grant, such as source/github-app, which source-api and control-api
// read: no project has that name, yet deleting the app breaks its readers.
// Listing grants needs platform.workspace.manage; err reports a refusal.
func sharedApps(base *secretsCtx) (map[string]bool, error) {
	answer, err := base.configAPI().listSecretGrants(base.workspaceID)
	if err != nil {
		return nil, err
	}
	shared := map[string]bool{}
	for _, grant := range clicore.Deref(answer.Grants) {
		if name := strings.TrimSpace(clicore.Deref(grant.AppName)); name != "" && clicore.Deref(grant.Active) {
			shared[name] = true
		}
	}
	return shared, nil
}

// splitWorkspaceApps splits projects with splitLeftoverApps. It reads the
// secret grants only when an app is outside the checkout, so a status with no
// leftover candidate makes no extra call.
func splitWorkspaceApps(base *secretsCtx, projects []statusProject, checkout map[string]bool) ([]statusProject, []string, []string, error) {
	var shared map[string]bool
	var err error
	if len(checkout) > 0 && slices.ContainsFunc(projects, func(project statusProject) bool { return !checkout[project.Name] }) {
		shared, err = sharedApps(base)
	}
	kept, leftover, sharedNames := splitLeftoverApps(projects, checkout, shared)
	return kept, leftover, sharedNames, err
}

// splitLeftoverApps keeps the config apps a project of the checkout answers
// to, sets aside the shared ones, and names the others: config-api keeps an
// app under every name a project ever had, and a renamed project leaves its
// old app behind. A shared app is neither read nor counted. Without a project
// in the checkout to compare with, every app is kept.
func splitLeftoverApps(projects []statusProject, checkout, shared map[string]bool) ([]statusProject, []string, []string) {
	if len(checkout) == 0 {
		return projects, nil, nil
	}
	kept := projects[:0:0]
	var leftover, sharedNames []string
	for _, project := range projects {
		switch {
		case checkout[project.Name]:
			kept = append(kept, project)
		case shared[project.Name]:
			sharedNames = append(sharedNames, project.Name)
		default:
			leftover = append(leftover, project.Name)
		}
	}
	return kept, leftover, sharedNames
}

// withLeftoverApps adds the config apps no project answers to as one
// degraded check, and names the shared apps in the facts. The leftover keys
// are not read and do not count, and the check names no fix: no command
// deletes a config app. When the secret grants were unreadable, a leftover
// may be a shared app, so the check is unknown.
func withLeftoverApps(node clicore.StatusNode, entry string, leftover, shared []string, sharedErr error) clicore.StatusNode {
	if node.State == clicore.StatusUnknown && len(node.Children) == 0 {
		return node
	}
	if len(shared) > 0 {
		if node.Facts == nil {
			node.Facts = map[string]string{}
		}
		node.Facts["shared_apps"] = strings.Join(shared, ",")
	}
	if len(leftover) == 0 {
		return node
	}
	check := clicore.StatusNode{
		ID: entry + ".leftover", Title: "no project", State: clicore.StatusDegraded,
		Detail: fmt.Sprintf("%s under a name no project of this checkout has: %s",
			countOf(len(leftover), "config app", "config apps"), listNames(leftover)),
		Facts: map[string]string{"apps": strings.Join(leftover, ",")},
	}
	if sharedErr != nil {
		check.State = clicore.StatusUnknown
		check.Detail += "; secret grants unreadable, so some may be shared: " +
			clicore.UnknownStatus("", "", sharedErr, "").Detail
	}
	node.Children = append(node.Children, check)
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail += fmt.Sprintf(", %s with no project", countOf(len(leftover), "config app", "config apps"))
	return node
}

// listNames lists at most three names, then how many more.
func listNames(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
