package credentialcustody

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	sdkcli "go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
)

// The workspace a publication e2e fixture publishes from, and the members it
// publishes: one npm package through the real TypeScript extension and one Go
// module through the real Go extension.
const (
	e2eNamespace     = "publication-e2e"
	e2eChannel       = "pr-0"
	e2eNPMCoordinate = "@fixture/web"
	e2eNPMProject    = "web"
	e2eGoCoordinate  = "example.com/fixture/mod"
	e2eGoProject     = "mod"
	// e2eRuntimeVersion is the version both real extension runtimes are built
	// with and installed at.
	e2eRuntimeVersion = "1.0.0"
	// e2eUnheldRevision is a full revision that no fixture snapshot holds: the
	// source revision of a head that is not forward.
	e2eUnheldRevision = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// e2ePublishArgs is the engine role's command: publish every member to
	// e2eChannel through the publish provider.
	e2ePublishArgs = "publish\n--all\n--channel\n" + e2eChannel + "\n--providers\npublish"
)

// runStageRole is the package step of a publication e2e fixture: an SDK job
// that stages the member the release-set plan assigns its project where the
// real extension's publication job reads it, as that extension's own package
// step does, then appends "staged <project path>" to the order log.
func runStageRole() int {
	sdkcli.RunSubcommand(map[string]sdkcli.JobFunc{
		"stage-npm": stageNPMPackage,
		"stage-go":  stageGoModule,
	})
	return 0
}

// stageNPMPackage stages the project's npm package at its planned version.
func stageNPMPackage(ctx *pctx.Context, _ *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	member, err := plannedMember(ctx, "npm", ctx.Project.Name)
	if err != nil {
		return "FAILED", nil, err
	}
	manifest, err := json.Marshal(map[string]string{"name": member.Coordinate, "version": member.Version, "main": "index.js"})
	if err != nil {
		return "FAILED", nil, err
	}
	dir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "npm")
	if err := writeFiles(dir, map[string][]byte{"package.json": manifest, "index.js": []byte("module.exports = 1;\n")}); err != nil {
		return "FAILED", nil, err
	}
	if err := pkgmeta.WriteChannelRecord(dir, pkgmeta.ChannelRecord{Version: member.Version, Channels: []string{"npm"}}); err != nil {
		return "FAILED", nil, err
	}
	if err := logStaged(ctx); err != nil {
		return "FAILED", nil, err
	}
	return "OK", nil, nil
}

// stageGoModule stages the project's Go module at its planned version: the
// module zip, go.mod, the .info document and the module metadata.
func stageGoModule(ctx *pctx.Context, _ *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	projectRoot := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	goMod, err := os.ReadFile(filepath.Join(projectRoot, "go.mod"))
	if err != nil {
		return "FAILED", nil, err
	}
	source, err := os.ReadFile(filepath.Join(projectRoot, "mod.go"))
	if err != nil {
		return "FAILED", nil, err
	}
	modulePath := goModulePath(goMod)
	member, err := plannedMember(ctx, "go", modulePath)
	if err != nil {
		return "FAILED", nil, err
	}
	archive, err := moduleZip(modulePath, member.Version, map[string][]byte{"go.mod": goMod, "mod.go": source})
	if err != nil {
		return "FAILED", nil, err
	}
	dir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "go")
	meta := pkgmeta.GoModuleMetadata{
		ModulePath: modulePath, Version: member.Version, SourceDir: projectRoot,
		ZipPath: filepath.Join(dir, "module.zip"), ModPath: filepath.Join(dir, "go.mod"), InfoPath: filepath.Join(dir, "version.info"),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "FAILED", nil, err
	}
	info := fmt.Sprintf(`{"Version":%q,"Time":"2020-01-01T00:00:00Z"}`, member.Version)
	files := map[string][]byte{"module.zip": archive, "go.mod": goMod, "version.info": []byte(info), "module.json": metaJSON}
	if err := writeFiles(dir, files); err != nil {
		return "FAILED", nil, err
	}
	if err := pkgmeta.WriteChannelRecord(dir, pkgmeta.ChannelRecord{Version: member.Version, Channels: []string{"go"}}); err != nil {
		return "FAILED", nil, err
	}
	if err := logStaged(ctx); err != nil {
		return "FAILED", nil, err
	}
	return "OK", nil, nil
}

// plannedMember is the member of the job's release-set plan that ecosystem
// and coordinate name; the plan must select it.
func plannedMember(ctx *pctx.Context, ecosystem distribution.Ecosystem, coordinate string) (releaseset.PlannedMember, error) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil {
		return releaseset.PlannedMember{}, err
	}
	if plan == nil {
		return releaseset.PlannedMember{}, errors.New("the job context carries no release-set plan")
	}
	member, ok := plan.Member(ecosystem, coordinate)
	if !ok || !member.Selected {
		return releaseset.PlannedMember{}, fmt.Errorf("the release-set plan does not select %s %s", ecosystem, coordinate)
	}
	return member, nil
}

// logStaged records in the custody order log that the job staged its project.
func logStaged(ctx *pctx.Context) error {
	return appendLog(os.Getenv(custodyOrderEnv), "staged "+ctx.Project.Path)
}

