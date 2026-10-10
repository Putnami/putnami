package workspaceclient

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

type clientGenConfig struct {
	ThirdParty bool     `json:"thirdParty,omitempty"`
	Targets    []string `json:"targets"`
	TS         struct {
		Output string `json:"output"`
	} `json:"ts"`
	Go struct {
		Output string `json:"output"`
	} `json:"go"`
}

type workspaceProjectIndex struct {
	Version  int `json:"version"`
	Projects []struct {
		Path string `json:"path"`
	} `json:"projects"`
}

type openAPIDocument struct {
	Client json.RawMessage                        `json:"x-putnami-client"`
	Paths  map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIOperation struct {
	OperationID string          `json:"operationId"`
	Client      json.RawMessage `json:"x-putnami-client"`
	// External names the authority that owns the operation's wire contract.
	// Such an operation is served and documented, never generated.
	External json.RawMessage `json:"x-putnami-external-contract"`
}

type provider struct {
	root           string
	rel            string
	config         *clientGenConfig
	specPath       string
	spec           []byte
	classification Classification
	document       *clientcontract.DocumentV1
	operations     []clientcontract.GeneratedOperation
	security       map[string]clientcontract.Security
	contractHash   string
	// unreadOperations counts the contract operations discovery could not turn
	// into a generated one AND could not read as deliberately absent — an
	// unmarked operation, an invalid x-putnami-client marker, a missing
	// operationId, and a contradictory x-putnami-external-contract (both
	// extensions at once, a non-string value, a blank authority). Each is
	// already a finding; the count exists so an UNREADABLE contract is never
	// mistaken for an EMPTY one (see emptyFirstPartyContract).
	//
	// An operation whose external marker is VALID is not counted: discovery
	// read it and the authority named in it is the answer. That is the one way
	// an operation leaves the first-party set without leaving a doubt.
	unreadOperations int
}

// emptyFirstPartyContract reports a first-party contract that declares a
// service identity and no operation for a client to carry: every route it
// serves is owned by an external authority (x-putnami-external-contract), or it
// serves none. The declaration is the provider's COMMITTED contract sidecar —
// the document-level x-putnami-client marker with an empty first-party
// operation set — so a cold clone reads exactly the same bytes as a tree the
// session has just built, and the guard needs no configuration, no target and
// no manifest to be satisfied by it.
//
// It is deliberately NOT a blanket suppression. A contract whose operations
// discovery could not read is not empty, and a contract that declares even one
// first-party operation still owes its declared targets a generated client.
func (p provider) emptyFirstPartyContract() bool {
	return p.classification == ClassificationFirstParty && p.document != nil &&
		len(p.operations) == 0 && p.unreadOperations == 0
}

// httpMethods is the set of path-item members the guard reads as an operation.
// It MIRRORS the generator's own walk — httpMethodOrder in
// go/framework/api/clientir.go, and HTTP_OPERATION_KEYS in
// typescript/framework/client/src/generator/openapi-reader.ts — and must stay
// equal to it. The two live in different modules, so neither can import the
// other; TestTheGuardReadsExactlyTheMethodsTheGeneratorGenerates spells the set
// out and fails when they diverge.
//
// A method missing here is not a method the guard ignores harmlessly: the
// operation never reaches the loop below, so it is neither a generated
// operation nor an unread one, and a provider that has only such operations
// would read as a contract that is empty by design.
var httpMethods = map[string]bool{
	"delete": true, "get": true, "head": true, "options": true,
	"patch": true, "post": true, "put": true, "trace": true,
}

// discover reads the BUILT tree: the generation contract the provider's build
// wrote (.gen/clientgen/config.json) and the built contract in preference to
// the committed sidecar. It is the sync, adopt and generation view — a fresh
// build precedes each of them.
func discover(view workspaceView) ([]provider, []Finding) {
	return discoverWith(view, builtSpecPath, false)
}

// discoverCommitted reads the COMMITTED tree, which is the only tree the guard
// may trust: on a cold clone nothing under .gen exists, and on a warm one the
// bytes there describe whatever build last ran — possibly another commit's.
// The committed contract sidecar wins over a built one, and a provider whose
// build never ran here still names its targets through the committed
// client.putnami.json manifests under its tree. A provider the session did
// build converged the sidecar it commits, so the two views agree for it. In
// the candidate cut a .gen file exists only when the project commits it.
func discoverCommitted(view workspaceView) ([]provider, []Finding) {
	return discoverWith(view, committedSpecPath, true)
}

func discoverWith(view workspaceView, specPathFor func(files workspaceFiles, projectRel string) string, targetsFromManifests bool) ([]provider, []Finding) {
	var findings []Finding
	workspaceRoot := view.root
	projectPaths, err := view.members()
	if err != nil {
		return nil, append(findings, Finding{Code: "clientgen.discovery", Path: workspaceRoot, Message: err.Error()})
	}

	providers := make([]provider, 0, len(projectPaths))
	for _, projectRel := range projectPaths {
		item := &provider{root: filepath.Join(workspaceRoot, filepath.FromSlash(projectRel)), rel: projectRel}
		configRel := joinRel(projectRel, ".gen/clientgen/config.json")
		configData, configErr := view.Read(configRel)
		if configErr == nil {
			var config clientGenConfig
			if parseErr := json.Unmarshal(configData, &config); parseErr != nil {
				findings = append(findings, Finding{Code: "clientgen.invalid-config", Path: configRel, Message: parseErr.Error()})
			} else {
				item.config = &config
			}
		} else if !errors.Is(configErr, fs.ErrNotExist) {
			findings = append(findings, Finding{Code: "clientgen.invalid-config", Path: configRel, Message: configErr.Error()})
		}
		item.specPath = specPathFor(view.workspaceFiles, projectRel)
		if item.config == nil && targetsFromManifests {
			// No build wrote a contract here, but the tree may commit generated
			// targets: each committed manifest names its own language and
			// directory, which is exactly what the contract would have said. A
			// project that commits none is not a provider, exactly as a project
			// with no generation contract never was. One that commits targets
			// but no contract has clients nothing can judge, and says so below
			// rather than being skipped as a non-provider.
			config, manifestFindings := configFromCommittedManifests(view.workspaceFiles, projectRel)
			findings = append(findings, manifestFindings...)
			if config != nil {
				item.config = config
			}
		}
		if item.specPath == "" {
			if item.config != nil {
				findings = append(findings, Finding{Code: "clientgen.missing-contract", Path: item.rel,
					Message: "the project generates clients (a clientgen config or committed client manifests) but no OpenAPI contract was found, built or committed"})
			}
			continue
		}
		data, readErr := readSpec(view.workspaceFiles, item.specPath)
		if readErr != nil {
			findings = append(findings, Finding{Code: "clientgen.invalid-contract", Path: item.specPath, Message: readErr.Error()})
			continue
		}
		item.spec = data
		sum := sha256.Sum256(data)
		item.contractHash = hex.EncodeToString(sum[:])
		var document openAPIDocument
		if parseErr := json.Unmarshal(data, &document); parseErr != nil {
			findings = append(findings, Finding{Code: "clientgen.invalid-contract", Path: item.specPath, Message: parseErr.Error()})
			continue
		}
		if len(document.Client) == 0 {
			if item.config != nil {
				if item.config.ThirdParty {
					item.classification = ClassificationThirdParty
				} else {
					item.classification = ClassificationFirstParty
					findings = append(findings, Finding{Code: "clientgen.missing-first-party-marker", Path: item.specPath,
						Message: "configured provider contract has no x-putnami-client metadata; set thirdParty:true only for an external authority"})
				}
				providers = append(providers, *item)
			}
			continue
		}
		item.classification = ClassificationFirstParty
		if item.config != nil && item.config.ThirdParty {
			findings = append(findings, Finding{Code: "clientgen.first-party-thirdparty-bypass", Path: configRel,
				Message: "a marked x-putnami-client provider cannot opt into thirdParty compatibility mode"})
		}
		metadata, documentDiags := clientcontract.ParseAndValidateDocument(document.Client)
		findings = append(findings, diagnostics(item.specPath, "clientgen.invalid-contract", documentDiags)...)
		if metadata == nil {
			providers = append(providers, *item)
			continue
		}
		item.document = metadata
		item.security = map[string]clientcontract.Security{}
		paths := make([]string, 0, len(document.Paths))
		for route := range document.Paths {
			paths = append(paths, route)
		}
		sort.Strings(paths)
		for _, route := range paths {
			methods := make([]string, 0, len(document.Paths[route]))
			for method := range document.Paths[route] {
				if httpMethods[strings.ToLower(method)] {
					methods = append(methods, method)
				}
			}
			sort.Strings(methods)
			for _, method := range methods {
				op := document.Paths[route][method]
				// An operation an external authority owns is not a contract
				// operation: no target generates it, so no manifest lists it.
				authority, externalDiags := clientcontract.ExternalContractAuthority(op.Client, op.External)
				if len(externalDiags) > 0 {
					for _, finding := range diagnostics(item.specPath, "clientgen.invalid-operation-contract", externalDiags) {
						finding.ServiceID = metadata.Service.ID
						finding.Message = fmt.Sprintf("%s %s: %s", strings.ToUpper(method), route, finding.Message)
						findings = append(findings, finding)
					}
					item.unreadOperations++
					continue
				}
				if authority != "" {
					continue
				}
				if len(op.Client) == 0 {
					findings = append(findings, Finding{Code: "clientgen.missing-operation-contract", Path: item.specPath,
						ServiceID: metadata.Service.ID, Message: fmt.Sprintf("%s %s has no x-putnami-client metadata", strings.ToUpper(method), route)})
					item.unreadOperations++
					continue
				}
				opMetadata, opDiags := clientcontract.ParseAndValidateOperation(op.Client, metadata)
				findings = append(findings, diagnostics(item.specPath, "clientgen.invalid-operation-contract", opDiags)...)
				if opMetadata == nil || blank(op.OperationID) {
					if blank(op.OperationID) {
						findings = append(findings, Finding{Code: "clientgen.missing-operation-id", Path: item.specPath,
							ServiceID: metadata.Service.ID, Message: fmt.Sprintf("%s %s has no operationId", strings.ToUpper(method), route)})
					}
					item.unreadOperations++
					continue
				}
				operation := clientcontract.GeneratedOperation{
					OperationID: op.OperationID, Stream: opMetadata.Stream, Transports: opMetadata.Transports,
				}
				if opMetadata.Resilience != nil {
					operation.Cache = opMetadata.Resilience.Cache
				}
				item.operations = append(item.operations, operation)
				item.security[op.OperationID] = opMetadata.Security
			}
		}
		sort.Slice(item.operations, func(i, j int) bool { return item.operations[i].OperationID < item.operations[j].OperationID })
		// A contract with no first-party operation declares that it generates
		// nothing, so it owes no generation configuration either — on a cold
		// clone that absence is the only thing there is to read.
		if item.config == nil && !item.emptyFirstPartyContract() {
			findings = append(findings, Finding{Code: "clientgen.missing-config", Path: item.rel,
				ServiceID: metadata.Service.ID, Message: "first-party provider has no .gen/clientgen/config.json"})
		}
		providers = append(providers, *item)
	}
	return providers, findings
}

func indexedProjectPaths(workspaceRoot string) ([]string, error) {
	path := filepath.Join(workspaceRoot, ".putnami", "workspace-index.json")
	data, err := os.ReadFile(path) //nolint:gosec // fixed Putnami workspace index
	if err != nil {
		return nil, fmt.Errorf("read Putnami workspace project index: %w", err)
	}
	var index workspaceProjectIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse Putnami workspace project index: %w", err)
	}
	if index.Version <= 0 || index.Projects == nil {
		return nil, fmt.Errorf("putnami workspace project index is incomplete; run `putnami projects sync`")
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(index.Projects))
	for _, project := range index.Projects {
		clean, pathErr := safeInventoryProject(project.Path)
		if pathErr != nil || clean != project.Path || seen[project.Path] {
			return nil, fmt.Errorf("putnami workspace project index contains invalid or duplicate path %q", project.Path)
		}
		seen[project.Path] = true
		paths = append(paths, project.Path)
	}
	sort.Strings(paths)
	return paths, nil
}

