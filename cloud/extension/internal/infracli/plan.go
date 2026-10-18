// Package infracli serves `putnami cloud infra plan`: it reads the aggregated
// infra-requirements manifest of each deployable workload and prints the
// deployment plan a deployer would have to interpret.
//
// The command does not load the workspace. The putnami CLI lists every
// workspace project, with its resolved type, in the job context it hands the
// extension, and that type is how the command tells a workload from a library.
package infracli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/job"
)

// Plan serves `putnami cloud infra plan [<project-name>]`. args are the
// arguments after `plan`; projects are the workspaceProjects of the job
// context.
//
// With no project name it plans every workload of the workspace, sorted by
// name; with one it plans that project and fails when it is unknown or is not
// a workload. A workload without an aggregated manifest is reported, not built.
//
// Human output is plain text. Under --output=json or jsonl, the command writes
// one version-2 result envelope whose data is one entry per workload: an object
// for one workload, an array for several, and [] for none.
func Plan(params map[string]any, args []string, projects []job.ProjectRef, ioctx clicore.IO) error {
	if err := requireProjectTypes(projects); err != nil {
		return err
	}
	target := strings.TrimSpace(strings.Join(clicore.Positionals(args), " "))
	workloads, err := selectWorkloads(projects, target)
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		data, err := structuredPlan(workloads)
		if err != nil {
			return err
		}
		clicore.WriteResult(data, params, ioctx, "")
		return nil
	}
	var out strings.Builder
	humanPlan(&out, workloads)
	writeLines(ioctx, out.String())
	return nil
}

// requireProjectTypes declines a job context that cannot say which projects
// are workloads. The type of a workspaceProjects entry is resolved by the CLI;
// an entry without it comes from a CLI that predates the member, and reading
// it as "application" would plan a library as a workload.
func requireProjectTypes(projects []job.ProjectRef) error {
	// nil means the job context has no workspaceProjects member (an older CLI);
	// an empty non-nil slice means a workspace with no projects.
	if projects == nil {
		return errors.New("cloud infra plan needs the workspace projects from the putnami CLI, and the job context lists none: " +
			"an older putnami CLI omits them, so upgrade the CLI")
	}
	for _, p := range projects {
		if p.Type == "" {
			return fmt.Errorf("cloud infra plan needs the resolved type of every workspace project, and the job context gives none for %q: "+
				"the putnami CLI that started this command predates the member, so upgrade the CLI", p.Name)
		}
	}
	return nil
}

