package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"

	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/typescript/extension/internal/pnpmworkspace"
	"go.putnami.dev/typescript/extension/internal/project"
)

// readJobCredential reads the read credential the engine hands this job.
// Tests replace it.
var readJobCredential = registrycred.ReadJobCredential

// committedLockFile is the lock workspace-fetch downloads for and a hosted
// install reads unchanged in a Bun workspace: the text lock, whose members the
// fetch reads.
const committedLockFile = "bun.lock"

// fetchLockfiles are the lockfiles a hosted run of m installs from, in the
// order workspace-fetch looks for them.
func fetchLockfiles(m packageManager) []string {
	if m.Name == managerBun {
		return []string{committedLockFile}
	}
	return m.lockfiles()
}

// noCommittedLock is the failure of a fetch in a workspace without the lock
// its package manager installs from.
func noCommittedLock(m packageManager) error {
	return fmt.Errorf("a hosted run installs from a committed %s; run `putnami install` locally and commit it",
		strings.Join(fetchLockfiles(m), " or "))
}

// fetchInstallArgs is the scratch install of a Bun workspace: it downloads
// every package the lock names and runs no lifecycle script, the project's own
// included.
var fetchInstallArgs = []string{"install", "--ignore-scripts", "--frozen-lockfile"}

// managerFetchArgs is the scratch install of m: frozen to the committed lock,
// with no lifecycle script and no Yarn build step; fetchProcessEnv keeps the
// pnpmfile and a yarnPath out. The cache the
// install writes to is added by managerCache.
func managerFetchArgs(m packageManager) installInvocation {
	switch m.Name {
	case managerNPM:
		return installInvocation{args: []string{"ci", "--ignore-scripts", "--no-audit", "--no-fund"}}
	case managerPNPM:
		// pnpm fetch downloads what the lock names and reads no manifest, so it
		// does not compare the pnpmfile checksum the lock records, which a
		// frozen install that ignores the pnpmfile refuses.
		return installInvocation{args: []string{"fetch", "--ignore-scripts"}}
	case managerYarn:
		if m.Berry && yarnMajor(m) == 2 {
			return installInvocation{args: []string{"install", "--immutable", "--skip-builds"}, env: map[string]string{"YARN_ENABLE_SCRIPTS": "0"}}
		}
		if m.Berry {
			return installInvocation{args: []string{"install", "--immutable", "--mode=skip-build"}, env: map[string]string{"YARN_ENABLE_SCRIPTS": "0"}}
		}
		return installInvocation{args: []string{"install", "--frozen-lockfile", "--ignore-scripts", "--non-interactive"}}
	}
	return installInvocation{args: fetchInstallArgs}
}

