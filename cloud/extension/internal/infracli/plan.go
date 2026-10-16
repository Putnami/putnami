// Package infracli serves `putnami infra plan`: it reads the aggregated
// infra-requirements manifest of each deployable workload and prints the
// deployment plan a deployer would have to interpret.
//
// The command runs as an interactive extension subcommand, so the CLI hands
// this process the terminal's streams and a job context. The context lists
// every workspace project with its resolved type, which is how the command
// tells a workload from a library without loading the workspace itself.
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

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/job"
)

// RuntimeCommand is the first argument the task passes to the extension
// runtime to run this command.
const RuntimeCommand = "infra-plan"

// commandPath names the command in its result envelope.
const commandPath = "infra plan"

// contextFlag is the argument the CLI passes the job context file with.
const contextFlag = "--putnamiContext"

// Run serves `putnami infra plan [<project-name>]` and returns the exit code.
//
// args are the runtime arguments after RuntimeCommand: the user's arguments and
// the job context the CLI appends. With no project name it plans every
// workload of the workspace, sorted by name; with one it plans that project and
// fails when it is unknown or is not a workload. A workload without an
// aggregated manifest is reported, not built.
//
// Human output is plain text on stdout. Under --output=json or jsonl, stdout
// carries one version-2 result envelope whose data is one entry per workload:
// an object for one workload, an array for several, and [] for none. A failure
// prints `putnami: <message>` on stderr and, in a structured mode, a failure
// envelope on stdout.
func Run(args []string, stdout, stderr io.Writer) int {
	contextPath, positionals := splitContext(args)
	mode := protocolcli.OutputAuto
	ctx, err := loadContext(contextPath)
	if err == nil {
		mode = outputMode(ctx.Params)
		err = plan(ctx, strings.TrimSpace(strings.Join(positionals, " ")), mode, stdout)
	}
	if err == nil {
		return protocolcli.ExitSuccess
	}
	if !mode.IsStructured() {
		_, _ = fmt.Fprintf(stderr, "putnami: %v\n", err)
		if next := protocolcli.SuggestedNext(err); next != "" {
			_, _ = fmt.Fprintf(stderr, "  Try: %s\n", next)
		}
		return protocolcli.ExitCodeForError(err)
	}
	code, _ := protocolcli.WriteResultV2(stdout, mode, protocolcli.NewResultV2(commandPath, nil, err))
	_, _ = fmt.Fprintf(stderr, "putnami: %v\n", err)
	return code
}

// splitContext separates the job context file from the user's arguments.
func splitContext(args []string) (string, []string) {
	var contextPath string
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == contextFlag && i+1 < len(args):
			contextPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], contextFlag+"="):
			contextPath = strings.TrimPrefix(args[i], contextFlag+"=")
		default:
			positionals = append(positionals, args[i])
		}
	}
	return contextPath, positionals
}

func loadContext(path string) (*job.Context, error) {
	if path == "" {
		return nil, protocolcli.Classify(
			fmt.Errorf("%s runs through the putnami CLI: run `putnami infra plan`", RuntimeCommand),
			protocolcli.ErrUsage)
	}
	return job.ParseFile(path)
}

// outputMode reads the output mode the CLI resolved and forwarded as the
// `output` parameter. An absent or unknown value is human output.
func outputMode(params job.Params) protocolcli.OutputMode {
	mode := protocolcli.OutputMode(params.String("output"))
	if !mode.Valid() {
		return protocolcli.OutputAuto
	}
	return mode
}

func plan(ctx *job.Context, target string, mode protocolcli.OutputMode, stdout io.Writer) error {
	workloads, err := selectWorkloads(ctx.WorkspaceProjects, target)
	if err != nil {
		return err
	}
	if mode.IsStructured() {
		return writeStructured(workloads, mode, stdout)
	}
	if len(workloads) == 0 {
		_, err := fmt.Fprintln(stdout, "No workloads found.")
		return err
	}
	for i, p := range workloads {
		if i > 0 {
			_, _ = fmt.Fprintln(stdout)
		}
		writeWorkloadPlan(stdout, p)
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
// type is "application", or empty, which the CLI reads as "application".
func isWorkload(p job.ProjectRef) bool {
	return p.Type == "" || p.Type == "application"
}

func manifestPath(p job.ProjectRef) string {
	return filepath.Join(p.FullPath, infra.AggregatedManifestDir, infra.AggregatedManifestFilename)
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

// planEntry is one workload's structured plan. Its JSON keys are in the order
// the result envelope prints them: data is re-decoded before it is written, so
// every object's keys print sorted.
type planEntry struct {
	Diagnostics []diag.Diagnostic        `json:"diagnostics,omitempty"`
	Manifest    *infra.AggregatedManifest `json:"manifest,omitempty"`
	Status      string                   `json:"status"`
	Workload    string                   `json:"workload"`
}

// writeStructured writes the plan as one result envelope. Each entry is
// encoded, then decoded into plain JSON values, so the envelope prints every
// object with sorted keys and every number as a JSON number, whatever Go type
// the manifest declares.
func writeStructured(workloads []job.ProjectRef, mode protocolcli.OutputMode, stdout io.Writer) error {
	documents := make([]any, 0, len(workloads))
	for _, p := range workloads {
		entry := planEntry{Workload: p.Name}
		path := manifestPath(p)
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			entry.Status = "missing"
		} else if manifest, diags := infra.LoadAggregatedManifest(path); diag.HasErrors(diags) {
			entry.Status = "invalid"
			entry.Diagnostics = diags
		} else {
			entry.Status = "ok"
			entry.Manifest = manifest
		}
		document, err := plainJSON(entry)
		if err != nil {
			return err
		}
		documents = append(documents, document)
	}
	var data any = documents
	if len(documents) == 1 {
		data = documents[0]
	}
	_, err := protocolcli.WriteResultV2(stdout, mode, protocolcli.NewResultV2(commandPath, data, nil))
	return err
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