// goModulePath is the module path the module line of goMod declares.
func goModulePath(goMod []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(goMod))
	for scanner.Scan() {
		if path, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "module "); ok {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// e2eFileTime is the modification time of every file an e2e archive holds, so
// that two runs of one commit pack the same bytes.
var e2eFileTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// moduleZip is the module zip of modulePath at version holding files, in name
// order, under the <module>@<version>/ prefix the module zip format sets.
func moduleZip(modulePath, version string, files map[string][]byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: modulePath + "@" + version + "/" + name, Method: zip.Deflate, Modified: e2eFileTime})
		if err != nil {
			return nil, err
		}
		if _, err := entry.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// runBunRole is the bun a real TypeScript extension packs with: run as `bun
// pm pack ... --destination <dir>` in a staged npm package directory, it
// writes one gzip tarball of that directory to dir, the same bytes for the
// same files. It refuses every other command.
func runBunRole() int {
	args := os.Args[1:]
	destination := ""
	for i, arg := range args {
		if arg == "--destination" && i+1 < len(args) {
			destination = args[i+1]
		}
	}
	if len(args) < 2 || args[0] != "pm" || args[1] != "pack" || destination == "" {
		fmt.Fprintf(os.Stderr, "custody bun: unsupported command %q\n", args)
		return 2
	}
	if err := packNPMDirectory(destination); err != nil {
		fmt.Fprintf(os.Stderr, "custody bun: %v\n", err)
		return 1
	}
	return 0
}

// packNPMDirectory packs the working directory, the channel record excluded,
// into <name>-<version>.tgz in destination, every entry under package/.
func packNPMDirectory(destination string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return err
	}
	var manifest struct{ Name, Version string }
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == pkgmeta.ChannelRecordFile {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		return err
	}
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		header := &tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(files[name])), ModTime: e2eFileTime, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if _, err := archive.Write(files[name]); err != nil {
			return err
		}
	}
	if err := errors.Join(archive.Close(), compressed.Close()); err != nil {
		return err
	}
	name := strings.NewReplacer("@", "", "/", "-").Replace(manifest.Name) + "-" + manifest.Version + ".tgz"
	return os.WriteFile(filepath.Join(destination, name), buffer.Bytes(), 0o644)
}

// runExecProbeRole is the publication job of a real extension behind a probe:
// it hunts for the publish credential as every hostile role does, then runs
// the extension binary custodyExecEnv names with this process's arguments,
// standard streams and environment, and exits with its status.
func runExecProbeRole() int {
	if code := runHostileRole("publication"); code != 0 {
		return code
	}
	cmd := exec.Command(os.Getenv(custodyExecEnv), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		fmt.Fprintf(os.Stderr, "custody exec-probe: %v\n", err)
		return 1
	}
}

func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// custodyGoRegistry is a Go module registry that stores what it is sent with
// custodyBearer and refuses every other request.
type custodyGoRegistry struct {
	server *httptest.Server

	mu      sync.Mutex
	blobs   map[string][]byte
	zips    map[string][]byte
	mods    map[string][]byte
	refused int
}

func newCustodyGoRegistry(t *testing.T) *custodyGoRegistry {
	t.Helper()
	reg := &custodyGoRegistry{}
	reg.reset()
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *custodyGoRegistry) reset() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.blobs, reg.zips, reg.mods, reg.refused = map[string][]byte{}, map[string][]byte{}, map[string][]byte{}, 0
}

func (reg *custodyGoRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+custodyBearer {
		reg.refused++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/-/blobs/upload"):
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
		reg.blobs[digest] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"digest": digest})
	case r.Method == http.MethodPut && strings.Contains(path, "/@v/"):
		var request struct {
			GoMod     string `json:"go_mod"`
			ZipDigest string `json:"zip_digest"`
		}
		if err := json.Unmarshal(body, &request); err != nil || reg.blobs[request.ZipDigest] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reg.zips[path] = reg.blobs[request.ZipDigest]
		reg.mods[path] = []byte(request.GoMod)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".zip"):
		answerStored(w, reg.zips[strings.TrimSuffix(path, ".zip")])
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".mod"):
		answerStored(w, reg.mods[strings.TrimSuffix(path, ".mod")])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func answerStored(w http.ResponseWriter, data []byte) {
	if data == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(data)
}

// stored returns the module versions the registry stores, as
// <module>@<version> mapped to the digest of the zip, and how many requests
// it refused.
func (reg *custodyGoRegistry) stored() (map[string]string, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	members := make(map[string]string, len(reg.zips))
	for path, data := range reg.zips {
		module, version, _ := strings.Cut(path, "/@v/")
		members[module+"@"+version] = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	}
	return members, reg.refused
}

func (reg *custodyNPMRegistry) reset() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.tarballs, reg.refused = map[string]custodyTarball{}, 0
}

// storedDigests returns the package versions the registry stores, as
// <name>@<version> mapped to the digest of the tarball.
func (reg *custodyNPMRegistry) storedDigests() map[string]string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	members := make(map[string]string, len(reg.tarballs))
	for member, tarball := range reg.tarballs {
		members[member] = fmt.Sprintf("sha256:%x", sha256.Sum256(tarball.data))
	}
	return members
}

