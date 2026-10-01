package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"

	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// readJobCredential reads the read credential the engine hands this job.
// Tests replace it.
var readJobCredential = registrycred.ReadJobCredential

// committedLockFile is the lock workspace-fetch downloads for and a hosted
// install reads unchanged.
const committedLockFile = "bun.lock"

// errNoCommittedLock is the failure of a fetch in a workspace without a lock.
var errNoCommittedLock = errors.New("a hosted run installs from a committed bun.lock; run `putnami install` locally and commit it")

// fetchInstallArgs is the scratch install: it downloads every package the lock
// names and runs no lifecycle script, the project's own included.
var fetchInstallArgs = []string{"install", "--ignore-scripts", "--frozen-lockfile"}

// npmrcBearer is the RFC 6750 b64token grammar. A bearer in it is written to an
// .npmrc value verbatim: none of its characters starts a comment, an escape,
// a quoted value or a variable reference.
var npmrcBearer = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// runWorkspaceFetch downloads every package the committed bun.lock names into
// the bun cache workspace-install reads, and runs none of their code.
//
// It reads the job credential before it starts any process, so no process
// inherits the descriptor. It reads the committed manifests as they are, as
// the hosted workspace-install does, and runs `bun install --ignore-scripts
// --frozen-lockfile` in a private scratch copy of the files a frozen install
// reads: no lifecycle script runs, the lock is never rewritten, and the
// workspace gets no node_modules. A handed bearer is written only to the .npmrc
// of a private home that this one bun reads; the home and the scratch copy are
// removed when bun exits, on a failure and on an interrupt or termination
// signal too. Without a handed credential bun reads the machine's native
// credentials and the workspace .npmrc as it is.
func runWorkspaceFetch(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	credential, err := readJobCredential()
	if err != nil {
		return "FAILED", nil, err
	}
	// A hosted run downloads no toolchain: the job runs a bun the runner
	// holds. A bun the repository commits into its own tree is repository
	// code, and this job holds the read credential: it is never started, not
	// even to read its version.
	bunBin, err := provisionBunBin(ctx, emit, toolchain.BunFind, func(path string) string {
		if within(ctx.WorkspaceRoot, path) {
			return "it is inside the workspace"
		}
		return ""
	})
	if err != nil {
		return "FAILED", nil, err
	}
	if within(ctx.WorkspaceRoot, bunBin) {
		return "FAILED", nil, fmt.Errorf("workspace-fetch does not run %s: it is inside the workspace; "+
			"put a bun outside the workspace first on PATH, or pin bun in the workspace lock so the engine provides it", bunBin)
	}
	if err := requireCommittedLock(ctx.WorkspaceRoot); err != nil {
		return "FAILED", nil, err
	}
	// A descriptor that held no credential still marks a job of a hosted run:
	// the refresh then starts no process (registrycred.EnsureNativeCredential).
	if credential == nil {
		refreshPutnamiNpmCredential(ctx, emit)
	}

	stopped, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	emit.PhaseStart("fetch")
	if err := fetchWorkspaceDependencies(stopped, ctx.WorkspaceRoot, bunBin, credential, emit); err != nil {
		emit.PhaseEnd("fetch", "failed")
		return "FAILED", nil, err
	}
	emit.PhaseEnd("fetch", "success")
	return "OK", nil, nil
}

// requireCommittedLock fails unless the workspace root holds a bun.lock file.
func requireCommittedLock(root string) error {
	info, err := os.Stat(filepath.Join(root, committedLockFile))
	if err != nil {
		if os.IsNotExist(err) {
			return errNoCommittedLock
		}
		return fmt.Errorf("stat %s: %w", committedLockFile, err)
	}
	if !info.Mode().IsRegular() {
		return errNoCommittedLock
	}
	return nil
}

