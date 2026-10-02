package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registryproto "go.putnami.dev/protocol/registry"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// publicationBearer is the publish credential the test provider issues.
const publicationBearer = "publication-bearer-fixture"

// sessionPublication is the publication surface of one providertest session,
// as the credential provider's broker serves it: every request is checked
// with the protocol's own parser first, a refusal is an error that names its
// code, and the publish bearer goes only to a host the credential serves.
type sessionPublication struct {
	session *providertest.Session

	mu       sync.Mutex
	ops      []string
	releases []registryproto.ReleaseParams
	// onOpen runs when open is asked, before the provider answers.
	onOpen func()
}

func newSessionPublication(t *testing.T, config providertest.Config) (*providertest.Provider, *sessionPublication) {
	t.Helper()
	provider := providertest.New(config)
	session := provider.NewSession()
	session.Initialize([]string{registryproto.CapabilityCredentialV1, registryproto.CapabilityPublicationV1})
	if !session.Publication() {
		t.Fatal("the test provider did not negotiate publication-v1")
	}
	return provider, &sessionPublication{session: session}
}

func (p *sessionPublication) record(op string) {
	p.mu.Lock()
	p.ops = append(p.ops, op)
	p.mu.Unlock()
}

// recorded returns the ops asked so far, in order.
func (p *sessionPublication) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ops)
}

func refusalError(op string, refusal *registryproto.CredentialRefusal) error {
	return fmt.Errorf("the credential provider refused %s (%s): %s", op, refusal.Code, refusal.Message)
}

func (p *sessionPublication) Resolve(_ context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	p.record("resolve")
	result, refusal := p.session.Resolve(*request)
	if refusal != nil {
		return nil, refusalError("resolve", refusal)
	}
	return &result.Response, nil
}

func (p *sessionPublication) Open(_ context.Context, params *registryproto.OpenParams) error {
	p.record("open")
	if p.onOpen != nil {
		p.onOpen()
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	parsed, err := registryproto.ParseOpenParams(encoded)
	if err != nil {
		return fmt.Errorf("the engine sent an invalid open: %w", err)
	}
	if _, refusal := p.session.Open(*parsed); refusal != nil {
		return refusalError("open", refusal)
	}
	return nil
}

func (p *sessionPublication) Release(_ context.Context, params *registryproto.ReleaseParams) (*distribution.ReleaseResponse, error) {
	p.record("release")
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	parsed, err := registryproto.ParseReleaseParams(encoded)
	if err != nil {
		return nil, fmt.Errorf("the engine sent an invalid release: %w", err)
	}
	p.mu.Lock()
	p.releases = append(p.releases, *parsed)
	p.mu.Unlock()
	result, refusal := p.session.Release(*parsed)
	if refusal != nil {
		return nil, refusalError("release", refusal)
	}
	return &result.Response, nil
}

func (p *sessionPublication) PublishBearer(_ context.Context, target *url.URL) (string, error) {
	p.record("credential")
	result, refusal := p.session.Credential(registryproto.PurposePublish)
	if refusal != nil {
		return "", refusalError("credential", refusal)
	}
	if result.Credential == nil || !result.Credential.Serves(target) {
		return "", fmt.Errorf("the credential provider holds no publish credential for %s", target.Host)
	}
	return result.Credential.Bearer, nil
}

// fixedAncestry is an ancestry snapshot holding the commits it lists, the
// bound commit first.
type fixedAncestry []string

func (a fixedAncestry) SourceRevision() string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}
func (a fixedAncestry) Commits() int  { return len(a) }
func (a fixedAncestry) Shallow() bool { return false }
func (a fixedAncestry) Err() error    { return nil }
func (a fixedAncestry) Position(rev string) (int, bool) {
	index := slices.Index(a, rev)
	return index, index >= 0
}

// publicationHarness is a workspace whose projects publish one managed member
// each through a fixture publication job, planned as a publication-v1 run over
// a providertest session.
type publicationHarness struct {
	t         *testing.T
	root      string
	provider  *providertest.Provider
	session   *sessionPublication
	publisher *modelextension.ExtensionDescription
	projects  []*workspace.Project
	members   []releaseset.PlannedMember
	routes    map[string]releaseMemberRoute
	jobs      []*ScheduledJob
	publish   map[string]*ScheduledJob
	run       *ReleaseSetRun
	// cache serves and stores reusable results when set; without it every
	// job executes.
	cache *store.CacheManager
	// renderer records what the last execute rendered.
	renderer *mockRenderer
}

func newPublicationHarness(t *testing.T, config providertest.Config) *publicationHarness {
	t.Helper()
	provider, session := newSessionPublication(t, config)
	root := t.TempDir()
	return &publicationHarness{
		t: t, root: root, provider: provider, session: session,
		publisher: &modelextension.ExtensionDescription{Name: "@test/publisher", Path: root},
		routes:    map[string]releaseMemberRoute{},
		publish:   map[string]*ScheduledJob{},
	}
}