// npmrcBearer is the RFC 6750 b64token grammar. A bearer in it is written to an
// .npmrc value verbatim: none of its characters starts a comment, an escape,
// a quoted value or a variable reference.
var npmrcBearer = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// runWorkspaceFetch downloads every package the committed lock of the
// workspace's package manager names into the cache workspace-install reads,
// and runs none of their code.
//
// It reads the job credential before it starts any process, so no process
// inherits the descriptor. It reads the committed manifests as they are, as
// the hosted workspace-install does, and runs a frozen install with no
// lifecycle script in a private scratch copy of the files that install reads:
// `bun install --ignore-scripts --frozen-lockfile`, `npm ci --ignore-scripts`,
// `pnpm fetch --ignore-scripts` with the pnpmfile ignored, `yarn install
// --frozen-lockfile --ignore-scripts` on Yarn 1, and `yarn install --immutable`
// with builds and scripts off on Yarn 2 and later. The
// lock is never rewritten, and the workspace gets no node_modules. A handed
// bearer is written only to the .npmrc, or for Yarn 2 and later the
// .yarnrc.yml, of a private home that this one install reads; the home and the
// scratch copy are removed when the install exits, on a failure and on an
// interrupt or termination signal too. Without a handed credential the
// manager reads the machine's native credentials and the workspace files as
// they are.
//
// It runs no code the repository holds: a manager inside the workspace, a
// Yarn release a yarnPath names and a Yarn plugin are refused before any
// process starts.
func runWorkspaceFetch(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	credential, err := readJobCredential()
	if err != nil {
		return "FAILED", nil, err
	}
	manager, err := detectPackageManager(ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}
	// The job installs no package manager: it runs the bun the CLI resolved,
	// or the npm, pnpm or yarn on PATH, outside the workspace. A manager the
	// repository commits into its own tree is repository code, and this job
	// holds the read credential: it is never started, not even to read its
	// version.
	managerBin, err := resolveManagerBin(manager)
	if err != nil {
		return "FAILED", nil, err
	}
	if within(ctx.WorkspaceRoot, managerBin) {
		remedy := "put a " + manager.Name + " outside the workspace first on PATH"
		if manager.Name == managerBun {
			remedy += ", or pin bun in the workspace lock so the engine provides it"
		}
		return "FAILED", nil, fmt.Errorf("workspace-fetch does not run %s: it is inside the workspace; %s", managerBin, remedy)
	}
	if err := refuseRepositoryManagerCode(ctx.WorkspaceRoot, manager); err != nil {
		return "FAILED", nil, err
	}
	lock, err := requireCommittedLock(ctx.WorkspaceRoot, manager)
	if err != nil {
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
	if err := fetchWorkspaceDependencies(stopped, ctx.WorkspaceRoot, manager, managerBin, lock, credential, emit); err != nil {
		emit.PhaseEnd("fetch", "failed")
		reportRegistryRefusal(emit, err.Error())
		return "FAILED", nil, err
	}
	emit.PhaseEnd("fetch", "success")
	return "OK", nil, nil
}

// requireCommittedLock returns the lock a hosted run of m installs from, the
// first of fetchLockfiles that is a regular file at root, and fails when there
// is none.
func requireCommittedLock(root string, m packageManager) (string, error) {
	for _, name := range fetchLockfiles(m) {
		info, err := os.Stat(filepath.Join(root, name))
		switch {
		case err == nil && info.Mode().IsRegular():
			return name, nil
		case err != nil && !os.IsNotExist(err):
			return "", fmt.Errorf("stat %s: %w", name, err)
		}
	}
	return "", noCommittedLock(m)
}

// yarnClassicConfig is the configuration file of Yarn 1.
const yarnClassicConfig = ".yarnrc"

// repositoryCodeSettings are, by manager, the configuration files that can
// make the manager run code the repository holds, and the words whose mention
// in them, in any letter case, refuses the fetch: a Yarn release a yarnPath or
// yarn-path names, which both Yarns run in place of themselves, a Yarn plugin,
// a pnpm configuration dependency, which pnpm fetches and loads before any
// flag applies, and a pnpmfile. backslash refuses an escape, by which a YAML
// key spells one of the words without writing it. ancestors also reads the
// files of each directory above the root, which Yarn reads too.
var repositoryCodeSettings = map[string]struct {
	files     []string
	words     []string
	backslash []string
	ancestors bool
}{
	managerYarn: {
		files:     []string{yarnBerryConfig, yarnClassicConfig},
		words:     []string{"yarnpath", "yarn-path", "plugins"},
		backslash: []string{yarnBerryConfig, yarnClassicConfig},
		ancestors: true,
	},
	managerPNPM: {
		files:     []string{pnpmworkspace.File, ".npmrc"},
		words:     []string{"configdependencies", "config-dependencies", "pnpmfile"},
		backslash: []string{pnpmworkspace.File, ".npmrc"},
	},
}

// refuseRepositoryManagerCode fails, before any process starts, when a
// configuration file of the workspace's manager mentions a setting that runs
// code the repository holds (repositoryCodeSettings). It reads text, not YAML,
// so a quoted, escaped, indented or flow-style key cannot slip past it; a
// comment that names the setting is refused too. Every process the fetch
// starts also runs with the switches that ignore a yarnPath and a pnpmfile
// (fetchProcessEnv).
func refuseRepositoryManagerCode(root string, m packageManager) error {
	settings, ok := repositoryCodeSettings[m.Name]
	if !ok {
		return nil
	}
	dirs := []string{root}
	if settings.ancestors {
		home, _ := os.UserHomeDir()
		for dir := filepath.Dir(root); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if home == "" || filepath.Clean(dir) != filepath.Clean(home) {
				dirs = append(dirs, dir)
			}
		}
	}
	for _, dir := range dirs {
		for _, name := range settings.files {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) || (err == nil && len(data) == 0) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			text := strings.ToLower(string(data))
			for _, word := range settings.words {
				if strings.Contains(text, word) {
					return fmt.Errorf("workspace-fetch does not run %s: %s mentions %q, a setting that runs code the repository holds next to the registry credential; "+
						"remove it (declare the Yarn release in package.json#packageManager, which Corepack provides)", m.Name, path, word)
				}
			}
			if slices.Contains(settings.backslash, name) && strings.Contains(text, "\\") {
				return fmt.Errorf("workspace-fetch does not run %s: %s holds a backslash, which can spell a setting that runs code the repository holds", m.Name, path)
			}
		}
	}
	return nil
}