// fetchWorkspaceDependencies runs the scratch install and removes the scratch
// copy and the private home before it returns.
func fetchWorkspaceDependencies(parent context.Context, root, bunBin string, credential *registry.Credential, emit *jsonl.Emitter) error {
	authLines, err := credentialNpmrc(credential)
	if err != nil {
		return err
	}
	inputs, err := fetchInputs(root)
	if err != nil {
		return err
	}
	cacheDir, err := bunCacheDir(parent, bunBin, root)
	if err != nil {
		return err
	}

	scratch, err := os.MkdirTemp("", "putnami-ts-fetch-")
	if err != nil {
		return fmt.Errorf("create the fetch copy: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := copyFetchInputs(root, scratch, inputs, credential != nil); err != nil {
		return err
	}

	env := map[string]string{"BUN_INSTALL_CACHE_DIR": cacheDir}
	if credential != nil {
		home, err := os.MkdirTemp("", "putnami-ts-fetch-home-")
		if err != nil {
			return fmt.Errorf("create the fetch home: %w", err)
		}
		defer func() { _ = os.RemoveAll(home) }()
		if err := writePrivateFile(filepath.Join(home, ".npmrc"), authLines); err != nil {
			return fmt.Errorf("write the fetch credential: %w", err)
		}
		for name, value := range privateHomeEnv(home) {
			env[name] = value
		}
	}

	result, err := runBunWithin(parent, "bun install", bunBin, fetchInstallArgs, scratch, bunNetworkTimeout, exec.Env(env))
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("bun %s failed on the committed %s: %s",
			strings.Join(fetchInstallArgs, " "), committedLockFile, redactBearer(strings.TrimSpace(result.Stderr), credential))
	}
	emit.Log("info", "fetched the packages "+committedLockFile+" names into "+cacheDir)
	return nil
}

// privateHomeEnv points every variable bun reads a user .npmrc or bunfig from
// at home.
func privateHomeEnv(home string) map[string]string {
	env := map[string]string{"HOME": home, "XDG_CONFIG_HOME": home}
	if runtime.GOOS == "windows" {
		env["USERPROFILE"] = home
	}
	return env
}

// credentialNpmrc renders one `//<host>/:_authToken=<bearer>` line per host of
// the credential, and nothing for no credential. A credential that fails the
// protocol's validation, or whose bearer an .npmrc value cannot carry
// verbatim, is refused without echoing the bearer.
func credentialNpmrc(credential *registry.Credential) ([]byte, error) {
	if credential == nil {
		return nil, nil
	}
	if err := registry.ValidateCredential(*credential); err != nil {
		return nil, fmt.Errorf("the job credential is invalid: %w", err)
	}
	if !npmrcBearer.MatchString(credential.Bearer) {
		return nil, errors.New("the job credential's bearer holds a character an .npmrc value cannot carry verbatim")
	}
	var lines strings.Builder
	for _, host := range credential.Hosts {
		lines.WriteString("//" + host + "/:_authToken=" + credential.Bearer + "\n")
	}
	return []byte(lines.String()), nil
}

// redactBearer removes the bearer from text a job reports.
func redactBearer(text string, credential *registry.Credential) string {
	if credential == nil || credential.Bearer == "" {
		return text
	}
	return strings.ReplaceAll(text, credential.Bearer, "<redacted>")
}