// addMember adds a project publishing ecosystem/coordinate@version, with its
// package job and a publication job that runs script.
func (h *publicationHarness) addMember(ecosystem, coordinate, version, path string, registries map[string]string, script fixtureScript) *ScheduledJob {
	h.t.Helper()
	project := &workspace.Project{
		ID: "/" + path, Name: path, Path: path, Version: version, Type: "library",
		Registries: map[string]json.RawMessage{},
	}
	for name, entry := range registries {
		project.Registries[name] = json.RawMessage(entry)
	}
	if err := os.MkdirAll(filepath.Join(h.root, path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.projects = append(h.projects, project)
	member := releaseset.PlannedMember{
		Ecosystem: distribution.Ecosystem(ecosystem), Coordinate: coordinate, Version: version,
		Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision,
		SelectionFingerprint: testFingerprint(project.ID), Selected: true, ProjectID: project.ID,
	}
	h.members = append(h.members, member)
	h.routes[releaseset.MemberKey(member.Ecosystem, coordinate)] = releaseMemberRoute{
		projectID: project.ID, publisher: h.publisher.Name, packagePublisher: h.publisher.Name,
		packageStep: ecosystem, publishCommand: "publish", publishStep: ecosystem,
	}
	packageJob := &ScheduledJob{Project: project, Extension: h.publisher, JobDef: &modelextension.JobDefinition{
		Name: "package~" + ecosystem, CommandName: "package", StepID: ecosystem, ExtensionName: h.publisher.Name,
	}}
	fixtureTask(h.t, packageJob.JobDef, fixtureScript{{"print", releaseSetSuccessLine}})
	publishJob := &ScheduledJob{Project: project, Extension: h.publisher, JobDef: &modelextension.JobDefinition{
		Name: "publish~" + ecosystem, CommandName: "publish", StepID: ecosystem, ExtensionName: h.publisher.Name,
	}, DependsOn: []string{packageJob.Key()}}
	fixtureTask(h.t, publishJob.JobDef, script.then(fixtureScript{{"print", releaseSetSuccessLine}}))
	h.jobs = append(h.jobs, packageJob, publishJob)
	h.publish[ecosystem] = publishJob
	return publishJob
}

// addGate adds a test job to the project at path that writes marker.
func (h *publicationHarness) addGate(path, marker string) *ScheduledJob {
	h.t.Helper()
	index := slices.IndexFunc(h.projects, func(project *workspace.Project) bool { return project.Path == path })
	if index < 0 {
		h.t.Fatalf("no project at %s", path)
	}
	gate := &ScheduledJob{Project: h.projects[index], Extension: h.publisher, JobDef: &modelextension.JobDefinition{
		Name: "test~unit", CommandName: "test", StepID: "unit", ExtensionName: h.publisher.Name,
	}}
	fixtureTask(h.t, gate.JobDef, fixtureScript{{"write", "passed", marker}, {"print", releaseSetSuccessLine}})
	h.jobs = append(h.jobs, gate)
	return gate
}

// plan builds the run over the members added, the plan nodes, and their
// in-process runners, as the engine does: plan, bind, attach.
func (h *publicationHarness) plan(heads map[string]*distribution.ChannelHead, ancestry AncestryReader, barrier []string) ([]*ScheduledJob, map[string]InternalJobRunner) {
	h.t.Helper()
	if heads == nil {
		heads = map[string]*distribution.ChannelHead{"canary": nil}
	}
	members := slices.Clone(h.members)
	slices.SortFunc(members, func(a, b releaseset.PlannedMember) int {
		return strings.Compare(releaseset.MemberKey(a.Ecosystem, a.Coordinate), releaseset.MemberKey(b.Ecosystem, b.Coordinate))
	})
	plan := &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami",
		Channels: []string{"canary"}, Heads: heads, Members: members,
	}
	if err := releaseset.ValidatePlan(plan); err != nil {
		h.t.Fatalf("plan: %v", err)
	}
	h.run = &ReleaseSetRun{
		provider: sessionResolver{h.session}, publication: h.session, plan: plan, routes: h.routes,
		sourceRevision: testRevision, root: h.root,
	}
	planned, err := h.run.AttachPlan(slices.Clone(h.jobs))
	if err != nil {
		h.t.Fatalf("attach plan: %v", err)
	}
	if err := h.run.BindPublication(ancestry, barrier); err != nil {
		h.t.Fatalf("bind publication: %v", err)
	}
	planned, internal, err := h.run.AttachBarrier(planned)
	if err != nil {
		h.t.Fatalf("attach publication: %v", err)
	}
	return planned, internal
}

// execute runs the plan in ctx, then the session finalizer, and returns every
// result.
func (h *publicationHarness) execute(ctx context.Context, planned []*ScheduledJob, internal map[string]InternalJobRunner, authorization *ProcessCapabilityAuthorization) map[string]*JobResult {
	h.t.Helper()
	ctx, closeOutboxes, err := h.run.PublicationContext(ctx)
	if err != nil {
		h.t.Fatalf("publication context: %v", err)
	}
	defer closeOutboxes()
	h.renderer = &mockRenderer{}
	result := RunPlan(ctx, RunRequest{
		Workspace: workspace.NewWorkspace(h.root, &wsproto.Config{Name: "putnami"}, h.projects),
		Plan:      planned, InternalJobs: internal, ProcessCapabilityAuthorization: authorization,
		Config:   SchedulerConfig{MaxParallel: 4, NoCache: h.cache == nil, ContinueOnError: true},
		Renderer: h.renderer, Cache: h.cache,
	})
	if finalize := h.run.Finalizer(ctx); finalize != nil {
		finalize(result.Results)
	}
	return result.Results
}

// uploadKey is the key of the upload node of the publication job publish.
func (h *publicationHarness) uploadKey(publish *ScheduledJob) string {
	h.t.Helper()
	for upload, job := range h.run.uploads {
		if job == publish.Key() {
			return upload
		}
	}
	h.t.Fatalf("%s has no upload node", publish.Key())
	return ""
}

// stage writes one outbox holding members under a new directory and returns
// it.
func stageOutbox(t *testing.T, write func(*publicationoutbox.Writer) []extensionproto.OutboxMember) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outbox")
	writer, err := publicationoutbox.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range write(writer) {
		if err := writer.Add(member); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func npmOutboxMember(t *testing.T, writer *publicationoutbox.Writer, project, name, version string) extensionproto.OutboxMember {
	t.Helper()
	tarball, err := writer.WriteFile("npm/"+strings.ReplaceAll(name, "/", "_")+".tgz", []byte("tarball of "+name+"@"+version))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := writer.WriteFile("npm/"+strings.ReplaceAll(name, "/", "_")+".json", []byte(fmt.Sprintf(`{"name":%q,"version":%q}`, name, version)))
	if err != nil {
		t.Fatal(err)
	}
	return extensionproto.OutboxMember{
		Ecosystem: extensionproto.OutboxEcosystemNPM, Coordinate: name, Version: version, Project: project,
		NPM: &extensionproto.OutboxNPM{Tarball: tarball, Manifest: manifest},
	}
}

// npmRegistry is an npm registry that stores what it is sent with the
// publication bearer.
type npmRegistry struct {
	server *httptest.Server

	mu       sync.Mutex
	tarballs map[string]npmTarball
	requests int
}

type npmTarball struct {
	name, version string
	data          []byte
}

func newNPMRegistry(t *testing.T) *npmRegistry {
	t.Helper()
	reg := &npmRegistry{tarballs: map[string]npmTarball{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *npmRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.requests++
	if r.Header.Get("Authorization") != "Bearer "+publicationBearer {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var payload npmpublish.Payload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for version := range payload.Versions {
			for _, attachment := range payload.Attachments {
				data, err := base64.StdEncoding.DecodeString(attachment.Data)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				reg.tarballs[payload.Name+"@"+version] = npmTarball{name: payload.Name, version: version, data: data}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		for _, tarball := range reg.tarballs {
			target, _ := npmpublish.TarballURL(reg.server.URL, tarball.name, tarball.version)
			if strings.TrimPrefix(target, reg.server.URL) == r.URL.EscapedPath() {
				_, _ = w.Write(tarball.data)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (reg *npmRegistry) stored(name, version string) ([]byte, int) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.tarballs[name+"@"+version].data, reg.requests
}

// goRegistry is a Go module registry that stores what it is sent with the
// publication bearer.
type goRegistry struct {
	server *httptest.Server

	mu    sync.Mutex
	blobs map[string][]byte
	zips  map[string][]byte
	mods  map[string][]byte
}

func newGoRegistry(t *testing.T) *goRegistry {
	t.Helper()
	reg := &goRegistry{blobs: map[string][]byte{}, zips: map[string][]byte{}, mods: map[string][]byte{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.server.Close)
	return reg
}

func (reg *goRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+publicationBearer {
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
		reg.answer(w, reg.zips[strings.TrimSuffix(path, ".zip")])
	case r.Method == http.MethodGet && strings.HasSuffix(path, ".mod"):
		reg.answer(w, reg.mods[strings.TrimSuffix(path, ".mod")])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (reg *goRegistry) answer(w http.ResponseWriter, data []byte) {
	if data == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(data)
}

func serverHost(t *testing.T, server *httptest.Server) string {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return target.Host
}

func registriesEntry(field, endpoint string) string {
	return fmt.Sprintf(`{%q:%q}`, field, endpoint)
}

func uploadEvents(t *testing.T, result *JobResult) []*extensionproto.PublishedMember {
	t.Helper()
	var members []*extensionproto.PublishedMember
	if result == nil {
		return nil
	}
	for _, event := range result.Events {
		if kind, _ := event.Data["kind"].(string); event.Type != EventTypeArtifact || kind != extensionproto.PublishedMemberEventKind {
			continue
		}
		member, err := parsePublishedMemberEvent(event.Data)
		if err != nil {
			t.Fatalf("published-member event: %v", err)
		}
		members = append(members, member)
	}
	return members
}

func resultMessage(result *JobResult) string {
	if result == nil {
		return "<no result>"
	}
	if result.Error == nil {
		return result.Status
	}
	return result.Status + ": " + result.Error.Message
}

// The open node waits for every task that neither publishes nor depends on a
// publication, or, for a bound request, for every task of its barrier
// commands. It is sent once, with the plan the release-plan contract states,
// and before any publish credential is asked.
func TestSessionOpenIsSentOnceAfterEveryBarrierLeaf(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "open-follows-every-barrier-leaf-once")
	for name, barrier := range map[string][]string{"local run": nil, "bound request": {"test"}} {
		t.Run(name, func(t *testing.T) {
			npm := newNPMRegistry(t)
			h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)}})
			outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
				return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
			})
			h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)},
				fixtureScript{{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"}})
			markers := []string{filepath.Join(h.root, "first-gate"), filepath.Join(h.root, "second-gate")}
			first := h.addGate("web", markers[0])
			second := &ScheduledJob{Project: first.Project, Extension: h.publisher, JobDef: &modelextension.JobDefinition{
				Name: "test~integration", CommandName: "test", StepID: "integration", ExtensionName: h.publisher.Name,
			}, DependsOn: []string{first.Key()}}
			fixtureTask(t, second.JobDef, fixtureScript{{"write", "passed", markers[1]}, {"print", releaseSetSuccessLine}})
			h.jobs = append(h.jobs, second)
			var openedAfter []bool
			h.session.onOpen = func() {
				for _, marker := range markers {
					_, err := os.Stat(marker)
					openedAfter = append(openedAfter, err == nil)
				}
			}

			planned, internal := h.plan(nil, fixedAncestry{testRevision}, barrier)
			open := jobsByPlanKey(planned)[publicationOpenKey]
			if open == nil || !slices.Contains(open.DependsOn, first.Key()) || !slices.Contains(open.DependsOn, second.Key()) {
				t.Fatalf("open waits for %v, want both gate tasks", open.DependsOn)
			}
			if slices.Contains(open.DependsOn, h.publish["npm"].Key()) {
				t.Fatalf("open waits for the publication job %s", h.publish["npm"].Key())
			}
			handoff, err := h.run.capabilityPlan()
			if err != nil || h.run.open.Plan.PlanDigest != handoff.PlanDigest {
				t.Fatalf("open plan digest %s, release-plan contract %s (%v)", h.run.open.Plan.PlanDigest, handoff.PlanDigest, err)
			}

			results := h.execute(context.Background(), planned, internal, nil)
			if outcome := results[releaseSetResultKey]; outcome == nil || outcome.Status != "success" {
				t.Fatalf("release: %s", resultMessage(outcome))
			}
			ops := h.session.recorded()
			if count := len(slices.DeleteFunc(slices.Clone(ops), func(op string) bool { return op != "open" })); count != 1 {
				t.Fatalf("ops %v, want exactly one open", ops)
			}
			if !slices.Equal(openedAfter, []bool{true, true}) {
				t.Fatalf("open was sent before every gate finished: %v", openedAfter)
			}
			if index := slices.Index(ops, "open"); index < 0 || slices.Index(ops, "credential") < index {
				t.Fatalf("ops %v: a publish credential was asked before open", ops)
			}
			if opens := h.provider.Opens(); len(opens) != 1 || opens[0].Plan.PlanDigest != h.run.open.Plan.PlanDigest {
				t.Fatalf("the provider opened %d plans", len(opens))
			}
		})
	}
}

// A publication job of a publication-v1 run receives a private outbox and no
// credential: not the captured cloud token, not the job credential
// descriptor, not a registry route, and not the publish bearer the engine
// uploads with.
func TestPublishJobReceivesNoCredentialUnderPublicationV1(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-publication-job-holds-no-credential")
	const token = "captured-cloud-token-fixture"
	t.Setenv("PUTNAMI_REGISTRY_NPM_URL", "http://127.0.0.1:1/npm")
	t.Setenv("PUTNAMI_REGISTRY_FUTUREKIND_URL", "http://127.0.0.1:1/futurekind")
	t.Setenv(extensionproto.PublicationOutboxEnv, filepath.Join(t.TempDir(), "inherited-outbox"))
	ctx := capturedCapabilityContext(t, token, "test")
	npm := newNPMRegistry(t)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)}})
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
	})
	environ := filepath.Join(h.root, "publication-environ")
	mode := filepath.Join(h.root, "publication-outbox-mode")
	h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)}, fixtureScript{
		{"write-environ", environ},
		{"write-mode", extensionproto.PublicationOutboxEnv, mode},
		{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"},
	})
	h.addGate("web", filepath.Join(h.root, "gate"))
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	if err := PlanProcessCapabilityGates(ctx, planned); err != nil {
		t.Fatal(err)
	}
	authorization, err := AuthorizeProcessCapabilities(ctx, planned, true)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	results := h.execute(ctx, planned, internal, authorization)
	if outcome := results[releaseSetResultKey]; outcome == nil || outcome.Status != "success" {
		t.Fatalf("release: %s; publish %s; upload %s", resultMessage(outcome),
			resultMessage(results[h.publish["npm"].Key()]), resultMessage(results[h.uploadKey(h.publish["npm"])]))
	}
	dump, err := os.ReadFile(environ)
	if err != nil {
		t.Fatal(err)
	}
	outboxes := 0
	for _, line := range strings.Split(string(dump), "\n") {
		name, value, _ := strings.Cut(line, "=")
		if withheldFromOutboxJob(name) {
			t.Errorf("the publication job received %s", name)
		}
		if strings.Contains(value, token) || strings.Contains(value, publicationBearer) {
			t.Errorf("the publication job's %s carries a credential", name)
		}
		if name == extensionproto.PublicationOutboxEnv {
			outboxes++
			if strings.HasSuffix(value, "inherited-outbox") {
				t.Errorf("the publication job received the inherited %s", name)
			}
		}
	}
	if outboxes != 1 {
		t.Fatalf("the publication job received %d %s entries, want 1", outboxes, extensionproto.PublicationOutboxEnv)
	}
	if got, err := os.ReadFile(mode); err != nil || string(got) != "0700" {
		t.Fatalf("the outbox mode is %q (%v), want 0700", got, err)
	}
	if tarball, _ := npm.stored("@putnami/web", "1.2.3"); string(tarball) != "tarball of @putnami/web@1.2.3" {
		t.Fatalf("the engine uploaded %q", tarball)
	}
}

