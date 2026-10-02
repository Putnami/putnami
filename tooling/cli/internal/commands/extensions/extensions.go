package extensions

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ExtensionsInstall installs extensions from the workspace config and lock file.
// If a specific name is given, only that extension is installed/added.
// Pass --latest in args to ignore the lock file and resolve the latest versions.
// outputFormat selects the result rendering ("jsonl" for machine-readable).
func ExtensionsInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return extensionsInstall(ctx, wsRoot, cfg, args, outputFormat, os.Stdout)
}

// ExtensionsInstallWithWriter is ExtensionsInstall with the human progress
// stream chosen by the caller. It exists for internal/commands' combined
// `install` lifecycle command (install.go), which is outside this vertical and
// threads its own writer through so an implicit install under
// --output=json|jsonl never writes to the real stdout (slice A5a).
func ExtensionsInstallWithWriter(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, out io.Writer) error {
	return ExtensionsInstallWithOptions(ctx, wsRoot, cfg, args, InstallOptions{OutputFormat: outputFormat, Out: out})
}

// ExtensionsInstallWithOptions installs extensions and reports typed outcomes
// without requiring lifecycle callers to parse the human progress stream.
func ExtensionsInstallWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, opts InstallOptions) error {
	return extensionsInstallWithOptions(ctx, wsRoot, cfg, args, opts)
}

// extensionsInstall is ExtensionsInstall with the human progress stream chosen by
// the caller. Install threads the first-use bootstrap's stream through here so an
// implicit install under --output=json|jsonl cannot write to the real stdout
// (slice A5a).
func extensionsInstall(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, out io.Writer) error {
	return extensionsInstallWithOptions(ctx, wsRoot, cfg, args, InstallOptions{OutputFormat: outputFormat, Out: out})
}

func extensionsInstallWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, opts InstallOptions) error {
	rest, target, err := parseArtifactTarget(wsRoot, args)
	if err != nil {
		return err
	}
	if err := installArtifactsWithOptions(ctx, wsRoot, cfg, rest, extensionOpsForTarget(wsRoot, target), opts); err != nil {
		return err
	}
	// Install hooks are the host extension's own onInstall: running them after
	// materializing a foreign platform's bytes would execute THIS machine's
	// extension against a tree it does not own, and running them at all after a
	// --dest materialization would mutate a workspace the command deliberately
	// left untouched. Skip them, loudly.
	if target.Materializes() {
		return completeMaterialization(wsRoot, target, materializeNoticeWriter(opts.OutputFormat, opts.Out))
	}
	if opts.DeferInstallHooks {
		return nil
	}
	if err := runExtensionInstallHooks(ctx, wsRoot, cfg, rest, opts.OutputFormat, opts.Out); err != nil {
		return fmt.Errorf("extension install hooks: %w", err)
	}
	return nil
}

// completeMaterialization closes out a `--platform`/`--dest` run: it normalizes
// the destination into a reproducible drop-in artifact-store root and then
// states, on the human stream, what the run deliberately did not do.
func completeMaterialization(wsRoot string, target extension.ArtifactTarget, notice io.Writer) error {
	installer := extension.NewInstaller(wsRoot)
	installer.Target = target
	if err := installer.FinalizeMaterialization(); err != nil {
		return fmt.Errorf("finalize extension materialization: %w", err)
	}
	printMaterializeNotice(notice, target)
	return nil
}

// ExtensionsInstallMaterializes reports whether an `extensions install` argument
// list asks for a packaging materialization (`--platform` for another platform,
// or `--dest`) rather than an install into this workspace.
//
// The dispatcher calls it to decide whether to regenerate the shared AI context
// afterwards: that context describes what THIS workspace can run, so rewriting
// it from a linux/amd64 materialization would be a lie written into a committed
// file. A malformed flag reports false and lets the command itself produce the
// usage error.
func ExtensionsInstallMaterializes(args []string) bool {
	_, target, err := parseArtifactTarget("", args)
	if err != nil {
		return false
	}
	return target.Materializes()
}