// builtSpecPath prefers the contract the provider's build wrote over the
// committed sidecar: after a build the built one is the authority.
func builtSpecPath(files workspaceFiles, projectRel string) string {
	return firstSpecPath(files, projectRel,
		".gen/schema/openapi.json",
		".gen/schema/openapi.json.gz",
		"schema/openapi.json")
}

// committedSpecPath prefers the committed sidecar: it is what the checkout
// holds, whether or not anything under .gen exists or is current. A provider
// with no sidecar resolves to a contract under .gen only when one is there to
// read, which in the candidate cut means the project commits it.
func committedSpecPath(files workspaceFiles, projectRel string) string {
	return firstSpecPath(files, projectRel,
		"schema/openapi.json",
		".gen/schema/openapi.json",
		".gen/schema/openapi.json.gz")
}

func firstSpecPath(files workspaceFiles, projectRel string, candidates ...string) string {
	for _, projectPath := range candidates {
		rel := joinRel(projectRel, projectPath)
		if files.isRegularFile(rel) {
			return rel
		}
	}
	return ""
}

// configFromCommittedManifests synthesizes the generation contract a cold tree
// lacks from the client.putnami.json manifests committed under the provider:
// one target per manifest language, at the directory that holds it. A tree
// that commits none yields nil — the project is not a provider; a manifest
// that cannot be read as a first-party target is a finding, because a closure
// nobody can read is not an empty one. Directories the snapshot walk skips
// (dot-directories, installs, vendored trees, testdata) are skipped here for
// the same reason.
//
// The manifest is read through clientcontract.DecodeGeneratedClientReference,
// the same decoder the workspace graph resolves a contract edge with. One
// decoder is what keeps the two answers about a target — which language it is
// generated for, and which provider it belongs to — from disagreeing about
// which files are targets at all.
func configFromCommittedManifests(files workspaceFiles, projectRel string) (*clientGenConfig, []Finding) {
	config := &clientGenConfig{}
	var findings []Finding
	manifests := committedManifestPaths(files, projectRel)
	if len(manifests) == 0 {
		return nil, findings
	}
	for _, manifestRel := range manifests {
		data, readErr := files.Read(manifestRel)
		if readErr != nil {
			findings = append(findings, Finding{Code: "clientgen.invalid-manifest", Path: manifestRel,
				Message: fmt.Sprintf("committed generated client manifest could not be read: %v", readErr)})
			continue
		}
		reference, ok := clientcontract.DecodeGeneratedClientReference(data)
		if !ok {
			findings = append(findings, Finding{Code: "clientgen.invalid-manifest", Path: manifestRel,
				Message: "committed generated client manifest does not name a first-party target " +
					"(generatedBy, protocolVersion, service.id, contractSha256), so the target it names cannot be judged"})
			continue
		}
		output := path.Dir(manifestRel)
		if projectRel != "." {
			output = strings.TrimPrefix(output, projectRel+"/")
		}
		switch reference.Language {
		case clientcontract.GeneratedLanguageGo:
			if !hasConfiguredTarget(config.Targets, "go") {
				config.Targets = append(config.Targets, "go")
				config.Go.Output = output
			}
		case clientcontract.GeneratedLanguageTypeScript:
			if !hasConfiguredTarget(config.Targets, "ts") {
				config.Targets = append(config.Targets, "ts")
				config.TS.Output = output
			}
		default:
			findings = append(findings, Finding{Code: "clientgen.invalid-manifest", Path: manifestRel,
				Message: fmt.Sprintf("committed generated client manifest names no supported language (%q)", reference.Language)})
		}
	}
	sort.Strings(config.Targets)
	return config, findings
}