// A job that packs into an outbox is denied the loopback registry route of
// every kind, including a kind no publisher reads yet, and every listed
// credential variable. The download mirror and ordinary variables stay.
func TestOutboxJobIsDeniedEveryRegistryRoute(t *testing.T) {
	for _, name := range []string{
		"PUTNAMI_REGISTRY_NPM_URL",
		"PUTNAMI_REGISTRY_GOMOD_URL",
		"PUTNAMI_REGISTRY_OCI_URL",
		"PUTNAMI_REGISTRY_PUT_URL",
		"PUTNAMI_REGISTRY_FUTUREKIND_URL",
		extensionproto.CloudTokenEnv,
		extensionproto.JobCredentialFDEnv,
		InternalReleasePlanCallbackEnv,
	} {
		if !withheldFromOutboxJob(name) {
			t.Errorf("%s reaches a job that packs into an outbox", name)
		}
	}
	for _, name := range []string{"PUTNAMI_REGISTRY_URL", "PUTNAMI_REGISTRY_NPM", "PATH", "HOME"} {
		if withheldFromOutboxJob(name) {
			t.Errorf("%s is withheld from a job that packs into an outbox", name)
		}
	}
}

// The engine uploads every packed member with the provider's publish
// credential, records the published image beside the publication job's
// output, reports one published-member event per upload, and releases the
// uploaded digests with their routes as evidence.
func TestEngineUploadsManagedMembersAndEmitsPublishedMembers(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "each-packed-member-is-uploaded-by-the-engine")
	npm := newNPMRegistry(t)
	gomod := newGoRegistry(t)
	ociServer := httptest.NewServer(registry.New())
	t.Cleanup(ociServer.Close)
	ociHost := serverHost(t, ociServer)
	hosts := []string{serverHost(t, npm.server), serverHost(t, gomod.server), ociHost}
	slices.Sort(hosts)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: slices.Compact(hosts)})

	npmOutbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
	})
	zip := []byte("module zip of example.test/mod@v1.2.3")
	goMod := []byte("module example.test/mod\n")
	goOutbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		zipFile, err := w.WriteFile("go/mod.zip", zip)
		if err != nil {
			t.Fatal(err)
		}
		modFile, err := w.WriteFile("go/go.mod", goMod)
		if err != nil {
			t.Fatal(err)
		}
		infoFile, err := w.WriteFile("go/mod.info", []byte(`{"Version":"v1.2.3"}`))
		if err != nil {
			t.Fatal(err)
		}
		return []extensionproto.OutboxMember{{
			Ecosystem: extensionproto.OutboxEcosystemGo, Coordinate: "example.test/mod", Version: "v1.2.3", Project: "/mod",
			Go: &extensionproto.OutboxGo{Zip: zipFile, Mod: modFile, Info: infoFile},
		}}
	})
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "layout")
	imageDigest, err := oci.WriteLayout(layout, image)
	if err != nil {
		t.Fatal(err)
	}
	ociOutbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		if err := w.CopyLayout("oci/layout", layout); err != nil {
			t.Fatal(err)
		}
		return []extensionproto.OutboxMember{{
			Ecosystem: extensionproto.OutboxEcosystemOCI, Coordinate: "team/app", Version: "1.2.3", Project: "/app",
			OCI: &extensionproto.OutboxOCI{Layout: "oci/layout", Repository: ociHost + "/team/app", Digest: imageDigest, Tags: []string{"1.2.3"}},
		}}
	})
	copyOutbox := func(dir string) fixtureScript {
		return fixtureScript{{"copy-tree", dir, "$PUTNAMI_PUBLICATION_OUTBOX"}}
	}
	npmJob := h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)}, copyOutbox(npmOutbox))
	goJob := h.addMember("go", "example.test/mod", "v1.2.3", "mod", map[string]string{"go": registriesEntry("origin", gomod.server.URL)}, copyOutbox(goOutbox))
	ociJob := h.addMember("oci", "team/app", "1.2.3", "app", map[string]string{"oci": registriesEntry("publish", ociHost)}, copyOutbox(ociOutbox))
	candidate := pkgmeta.DockerManifest{Image: ociHost + "/team/app", Version: "1.2.3", ContentHash: "content-hash", Digest: imageDigest, Layout: "layout"}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(pkgmeta.PackageOutputDir(h.root, "app", "docker"), "manifest.json")
	if err := os.MkdirAll(filepath.Dir(candidatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidatePath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)
	outcome := results[releaseSetResultKey]
	if outcome == nil || outcome.Status != "success" {
		t.Fatalf("release: %s; uploads %s, %s, %s", resultMessage(outcome), resultMessage(results[h.uploadKey(npmJob)]),
			resultMessage(results[h.uploadKey(goJob)]), resultMessage(results[h.uploadKey(ociJob)]))
	}
	tarball, _ := npm.stored("@putnami/web", "1.2.3")
	want := map[string]string{
		"npm\x00@putnami/web":    npmpublish.Digest(tarball),
		"go\x00example.test/mod": fmt.Sprintf("sha256:%x", sha256.Sum256(zip)),
		"oci\x00team/app":        imageDigest,
	}
	for _, job := range []*ScheduledJob{npmJob, goJob, ociJob} {
		if events := uploadEvents(t, results[job.Key()]); len(events) != 0 {
			t.Fatalf("the publication job %s reported %d published members", job.Key(), len(events))
		}
		events := uploadEvents(t, results[h.uploadKey(job)])
		if len(events) != 1 || events[0].ArtifactDigest != want[events[0].Ecosystem+"\x00"+events[0].Coordinate] {
			t.Fatalf("upload of %s reported %+v", job.Key(), events)
		}
		rendered := uploadEvents(t, &JobResult{Events: h.renderer.eventsOfType(h.uploadKey(job), EventTypeArtifact)})
		if len(rendered) != 1 || !reflect.DeepEqual(rendered[0], events[0]) {
			t.Fatalf("upload of %s rendered %+v; want its published member %+v", job.Key(), rendered, events[0])
		}
	}
	head, moved := h.provider.Head("putnami", "canary")
	set, stored := h.provider.ReleaseSet(head)
	if !moved || !stored || len(set.Members) != 3 {
		t.Fatalf("released head %v (%t), set %+v", head, moved, set)
	}
	for _, member := range set.Members {
		if member.ArtifactDigest != want[string(member.Ecosystem)+"\x00"+member.Coordinate] {
			t.Fatalf("released %s/%s at %s", member.Ecosystem, member.Coordinate, member.ArtifactDigest)
		}
	}
	published, err := pkgmeta.ReadPublishedImageManifest(filepath.Join(h.root, ".putnami", "out", "app", "publish"))
	if err != nil || published.Digest != imageDigest || !published.Verified || published.ImmutableRef != ociHost+"/team/app@"+imageDigest {
		t.Fatalf("published image %+v, %v", published, err)
	}
	releases := h.session.releases
	if len(releases) != 1 || len(releases[0].Evidence.Members) != 3 || len(releases[0].Evidence.Images) != 1 ||
		releases[0].Evidence.Images[0].Project != "app" || releases[0].Evidence.Images[0].Digest != imageDigest ||
		releases[0].PlanDigest != h.run.open.Plan.PlanDigest {
		t.Fatalf("release evidence %+v", releases)
	}
}