// parseArtifactTarget splits the materialization flags out of an install
// argument list and validates them, returning the remaining arguments (the
// optional artifact name, --latest, …) untouched.
//
// It is a hand-rolled pass rather than a catalog parse because the arguments
// reach this package already stripped of global flags, and because the same
// list is re-scanned downstream for the artifact name — leaving `--platform`
// and its `linux/amd64` value in it would make the value look like an
// extension name.
//
// wsRoot may be empty for a pure validity check (ExtensionsInstallMaterializes);
// the machine-global-store guard is then skipped.
func parseArtifactTarget(wsRoot string, args []string) ([]string, extension.ArtifactTarget, error) {
	var target extension.ArtifactTarget
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		flag, value, hasInline := strings.Cut(arg, "=")
		if flag != platformFlag && flag != destFlag {
			rest = append(rest, arg)
			continue
		}
		if !hasInline {
			// Take the value from the remainder rather than by index, so the
			// bound is carried by the slice itself.
			tail := args[i+1:]
			if len(tail) == 0 {
				return nil, target, cmderr.Usagef("flag %s requires a value: %s %s", flag, flag, artifactTargetValueName(flag))
			}
			value = tail[0]
			i++
		}
		switch flag {
		case platformFlag:
			goos, goarch, ok := lockfile.SplitPlatformKey(value)
			if !ok {
				return nil, target, cmderr.Usagef(
					"invalid %s value %q: expected <os>/<arch>, for example linux/amd64", platformFlag, value)
			}
			target.OS, target.Arch = goos, goarch
		case destFlag:
			if strings.TrimSpace(value) == "" {
				return nil, target, cmderr.Usagef("flag %s requires a directory", destFlag)
			}
			abs, err := filepath.Abs(value)
			if err != nil {
				return nil, target, cmderr.Usagef("invalid %s value %q: %v", destFlag, value, err)
			}
			target.StoreRoot = abs
		}
	}

	if err := checkDestNotGlobalStore(wsRoot, target.StoreRoot); err != nil {
		return nil, target, err
	}
	return rest, target, nil
}

const (
	platformFlag = "--platform"
	destFlag     = "--dest"
)

func artifactTargetValueName(flag string) string {
	if flag == platformFlag {
		return "<os>/<arch>"
	}
	return "<dir>"
}

// checkDestNotGlobalStore refuses a --dest that IS the machine-global artifact
// store. The destination is finalized as a packaging output — its advisory
// lock, staging and per-digest lock directories are deleted and its modes
// relaxed — which on a live store would break the cross-process lock identity
// that keeps GC from reaping an in-flight admit, and would widen a namespace
// every repo on the machine executes from.
func checkDestNotGlobalStore(wsRoot, dest string) error {
	if wsRoot == "" || dest == "" {
		return nil
	}
	global := store.ResolveArtifactStoreRoot(wsRoot)
	if !sameDirPath(dest, global) {
		return nil
	}
	return cmderr.Usagef(
		"%s must not be the machine-global artifact store (%s): the destination is finalized as a packaging output, "+
			"which strips the store's locks and recency sidecars; point it at a staging directory instead",
		destFlag, global)
}

// sameDirPath compares two directory paths, following directory links when
// both exist, a junction on Windows included, so a /tmp → /private/tmp style
// indirection cannot slip past the guard.
func sameDirPath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := dirlink.Resolve(a)
	rb, errB := dirlink.Resolve(b)
	return errA == nil && errB == nil && ra == rb
}

// materializeNoticeWriter picks the stream the materialization notice goes to:
// the caller's human stream normally, stderr under a structured output format
// where stdout carries the machine contract.
func materializeNoticeWriter(outputFormat string, out io.Writer) io.Writer {
	if outputFormat == "jsonl" {
		return os.Stderr
	}
	return out
}

// printMaterializeNotice states what a materialization deliberately did NOT do,
// so "the hooks did not run" is never something a user has to infer.
func printMaterializeNotice(w io.Writer, target extension.ArtifactTarget) {
	if w == nil {
		return
	}
	platform := "this host's platform"
	if target.OS != "" && target.Arch != "" {
		platform = lockfile.PlatformKey(target.OS, target.Arch)
	}
	iox.Fprintf(w, "  ℹ materialized extensions for %s\n", platform)
	if target.StoreRoot != "" {
		iox.Fprintf(w, "    artifact store: %s\n", target.StoreRoot)
	}
	iox.Fprintf(w, "    skipped: install hooks, the stable extension links, and the %s write —\n"+
		"    this tree is packaging output, not an install for this workspace\n", lockfile.LockFilename)
}

