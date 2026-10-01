package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// InfraPlan reads the aggregated infra-requirements manifest for one or
// more workloads and prints a human-readable deployment plan. It is the
// reference consumer for the infra protocol: it exercises every shape
// (databases, events, storage, secrets, scheduled jobs, runtime) and
// surfaces what a real deployer would have to interpret.
//
// Usage:
//
//	putnami infra plan                 # plan every workload in the workspace
//	putnami infra plan <project-name>  # plan one named workload
//
// A workload that has no aggregated manifest (never built, or build did
// not finish) is reported as such; the command does not run the build.
func InfraPlan(wsRoot string, args []string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	target := strings.TrimSpace(strings.Join(args, " "))
	workloads, err := selectInfraWorkloads(ws, target)
	if err != nil {
		return err
	}
	if len(workloads) == 0 {
		iox.Fprintln(os.Stdout, "No workloads found.")
		return nil
	}

	first := true
	for _, p := range workloads {
		if !first {
			iox.Fprintln(os.Stdout)
		}
		first = false
		if err := printWorkloadInfraPlan(ws.Root, p); err != nil {
			return err
		}
	}
	return nil
}

// selectInfraWorkloads returns the workloads to plan. With no target,
// every application workload in the workspace is included; with a target,
// only that project is returned (and the function errors if it isn't a
// workload).
func selectInfraWorkloads(ws *workspace.Workspace, target string) ([]*workspace.Project, error) {
	if target == "" {
		var out []*workspace.Project
		for _, p := range ws.Projects {
			if isInfraWorkload(p) {
				out = append(out, p)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}

	p := ws.ProjectByName(target)
	if p == nil {
		return nil, cmderr.NotFoundf("project %q not found in workspace", target)
	}
	if !isInfraWorkload(p) {
		return nil, cmderr.NotFoundf("project %q is not an application workload (type=%q)", target, p.Type)
	}
	return []*workspace.Project{p}, nil
}

// isInfraWorkload mirrors the aggregator's isWorkloadProject: empty type
// or "application" counts as a deployable workload.
func isInfraWorkload(p *workspace.Project) bool {
	return p.Type == "" || p.Type == "application"
}

// printWorkloadInfraPlan loads and pretty-prints the aggregated manifest
// for a single workload. A missing manifest is reported, not an error —
// the caller may want to plan multiple workloads where only some have built.
func printWorkloadInfraPlan(wsRoot string, p *workspace.Project) error {
	heading := fmt.Sprintf("Workload: %s", p.Name)
	iox.Fprintln(os.Stdout, heading)
	iox.Fprintln(os.Stdout, strings.Repeat("─", len(heading)))

	path := filepath.Join(wsRoot, p.Path, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
	manifest, diags := infra.LoadAggregatedManifest(path)
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		iox.Fprintf(os.Stdout, "  No aggregated manifest at %s.\n", path)
		iox.Fprintln(os.Stdout, "  Run `putnami build` for this workload to emit one.")
		return nil
	}
	if diag.HasErrors(diags) {
		iox.Fprintln(os.Stdout, "  Manifest failed to parse or validate:")
		for _, d := range diags {
			iox.Fprintf(os.Stdout, "    [%s] %s\n", d.Code, d.Message)
		}
		return nil
	}
	if manifest == nil {
		iox.Fprintln(os.Stdout, "  Manifest is empty.")
		return nil
	}

	printDatabases(manifest.Databases)
	printEvents(manifest.Events)
	printStorage(manifest.Storage)
	printSecrets(manifest.Secrets)
	printScheduledJobs(manifest.ScheduledJobs)
	printRuntime(manifest.Runtime)

	if hasNoResources(manifest) && manifest.Runtime == nil {
		iox.Fprintln(os.Stdout, "  (no infrastructure declared)")
	}
	return nil
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

func printDatabases(dbs []infra.AggregatedDatabase) {
	if len(dbs) == 0 {
		return
	}
	iox.Fprintf(os.Stdout, "\n  Databases (%d):\n", len(dbs))
	for _, db := range dbs {
		iox.Fprintf(os.Stdout, "    %s://%s\n", db.Engine, db.Name)
		if len(db.Schemas) > 0 {
			iox.Fprintf(os.Stdout, "      schemas: %s\n", strings.Join(db.Schemas, ", "))
		}
		printSources("      ", db.Sources)
	}
}

func printEvents(ev *infra.AggregatedEvents) {
	if ev == nil || (len(ev.Publishes) == 0 && len(ev.Subscribes) == 0) {
		return
	}
	iox.Fprintln(os.Stdout, "\n  Events:")
	if len(ev.Publishes) > 0 {
		iox.Fprintln(os.Stdout, "    publishes:")
		for _, t := range ev.Publishes {
			iox.Fprintf(os.Stdout, "      %s\n", t.Name)
			printSources("        ", t.Sources)
		}
	}
	if len(ev.Subscribes) > 0 {
		iox.Fprintln(os.Stdout, "    subscribes:")
		for _, t := range ev.Subscribes {
			iox.Fprintf(os.Stdout, "      %s\n", t.Name)
			printSources("        ", t.Sources)
		}
	}
}

func printStorage(s []infra.AggregatedStorage) {
	if len(s) == 0 {
		return
	}
	iox.Fprintf(os.Stdout, "\n  Storage (%d):\n", len(s))
	for _, b := range s {
		if b.Retention != "" {
			iox.Fprintf(os.Stdout, "    %s (retention: %s)\n", b.Name, b.Retention)
		} else {
			iox.Fprintf(os.Stdout, "    %s\n", b.Name)
		}
		printSources("      ", b.Sources)
	}
}

func printSecrets(s []infra.AggregatedSecret) {
	if len(s) == 0 {
		return
	}
	iox.Fprintf(os.Stdout, "\n  Secrets (%d):\n", len(s))
	for _, sec := range s {
		iox.Fprintf(os.Stdout, "    %s\n", sec.Name)
		printSources("      ", sec.Sources)
	}
}

func printScheduledJobs(jobs []infra.AggregatedScheduledJob) {
	if len(jobs) == 0 {
		return
	}
	iox.Fprintf(os.Stdout, "\n  Scheduled jobs (%d):\n", len(jobs))
	for _, j := range jobs {
		if j.Entrypoint != "" {
			iox.Fprintf(os.Stdout, "    %s — %q → %s\n", j.Name, j.Schedule, j.Entrypoint)
		} else {
			iox.Fprintf(os.Stdout, "    %s — %q\n", j.Name, j.Schedule)
		}
		printSources("      ", j.Sources)
	}
}

func printRuntime(rt *infra.Runtime) {
	if rt == nil {
		return
	}
	iox.Fprintln(os.Stdout, "\n  Runtime:")
	if rt.Ingress != nil {
		parts := make([]string, 0, 2)
		if rt.Ingress.Domain != nil && *rt.Ingress.Domain != "" {
			parts = append(parts, *rt.Ingress.Domain)
		}
		public := "private"
		if rt.Ingress.Public != nil && *rt.Ingress.Public {
			public = "public"
		}
		parts = append(parts, public)
		iox.Fprintf(os.Stdout, "    ingress: %s\n", strings.Join(parts, " · "))
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
			iox.Fprintf(os.Stdout, "    scaling: %s\n", strings.Join(parts, " · "))
		}
	}
}

func printSources(indent string, sources []infra.Source) {
	if len(sources) == 0 {
		return
	}
	iox.Fprintf(os.Stdout, "%ssources:\n", indent)
	for _, s := range sources {
		iox.Fprintf(os.Stdout, "%s  %s (%s)\n", indent, s.Project, s.Contributor)
	}
}

// InfraPlanJSON is the structured equivalent of InfraPlan for tooling.
// It dumps the aggregated manifest (or an error) for each selected
// workload as one JSON object per line, so downstream tools can consume
// without parsing the human-readable output.
func InfraPlanJSON(wsRoot string, args []string) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	target := strings.TrimSpace(strings.Join(args, " "))
	workloads, err := selectInfraWorkloads(ws, target)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(iox.Stdout())
	for _, p := range workloads {
		entry := map[string]any{"workload": p.Name}
		path := filepath.Join(ws.Root, p.Path, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			entry["status"] = "missing"
			_ = enc.Encode(entry)
			continue
		}
		manifest, diags := infra.LoadAggregatedManifest(path)
		if diag.HasErrors(diags) {
			entry["status"] = "invalid"
			entry["diagnostics"] = diags
			_ = enc.Encode(entry)
			continue
		}
		entry["status"] = "ok"
		entry["manifest"] = manifest
		_ = enc.Encode(entry)
	}
	return nil
}