// An artifact changed after the descriptor named its digest is refused before
// any credential is asked or any byte reaches the registry.
func TestTamperedOutboxArtifactIsRefusedByDigest(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-tampered-artifact-is-refused-by-digest")
	npm := newNPMRegistry(t)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)}})
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
	})
	tampered := t.TempDir()
	original, err := os.ReadFile(filepath.Join(outbox, "npm", "@putnami_web.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tampered, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tampered, "npm", "@putnami_web.tgz"), []byte(strings.ToUpper(string(original))), 0o644); err != nil {
		t.Fatal(err)
	}
	job := h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)}, fixtureScript{
		{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"},
		{"copy-tree", tampered, "$PUTNAMI_PUBLICATION_OUTBOX"},
	})
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)

	upload := results[h.uploadKey(job)]
	if upload == nil || upload.Status != "failed" || upload.Error == nil || !strings.Contains(upload.Error.Message, "digest mismatch") {
		t.Fatalf("upload of a tampered artifact: %s", resultMessage(upload))
	}
	if _, requests := npm.stored("@putnami/web", "1.2.3"); requests != 0 {
		t.Fatalf("the registry received %d requests", requests)
	}
	if slices.Contains(h.session.recorded(), "credential") || slices.Contains(h.session.recorded(), "release") {
		t.Fatalf("ops %v after a tampered artifact", h.session.recorded())
	}
	if _, moved := h.provider.Head("putnami", "canary"); moved {
		t.Fatal("a tampered artifact moved the channel")
	}
}