// publicationE2EFixture is a git workspace with two projects, web and mod,
// that publish an npm package and a Go module through the real TypeScript and
// Go extension runtimes, and a credential provider extension whose native
// runtime is this test binary in the "credential-provider" role. All three
// extensions are installed in an artifact store outside every probed root,
// because the provider's runtime holds custodyBearer as a constant. Each
// extension's package step is this binary in the "stage" role, and each
// publication job is the real runtime behind the "exec-probe" role. Every log
// and report lies outside every probed root.
type publicationE2EFixture struct {
	self, wsRoot, store, fakeBin string
	// order, ledger, setup and providerLog are the provider's files;
	// npmProbe and goProbe are the reports of the two publication jobs.
	order, ledger, setup, providerLog string
	npmProbe, goProbe                 string
	// base is the first commit, the source revision of a forward head; head
	// is the commit a run publishes.
	base, head string
	npm        *custodyNPMRegistry
	gomod      *custodyGoRegistry
}

// writePublicationE2EFixture builds the fixture and the two real extension
// runtimes.
func writePublicationE2EFixture(t *testing.T) *publicationE2EFixture {
	t.Helper()
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reports := t.TempDir()
	fx := &publicationE2EFixture{
		self: self, wsRoot: t.TempDir(), store: t.TempDir(), fakeBin: t.TempDir(),
		order:       filepath.Join(reports, "order.log"),
		ledger:      filepath.Join(reports, "ledger.jsonl"),
		setup:       filepath.Join(reports, "setup.json"),
		providerLog: filepath.Join(reports, "provider.log"),
		npmProbe:    filepath.Join(reports, "npm-probe.jsonl"),
		goProbe:     filepath.Join(reports, "go-probe.jsonl"),
		npm:         newCustodyNPMRegistry(t),
		gomod:       newCustodyGoRegistry(t),
	}
	tsRuntime, goRuntime := buildExtensionRuntimes(t, fx.store)

	clitest.WriteFile(t, filepath.Join(fx.fakeBin, "bun"),
		fmt.Sprintf("#!/bin/sh\n%s=bun exec %s \"$@\"\n", custodyRoleEnv, shellQuote(self)))
	if err := os.Chmod(filepath.Join(fx.fakeBin, "bun"), 0o755); err != nil {
		t.Fatal(err)
	}

	stageEnv := map[string]string{custodyRoleEnv: "stage", custodyOrderEnv: fx.order}
	fx.installExtension(t, extensionFixture{
		name: "@putnami/typescript", shortName: "typescript", runtime: tsRuntime, source: "typescript/extension", step: "npm",
		packageTask: "package-npm", stage: stageTask("stage-npm", stageEnv), publishTask: "publish-npm",
		publishEnv: map[string]string{custodyRoleEnv: "exec-probe", custodyExecEnv: tsRuntime, custodyReportEnv: fx.npmProbe},
	})
	fx.installExtension(t, extensionFixture{
		name: "@putnami/go", shortName: "go", runtime: goRuntime, source: "go/extension", step: "go",
		packageTask: "package-go", stage: stageTask("stage-go", stageEnv), publishTask: "publish-go",
		publishEnv: map[string]string{custodyRoleEnv: "exec-probe", custodyExecEnv: goRuntime, custodyReportEnv: fx.goProbe},
	})
	fx.installProvider(t)

	write := func(rel, content string) {
		t.Helper()
		clitest.WriteFile(t, filepath.Join(fx.wsRoot, rel), content)
	}
	write("putnami.workspace.json", fmt.Sprintf(`{
  "name": %q,
  "includes": [%q, %q],
  "extensions": { "@putnami/typescript": %q, "@putnami/go": %q, "@fixture/provider": %q },
  "registries": { "npm": { "publish": %s }, "go": { "origin": %s } }
}`, e2eNamespace, e2eNPMProject, e2eGoProject, e2eRuntimeVersion, e2eRuntimeVersion, e2eRuntimeVersion,
		jsonString(fx.npm.server.URL), jsonString(fx.gomod.server.URL)))
	write(".gitignore", ".putnami/\n.gen/\n")
	write(e2eNPMProject+"/putnami.json", fmt.Sprintf(`{"name":%q,"extensions":["@putnami/typescript"],"publish":["npm"]}`, e2eNPMCoordinate))
	write(e2eNPMProject+"/package.json", fmt.Sprintf(`{"name":%q,"version":"0.0.0","main":"index.js"}`, e2eNPMCoordinate))
	write(e2eNPMProject+"/index.js", "module.exports = 1;\n")
	write(e2eGoProject+"/putnami.json", `{"name":"mod","extensions":["@putnami/go"],"publish":["go"]}`)
	write(e2eGoProject+"/go.mod", "module "+e2eGoCoordinate+"\n\ngo 1.22\n")
	write(e2eGoProject+"/mod.go", "package mod\n\n// Answer is the answer.\nconst Answer = 42\n")
	clitest.InitGitRepo(t, fx.wsRoot)
	fx.base = strings.TrimSpace(clitest.GitOutput(t, fx.wsRoot, "rev-parse", "HEAD"))
	clitest.RunGit(t, fx.wsRoot, "commit", "--allow-empty", "-m", "second")
	fx.head = strings.TrimSpace(clitest.GitOutput(t, fx.wsRoot, "rev-parse", "HEAD"))
	return fx
}