// fetchProcessEnv is the environment every process workspace-fetch starts
// adds, the cache probes in the workspace root included: a Corepack shim reads
// no .corepack.env, which could name a registry or allow a manager from a URL;
// a Yarn ignores the release yarnPath names; a pnpm ignores the pnpmfile.
func fetchProcessEnv(m packageManager) map[string]string {
	switch m.Name {
	case managerYarn:
		return map[string]string{"COREPACK_ENV_FILE": "0", "YARN_IGNORE_PATH": "1"}
	case managerPNPM:
		return map[string]string{"COREPACK_ENV_FILE": "0", "npm_config_ignore_pnpmfile": "true"}
	case managerNPM:
		return map[string]string{"COREPACK_ENV_FILE": "0"}
	}
	return map[string]string{}
}

// fetchUnsetEnv are the inherited variables no process workspace-fetch starts
// sees: Corepack runs only a published release of the manager the workspace
// declares, never one from a URL.
var fetchUnsetEnv = []string{"COREPACK_ENABLE_UNSAFE_CUSTOM_URLS"}

// fetchWorkspaceDependencies runs the scratch install of m, from lock, into
// the cache the hosted install reads, and removes the scratch copy and the
// private home before it returns.
func fetchWorkspaceDependencies(parent context.Context, root string, m packageManager, managerBin, lock string, credential *registry.Credential, emit *jsonl.Emitter) error {
	authLines, err := credentialNpmrc(credential)
	if err != nil {
		return err
	}
	inputs, err := fetchInputs(root, m, lock)
	if err != nil {
		return err
	}
	cache, err := managerCache(parent, m, managerBin, root)
	if err != nil {
		return err
	}
	// The Corepack shim a non-Bun manager may run through keeps the release
	// it runs in the user's cache; the private home must not hide it.
	corepack := ""
	if m.Name != managerBun {
		corepack = corepackHome()
	}

	scratch, err := os.MkdirTemp("", "putnami-ts-fetch-")
	if err != nil {
		return fmt.Errorf("create the fetch copy: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := copyFetchInputs(root, scratch, inputs, credential != nil); err != nil {
		return err
	}

	install := managerFetchArgs(m)
	args := append(append([]string(nil), install.args...), cache.args...)
	env := fetchProcessEnv(m)
	for name, value := range install.env {
		env[name] = value
	}
	for name, value := range cache.env {
		env[name] = value
	}
	if credential != nil {
		home, err := os.MkdirTemp("", "putnami-ts-fetch-home-")
		if err != nil {
			return fmt.Errorf("create the fetch home: %w", err)
		}
		defer func() { _ = os.RemoveAll(home) }()
		if err := writePrivateFile(privateNpmrc(home), authLines); err != nil {
			return fmt.Errorf("write the fetch credential: %w", err)
		}
		if m.Name == managerYarn && m.Berry {
			if err := writePrivateFile(filepath.Join(home, yarnBerryConfig), credentialYarnrc(credential)); err != nil {
				return fmt.Errorf("write the fetch credential: %w", err)
			}
		}
		for name, value := range privateHomeEnv(home) {
			env[name] = value
		}
		if corepack != "" {
			env["COREPACK_HOME"] = corepack
		}
	}

	label := m.Name + " " + strings.Join(install.args, " ")
	result, err := runBunWithin(parent, m.Name+" install", managerBin, args, scratch, bunNetworkTimeout, exec.Env(env), exec.UnsetEnv(fetchUnsetEnv...))
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("%s failed on the committed %s: %s",
			label, lock, redactBearer(strings.TrimSpace(result.Stderr+"\n"+result.Stdout), credential))
	}
	emit.Log("info", "fetched the packages "+lock+" names into "+cache.dir)
	return nil
}