// bunCacheDir asks bun, in the workspace root and with this job's environment,
// which cache directory an install there uses: BUN_INSTALL_CACHE_DIR, the
// bunfig.toml cache setting, BUN_INSTALL, XDG_CACHE_HOME or the home, in bun's
// own order. The scratch install writes to that directory whatever home it
// runs with.
func bunCacheDir(parent context.Context, bunBin, root string) (string, error) {
	result, err := runBunWithin(parent, "bun pm cache", bunBin, []string{"pm", "cache"}, root, bunVersionTimeout)
	if err != nil {
		return "", err
	}
	if !result.Success {
		return "", fmt.Errorf("bun pm cache exited with code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	dir := strings.TrimSpace(result.Stdout)
	if dir == "" || strings.ContainsAny(dir, "\r\n") || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("bun pm cache answered %q, not an absolute directory", dir)
	}
	return dir, nil
}

// fetchInputs lists, relative to the workspace root in slash form, every file
// a frozen `bun install` reads besides the .npmrc: the root package.json, the
// lock, bunfig.toml, each member's package.json, each patch file the root
// names in patchedDependencies, and each local `file:` dependency — the
// package.json of a directory, which is all bun reads to resolve it, or the
// tarball itself. Members are the root `workspaces` entries together with the
// members the lock records. A `link:` dependency, which resolves through
// `bun link` on one machine, and a local dependency outside the workspace are
// refused: a hosted run holds neither.
func fetchInputs(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "package.json")); err != nil {
		return nil, fmt.Errorf("the workspace root has no readable package.json: %w", err)
	}
	inputs := map[string]bool{"package.json": true, committedLockFile: true}
	if isRegularFile(filepath.Join(root, "bunfig.toml")) {
		inputs["bunfig.toml"] = true
	}

	// Workspace members install their devDependencies too; a local
	// dependency outside the members does not.
	type manifest struct {
		rel string
		dev bool
	}
	manifests := []manifest{{rel: "package.json", dev: true}}
	for _, member := range fetchMembers(root) {
		rel := path.Join(member, "package.json")
		if inputs[rel] || !isRegularFile(filepath.Join(root, filepath.FromSlash(rel))) {
			continue
		}
		inputs[rel] = true
		manifests = append(manifests, manifest{rel: rel, dev: true})
	}

	patches, err := patchFiles(root)
	if err != nil {
		return nil, err
	}
	for _, rel := range patches {
		inputs[rel] = true
	}

	for i := 0; i < len(manifests); i++ {
		deps, err := localDependencies(root, manifests[i].rel, manifests[i].dev)
		if err != nil {
			return nil, err
		}
		for _, rel := range deps {
			if inputs[rel] {
				continue
			}
			inputs[rel] = true
			if path.Base(rel) == "package.json" {
				manifests = append(manifests, manifest{rel: rel})
			}
		}
	}

	out := make([]string, 0, len(inputs))
	for rel := range inputs {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// fetchMembers returns the member directories, relative to root in slash
// form: the matches of the root `workspaces` patterns and the members the lock
// records, inside root only.
func fetchMembers(root string) []string {
	seen := map[string]bool{}
	var members []string
	add := func(rel string) {
		rel, ok := insideRoot(rel)
		if !ok || rel == "." || seen[rel] {
			return
		}
		seen[rel] = true
		members = append(members, rel)
	}
	for _, dir := range workspaceMemberDirs(root) {
		if rel, err := filepath.Rel(root, dir); err == nil {
			add(filepath.ToSlash(rel))
		}
	}
	for _, rel := range lockWorkspaceMembers(root) {
		add(rel)
	}
	sort.Strings(members)
	return members
}

// lockWorkspaceMembers returns the member paths the text lock records under
// `workspaces`, the root's "" excluded. A lock bun cannot read either yields
// nothing here and fails the install that reads it.
func lockWorkspaceMembers(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, committedLockFile))
	if err != nil {
		return nil
	}
	var lock struct {
		Workspaces map[string]json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(project.StripJSONC(data), &lock) != nil {
		return nil
	}
	members := make([]string, 0, len(lock.Workspaces))
	for rel := range lock.Workspaces {
		if rel != "" {
			members = append(members, rel)
		}
	}
	sort.Strings(members)
	return members
}