// buildExtensionRuntimes builds the TypeScript and Go extension runtimes from
// their sources, as their prepare steps do, stamped e2eRuntimeVersion, into
// the extensions of store, and returns their paths.
func buildExtensionRuntimes(t *testing.T, store string) (tsRuntime, goRuntime string) {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go unavailable")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tsRuntime = filepath.Join(store, "extensions", "typescript@"+e2eRuntimeVersion, "compiled", "putnami-ts")
	goRuntime = filepath.Join(store, "extensions", "go@"+e2eRuntimeVersion, "compiled", "putnami-go")
	build := func(source, pkg, output string) error {
		cmd := exec.Command(goBinary, "build", "-o", output, "-ldflags", "-X main.runtimeVersion="+e2eRuntimeVersion, pkg)
		cmd.Dir = filepath.Join(root, source)
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", source, err, out)
		}
		return nil
	}
	var wg sync.WaitGroup
	var tsErr, goErr error
	wg.Add(2)
	go func() { defer wg.Done(); tsErr = build("typescript/extension", "./cmd/putnami-ts", tsRuntime) }()
	go func() { defer wg.Done(); goErr = build("go/extension", "./cmd/putnami-go", goRuntime) }()
	wg.Wait()
	if err := errors.Join(tsErr, goErr); err != nil {
		t.Fatal(err)
	}
	return tsRuntime, goRuntime
}

// fixtureManifest is the extension manifest a publication e2e fixture
// installs.
type fixtureManifest struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	CLIContract int                        `json:"cliContract"`
	Runtime     manifestRuntime            `json:"runtime"`
	Workspace   map[string]json.RawMessage `json:"workspace,omitempty"`
	Ecosystems  json.RawMessage            `json:"ecosystems,omitempty"`
	Commands    map[string]manifestCommand `json:"commands"`
	Tasks       map[string]json.RawMessage `json:"tasks"`
}

type manifestRuntime struct {
	Executable string `json:"executable"`
}

type manifestCommand struct {
	Description string         `json:"description,omitempty"`
	DependsOn   []string       `json:"dependsOn,omitempty"`
	Run         []manifestStep `json:"run"`
}

type manifestStep struct {
	ID   string `json:"id"`
	Task string `json:"task"`
}

// manifestTask is a command task of a fixture manifest.
type manifestTask struct {
	Kind      string                       `json:"kind"`
	Command   string                       `json:"command"`
	Args      []string                     `json:"args"`
	Cwd       string                       `json:"cwd,omitempty"`
	TimeoutMs int                          `json:"timeoutMs,omitempty"`
	Cache     bool                         `json:"cache"`
	Env       map[string]string            `json:"env"`
	Inputs    map[string]manifestTaskInput `json:"inputs,omitempty"`
}

type manifestTaskInput struct {
	From string `json:"from"`
}

// stageTask is a package task that runs as the SDK job named job, with the
// release-set plan in its context; installExtension makes its command this
// binary.
func stageTask(job string, env map[string]string) manifestTask {
	return manifestTask{
		Kind: "command", Args: []string{job}, Cwd: "{projectRoot}", TimeoutMs: 60000, Env: env,
		Inputs: map[string]manifestTaskInput{"releaseSetPlan": {From: "params"}},
	}
}

// extensionFixture is a real extension a publication e2e fixture installs:
// its runtime, its source, the package task that stages its member and the
// environment its shipped publication task runs in.
type extensionFixture struct {
	name, shortName, runtime, source, step string
	packageTask                            string
	stage                                  manifestTask
	publishTask                            string
	publishEnv                             map[string]string
}

// installExtension installs ext in the store, with its runtime as the native
// runtime. The manifest keeps the shipped ecosystems, the shipped workspace
// probe scope without its sync task, and the shipped publication task, whose
// command becomes this binary in ext's publish environment. Its package
// command runs the stage task, and its publish command the publication task.
func (fx *publicationE2EFixture) installExtension(t *testing.T, ext extensionFixture) {
	t.Helper()
	shipped := readShippedManifest(t, ext.source)
	var publish map[string]json.RawMessage
	if err := json.Unmarshal(shipped.Tasks[ext.publishTask], &publish); err != nil {
		t.Fatalf("shipped task %s of %s: %v", ext.publishTask, ext.name, err)
	}
	publish["command"], publish["env"] = rawJSON(t, fx.self), rawJSON(t, ext.publishEnv)
	ext.stage.Command = fx.self
	delete(shipped.Workspace, "syncTask")
	fx.install(t, ext.name, ext.shortName, fixtureManifest{
		Name: ext.name, Version: e2eRuntimeVersion, CLIContract: protocolcli.CurrentContract,
		Runtime:    manifestRuntime{Executable: "compiled/" + filepath.Base(ext.runtime)},
		Workspace:  shipped.Workspace,
		Ecosystems: shipped.Ecosystems,
		Commands: map[string]manifestCommand{
			"package": {Run: []manifestStep{{ID: ext.step, Task: ext.packageTask}}},
			"publish": {DependsOn: []string{"package"}, Run: []manifestStep{{ID: ext.step, Task: ext.publishTask}}},
		},
		Tasks: map[string]json.RawMessage{ext.packageTask: rawJSON(t, ext.stage), ext.publishTask: rawJSON(t, publish)},
	})
}