// fetchCache is where the scratch install of a manager writes the packages it
// downloads: the directory, and the arguments and environment that point the
// install at it.
type fetchCache struct {
	dir  string
	args []string
	env  map[string]string
}

// managerCache asks the manager, in the workspace root and with this job's
// environment, which cache the hosted install there reads, and points the
// scratch install at it, whatever home that install runs with: bun pm cache,
// pnpm store path, npm config get cache, yarn cache dir on Yarn 1, and yarn
// config get cacheFolder, globalFolder and enableGlobalCache on Yarn 2 and
// later.
func managerCache(parent context.Context, m packageManager, managerBin, root string) (fetchCache, error) {
	switch m.Name {
	case managerBun:
		dir, err := bunCacheDir(parent, managerBin, root)
		if err != nil {
			return fetchCache{}, err
		}
		return fetchCache{dir: dir, env: map[string]string{"BUN_INSTALL_CACHE_DIR": dir}}, nil
	case managerPNPM:
		dir, err := managerAnswer(parent, m, managerBin, root, true, "store", "path")
		if err != nil {
			return fetchCache{}, err
		}
		return fetchCache{dir: dir, args: []string{"--store-dir", dir}}, nil
	case managerNPM:
		dir, err := managerAnswer(parent, m, managerBin, root, true, "config", "get", "cache")
		if err != nil {
			return fetchCache{}, err
		}
		return fetchCache{dir: dir, args: []string{"--cache", dir}}, nil
	case managerYarn:
		if !m.Berry {
			// Yarn 1 answers the versioned folder it keeps under the cache
			// folder (.../yarn/v6); --cache-folder names the folder above it.
			dir, err := managerAnswer(parent, m, managerBin, root, true, "cache", "dir")
			if err != nil {
				return fetchCache{}, err
			}
			folder := dir
			if yarnCacheVersion.MatchString(filepath.Base(dir)) {
				folder = filepath.Dir(dir)
			}
			return fetchCache{dir: dir, args: []string{"--cache-folder", folder}}, nil
		}
		dir, err := managerAnswer(parent, m, managerBin, root, true, "config", "get", "cacheFolder")
		if err != nil {
			return fetchCache{}, err
		}
		global, err := managerAnswer(parent, m, managerBin, root, true, "config", "get", "globalFolder")
		if err != nil {
			return fetchCache{}, err
		}
		enabled, err := managerAnswer(parent, m, managerBin, root, false, "config", "get", "enableGlobalCache")
		if err != nil {
			return fetchCache{}, err
		}
		if enabled == "true" {
			dir = filepath.Join(global, "cache")
		}
		return fetchCache{dir: dir, env: map[string]string{
			"YARN_CACHE_FOLDER":        dir,
			"YARN_GLOBAL_FOLDER":       global,
			"YARN_ENABLE_GLOBAL_CACHE": enabled,
		}}, nil
	}
	return fetchCache{}, fmt.Errorf("workspace-fetch knows no cache of %s", m.Name)
}