// committedManifestPaths lists, in directory-walk order, the workspace-relative
// client.putnami.json manifests below the provider projectRel ("." is the
// root), outside the directories the snapshot walk skips and never at the
// project's own root. A manifest at the project's OWN root is a generated
// client the project IS — an indexed client package inside its provider's
// output directory — not a target the project generates; its provider names
// it from above. In the candidate cut an ignored manifest is absent.
func committedManifestPaths(files workspaceFiles, projectRel string) []string {
	var manifests []string
	if files.tree != nil {
		for _, rel := range files.candidatesBelow(projectRel, excludedSnapshotDir) {
			if path.Base(rel) != clientcontract.GeneratedManifestFile || path.Dir(rel) == projectRel {
				continue
			}
			manifests = append(manifests, rel)
		}
		sort.SliceStable(manifests, func(i, j int) bool { return walkOrderLess(manifests[i], manifests[j]) })
		return manifests
	}
	projectRoot := filepath.Join(files.root, filepath.FromSlash(projectRel))
	_ = filepath.WalkDir(projectRoot, func(walked string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if walked != projectRoot && excludedSnapshotDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != clientcontract.GeneratedManifestFile || filepath.Dir(walked) == projectRoot {
			return nil
		}
		if rel, relErr := filepath.Rel(files.root, walked); relErr == nil {
			manifests = append(manifests, filepath.ToSlash(rel))
		}
		return nil
	})
	return manifests
}

// walkOrderLess orders two slash paths as a lexical directory walk visits
// them: segment by segment, so "a/b" precedes "a-c/d" although '-' sorts
// before '/'.
func walkOrderLess(a, b string) bool {
	left, right := strings.Split(a, "/"), strings.Split(b, "/")
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return len(left) < len(right)
}

func readSpec(files workspaceFiles, rel string) ([]byte, error) {
	data, err := files.Read(rel)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(rel, ".gz") {
		return data, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	decompressed, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return decompressed, nil
}

func diagnostics(path, code string, values []diag.Diagnostic) []Finding {
	findings := make([]Finding, 0, len(values))
	for _, value := range values {
		if value.Severity != diag.Error {
			continue
		}
		findings = append(findings, Finding{Code: code, Path: path, Message: value.String()})
	}
	return findings
}

func blank(value string) bool { return strings.TrimSpace(value) == "" }