// Every packed member is checked against the plan before any upload: a member
// the plan does not select is refused, and a job that packed nothing
// published on its own route.
func TestPackedMembersAreCheckedAgainstThePlan(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-member-the-plan-does-not-select-is-refused")
	t.Run("a member the plan does not select", func(t *testing.T) {
		npm := newNPMRegistry(t)
		h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)}})
		outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
			return []extensionproto.OutboxMember{
				npmOutboxMember(t, w, "/web", "@putnami/other", "1.2.3"),
				npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3"),
			}
		})
		job := h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)},
			fixtureScript{{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"}})
		planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
		results := h.execute(context.Background(), planned, internal, nil)
		upload := results[h.uploadKey(job)]
		if upload == nil || upload.Status != "failed" || upload.Error == nil || !strings.Contains(upload.Error.Message, "which the plan does not select") {
			t.Fatalf("upload of an unselected member: %s", resultMessage(upload))
		}
		if _, requests := npm.stored("@putnami/web", "1.2.3"); requests != 0 {
			t.Fatalf("the registry received %d requests", requests)
		}
	})
	t.Run("a job that packed nothing", func(t *testing.T) {
		h := newPublicationHarness(t, providertest.Config{})
		h.addMember("npm", "@putnami/web", "1.2.3", "web", nil, fixtureScript{})
		planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
		publish := h.publish["npm"]
		publish.JobDef.Env = nil
		member := h.run.plan.SelectedMembers()[0]
		fixtureTask(t, publish.JobDef, fixtureScript{
			{"print", publishedMemberRuntimeLine(member, digestFor('e'))},
			{"print", releaseSetSuccessLine},
		})
		results := h.execute(context.Background(), planned, internal, nil)
		if upload := results[h.uploadKey(publish)]; upload == nil || upload.Status != "success" || len(upload.Events) != 0 {
			t.Fatalf("upload of an empty outbox: %s", resultMessage(upload))
		}
		if outcome := results[releaseSetResultKey]; outcome == nil || outcome.Status != "success" {
			t.Fatalf("release: %s", resultMessage(outcome))
		}
	})
}