// installProvider installs @fixture/provider, whose native runtime is a copy
// of this binary that serves the credential-provider command.
func (fx *publicationE2EFixture) installProvider(t *testing.T) {
	t.Helper()
	extRoot := filepath.Join(fx.store, "extensions", "provider@"+e2eRuntimeVersion)
	fixtureproc.Binary(t, filepath.Join(extRoot, "compiled", "runtime"))
	hosts := []string{serverHostOf(t, fx.npm.server), serverHostOf(t, fx.gomod.server)}
	fx.install(t, "@fixture/provider", "provider", fixtureManifest{
		Name: "@fixture/provider", Version: e2eRuntimeVersion, CLIContract: protocolcli.CurrentContract,
		Runtime: manifestRuntime{Executable: "compiled/runtime"},
		Commands: map[string]manifestCommand{
			registry.CredentialProviderCommand: {Description: "Serve credentials.", Run: []manifestStep{{ID: "serve", Task: "credential-provider"}}},
		},
		Tasks: map[string]json.RawMessage{"credential-provider": rawJSON(t, manifestTask{
			Kind: "command", Command: "{extensionRuntime}", Args: []string{"credential-provider"},
			Env: map[string]string{
				custodyRoleEnv: "credential-provider", custodyReportEnv: fx.providerLog, custodyHostsEnv: strings.Join(hosts, ","),
				custodyOrderEnv: fx.order, custodyLedgerEnv: fx.ledger, custodySetupEnv: fx.setup,
			},
		})},
	})
}

// install writes manifest into the store as name's installed extension and
// links it from the workspace, as a registry pin is.
func (fx *publicationE2EFixture) install(t *testing.T, name, shortName string, manifest fixtureManifest) {
	t.Helper()
	extRoot := filepath.Join(fx.store, "extensions", shortName+"@"+e2eRuntimeVersion)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), string(data))
	link := layout.StableDir(fx.wsRoot, layout.Extensions, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extRoot, link); err != nil {
		t.Fatal(err)
	}
}

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// shippedManifest is the part of a shipped extension manifest a fixture
// reuses.
type shippedManifest struct {
	Workspace  map[string]json.RawMessage `json:"workspace"`
	Ecosystems json.RawMessage            `json:"ecosystems"`
	Tasks      map[string]json.RawMessage `json:"tasks"`
}

func readShippedManifest(t *testing.T, source string) shippedManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "..", source, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest shippedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func serverHostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return target.Host
}

// e2eRun is what one publish run of the fixture left: its exit code and
// output, the order log, and the provider's ledger.
type e2eRun struct {
	code       int
	output     string
	order      []string
	initialize []ledgerEntry
	openRaw    []json.RawMessage
	opens      []registry.OpenParams
	releases   []registry.ReleaseParams
	end        ledgerEntry
}