// selectWorkloads returns the workloads to plan. With no target, every
// workload, sorted by name; with a target, the project of that name, which
// must be a workload.
func selectWorkloads(projects []job.ProjectRef, target string) ([]job.ProjectRef, error) {
	if target == "" {
		var out []job.ProjectRef
		for _, p := range projects {
			if isWorkload(p) {
				out = append(out, p)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	for _, p := range projects {
		if p.Name != target {
			continue
		}
		if !isWorkload(p) {
			return nil, protocolcli.NotFoundf("project %q is not an application workload (type=%q)", target, p.Type)
		}
		return []job.ProjectRef{p}, nil
	}
	return nil, protocolcli.NotFoundf("project %q not found in workspace", target)
}

// isWorkload reports whether a project is a deployable workload: its resolved
// type is "application". requireProjectTypes has already refused an empty type.
func isWorkload(p job.ProjectRef) bool {
	return p.Type == "application"
}

func manifestPath(p job.ProjectRef) string {
	return filepath.Join(p.FullPath, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
}

// writeLines sends text to the Stdout sink, one call per line. Blank lines
// stay, so the sink prints the text byte for byte.
func writeLines(ioctx clicore.IO, text string) {
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		ioctx.Stdout(line)
	}
}

func humanPlan(w io.Writer, workloads []job.ProjectRef) {
	if len(workloads) == 0 {
		_, _ = fmt.Fprintln(w, "No workloads found.")
		return
	}
	for i, p := range workloads {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		writeWorkloadPlan(w, p)
	}
}

// writeWorkloadPlan prints one workload's plan. A missing, invalid or empty
// manifest is reported in the plan, not returned as an error, because a plan
// of several workloads may hold some that have not been built.
func writeWorkloadPlan(w io.Writer, p job.ProjectRef) {
	heading := fmt.Sprintf("Workload: %s", p.Name)
	_, _ = fmt.Fprintln(w, heading)
	_, _ = fmt.Fprintln(w, strings.Repeat("─", len(heading)))

	path := manifestPath(p)
	manifest, diags := infra.LoadAggregatedManifest(path)
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(w, "  No aggregated manifest at %s.\n", path)
		_, _ = fmt.Fprintln(w, "  Run `putnami build` for this workload to emit one.")
		return
	}
	if diag.HasErrors(diags) {
		_, _ = fmt.Fprintln(w, "  Manifest failed to parse or validate:")
		for _, d := range diags {
			_, _ = fmt.Fprintf(w, "    [%s] %s\n", d.Code, d.Message)
		}
		return
	}
	if manifest == nil {
		_, _ = fmt.Fprintln(w, "  Manifest is empty.")
		return
	}

	writeDatabases(w, manifest.Databases)
	writeEvents(w, manifest.Events)
	writeStorage(w, manifest.Storage)
	writeSecrets(w, manifest.Secrets)
	writeScheduledJobs(w, manifest.ScheduledJobs)
	writeRuntime(w, manifest.Runtime)

	if hasNoResources(manifest) && manifest.Runtime == nil {
		_, _ = fmt.Fprintln(w, "  (no infrastructure declared)")
	}
}

func hasNoResources(m *infra.AggregatedManifest) bool {
	if m == nil {
		return true
	}
	if m.Events != nil && (len(m.Events.Publishes) > 0 || len(m.Events.Subscribes) > 0) {
		return false
	}
	return len(m.Databases) == 0 && len(m.Storage) == 0 && len(m.Secrets) == 0 && len(m.ScheduledJobs) == 0
}

func writeDatabases(w io.Writer, dbs []infra.AggregatedDatabase) {
	if len(dbs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  Databases (%d):\n", len(dbs))
	for _, db := range dbs {
		_, _ = fmt.Fprintf(w, "    %s://%s\n", db.Engine, db.Name)
		if len(db.Schemas) > 0 {
			_, _ = fmt.Fprintf(w, "      schemas: %s\n", strings.Join(db.Schemas, ", "))
		}
		writeSources(w, "      ", db.Sources)
	}
}

func writeEvents(w io.Writer, ev *infra.AggregatedEvents) {
	if ev == nil || (len(ev.Publishes) == 0 && len(ev.Subscribes) == 0) {
		return
	}
	_, _ = fmt.Fprintln(w, "\n  Events:")
	if len(ev.Publishes) > 0 {
		_, _ = fmt.Fprintln(w, "    publishes:")
		for _, t := range ev.Publishes {
			_, _ = fmt.Fprintf(w, "      %s\n", t.Name)
			writeSources(w, "        ", t.Sources)
		}
	}
	if len(ev.Subscribes) > 0 {
		_, _ = fmt.Fprintln(w, "    subscribes:")
		for _, t := range ev.Subscribes {
			_, _ = fmt.Fprintf(w, "      %s\n", t.Name)
			writeSources(w, "        ", t.Sources)
		}
	}
}

func writeStorage(w io.Writer, buckets []infra.AggregatedStorage) {
	if len(buckets) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  Storage (%d):\n", len(buckets))
	for _, b := range buckets {
		if b.Retention != "" {
			_, _ = fmt.Fprintf(w, "    %s (retention: %s)\n", b.Name, b.Retention)
		} else {
			_, _ = fmt.Fprintf(w, "    %s\n", b.Name)
		}
		writeSources(w, "      ", b.Sources)
	}
}

func writeSecrets(w io.Writer, secrets []infra.AggregatedSecret) {
	if len(secrets) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  Secrets (%d):\n", len(secrets))
	for _, s := range secrets {
		_, _ = fmt.Fprintf(w, "    %s\n", s.Name)
		writeSources(w, "      ", s.Sources)
	}
}

func writeScheduledJobs(w io.Writer, jobs []infra.AggregatedScheduledJob) {
	if len(jobs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n  Scheduled jobs (%d):\n", len(jobs))
	for _, j := range jobs {
		if j.Entrypoint != "" {
			_, _ = fmt.Fprintf(w, "    %s — %q → %s\n", j.Name, j.Schedule, j.Entrypoint)
		} else {
			_, _ = fmt.Fprintf(w, "    %s — %q\n", j.Name, j.Schedule)
		}
		writeSources(w, "      ", j.Sources)
	}
}

func writeRuntime(w io.Writer, rt *infra.Runtime) {
	if rt == nil {
		return
	}
	_, _ = fmt.Fprintln(w, "\n  Runtime:")
	if rt.Ingress != nil {
		parts := make([]string, 0, 2)
		if rt.Ingress.Domain != nil && *rt.Ingress.Domain != "" {
			parts = append(parts, *rt.Ingress.Domain)
		}
		visibility := "private"
		if rt.Ingress.Public != nil && *rt.Ingress.Public {
			visibility = "public"
		}
		parts = append(parts, visibility)
		_, _ = fmt.Fprintf(w, "    ingress: %s\n", strings.Join(parts, " · "))
	}
	if rt.Scaling != nil {
		var parts []string
		if rt.Scaling.Max != nil {
			parts = append(parts, fmt.Sprintf("max=%d", *rt.Scaling.Max))
		}
		if rt.Scaling.Concurrency != nil {
			parts = append(parts, fmt.Sprintf("concurrency=%d", *rt.Scaling.Concurrency))
		}
		if len(parts) > 0 {
			_, _ = fmt.Fprintf(w, "    scaling: %s\n", strings.Join(parts, " · "))
		}
	}
}

func writeSources(w io.Writer, indent string, sources []infra.Source) {
	if len(sources) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%ssources:\n", indent)
	for _, s := range sources {
		_, _ = fmt.Fprintf(w, "%s  %s (%s)\n", indent, s.Project, s.Contributor)
	}
}

// structuredPlan returns the data of the result envelope: one entry per
// workload, an object for one, an array for several and [] for none. Each entry
// is encoded, then decoded into plain JSON values, so the envelope prints every
// object with sorted keys and every number as a JSON number, whatever Go type
// the manifest declares.
func structuredPlan(workloads []job.ProjectRef) (any, error) {
	documents := make([]any, 0, len(workloads))
	for _, p := range workloads {
		entry := map[string]any{"workload": p.Name}
		path := manifestPath(p)
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			entry["status"] = "missing"
		} else if manifest, diags := infra.LoadAggregatedManifest(path); diag.HasErrors(diags) {
			entry["status"] = "invalid"
			entry["diagnostics"] = diags
		} else {
			entry["status"] = "ok"
			entry["manifest"] = manifest
		}
		document, err := plainJSON(entry)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	if len(documents) == 1 {
		return documents[0], nil
	}
	return documents, nil
}

func plainJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	return document, nil
}