// A channel head the ancestry snapshot does not hold is stated as not an
// ancestor, and the provider refuses the open: nothing is uploaded and no
// channel moves.
func TestNotForwardHeadIsRefusedAtOpen(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "a-head-that-is-not-forward-is-refused-at-open")
	const elsewhere = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	npm := newNPMRegistry(t)
	h := newPublicationHarness(t, providertest.Config{Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)}})
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
	})
	job := h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)},
		fixtureScript{{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"}})
	set := distribution.ReleaseSet{ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Members: []distribution.ReleaseSetMember{{
		Ecosystem: "npm", Coordinate: "@putnami/web", Version: "1.2.2", ArtifactDigest: digestFor('a'),
		Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: elsewhere, SelectionFingerprint: digestFor('b'),
	}}}
	ref, err := h.provider.SetHead("putnami", "canary", set)
	if err != nil {
		t.Fatal(err)
	}
	head := &distribution.ChannelHead{Ref: ref, Generation: 1, ReleaseSet: distribution.NormalizeReleaseSet(&set)}
	planned, internal := h.plan(map[string]*distribution.ChannelHead{"canary": head}, fixedAncestry{testRevision}, nil)
	if entry := h.run.open.Ancestry.Channels[0]; entry.HeadSourceRevision != elsewhere || entry.Ancestor {
		t.Fatalf("ancestry statement %+v, want %s stated as no ancestor", entry, elsewhere)
	}
	results := h.execute(context.Background(), planned, internal, nil)

	open := results[publicationOpenKey]
	if open == nil || open.Status != "failed" || open.Error == nil || !strings.Contains(open.Error.Message, registryproto.RefusalNotForward) {
		t.Fatalf("open: %s", resultMessage(open))
	}
	if upload := results[h.uploadKey(job)]; upload == nil || upload.Status == "success" {
		t.Fatalf("upload after a refused open: %s", resultMessage(upload))
	}
	if ops := h.session.recorded(); slices.Contains(ops, "credential") || slices.Contains(ops, "release") {
		t.Fatalf("ops %v after a refused open", ops)
	}
	if current, _ := h.provider.Head("putnami", "canary"); current != ref || h.provider.Releases() != 0 {
		t.Fatal("a refused open moved the channel")
	}
	if _, requests := npm.stored("@putnami/web", "1.2.3"); requests != 0 {
		t.Fatalf("the registry received %d requests", requests)
	}
}

// A release the provider refuses fails the run with a bounded error that
// names the code, and no channel moves.
func TestReleaseNamingAMemberWithNoArtifactIsRefusedWithABoundedError(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "a-refused-release-moves-no-channel")
	npm := newNPMRegistry(t)
	h := newPublicationHarness(t, providertest.Config{
		Bearer: publicationBearer, Hosts: []string{serverHost(t, npm.server)},
		Stored: func(string, string, string) (string, bool) { return "", false },
	})
	outbox := stageOutbox(t, func(w *publicationoutbox.Writer) []extensionproto.OutboxMember {
		return []extensionproto.OutboxMember{npmOutboxMember(t, w, "/web", "@putnami/web", "1.2.3")}
	})
	h.addMember("npm", "@putnami/web", "1.2.3", "web", map[string]string{"npm": registriesEntry("publish", npm.server.URL)},
		fixtureScript{{"copy-tree", outbox, "$PUTNAMI_PUBLICATION_OUTBOX"}})
	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	results := h.execute(context.Background(), planned, internal, nil)

	outcome := results[releaseSetResultKey]
	if outcome == nil || outcome.Status != "failed" || outcome.Error == nil ||
		!strings.Contains(outcome.Error.Message, registryproto.RefusalArtifactMissing) || len(outcome.Error.Message) > 1024 {
		t.Fatalf("release of a member with no artifact: %s", resultMessage(outcome))
	}
	if _, moved := h.provider.Head("putnami", "canary"); moved || h.provider.Releases() != 0 {
		t.Fatal("a refused release moved the channel")
	}
}