// managerAnswer runs the manager with args in root, with the fetch's process
// environment, and returns the one line it prints. With directory set, the
// answer must be an absolute path.
func managerAnswer(parent context.Context, m packageManager, managerBin, root string, directory bool, args ...string) (string, error) {
	label := filepath.Base(managerBin) + " " + strings.Join(args, " ")
	result, err := runBunWithin(parent, label, managerBin, args, root, bunVersionTimeout, exec.Env(fetchProcessEnv(m)), exec.UnsetEnv(fetchUnsetEnv...))
	if err != nil {
		return "", err
	}
	if !result.Success {
		return "", fmt.Errorf("%s exited with code %d: %s", label, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	answer := strings.TrimSpace(terminalEscape.ReplaceAllString(result.Stdout, ""))
	if directory && (answer == "" || strings.ContainsAny(answer, "\r\n") || !filepath.IsAbs(answer)) {
		return "", fmt.Errorf("%s answered %q, not an absolute directory", label, answer)
	}
	if answer == "" || strings.ContainsAny(answer, "\r\n") {
		return "", fmt.Errorf("%s answered %q, not one value", label, answer)
	}
	return answer, nil
}

// yarnCacheVersion is the name of the versioned folder Yarn 1 keeps its
// packages in under its cache folder.
var yarnCacheVersion = regexp.MustCompile(`^v[0-9]+$`)

// terminalEscape matches the terminal control sequences Yarn 1 writes around
// its answer, such as the line clear of its progress display.
var terminalEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// corepackHome is the directory Corepack keeps the package manager releases
// it runs in: COREPACK_HOME, else node/corepack under XDG_CACHE_HOME, under
// LOCALAPPDATA, or under the home's .cache (AppData/Local on Windows), in
// Corepack's own order. "" when no home is known.
func corepackHome() string {
	if dir := os.Getenv("COREPACK_HOME"); dir != "" {
		return dir
	}
	for _, name := range []string{"XDG_CACHE_HOME", "LOCALAPPDATA"} {
		if dir := os.Getenv(name); dir != "" {
			return filepath.Join(dir, "node", "corepack")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Local", "node", "corepack")
	}
	return filepath.Join(home, ".cache", "node", "corepack")
}

// credentialYarnrc renders the user .yarnrc.yml of Yarn 2 and later that
// carries the credential: one npmRegistries entry per host. Yarn 2 and later
// read no .npmrc. credentialNpmrc has validated the credential and its bearer,
// whose characters a double-quoted YAML scalar carries verbatim.
func credentialYarnrc(credential *registry.Credential) []byte {
	var lines strings.Builder
	lines.WriteString("npmRegistries:\n")
	for _, host := range credential.Hosts {
		lines.WriteString("  \"//" + host + "\":\n")
		lines.WriteString("    npmAuthToken: \"" + credential.Bearer + "\"\n")
	}
	return []byte(lines.String())
}

// privateNpmrc is the user .npmrc of the private home.
func privateNpmrc(home string) string {
	return filepath.Join(home, ".npmrc")
}

// privateHomeEnv points the home and config-home variables at home, and the
// npm user-config variables, which name the user .npmrc bun reads in place of
// the home's, at home's .npmrc.
func privateHomeEnv(home string) map[string]string {
	npmrc := privateNpmrc(home)
	env := map[string]string{
		"HOME":                  home,
		"XDG_CONFIG_HOME":       home,
		"NPM_CONFIG_USERCONFIG": npmrc,
		"npm_config_userconfig": npmrc,
	}
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
// a frozen install of m from lock reads besides the .npmrc and the
// .yarnrc.yml, which copyFetchInputs copies on its own: the root package.json,
// the lock, the manager's configuration (bunfig.toml for Bun,
// pnpm-workspace.yaml for pnpm, .yarnrc for Yarn 1), each member's
// package.json, each patch file the workspace names, and each local
// dependency: the package.json of a directory, which is all an install reads
// to resolve it, or the tarball itself. Members are the root workspaces, or
// pnpm-workspace.yaml packages, together with the members the lock records.
// For Bun, a `link:` dependency, which resolves through `bun link` on one
// machine, is refused; a local dependency outside the workspace is refused for
// every manager: a hosted run holds neither.
func fetchInputs(root string, m packageManager, lock string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "package.json")); err != nil {
		return nil, fmt.Errorf("the workspace root has no readable package.json: %w", err)
	}
	inputs := map[string]bool{"package.json": true, lock: true}
	for _, config := range managerConfigFiles(m) {
		if isRegularFile(filepath.Join(root, config)) {
			inputs[config] = true
		}
	}

	// Workspace members install their devDependencies too; a local
	// dependency outside the members does not.
	type manifest struct {
		rel string
		dev bool
	}
	manifests := []manifest{{rel: "package.json", dev: true}}
	for _, member := range fetchMembers(root, m, lock) {
		rel := path.Join(member, "package.json")
		if inputs[rel] || !isRegularFile(filepath.Join(root, filepath.FromSlash(rel))) {
			continue
		}
		inputs[rel] = true
		manifests = append(manifests, manifest{rel: rel, dev: true})
	}

	patches, err := patchFiles(root, m)
	if err != nil {
		return nil, err
	}
	for _, rel := range patches {
		inputs[rel] = true
	}

	for i := 0; i < len(manifests); i++ {
		deps, err := localDependencies(root, manifests[i].rel, manifests[i].dev, m)
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

// managerConfigFiles are the root files besides the manifests and the lock
// that a frozen install of m reads and that copyFetchInputs copies verbatim.
func managerConfigFiles(m packageManager) []string {
	switch m.Name {
	case managerBun:
		return []string{"bunfig.toml"}
	case managerPNPM:
		return []string{pnpmworkspace.File}
	case managerYarn:
		if !m.Berry {
			return []string{yarnClassicConfig}
		}
	}
	return nil
}

// fetchMembers returns the member directories, relative to root in slash
// form: the matches of the root `workspaces` patterns, or of the
// pnpm-workspace.yaml packages, and the members lock records, inside root
// only.
func fetchMembers(root string, m packageManager, lock string) []string {
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
	for _, rel := range lockWorkspaceMembers(root, m, lock) {
		add(rel)
	}
	sort.Strings(members)
	return members
}

// lockWorkspaceMembers returns the member paths lock records, the root
// excluded: the `workspaces` of the text bun.lock, the `importers` of
// pnpm-lock.yaml, the `packages` of an npm lock outside node_modules, and the
// `workspace:` resolutions of a Yarn 2 lock. A Yarn 1 lock records none. A
// lock that cannot be read yields nothing here and fails the install that
// reads it.
func lockWorkspaceMembers(root string, m packageManager, lock string) []string {
	data, err := os.ReadFile(filepath.Join(root, lock))
	if err != nil {
		return nil
	}
	var members []string
	switch m.Name {
	case managerBun:
		var parsed struct {
			Workspaces map[string]json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(project.StripJSONC(data), &parsed) != nil {
			return nil
		}
		for rel := range parsed.Workspaces {
			members = append(members, rel)
		}
	case managerNPM:
		var parsed struct {
			Packages map[string]json.RawMessage `json:"packages"`
		}
		if json.Unmarshal(data, &parsed) != nil {
			return nil
		}
		for rel := range parsed.Packages {
			if !strings.HasPrefix(rel, "node_modules/") && !strings.Contains(rel, "/node_modules/") {
				members = append(members, rel)
			}
		}
	case managerPNPM:
		members = pnpmLockImporters(string(data))
	case managerYarn:
		members = yarnLockWorkspaces(string(data))
	}
	out := members[:0]
	for _, rel := range members {
		if rel != "" && rel != "." {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// pnpmLockImporters returns the keys of the top-level `importers` mapping of a
// pnpm-lock.yaml: one line each, at the indentation of the first one.
func pnpmLockImporters(text string) []string {
	var members []string
	in := false
	indent := -1
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		if line[0] != ' ' {
			in = strings.HasPrefix(line, "importers:")
			continue
		}
		if !in {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 {
			indent = n
		}
		if n != indent {
			continue
		}
		key := strings.TrimSpace(line)
		key = strings.TrimSuffix(strings.TrimSuffix(key, "{}"), " ")
		key = strings.TrimSuffix(key, ":")
		members = append(members, strings.Trim(key, `'"`))
	}
	return members
}

// yarnLockWorkspaces returns the paths of the `@workspace:` resolutions of a
// Yarn 2 and later lock, such as "web@workspace:apps/web".
func yarnLockWorkspaces(text string) []string {
	var members []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "resolution:") {
			continue
		}
		value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "resolution:")), `"`)
		if _, rel, ok := strings.Cut(value, "@workspace:"); ok {
			members = append(members, rel)
		}
	}
	return members
}

// patchFiles returns the patch files the workspace names that exist: the
// patchedDependencies of the root package.json for Bun, those of its pnpm
// member and of pnpm-workspace.yaml for pnpm, and the files under .yarn/patches
// for Yarn 2 and later. A patch path outside the workspace is refused.
func patchFiles(root string, m packageManager) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	var pkg struct {
		Patched map[string]string `json:"patchedDependencies"`
		PNPM    struct {
			Patched map[string]string `json:"patchedDependencies"`
		} `json:"pnpm"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	declared := map[string]string{}
	switch m.Name {
	case managerBun:
		declared = pkg.Patched
	case managerPNPM:
		for dependency, file := range pkg.PNPM.Patched {
			declared[dependency] = file
		}
		if text, err := os.ReadFile(pnpmWorkspaceFile(root)); err == nil {
			fromWorkspace, err := pnpmworkspace.Mapping(string(text), "patchedDependencies")
			if err != nil {
				return nil, err
			}
			for dependency, file := range fromWorkspace {
				declared[dependency] = file
			}
		}
	case managerYarn:
		if m.Berry {
			return yarnPatchFiles(root)
		}
	}
	var patches []string
	for dependency, file := range declared {
		rel, ok := insideRoot(file)
		if !ok {
			return nil, fmt.Errorf("the workspace patches %s with %q, outside the workspace: a hosted run fetches only what the workspace holds", dependency, file)
		}
		if isRegularFile(filepath.Join(root, filepath.FromSlash(rel))) {
			patches = append(patches, rel)
		}
	}
	sort.Strings(patches)
	return patches, nil
}

// yarnPatchFiles returns the regular files under .yarn/patches, where `yarn
// patch-commit` writes the patches a Yarn 2 and later lock resolves.
func yarnPatchFiles(root string) ([]string, error) {
	dir := filepath.Join(root, ".yarn", "patches")
	var patches []string
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && file == dir {
				return filepath.SkipDir
			}
			return err
		}
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			patches = append(patches, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list .yarn/patches: %w", err)
	}
	sort.Strings(patches)
	return patches, nil
}

// localDependencies returns the files the manager reads for the local
// dependencies one manifest declares, relative to root in slash form: a
// directory's package.json or a tarball. withDev includes devDependencies,
// which the managers install for the root and the members only.
func localDependencies(root, manifestRel string, withDev bool, m packageManager) ([]string, error) {
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
			target, link, local := localDependencyTarget(set[name], m)
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
				// The install reports the missing path when it resolves the lock.
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

// localDependencyTarget reads a dependency spec the way m does: `file:` or a
// spec starting with `./`, `../`, `/` or `~/` names a directory or a tarball on
// disk. `link:` is a bun link for Bun, and a directory for pnpm and Yarn, as
// Yarn's `portal:` is.
func localDependencyTarget(spec string, m packageManager) (target string, link, local bool) {
	spec = strings.TrimSpace(spec)
	switch {
	case strings.HasPrefix(spec, "link:") && m.Name == managerBun:
		return "", true, false
	case strings.HasPrefix(spec, "link:"):
		return strings.TrimPrefix(spec, "link:"), false, true
	case strings.HasPrefix(spec, "portal:") && m.Name == managerYarn:
		return strings.TrimPrefix(spec, "portal:"), false, true
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
// relative path, then the root .npmrc and .yarnrc.yml. With handed set it drops
// their credential lines: a workspace line for a host outranks the private
// home's, so it would replace the handed bearer. Without a handed credential
// both stay verbatim, and the manager reads them as the install in the
// workspace does.
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
	for _, config := range []struct {
		name  string
		strip func([]byte) []byte
	}{
		{".npmrc", stripNpmrcCredentials},
		{yarnBerryConfig, stripYarnrcCredentials},
	} {
		data, err := os.ReadFile(filepath.Join(root, config.name))
		switch {
		case err == nil:
			if handed {
				data = config.strip(data)
			}
			if err := writePrivateFile(filepath.Join(scratch, config.name), data); err != nil {
				return fmt.Errorf("copy %s into the fetch copy: %w", config.name, err)
			}
		case !os.IsNotExist(err):
			return fmt.Errorf("read %s: %w", config.name, err)
		}
	}
	return nil
}

// stripYarnrcCredentials drops every .yarnrc.yml line that mentions
// npmAuthToken or npmAuthIdent, in any letter case and any style, a flow
// mapping on one line included, and keeps every other line verbatim.
func stripYarnrcCredentials(content []byte) []byte {
	lines := strings.SplitAfter(string(content), "\n")
	var out strings.Builder
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "npmauthtoken") || strings.Contains(lower, "npmauthident") {
			continue
		}
		out.WriteString(line)
	}
	return []byte(out.String())
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
