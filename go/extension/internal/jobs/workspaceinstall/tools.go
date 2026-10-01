package workspaceinstall

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/tools"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/scratch"
)

// devTools are the pinned tools the job installs, in installation order.
var devTools = []string{"golangci-lint", "staticcheck"}

// installTools makes the pinned development tools available and warms the
// sources of the tools a workspace packages. A tool that cannot be installed
// is a warning: lint skips its checks.
func (w *install) installTools(force bool) string {
	w.Emit.PhaseStart("tools")
	toolsOK := true

	if force {
		w.Emit.Log("info", "Force-reinstalling dev tools...")
		// Only the machine copy for the current key goes: another workspace on
		// another Go minor keeps its own.
		for _, tool := range devTools {
			spec, _ := tools.Lookup(tool)
			if binary := w.toolHomeBinary(tool, spec.Version); binary != "" {
				_ = os.Remove(binary)
			}
		}
	}

	for _, tool := range devTools {
		if binary, ok := w.resolveTool(tool); ok {
			w.Emit.Log("info", tool+" ready at "+binary)
		} else {
			w.Emit.Log("warn", tool+" installation failed (non-fatal)")
			toolsOK = false
		}
	}

	// The binaries above may all be prebuilt, which leaves the tool SOURCES out
	// of the module cache, and package~archives compiles those sources inside a
	// GOPROXY=off task graph. Only a workspace that contains the project owning
	// the pins packages them, so a consumer workspace skips this. Warming is a
	// download, which an offline run leaves to workspace-fetch.
	if manifest := w.workspaceToolManifest(); manifest != "" && w.mode == modeOffline {
		w.Emit.Log("info", "Module downloads are off on this run; workspace-fetch warmed the pinned tool sources")
	} else if manifest != "" {
		warmed := 0
		for _, spec := range pinnedInstallSpecs(manifest) {
			if spec == "" {
				continue
			}
			if !workspacejob.ValidModuleCoordinate(spec) {
				w.Emit.Diagnostic("error", "Refusing to warm the malformed tool pin '"+spec+"' from "+manifest, "", 0)
				w.Emit.PhaseEnd("tools", "failed")
				return statusFailed
			}
			if w.warmPinnedToolModule(spec) {
				warmed++
				continue
			}
			w.Emit.Diagnostic("warning",
				"Could not warm the sources of "+spec+"; packaging its archive will need the network", "", 0)
			toolsOK = false
		}
		if warmed > 0 {
			w.Emit.Log("info", "Warmed the sources of "+strconv.Itoa(warmed)+" pinned tool module(s) for offline packaging")
		}
		w.Emit.Metric("pinned-tool-modules-warmed", warmed, "count")
	}

	w.Emit.PhaseEnd("tools", "success")
	if !toolsOK {
		w.Emit.Diagnostic("warning", "Some tools could not be installed; lint may skip those checks", "", 0)
	}
	return statusOK
}

// resolveTool returns a binary of tool that reports the pinned version,
// trying the cheapest source first, in the order toolchain.InstallTool uses so
// `putnami install` warms exactly what `putnami lint` reads:
//
//  1. a PATH binary;
//  2. the machine tool home, then the read-only workspace locations;
//  3. the prebuilt binary the running extension artifact ships, copied into
//     the tool home;
//  4. `go install` into a throwaway GOPATH, then copied into the tool home.
func (w *install) resolveTool(tool string) (string, bool) {
	spec, ok := tools.Lookup(tool)
	if !ok || spec.Version == "" {
		w.Emit.Log("error", "Unknown tool: "+tool)
		return "", false
	}
	expected := spec.Version

	if system := w.LookPath(tool); system != "" {
		if w.toolVersionMatches(tool, system, expected) {
			return system, true
		}
		w.Emit.Log("info", "Ignoring "+tool+" at "+system+": expected "+expected)
	}

	dest := w.toolHomeBinary(tool, expected)
	if dest == "" {
		dest = filepath.Join(w.WorkspaceRoot, ".putnami", "bin", "extensions", "putnami-go", "bin", "tools", toolFileName(tool))
	}
	candidates := append([]string{dest}, toolchain.LegacyManagedToolPaths(tool, w.WorkspaceRoot)...)
	for _, candidate := range candidates {
		if candidate == "" || !workspacejob.IsExecutable(candidate) {
			continue
		}
		if w.toolVersionMatches(tool, candidate, expected) {
			return candidate, true
		}
		w.Emit.Log("info", "Ignoring managed "+tool+" at "+candidate+": expected "+expected)
	}

	if artifact := w.artifactBinary(tool); artifact != "" && w.artifactMatches(artifact, expected) {
		if err := publishToolBinary(artifact, dest); err == nil {
			w.Emit.Log("info", "Restored "+tool+" "+expected+" from the extension artifact")
			return dest, true
		}
		w.Emit.Log("warn", "Could not copy the "+tool+" artifact into "+dest+"; building from source")
	}

	if spec.Install == "" {
		w.Emit.Log("error", "No pinned install package for "+tool)
		return "", false
	}
	if w.mode == modeOffline {
		// Building a tool downloads its modules.
		w.Emit.Log("warn", tool+" "+expected+" is not installed, and module downloads are off on this run: "+
			"workspace-fetch installs it")
		return "", false
	}
	w.Emit.Log("info", "Installing "+tool+" "+expected+"...")
	if w.buildTool(tool, spec.Install, expected, dest) {
		return dest, true
	}
	w.Emit.Diagnostic("error", "Failed to install pinned "+tool+" "+expected, "", 0)
	return "", false
}