// A run whose provider negotiated publication-v1 resolves through that
// session: no provider process starts, and a provider failure fails the
// publish instead of falling back to one.
func TestResolveRunsThroughTheSessionNotTheNestedProvider(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "resolve-runs-through-the-session")
	ctx := context.Background()
	useReleaseSetProvenance(t, nil)
	nested := 0
	previous := newReleaseSetProvider
	newReleaseSetProvider = func(context.Context, *extension.ResolvedProvider) (releaseSetProvider, error) {
		nested++
		return nil, fmt.Errorf("no provider process may start")
	}
	t.Cleanup(func() { newReleaseSetProvider = previous })
	_, session := newSessionPublication(t, providertest.Config{})
	restore := InstallPublicationProvider(func(context.Context) (PublicationProvider, error) { return session, nil })
	t.Cleanup(restore)

	ws, upstream, downstream := releaseSetE2EWorkspace(t, "1.0.0")
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), ws, []*workspace.Project{upstream, downstream})
	if !run.Publication() || nested != 0 {
		t.Fatalf("publication %t, nested provider started %d times", run.Publication(), nested)
	}
	if ops := session.recorded(); !slices.Equal(ops, []string{"resolve"}) {
		t.Fatalf("ops %v, want one resolve through the session", ops)
	}

	failing := InstallPublicationProvider(func(context.Context) (PublicationProvider, error) {
		return nil, fmt.Errorf("the credential provider failed to start")
	})
	defer failing()
	if _, err := PrepareReleaseSet(ctx, releaseSetE2EAllOptions(), ws, []*workspace.Project{upstream, downstream},
		releaseSetE2EDiscovery(), releaseSetTestFingerprints(t, ws)); err == nil || nested != 0 {
		t.Fatalf("a failed provider: %v, nested provider started %d times", err, nested)
	}
}