// ExtensionsUpdate updates one or all extensions to the latest compatible version.
// outputFormat selects the result rendering ("jsonl" for machine-readable).
func ExtensionsUpdate(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return ExtensionsUpdateWithOptions(ctx, wsRoot, cfg, args, ArtifactUpdateOptions{}, outputFormat)
}

func ExtensionsUpdateWithOptions(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, opts ArtifactUpdateOptions, outputFormat string) error {
	return updateArtifacts(ctx, wsRoot, cfg, args, extensionOps(wsRoot), opts, outputFormat)
}

// parseExtensionArg parses "name" or "name@version" from args.
func parseExtensionArg(arg string) (string, string) {
	if isLocalExtensionRef(arg) {
		return arg, ""
	}

	// Handle @scope/name@version
	if strings.HasPrefix(arg, "@") {
		// @scope/name@version → split at second @
		rest := arg[1:]
		if idx := strings.IndexByte(rest, '@'); idx >= 0 {
			return "@" + rest[:idx], rest[idx+1:]
		}
		return arg, ""
	}
	// Handle name@version
	if idx := strings.IndexByte(arg, '@'); idx >= 0 {
		return arg[:idx], arg[idx+1:]
	}
	return arg, ""
}

func resolveLocalExtensionRef(wsRoot, ref string) (*extension.ExtensionDescription, error) {
	if !isLocalExtensionRef(ref) {
		return nil, nil
	}
	// A hosted run loads a local ref only as one of the workspace's own path
	// extensions, from the workspace and never from the working directory.
	if runcredential.Hosted() {
		if ext := extension.LoadWorkspacePathExtension(wsRoot, ref); ext != nil {
			return ext, nil
		}
		return nil, fmt.Errorf("local extension %s: a hosted run (--credential-fd) runs only extensions installed from the artifact store "+
			"and the workspace's own path extensions, declared by a path inside the workspace", ref)
	}

	for _, dir := range localExtensionCandidateDirs(wsRoot, ref) {
		ext := extension.LoadExtensionFromDir(dir, ref)
		if ext != nil {
			return ext, nil
		}
	}

	return nil, fmt.Errorf("local extension path does not contain putnami.extension.json")
}

func isLocalExtensionRef(ref string) bool {
	return strings.HasPrefix(ref, "/") ||
		strings.HasPrefix(ref, "./") ||
		strings.HasPrefix(ref, "../") ||
		filepath.IsAbs(filepath.FromSlash(ref))
}

func localExtensionCandidateDirs(wsRoot, ref string) []string {
	var candidates []string
	cleanRef := filepath.FromSlash(ref)

	if filepath.IsAbs(cleanRef) {
		candidates = append(candidates, cleanRef)
	}

	if strings.HasPrefix(ref, "/") {
		trimmed := strings.TrimLeft(ref, "/")
		if trimmed != "" {
			candidates = append(candidates, filepath.Join(wsRoot, filepath.FromSlash(trimmed)))
		}
	} else if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
		if cwd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(cwd, cleanRef))
		}
		candidates = append(candidates, filepath.Join(wsRoot, cleanRef))
	}

	return dedupePaths(candidates)
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	deduped := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		deduped = append(deduped, clean)
	}
	return deduped
}

func printLocalExtension(w io.Writer, ref string, ext *extension.ExtensionDescription) {
	if ext.Name != "" && ext.Name != ref {
		iox.Fprintf(w, "  ✓ %s (local: %s)\n", ref, ext.Name)
		return
	}
	iox.Fprintf(w, "  ✓ %s (local)\n", ref)
}

// RunExtensionInstallHooks runs workspace-scoped hooks declared by installed
// extensions. The args parameter mirrors ExtensionsInstall so a specifically
// installed extension can run its hook even before the config has been reloaded.
func RunExtensionInstallHooks(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	return runExtensionInstallHooks(ctx, wsRoot, cfg, args, outputFormat, os.Stdout)
}