// buildTool compiles the pinned tool into a throwaway GOPATH and publishes it
// at dest. The module cache stays shared: SetupGoEnv exported GOMODCACHE.
func (w *install) buildTool(tool, installSpec, expected, dest string) bool {
	gopath, err := scratch.New("putnami-go-tool-")
	if err != nil {
		return false
	}
	defer func() { _ = gopath.Remove() }()

	env := []string{"GOPATH=" + gopath.Path()}
	if goRoot := w.Env.Get("GOROOT"); goRoot != "" {
		env = append(env,
			"GOROOT="+goRoot,
			"PATH="+filepath.Join(goRoot, "bin")+string(filepath.ListSeparator)+w.Env.Get("PATH"))
	}
	// The go command downloads the tool's modules, so on workspace-fetch it
	// gets the job credential. The version check below runs the built tool,
	// which must not: the credential file is gone by then.
	installed := w.withCredential(env, func(env []string) bool {
		return w.Inherit(false, env, w.GoBinary, "install", installSpec) == nil
	})
	if !installed {
		return false
	}
	binary := filepath.Join(gopath.Path(), "bin", toolFileName(tool))
	if !workspacejob.IsExecutable(binary) || !w.toolVersionMatches(tool, binary, expected) {
		return false
	}
	return publishToolBinary(binary, dest) == nil
}

// toolVersionMatches reports whether binary reports version, asking the way
// each tool answers: `golangci-lint version --short`, `<tool> -version`.
func (w *install) toolVersionMatches(tool, binary, version string) bool {
	args := []string{"-version"}
	if tool == "golangci-lint" {
		args = []string{"version", "--short"}
	}
	output, err := w.Combined(nil, binary, args...)
	if err != nil {
		return false
	}
	return VersionReported(output, version)
}

// VersionReported reports whether a line of output names version (with or
// without its "v") as a whole number sequence: 2.10.1 matches "v2.10.1" and
// "golangci-lint has version 2.10.1", not "2.10.12".
func VersionReported(output, version string) bool {
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return false
	}
	pattern := regexp.MustCompile(`(?m)(^|[^0-9])` + regexp.QuoteMeta(version) + `([^0-9]|$)`)
	return pattern.MatchString(output)
}

// toolHomeBinary is the machine path of one pinned tool, or "" when no machine
// root or no local Go minor resolves. The layout is toolchain.ToolHome's:
// <root>/<tool>/<version>/go<major.minor>/<goos>-<goarch>/<tool>.
func (w *install) toolHomeBinary(tool, version string) string {
	root := toolchain.ResolveGoToolHomeRoot(w.Env.Get)
	if root == "" || version == "" {
		return ""
	}
	minor := w.localGoMinor()
	if minor == "" {
		return ""
	}
	return filepath.Join(root, tool, version, "go"+minor, runtime.GOOS+"-"+runtime.GOARCH, toolFileName(tool))
}

// localGoMinor is the major.minor the resolved go command runs as under
// GOTOOLCHAIN=local, such as "1.26". A tool built with an older minor panics
// type-checking newer sources, so the local minor keys the tool home.
func (w *install) localGoMinor() string {
	return majorMinor(w.LocalGoVersion())
}