// publish runs `publish` in the fixture, hosted or not, against a provider
// that starts from setup, with the registries and every log emptied first.
func (fx *publicationE2EFixture) publish(t *testing.T, hosted bool, setup providerSetup) e2eRun {
	t.Helper()
	for _, path := range []string{fx.order, fx.ledger, fx.providerLog, fx.npmProbe, fx.goProbe} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	setup.Namespace, setup.Channels = e2eNamespace, []string{e2eChannel}
	data, err := json.Marshal(setup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.setup, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.npm.reset()
	fx.gomod.reset()

	run := e2eRun{}
	run.code, run.output = runEngine(t, fx.self, fx.wsRoot, t.TempDir(), hosted,
		custodyArgsEnv+"="+e2ePublishArgs,
		"PUTNAMI_ARTIFACT_DIR="+fx.store,
		"PATH="+fx.fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if data, err := os.ReadFile(fx.order); err == nil {
		run.order = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	ledger, err := os.ReadFile(fx.ledger)
	if err != nil {
		t.Fatalf("the provider wrote no ledger: %v\n%s", err, run.output)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(ledger)), "\n") {
		var entry ledgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		switch entry.Op {
		case string(registry.CredentialOpInitialize):
			run.initialize = append(run.initialize, entry)
		case string(registry.CredentialOpOpen):
			var params registry.OpenParams
			if err := json.Unmarshal(entry.Payload, &params); err != nil {
				t.Fatalf("open payload: %v", err)
			}
			run.openRaw, run.opens = append(run.openRaw, entry.Payload), append(run.opens, params)
		case string(registry.CredentialOpRelease):
			var params registry.ReleaseParams
			if err := json.Unmarshal(entry.Payload, &params); err != nil {
				t.Fatalf("release payload: %v", err)
			}
			run.releases = append(run.releases, params)
		case "end":
			run.end = entry
		}
	}
	return run
}

// forwardHead is a head of e2eChannel whose members were published from
// revision: the two fixture members at an earlier version.
func forwardHead(revision string) distribution.ReleaseSet {
	member := func(ecosystem distribution.Ecosystem, coordinate, version string, fill byte) distribution.ReleaseSetMember {
		return distribution.ReleaseSetMember{
			Ecosystem: ecosystem, Coordinate: coordinate, Version: version,
			ArtifactDigest:       "sha256:" + strings.Repeat(string(fill), 64),
			Dependencies:         []distribution.ReleaseSetDependency{},
			SourceRevision:       revision,
			SelectionFingerprint: "sha256:" + strings.Repeat("f", 64),
		}
	}
	return distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       e2eNamespace,
		Members: []distribution.ReleaseSetMember{
			member("go", e2eGoCoordinate, "v0.0.1", 'a'),
			member("npm", e2eNPMCoordinate, "0.0.1", 'b'),
		},
	}
}

func setRef(t *testing.T, set distribution.ReleaseSet) distribution.ReleaseSetRef {
	t.Helper()
	ref, diagnostics := distribution.DeriveReleaseSetRef(distribution.NormalizeReleaseSet(&set))
	if len(diagnostics) > 0 {
		t.Fatalf("the set has no ref: %v", diagnostics)
	}
	return ref
}

// sessionMemberEvent is one published-member event of the latest session: the
// task that reported it and the member it names.
type sessionMemberEvent struct {
	key, member, digest string
}

// publishedMemberEvents reads the published-member events of the latest
// session of wsRoot.
func publishedMemberEvents(t *testing.T, wsRoot string) []sessionMemberEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsRoot, ".putnami", "sessions", "latest", "events.jsonl"))
	if err != nil {
		t.Fatalf("read the session events: %v", err)
	}
	var events []sessionMemberEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record struct {
			Record   string `json:"record"`
			Identity struct {
				Key string `json:"key"`
			} `json:"identity"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("session record %q: %v", line, err)
		}
		if record.Record != protocolcli.RecordTaskEvent {
			continue
		}
		// An event whose fields are not these strings is not a
		// published-member event.
		var event struct {
			Kind           string `json:"kind"`
			Coordinate     string `json:"coordinate"`
			Version        string `json:"version"`
			ArtifactDigest string `json:"artifactDigest"`
		}
		if json.Unmarshal(record.Event, &event) != nil || event.Kind != "published-member" {
			continue
		}
		events = append(events, sessionMemberEvent{key: record.Identity.Key, member: event.Coordinate + "@" + event.Version, digest: event.ArtifactDigest})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].member < events[j].member })
	return events
}

// assertProbesFoundNothing fails t when a probe of the publication job report
// did not run or found the publish credential, or when the job had no outbox.
func assertProbesFoundNothing(t *testing.T, role, report string) {
	t.Helper()
	findings := readFindings(t, report)
	rootExempt := runtime.GOOS == "linux" && os.Geteuid() == 0
	for _, probe := range searchProbes() {
		f, ok := findings[probe]
		switch {
		case !ok:
			t.Errorf("%s: probe %q did not run", role, probe)
		case !f.Checked:
			t.Errorf("%s: probe %q recorded no check (detail %q)", role, probe, f.Detail)
		case slices.Contains(f.Found, "bearer"):
			if rootExempt && slices.Contains(platformSearchProbes(), probe) {
				t.Logf("%s: probe %q read the engine as root; root is outside the same-user threat model", role, probe)
				continue
			}
			t.Errorf("%s: probe %q found the publish credential (detail %q)", role, probe, f.Detail)
		}
	}
	if values := findings[envValuesProbe]; values.Env["outbox"] == "" {
		t.Errorf("%s: the publication job had no outbox", role)
	}
}

// planMembers lists the members of plan as <ecosystem>/<coordinate>@<version>.
func planMembers(plan registry.PublicationPlan) []string {
	members := make([]string, 0, len(plan.Members))
	for _, member := range plan.Members {
		members = append(members, string(member.Ecosystem)+"/"+member.Coordinate+"@"+member.Version)
	}
	return members
}

// setMembers lists the members of set as <ecosystem>/<coordinate>@<version>,
// sorted.
func setMembers(set distribution.ReleaseSet) []string {
	members := make([]string, 0, len(set.Members))
	for _, member := range set.Members {
		members = append(members, string(member.Ecosystem)+"/"+member.Coordinate+"@"+member.Version)
	}
	slices.Sort(members)
	return members
}

// boundedRefusal fails t unless output names code on one line of at most
// 1 KiB.
func boundedRefusal(t *testing.T, output, code string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, code) {
			if len(line) > 1024 {
				t.Errorf("the refusal %s is reported on a line of %d bytes; want at most 1024", code, len(line))
			}
			return
		}
	}
	t.Errorf("the output does not name the refusal %s:\n%s", code, output)
}

// A publish run through the real TypeScript and Go extensions and a
// publication-v1 provider: the extensions' package steps stage an npm package
// and a Go module, their publication jobs, behind a probe, pack them into the
// outbox and find no credential, and the engine uploads both with the
// credential the provider issues after the one open, then releases the
// opened plan forward from the channel's head. A head that is not an ancestor
// of the commit is refused at open and nothing is uploaded; a release whose
// members the registries do not store is refused, and no channel moves.
func TestReleaseSetE2EPublicationV1UploadsInProcessAndReleasesForwardOnly(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "real-extension-jobs-pack-and-the-engine-uploads")
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "real-extensions-open-once-and-release-forward-only")
	fx := writePublicationE2EFixture(t)

	t.Run("forward", func(t *testing.T) {
		forward := forwardHead(fx.base)
		forwardRef := setRef(t, forward)
		run := fx.publish(t, false, providerSetup{Heads: map[string]distribution.ReleaseSet{e2eChannel: forward}})
		if run.code != 0 {
			t.Fatalf("publish exit=%d, want 0\n%s", run.code, run.output)
		}

		// One open, after every package step, then the publish credential,
		// then one release.
		opens := indexesOf(run.order, string(registry.CredentialOpOpen))
		credential := slices.Index(run.order, string(registry.CredentialOpCredential)+" "+registry.PurposePublish)
		release := indexesOf(run.order, string(registry.CredentialOpRelease))
		staged := indexesOf(run.order, "staged "+e2eNPMProject, "staged "+e2eGoProject)
		if len(opens) != 1 || len(staged) != 2 || slices.Max(staged) > opens[0] ||
			credential < opens[0] || len(release) != 1 || release[0] < credential {
			t.Errorf("order = %q; want both members staged, then one open, then the publish credential, then one release", run.order)
		}

		// The engine uploaded both members with the provider's credential.
		npmStored := fx.npm.storedDigests()
		_, npmRefused := fx.npm.stored()
		goStored, goRefused := fx.gomod.stored()
		if len(npmStored) != 1 || len(goStored) != 1 || npmRefused != 0 || goRefused != 0 {
			t.Errorf("the registries store npm %v and go %v and refused %d and %d requests; want one member each, every request with the publish credential",
				npmStored, goStored, npmRefused, goRefused)
		}
		// The publication jobs held no credential, and only upload nodes
		// reported a published member, one per stored artifact.
		assertProbesFoundNothing(t, "npm publication", fx.npmProbe)
		assertProbesFoundNothing(t, "go publication", fx.goProbe)
		events := publishedMemberEvents(t, fx.wsRoot)
		stored := maps.Clone(npmStored)
		maps.Copy(stored, goStored)
		if len(events) != 2 {
			t.Errorf("published-member events = %+v; want one per member", events)
		}
		for _, event := range events {
			if !strings.HasSuffix(event.key, "~upload") || stored[event.member] != event.digest {
				t.Errorf("published-member event %+v; want an upload node naming a stored artifact (%v)", event, stored)
			}
		}

		// The release names the opened plan, moves the head it read, states
		// the ancestry of open, and carries one evidence per member.
		if len(run.opens) != 1 || len(run.releases) != 1 {
			t.Fatalf("ledger has %d opens and %d releases; want one each", len(run.opens), len(run.releases))
		}
		open, rel := run.opens[0], run.releases[0]
		if rel.PlanDigest != open.Plan.PlanDigest || open.Plan.SourceRevision != fx.head {
			t.Errorf("release plan %s, open plan %s from %s; want the opened plan of %s", rel.PlanDigest, open.Plan.PlanDigest, open.Plan.SourceRevision, fx.head)
		}
		channels := rel.Request.Channels
		if len(channels) != 1 || channels[0].Name != e2eChannel || channels[0].Expected == nil || *channels[0].Expected != forwardRef {
			t.Errorf("release channels = %+v; want %s expected at %s", channels, e2eChannel, forwardRef.ID)
		}
		wantAncestry := registry.PublicationAncestry{
			SourceRevision: fx.head, SnapshotCommits: 2,
			Channels: []registry.PublicationChannelAncestry{{Name: e2eChannel, HeadSourceRevision: fx.base, Ancestor: true}},
		}
		if !ancestryEqual(open.Ancestry, wantAncestry) || !ancestryEqual(rel.Ancestry, wantAncestry) {
			t.Errorf("open ancestry %+v, release ancestry %+v; want %+v", open.Ancestry, rel.Ancestry, wantAncestry)
		}
		evidence := map[string]string{}
		for _, member := range rel.Evidence.Members {
			evidence[member.Coordinate+"@"+member.Version] = member.Digest
		}
		if len(evidence) != 2 || !maps.Equal(evidence, stored) {
			t.Errorf("release evidence %v; want the stored artifacts %v", evidence, stored)
		}

		// The accepted set is the plan, and the channel moved to it.
		if got, want := setMembers(rel.Request.ReleaseSet), planMembers(open.Plan); !slices.Equal(got, want) {
			t.Errorf("released set %q; want the plan %q", got, want)
		}
		if released := setRef(t, rel.Request.ReleaseSet); run.end.Releases != 1 || run.end.Heads[e2eChannel] != released.ID {
			t.Errorf("provider ended with %d releases and head %q; want one release and head %s", run.end.Releases, run.end.Heads[e2eChannel], released.ID)
		}
	})

	t.Run("not forward", func(t *testing.T) {
		behind := forwardHead(e2eUnheldRevision)
		behindRef := setRef(t, behind)
		run := fx.publish(t, false, providerSetup{Heads: map[string]distribution.ReleaseSet{e2eChannel: behind}})
		if run.code == 0 {
			t.Fatalf("publish over a head that is not forward exit=0, want a failure\n%s", run.output)
		}
		boundedRefusal(t, run.output, registry.RefusalNotForward)
		if len(run.opens) != 1 || len(run.releases) != 0 || slices.Contains(run.order, string(registry.CredentialOpCredential)+" "+registry.PurposePublish) {
			t.Fatalf("order = %q with %d releases; want one refused open, no publish credential and no release", run.order, len(run.releases))
		}
		if channel := run.opens[0].Ancestry.Channels; len(channel) != 1 || channel[0].HeadSourceRevision != e2eUnheldRevision || channel[0].Ancestor {
			t.Errorf("open ancestry %+v; want the unheld head revision, not an ancestor", channel)
		}
		npmStored, npmRefused := fx.npm.stored()
		goStored, goRefused := fx.gomod.stored()
		if len(npmStored) != 0 || len(goStored) != 0 || npmRefused != 0 || goRefused != 0 {
			t.Errorf("the registries received npm %v and go %v (%d and %d refused); want no upload", npmStored, goStored, npmRefused, goRefused)
		}
		if events := publishedMemberEvents(t, fx.wsRoot); len(events) != 0 {
			t.Errorf("published-member events %+v after a refused open", events)
		}
		if run.end.Releases != 0 || run.end.Heads[e2eChannel] != behindRef.ID {
			t.Errorf("provider ended with %d releases and head %q; want the head %s unchanged", run.end.Releases, run.end.Heads[e2eChannel], behindRef.ID)
		}
	})

	t.Run("artifact missing", func(t *testing.T) {
		head := forwardHead(fx.base)
		headRef := setRef(t, head)
		run := fx.publish(t, false, providerSetup{Heads: map[string]distribution.ReleaseSet{e2eChannel: head}, StoresNothing: true})
		if run.code == 0 {
			t.Fatalf("publish whose artifacts the registries do not store exit=0, want a failure\n%s", run.output)
		}
		boundedRefusal(t, run.output, registry.RefusalArtifactMissing)
		if len(run.opens) != 1 || len(run.releases) != 1 {
			t.Errorf("ledger has %d opens and %d releases; want one each, the release refused once", len(run.opens), len(run.releases))
		}
		if run.end.Releases != 0 || run.end.Heads[e2eChannel] != headRef.ID {
			t.Errorf("provider ended with %d releases and head %q; want the head %s unchanged", run.end.Releases, run.end.Heads[e2eChannel], headRef.ID)
		}
	})
}

// A local run and a hosted run of one commit, through the same provider, open
// byte-identical plans and release the same set. Only the hosted run's
// initialize carries the run credential, and neither run's publication jobs
// hold it or the publish credential.
func TestReleaseSetE2ELocalAndHostedRunsOpenTheSamePlan(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "a-local-and-a-hosted-run-open-the-same-plan")
	fx := writePublicationE2EFixture(t)

	runs := map[bool]e2eRun{}
	for _, hosted := range []bool{false, true} {
		run := fx.publish(t, hosted, providerSetup{})
		if run.code != 0 {
			t.Fatalf("publish (hosted=%t) exit=%d, want 0\n%s", hosted, run.code, run.output)
		}
		if len(run.initialize) != 1 || run.initialize[0].RunCredential != hosted {
			t.Errorf("hosted=%t: initialize %+v; want one, carrying the run credential only on the hosted run", hosted, run.initialize)
		}
		if len(run.openRaw) != 1 || len(run.releases) != 1 {
			t.Fatalf("hosted=%t: ledger has %d opens and %d releases; want one each\n%s", hosted, len(run.openRaw), len(run.releases), run.output)
		}
		assertProbesFoundNothing(t, fmt.Sprintf("npm publication (hosted=%t)", hosted), fx.npmProbe)
		assertProbesFoundNothing(t, fmt.Sprintf("go publication (hosted=%t)", hosted), fx.goProbe)
		runs[hosted] = run
	}
	local, hosted := runs[false], runs[true]
	if !bytes.Equal(local.openRaw[0], hosted.openRaw[0]) {
		t.Errorf("open payloads differ:\nlocal  %s\nhosted %s", local.openRaw[0], hosted.openRaw[0])
	}
	if local.opens[0].Plan.PlanDigest != hosted.opens[0].Plan.PlanDigest {
		t.Errorf("plan digests differ: local %s, hosted %s", local.opens[0].Plan.PlanDigest, hosted.opens[0].Plan.PlanDigest)
	}
	localSet, hostedSet := setRef(t, local.releases[0].Request.ReleaseSet), setRef(t, hosted.releases[0].Request.ReleaseSet)
	if localSet != hostedSet || local.end.Heads[e2eChannel] != localSet.ID || hosted.end.Heads[e2eChannel] != hostedSet.ID {
		t.Errorf("local released %s (head %q), hosted %s (head %q); want one set, accepted by both",
			localSet.ID, local.end.Heads[e2eChannel], hostedSet.ID, hosted.end.Heads[e2eChannel])
	}
}

// indexesOf returns the positions in lines of every line equal to one of
// values.
func indexesOf(lines []string, values ...string) []int {
	var indexes []int
	for i, line := range lines {
		if slices.Contains(values, line) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func ancestryEqual(a, b registry.PublicationAncestry) bool {
	return a.SourceRevision == b.SourceRevision && a.SnapshotCommits == b.SnapshotCommits && slices.Equal(a.Channels, b.Channels)
}
