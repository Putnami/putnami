package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// Adoption modes of `putnami init --from` (D-064).
const (
	// initModeImport hands the targets Putnami can map to its own runner and
	// cache.
	initModeImport = "import"
	// initModeDelegate keeps the tool running each target, with its cache.
	initModeDelegate = "delegate"
)

// initModeOption is the member of the extension's workspace options that
// records the mode, for its sync task to read.
const initModeOption = "mode"

// initSyncJob is the workspace-level job each extension's sync task runs as.
const initSyncJob = "workspace-sync"

// initDeclaredParam is the parameter of the preview's sync tasks that lists
// the extensions --apply would declare before its sync: a task that reads
// what the workspace declares, such as Nx's, reads them as declared, so the
// preview shows what --apply writes.
const initDeclaredParam = "declaredExtensions"

// initIsTerminal reports whether init can ask a question: stdin and stdout
// are terminals. Swapped in tests.
var initIsTerminal = func() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

// initPromptInput is where the answer to the mode question is read. Swapped
// in tests.
var initPromptInput io.Reader = os.Stdin

// previewInitSync runs the workspace sync tasks as a dry run with, as
// parameters, the mode and the extensions --apply would declare. Swapped in
// tests.
var previewInitSync = func(ctx context.Context, root string, cfg *wsproto.Config, mode string, declares, imports []string, env LifecycleEnv) error {
	params := initPreviewParams(mode, declares, imports)
	result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
		WorkspaceRoot: root,
		Config:        cfg,
		Job:           initSyncJob,
		Params:        params,
		DryRun:        true,
		Out:           env.out(),
	})
	if err != nil {
		return err
	}
	switch result.Outcome {
	case WorkspaceJobFailed:
		return fmt.Errorf("the dry run of %s failed", initSyncJob)
	case WorkspaceJobMissing, WorkspaceJobNoMatches:
		iox.Fprintln(os.Stdout, "  No project is a member yet, so no sync previews a change: --apply records the members and writes each one's putnami.json.")
	}
	return nil
}

// initPreviewParams are the parameters of the preview's sync tasks: a dry
// run, the mode, the extensions --apply would declare and, when not nil, the
// import list it would record.
func initPreviewParams(mode string, declares, imports []string) map[string]any {
	params := map[string]any{"dryRun": true}
	if mode != "" {
		params[initModeOption] = mode
	}
	if len(declares) > 0 {
		params[initDeclaredParam] = slices.Clone(declares)
	}
	if imports != nil {
		params[initImportOption] = slices.Clone(imports)
	}
	return params
}

func isCharDevice(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// offersBothModes reports whether the tool's extension adopts in import and
// in delegate mode, so init asks and records the mode.
func (s initSource) offersBothModes() bool {
	return slices.Contains(s.modes, initModeImport) && slices.Contains(s.modes, initModeDelegate)
}

// extensionsFor are the extensions a workspace adopted in mode declares: the
// tool's, then the ones its import mode hands targets to, which a delegated
// workspace that imports some projects declares too.
func (s initSource) extensionsFor(mode string, imports bool) []string {
	out := []string{s.starter.packageName}
	if mode == initModeImport || imports {
		out = append(out, s.importExtensions...)
	}
	return out
}

// initImportOption is the member of the extension's workspace options that
// lists the projects a delegated workspace imports.
const initImportOption = "import"

// splitImportFlag reads one --import value: comma-separated project paths.
// An empty item is kept, so the check names it.
func splitImportFlag(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		out = append(out, strings.TrimSpace(item))
	}
	return out
}