// majorMinor keeps the first two dot-separated fields of a version, as
// toolchain.ToolHome keys the directory lint reads.
func majorMinor(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// artifactBinary is the prebuilt tool the running extension ships, or "" when
// no extension root is exported.
func (w *install) artifactBinary(tool string) string {
	root := w.Env.Get("PUTNAMI_EXTENSION_ROOT")
	if root == "" {
		return ""
	}
	return filepath.Join(root, "compiled", "tools", toolFileName(tool))
}

// artifactMatches reports whether a prebuilt binary is the pinned build, from
// its own embedded build information: the main module version is the pin and
// the Go that built it serves the local one, the rule lint applies to the tool
// home (toolchain.ToolServesLocalGo). A release builds its tools with a newer
// Go than a workspace may pin, and that build serves the workspace.
func (w *install) artifactMatches(binary, version string) bool {
	if !workspacejob.FileExists(binary) {
		return false
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil || info.Main.Version != version {
		return false
	}
	return toolchain.ToolServesLocalGo(info.GoVersion, w.LocalGoVersion())
}

// publishToolBinary copies src over dest atomically: a full copy into a
// sibling temporary file, then toolchain.ReplaceExecutable, so a concurrent
// lint job holding no lock sees the previous binary or the new one, never a
// partial file, and a tool running on Windows does not keep the new one out.
func publishToolBinary(src, dest string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp." + strconv.Itoa(os.Getpid())
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return toolchain.ReplaceExecutable(tmp, dest)
}

// workspaceToolManifest returns the tools/versions.json of the first project
// the workspace builds that ships one, or "". A workspace names the extensions
// it builds as paths ("/go/extension") and those it consumes as package names,
// so the path entries are the candidates; the projects are read too.
func (w *install) workspaceToolManifest() string {
	data, err := os.ReadFile(workspacejob.WorkspaceConfigPath(w.WorkspaceRoot))
	if err != nil {
		return ""
	}
	for _, owner := range toolManifestOwners(data) {
		manifest := filepath.Join(w.WorkspaceRoot, owner, "tools", "versions.json")
		if workspacejob.FileExists(manifest) {
			return manifest
		}
	}
	return ""
}

// toolManifestOwners lists the path entries of "extensions" without their
// leading "/", then the string entries of "projects", as the script's jq
// program did. jq stops at the first value it cannot iterate, keeping what it
// printed before.
func toolManifestOwners(config []byte) []string {
	var doc struct {
		Extensions json.RawMessage `json:"extensions"`
		Projects   json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil
	}
	var owners []string
	extensions, ok := workspacejob.IterateOrEmpty(doc.Extensions)
	if !ok {
		return owners
	}
	for _, extension := range extensions {
		if path, isString := workspacejob.JSONString(extension); isString && strings.HasPrefix(path, "/") {
			owners = append(owners, strings.TrimPrefix(path, "/"))
		}
	}
	projects, ok := workspacejob.IterateOrEmpty(doc.Projects)
	if !ok {
		return owners
	}
	for _, project := range projects {
		if path, isString := workspacejob.JSONString(project); isString {
			owners = append(owners, path)
		}
	}
	return owners
}

// pinnedInstallSpecs returns the lines the script's
// `jq -r '.tools[]?.install // empty'` printed for a tool manifest, in
// document order: a missing, null or false install is skipped, and jq stops at
// the first tool entry that is not an object or null.
func pinnedInstallSpecs(manifest string) []string {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil
	}
	var doc struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	entries, ok := workspacejob.IterateOrEmpty(doc.Tools)
	if !ok {
		return nil
	}
	var specs []string
	for _, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if string(trimmed) == "null" {
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("{")) {
			return specs
		}
		var tool struct {
			Install json.RawMessage `json:"install"`
		}
		if err := json.Unmarshal(trimmed, &tool); err != nil {
			return specs
		}
		install := bytes.TrimSpace(tool.Install)
		if len(install) == 0 || string(install) == "null" || string(install) == "false" {
			continue
		}
		specs = append(specs, strings.Split(workspacejob.JQRawText(install), "\n")...)
	}
	return specs
}

// warmPinnedToolModule puts everything `go install <spec>` needs into the
// module cache without compiling: `-n` runs the module query, the deprecation
// query on <module>@latest and the build list download, and stops there.
//
// GOPATH is the machine's: Go keeps the authenticated checksum database
// checkpoint under GOPATH/pkg/sumdb, and the offline build needs it.
// GOFLAGS=-mod=mod because `go install pkg@version` refuses an inherited
// -mod=readonly or -mod=vendor.
//
// On workspace-fetch the go command gets the job credential for its run.
func (w *install) warmPinnedToolModule(spec string) bool {
	return w.withCredential([]string{"GOFLAGS=-mod=mod"}, func(env []string) bool {
		output, err := w.StderrOf(env, w.GoBinary, "install", "-n", "-trimpath", spec)
		if err != nil {
			w.Emit.Log("warn", "Could not warm the module of "+spec+": "+output)
			return false
		}
		return true
	})
}

func toolFileName(tool string) string {
	return pkgmeta.ExecutableName(runtime.GOOS, tool)
}