// Without a publication-v1 echo the run publishes as before: the provider
// process resolves and releases, the publication job uploads itself, and the
// plan holds no open and no upload node.
func TestLegacyPathIsUnchangedWhenTheCapabilityIsNotEchoed(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "open-credential-release-through-one-provider", "without-the-echo-the-legacy-path-runs")
	ctx := context.Background()
	useReleaseSetProvenance(t, nil)
	ledger := newReleaseSetE2ELedger()
	factoryCalls := useReleaseSetE2EProvider(t, ledger)
	restore := InstallPublicationProvider(func(context.Context) (PublicationProvider, error) { return nil, nil })
	t.Cleanup(restore)

	root := t.TempDir()
	project := &workspace.Project{
		ID: "/app", Name: "@putnami/app", Path: "app", Version: "1.0.0", Type: "library", Publish: []string{"npm"},
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/app"),
	}
	if err := os.MkdirAll(filepath.Join(root, project.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	run := releaseSetE2EPrepare(t, ctx, releaseSetE2EAllOptions(), ws, []*workspace.Project{project})
	if run.Publication() || *factoryCalls != 1 {
		t.Fatalf("publication %t, provider process started %d times", run.Publication(), *factoryCalls)
	}
	member := run.Plan().SelectedMembers()[0]
	owner := &modelextension.ExtensionDescription{Name: "test-provider", Path: root}
	environ := filepath.Join(root, "environ")
	packageJob := &ScheduledJob{Project: project, Extension: owner, JobDef: &modelextension.JobDefinition{
		Name: "package~npm", CommandName: "package", StepID: "npm", ExtensionName: owner.Name,
	}}
	fixtureTask(t, packageJob.JobDef, fixtureScript{{"print", releaseSetSuccessLine}})
	publish := &ScheduledJob{Project: project, Extension: owner, JobDef: &modelextension.JobDefinition{
		Name: "publish~npm", CommandName: "publish", StepID: "npm", ExtensionName: owner.Name,
	}, DependsOn: []string{packageJob.Key()}}
	fixtureTask(t, publish.JobDef, fixtureScript{
		{"write-environ", environ},
		{"print", publishedMemberRuntimeLine(member, digestFor('c'))},
		{"print", releaseSetSuccessLine},
	})
	planned, err := run.AttachPlan([]*ScheduledJob{packageJob, publish})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.BindPublication(nil, nil); err != nil || run.open != nil {
		t.Fatalf("bind without publication-v1: %v, open %v", err, run.open)
	}
	planned, internal, err := run.AttachBarrier(planned)
	if err != nil || len(internal) != 0 || len(planned) != 2 {
		t.Fatalf("legacy plan: %d nodes, %d internal, %v", len(planned), len(internal), err)
	}
	runCtx, closeOutboxes, err := run.PublicationContext(ctx)
	if err != nil || runCtx != ctx {
		t.Fatalf("legacy publication context changed: %v", err)
	}
	defer closeOutboxes()
	result := RunPlan(runCtx, RunRequest{
		Workspace: ws, Plan: planned, Config: SchedulerConfig{MaxParallel: 1, NoCache: true}, Renderer: &mockRenderer{},
	})
	run.Finalizer(runCtx)(result.Results)
	outcome := releaseSetE2EOutcome(t, result.Results)
	if released := ledger.mustReleaseSet(t, outcome.Ref); released.Members[0].ArtifactDigest != digestFor('c') {
		t.Fatalf("released %+v", released.Members)
	}
	dump, err := os.ReadFile(environ)
	if err != nil || strings.Contains(string(dump), extensionproto.PublicationOutboxEnv+"=") {
		t.Fatalf("the legacy publication job received an outbox (%v)", err)
	}
}