// patchFiles returns the patch files the root package.json names in
// patchedDependencies that exist. A patch path outside the workspace is
// refused.
func patchFiles(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	var pkg struct {
		Patched map[string]string `json:"patchedDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	var patches []string
	for dependency, file := range pkg.Patched {
		rel, ok := insideRoot(file)
		if !ok {
			return nil, fmt.Errorf("package.json patches %s with %q, outside the workspace: a hosted run fetches only what the workspace holds", dependency, file)
		}
		if isRegularFile(filepath.Join(root, filepath.FromSlash(rel))) {
			patches = append(patches, rel)
		}
	}
	sort.Strings(patches)
	return patches, nil
}

// localDependencies returns the files bun reads for the local dependencies one
// manifest declares, relative to root in slash form: a directory's
// package.json or a tarball. withDev includes devDependencies, which bun
// installs for the root and the members only.
func localDependencies(root, manifestRel string, withDev bool) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifestRel)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestRel, err)
	}
	var pkg struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestRel, err)
	}
	sets := []map[string]string{pkg.Dependencies, pkg.OptionalDependencies, pkg.PeerDependencies}
	if withDev {
		sets = append(sets, pkg.DevDependencies)
	}
	manifestDir := path.Dir(manifestRel)
	var files []string
	for _, set := range sets {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			target, link, local := localDependencyTarget(set[name])
			if link {
				return nil, fmt.Errorf("%s depends on %s through %q: a link: dependency resolves through `bun link` on one machine, so a hosted run cannot fetch it", manifestRel, name, set[name])
			}
			if !local {
				continue
			}
			rel, ok := "", false
			if !filepath.IsAbs(target) && !strings.HasPrefix(target, "~") {
				rel, ok = insideRoot(path.Join(manifestDir, filepath.ToSlash(target)))
			}
			if !ok {
				return nil, fmt.Errorf("%s depends on %s at %q, outside the workspace: a hosted run fetches only what the workspace holds", manifestRel, name, set[name])
			}
			abs := filepath.Join(root, filepath.FromSlash(rel))
			info, err := os.Stat(abs)
			switch {
			case err != nil:
				// bun reports the missing path when it resolves the lock.
				continue
			case info.IsDir():
				if isRegularFile(filepath.Join(abs, "package.json")) {
					files = append(files, path.Join(rel, "package.json"))
				}
			case info.Mode().IsRegular():
				files = append(files, rel)
			}
		}
	}
	return files, nil
}

// localDependencyTarget reads a dependency spec the way bun does: `link:` is a
// bun link, and `file:` or a spec starting with `./`, `../`, `/` or `~/` names
// a directory or a tarball on disk.
func localDependencyTarget(spec string) (target string, link, local bool) {
	spec = strings.TrimSpace(spec)
	switch {
	case strings.HasPrefix(spec, "link:"):
		return "", true, false
	case strings.HasPrefix(spec, "file:"):
		return strings.TrimPrefix(strings.TrimPrefix(spec, "file:"), "//"), false, true
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"),
		strings.HasPrefix(spec, "/"), strings.HasPrefix(spec, "~/"):
		return spec, false, true
	}
	return "", false, false
}

// insideRoot cleans a slash-form path relative to the workspace root and
// reports whether it stays inside the root.
func insideRoot(rel string) (string, bool) {
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// copyFetchInputs copies each input from root into scratch at the same
// relative path, and the root .npmrc. With handed set it drops the .npmrc
// credential lines: a workspace line for a host outranks the private home's,
// so it would replace the handed bearer. Without a handed credential the
// .npmrc stays verbatim, and bun reads it as the install in the workspace does.
func copyFetchInputs(root, scratch string, inputs []string, handed bool) error {
	for _, rel := range inputs {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if err := writePrivateFile(filepath.Join(scratch, filepath.FromSlash(rel)), data); err != nil {
			return fmt.Errorf("copy %s into the fetch copy: %w", rel, err)
		}
	}
	npmrc, err := os.ReadFile(filepath.Join(root, ".npmrc"))
	switch {
	case err == nil:
		if handed {
			npmrc = stripNpmrcCredentials(npmrc)
		}
		if err := writePrivateFile(filepath.Join(scratch, ".npmrc"), npmrc); err != nil {
			return fmt.Errorf("copy .npmrc into the fetch copy: %w", err)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("read .npmrc: %w", err)
	}
	return nil
}

// stripNpmrcCredentials drops every line whose key ends in _authToken, _auth
// or _password, in any case, and keeps every other line verbatim.
func stripNpmrcCredentials(content []byte) []byte {
	lines := strings.SplitAfter(string(content), "\n")
	var out strings.Builder
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasSuffix(key, "_authtoken") || strings.HasSuffix(key, "_auth") || strings.HasSuffix(key, "_password") {
			continue
		}
		out.WriteString(line)
	}
	return []byte(out.String())
}

// writePrivateFile creates path, and its missing parents, readable by this
// user only. It never overwrites a file.
func writePrivateFile(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// isRegularFile reports whether path names a regular file.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// within reports whether path is root or lies below it once both are resolved
// through their symbolic links. A root that cannot be resolved contains every
// path, so the check fails closed.
func within(root, path string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(resolvedRoot, resolveExisting(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting resolves path through the symbolic links of its nearest
// existing ancestor, and keeps the part below that ancestor as written.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}