// importKey is the form two import entries are compared in: without the
// leading / or ./, cleaned. ok is false for an entry that names no project
// directory under the workspace root or is no valid pattern. It mirrors the
// check of go.putnami.dev/tooling/stacks.NewImportList, which the sync runs
// and this module does not import.
func importKey(entry string) (string, bool) {
	trimmed := strings.TrimSpace(entry)
	key := path.Clean(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/"), "./"))
	if trimmed == "" || key == "." || key == ".." || strings.HasPrefix(key, "../") || strings.HasPrefix(key, "-") || strings.Contains(key, "**") {
		return "", false
	}
	if _, err := path.Match(key, ""); err != nil {
		return "", false
	}
	return key, true
}

// validateInitImport checks --import against --from and --mode before
// anything is written.
func validateInitImport(flags initFlags, source initSource) error {
	if !flags.importSet {
		return nil
	}
	if !source.importList {
		return cmderr.Usagef("--import: --from %s keeps no import list; it adopts in %s", flags.from, strings.Join(source.modes, " or "))
	}
	if len(flags.imports) == 0 {
		return cmderr.Usagef("--import needs project paths, such as --import /packages/pricing,/packages/billing")
	}
	for _, entry := range flags.imports {
		if _, ok := importKey(entry); !ok {
			return cmderr.Usagef("--import %q names no project directory under the workspace root", entry)
		}
	}
	if flags.modeSet && flags.mode != initModeDelegate {
		return cmderr.Usagef("--import selects the projects a delegated workspace imports; in %s mode every TypeScript project is imported already: give --mode %s", flags.mode, initModeDelegate)
	}
	return nil
}

// initImportList is the import list a run records: the recorded entries,
// then each --import entry they do not hold yet, which added lists. Without
// --import it is the recorded list. --import in a mode other than delegate
// is a usage error.
func initImportList(flags initFlags, mode string, recorded []string) (list, added []string, err error) {
	list = slices.Clone(recorded)
	if !flags.importSet {
		return list, nil, nil
	}
	if mode != initModeDelegate {
		return nil, nil, cmderr.Usagef("--import selects the projects a delegated workspace imports, and this workspace adopts in %s mode, where every TypeScript project is imported already", mode)
	}
	held := map[string]bool{}
	for _, entry := range recorded {
		if key, ok := importKey(entry); ok {
			held[key] = true
		}
	}
	for _, entry := range flags.imports {
		key, _ := importKey(entry)
		if held[key] {
			continue
		}
		held[key] = true
		list = append(list, entry)
		added = append(added, entry)
	}
	return list, added, nil
}

// recordedInitImport reads the import list the workspace config records for
// the extension.
func recordedInitImport(cfg *wsproto.Config, extension string) []string {
	if cfg == nil {
		return nil
	}
	items, _ := cfg.Options[extension][initImportOption].([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// validateInitMode checks --mode against --from before anything is written.
func validateInitMode(flags initFlags, source initSource) error {
	if !flags.modeSet {
		return nil
	}
	if !slices.Contains(source.modes, flags.mode) {
		return cmderr.Usagef("--mode %q: --from %s adopts in %s", flags.mode, flags.from, strings.Join(source.modes, " or "))
	}
	return nil
}

// resolveInitMode is the mode of an init --from run: --mode, else the mode
// the workspace records, else delegate with --import, else the answer to the
// question in a terminal, else import. A tool that offers one mode adopts in
// it.
func resolveInitMode(flags initFlags, source initSource, recorded string) (string, error) {
	if flags.modeSet {
		return flags.mode, nil
	}
	if !source.offersBothModes() {
		return source.modes[0], nil
	}
	if recorded != "" {
		return recorded, nil
	}
	if flags.importSet {
		return initModeDelegate, nil
	}
	if !initIsTerminal() {
		return initModeImport, nil
	}
	return askInitMode(flags.from, initPromptInput, os.Stdout)
}

// askInitMode asks for the mode, with import preselected: an empty answer
// imports.
func askInitMode(tool string, in io.Reader, out io.Writer) (string, error) {
	reader := bufio.NewReader(in)
	iox.Fprintf(out, "\n  How should Putnami adopt this %s repository?\n", tool)
	iox.Fprintf(out, "    1) import    Putnami's runner and cache take over the targets it can map (default)\n")
	iox.Fprintf(out, "    2) delegate  %s keeps running each target, with its cache\n", tool)
	for range 3 {
		iox.Fprint(out, "  Mode [1]: ")
		line, err := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		switch answer {
		case "", "1", initModeImport:
			if answer == "" && err != nil && err != io.EOF {
				return "", err
			}
			return initModeImport, nil
		case "2", initModeDelegate:
			return initModeDelegate, nil
		}
		if err != nil {
			return "", cmderr.Usagef("no mode chosen: rerun with --mode %s or --mode %s", initModeImport, initModeDelegate)
		}
		iox.Fprintf(out, "  Answer 1 (import) or 2 (delegate).\n")
	}
	return "", cmderr.Usagef("no mode chosen: rerun with --mode %s or --mode %s", initModeImport, initModeDelegate)
}

// initToolOptions is the option layer init writes for a tool's extension.
type initToolOptions struct {
	// Mode is the adoption mode the extension's sync task reads.
	Mode string `json:"mode"`
	// Import lists the projects a delegated workspace imports.
	Import []string `json:"import,omitempty"`
}

// initOptions is the `options` of a new workspace adopted from a tool that
// offers both modes: the mode and the import list, under the tool's
// extension.
func initOptions(source initSource, mode string, imports []string) map[string]initToolOptions {
	if !source.offersBothModes() {
		return nil
	}
	return map[string]initToolOptions{source.starter.packageName: {Mode: mode, Import: imports}}
}

// recordedInitMode reads the mode the workspace config records for the
// extension; empty when it records none.
func recordedInitMode(cfg *wsproto.Config, extension string) string {
	if cfg == nil {
		return ""
	}
	mode, _ := cfg.Options[extension][initModeOption].(string)
	return mode
}

// rerunInitFrom is init --from in a workspace that already exists: it shows
// what adopting in the resolved mode would change, and writes only with
// --apply, which records the mode, declares and installs the extensions the
// mode needs, and runs projects sync.
func rerunInitFrom(ctx context.Context, wsRoot string, flags initFlags, source initSource, env LifecycleEnv) error {
	cfg := wsproto.Load(wsRoot)
	ext := source.starter.packageName
	recorded := recordedInitMode(cfg, ext)
	mode, err := resolveInitMode(flags, source, recorded)
	if err != nil {
		return err
	}
	imports, addedImports, err := initImportList(flags, mode, recordedInitImport(cfg, ext))
	if err != nil {
		return err
	}
	declared := initDeclaredNames(wsRoot, cfg)
	var missing []string
	for _, e := range source.extensionsFor(mode, mode == initModeDelegate && len(imports) > 0) {
		if !slices.Contains(declared, e) {
			missing = append(missing, e)
		}
	}
	modeChanges := source.offersBothModes() && recorded != mode

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  This place is already a Putnami workspace: %s\n", wsRoot)
	iox.Fprintf(os.Stdout, "  Mode: %s\n", mode)
	if modeChanges {
		from := recorded
		if from == "" {
			from = "none"
		}
		iox.Fprintf(os.Stdout, "  %s: options[%q].%s %s → %s\n", wsproto.WorkspaceConfigFilename, ext, initModeOption, from, mode)
	}
	for _, e := range addedImports {
		iox.Fprintf(os.Stdout, "  %s: options[%q].%s + %s\n", wsproto.WorkspaceConfigFilename, ext, initImportOption, e)
	}
	for _, e := range missing {
		iox.Fprintf(os.Stdout, "  %s: extensions + %s\n", wsproto.WorkspaceConfigFilename, e)
	}
	// The preview's sync reads the list --apply would record; without
	// --import it reads the recorded one.
	var previewImports []string
	if flags.importSet {
		previewImports = imports
	}

	if !flags.apply {
		// An extension the workspace does not declare yet cannot preview its
		// sync: the preview covers the declared ones, which read the
		// extensions --apply declares as declared.
		if slices.Contains(declared, ext) {
			iox.Fprintln(os.Stdout)
			iox.Fprintf(os.Stdout, "  → Previewing the %s sync (nothing is written)…\n", flags.from)
			if err := previewInitSync(ctx, wsRoot, cfg, mode, missing, previewImports, env); err != nil {
				return protocolcli.WithNext(fmt.Errorf("preview the %s sync: %w", flags.from, err), "putnami projects sync --dry-run")
			}
		}
		iox.Fprintln(os.Stdout)
		iox.Fprintln(os.Stdout, "  Nothing was written. To record the mode and synchronize:")
		if flags.importSet {
			iox.Fprintf(os.Stdout, "    putnami init --from %s --mode %s --import %s --apply\n", flags.from, mode, strings.Join(flags.imports, ","))
		} else {
			iox.Fprintf(os.Stdout, "    putnami init --from %s --mode %s --apply\n", flags.from, mode)
		}
		return nil
	}

	if modeChanges || len(missing) > 0 || len(addedImports) > 0 {
		var record []string
		if len(addedImports) > 0 {
			record = imports
		}
		if err := recordInitAdoption(wsRoot, ext, mode, missing, source.offersBothModes(), record); err != nil {
			return fmt.Errorf("record the %s adoption in %s: %w", flags.from, wsproto.WorkspaceConfigFilename, err)
		}
		iox.Fprintf(os.Stdout, "  ✓ %s updated\n", wsproto.WorkspaceConfigFilename)
	}
	cfg = wsproto.Load(wsRoot)
	for _, e := range missing {
		if err := installExtension(ctx, wsRoot, cfg, []string{shared.ChannelArtifact(e, env.channel)}, ""); err != nil {
			return protocolcli.WithNext(fmt.Errorf("install extension %s: %w", e, err), "putnami extensions install")
		}
		cfg = wsproto.Load(wsRoot)
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Synchronizing the %s projects…\n", flags.from)
	before := initDeclaredNames(wsRoot, cfg)
	if err := syncInitProjects(ctx, wsRoot, cfg, env); err != nil {
		return protocolcli.WithNext(fmt.Errorf("synchronize the %s projects: %w", flags.from, err), "putnami projects sync")
	}
	if err := installSyncDeclaredExtensions(ctx, wsRoot, before, env); err != nil {
		return err
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  ✓ Adopted in %s mode\n", mode)
	return nil
}

// initDeclaredNames are the names of the extensions the workspace
// declares: a name as written, and an entry that is a path by the name its
// manifest states, such as a source checkout of the extension.
func initDeclaredNames(wsRoot string, cfg *wsproto.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, entry := range cfg.Extensions.Names() {
		out = append(out, entry)
		if !strings.HasPrefix(entry, "/") && !strings.HasPrefix(entry, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(strings.TrimPrefix(entry, "/")), "putnami.extension.json")) //nolint:gosec // a manifest the workspace declares
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Name != "" {
			out = append(out, manifest.Name)
		}
	}
	return out
}

// recordInitAdoption writes the mode, the missing extensions and, when not
// nil, the import list into the workspace config, keeping every other member
// and its order.
func recordInitAdoption(wsRoot, ext, mode string, missing []string, recordMode bool, imports []string) error {
	path := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(path)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		// extensions is a list of names, or an object of name to version
		// constraint: each keeps its form.
		value, _ := raw.Get("extensions")
		switch declared := value.(type) {
		case *jsonutil.OrderedMap:
			for _, e := range missing {
				declared.Set(e, "")
			}
		case []any:
			raw.Set("extensions", append(declared, stringsToAny(missing)...))
		case nil:
			raw.Set("extensions", stringsToAny(missing))
		default:
			return fmt.Errorf("extensions is neither a list nor an object")
		}
	}
	if recordMode {
		options := raw.GetMap("options")
		if options == nil {
			options = jsonutil.New()
			raw.Set("options", options)
		}
		layer := options.GetMap(ext)
		if layer == nil {
			layer = jsonutil.New()
			options.Set(ext, layer)
		}
		layer.Set(initModeOption, mode)
		if imports != nil {
			layer.Set(initImportOption, stringsToAny(imports))
		}
	}
	return jsonutil.WriteFile(path, raw)
}

func stringsToAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// installSyncDeclaredExtensions installs the extensions the workspace config
// declares after a tool's sync and did not declare before it, such as the
// language extensions moon's import mode maps projects to, then pins their
// toolchains, runs their installers and synchronizes the projects again, so
// their probes answer.
func installSyncDeclaredExtensions(ctx context.Context, root string, before []string, env LifecycleEnv) error {
	cfg := wsproto.Load(root)
	var added []string
	for _, name := range initDeclaredNames(root, cfg) {
		if !slices.Contains(before, name) && !slices.Contains(added, name) {
			added = append(added, name)
		}
	}
	if len(added) == 0 {
		return nil
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  → Adding the extensions the sync declared: %s…\n", strings.Join(added, ", "))
	for _, name := range added {
		if err := installExtension(ctx, root, cfg, []string{shared.ChannelArtifact(name, env.channel)}, ""); err != nil {
			return protocolcli.WithNext(fmt.Errorf("install extension %s: %w", name, err), "putnami install")
		}
		cfg = wsproto.Load(root)
	}
	if err := installInitWorkspace(ctx, root, cfg, env); err != nil {
		return err
	}
	if err := syncInitProjects(ctx, root, wsproto.Load(root), env); err != nil {
		return protocolcli.WithNext(fmt.Errorf("synchronize the projects: %w", err), "putnami projects sync")
	}
	return nil
}