// RunExtensionInstallHooksTo runs the hooks an install deferred
// (InstallOptions.DeferInstallHooks), with the human stream that install used.
func RunExtensionInstallHooksTo(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, out io.Writer) error {
	if err := runExtensionInstallHooks(ctx, wsRoot, cfg, args, "", out); err != nil {
		return fmt.Errorf("extension install hooks: %w", err)
	}
	return nil
}

// runExtensionInstallHooks is RunExtensionInstallHooks with the caller's human
// stream. A structured outputFormat still forces stderr: an onInstall hook's own
// stdout would corrupt the JSONL stream regardless of which writer the status
// lines use.
func runExtensionInstallHooks(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, out io.Writer) error {
	if wsRoot == "" {
		return nil
	}

	hookCfg := configWithExtensionInstallArgs(cfg, args)
	extensions, err := extension.DiscoverExtensions(wsRoot, hookCfg, nil)
	if err != nil {
		return fmt.Errorf("discover extensions: %w", err)
	}

	specificHookExt := extensionInstallHookTarget(wsRoot, args)
	hookExtensions := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, ext := range extensions {
		if specificHookExt != nil && !specificHookExt(ext) {
			continue
		}
		if ext != nil && ext.Hooks != nil && ext.Hooks.OnInstall != nil {
			hookExtensions = append(hookExtensions, ext)
		}
	}
	if len(hookExtensions) == 0 {
		return nil
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		ws = workspace.NewWorkspace(wsRoot, hookCfg, nil)
	}
	if err := jobs.SynchronizeExtensionRuntimes(ctx, ws, hookExtensions, nil); err != nil {
		return fmt.Errorf("synchronize extension runtimes: %w", err)
	}

	jsonlOut := outputFormat == "jsonl"
	// The hook's OWN stdout travels with the status lines: an onInstall hook that
	// prints to stdout during an implicit bootstrap install would corrupt the
	// invoking command's --output=json envelope just as surely as a status line.
	statusOut, hookStdout := out, out
	if statusOut == nil {
		statusOut, hookStdout = io.Discard, io.Discard
	}
	if jsonlOut {
		statusOut, hookStdout = os.Stderr, os.Stderr
	}

	iox.Fprintln(statusOut, "\n  Running extension install hooks...")
	for _, ext := range hookExtensions {
		iox.Fprintf(statusOut, "  → %s onInstall\n", ext.Name)
		if err := hooks.RunOnInstallHookWithWriters(ctx, ws, ext, false, hookStdout, os.Stderr); err != nil {
			return fmt.Errorf("%s onInstall: %w", ext.Name, err)
		}
		iox.Fprintf(statusOut, "  ✓ %s onInstall\n", ext.Name)
	}
	return nil
}

func configWithExtensionInstallArgs(cfg *wsproto.Config, args []string) *wsproto.Config {
	if cfg == nil {
		cfg = &wsproto.Config{}
	}
	next := *cfg
	next.Extensions.List = make(map[string]string, len(cfg.Extensions.List)+1)
	for name, constraint := range cfg.Extensions.List {
		next.Extensions.List[name] = constraint
	}

	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		name, version := parseExtensionArg(arg)
		if name == "" {
			continue
		}
		if version == "" {
			version = "latest"
		}
		next.Extensions.List[name] = version
		break
	}
	return &next
}

func extensionInstallHookTarget(wsRoot string, args []string) func(*extension.ExtensionDescription) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		name, _ := parseExtensionArg(arg)
		if name == "" {
			return nil
		}

		relPath, hasRelPath := normalizeInstallHookWorkspaceRef(name)
		candidates := localExtensionCandidateDirs(wsRoot, name)
		return func(ext *extension.ExtensionDescription) bool {
			if ext == nil {
				return false
			}
			if ext.Name == name || ext.RelPath == name {
				return true
			}
			if hasRelPath && ext.RelPath == relPath {
				return true
			}
			for _, candidate := range candidates {
				if filepath.Clean(ext.Path) == candidate {
					return true
				}
			}
			return false
		}
	}
	return nil
}

func normalizeInstallHookWorkspaceRef(ref string) (string, bool) {
	rel := strings.TrimLeft(ref, `/\`)
	if rel == "" {
		return "", false
	}
	rel = filepath.FromSlash(strings.ReplaceAll(rel, `\`, `/`))
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
