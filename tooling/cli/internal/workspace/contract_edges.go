package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The contract edge: a generated client and the provider whose contract it is
// generated from.
//
// Both ends are COMMITTED files, never a built one. A client names its
// provider's service in the `generatedFrom` member of its putnami.json, and a
// provider commits its OpenAPI contract at `schema/openapi.json`; whatever a
// build last wrote under `.gen` describes some other run's tree, so reading it
// would make the graph disagree between a cold clone and a warm checkout.
// Nothing beyond putnami.json is read in a workspace that declares no client.
//
// The contract is core reading a PROTOCOL artifact, not a language manifest:
// it is defined by `protocols/clientcontract` and decoded by it, the way
// `putnami.json` is defined by `protocols/workspace`. No language adapter can
// answer this edge, because a client and its provider are routinely written in
// different languages and neither module graph contains the other.

// committedContractFile is the provider's contract sidecar, project-relative.
const committedContractFile = "schema/openapi.json"

// resolveContractBindings fills each project's contract service identity from
// its committed contract, and each client's binding from its `generatedFrom`,
// resolved against those identities.
//
// The provider scan is skipped entirely when no project declares a client: with
// no client to bind, a service identity answers no question, and a workspace of
// hundreds of providers pays nothing for a relation it does not use.
func resolveContractBindings(root string, projects []*Project) {
	clients := false
	for _, project := range projects {
		if project == nil {
			continue
		}
		project.GeneratedClient = nil
		clients = clients || generatedFromOf(project) != nil
	}
	if !clients {
		return
	}
	providers := map[string][]*Project{}
	for _, project := range projects {
		if project == nil {
			continue
		}
		contract := readCommittedContract(root, project)
		project.ContractServiceID = contract.serviceID
		project.ContractPath = contract.path
		project.ContractSHA256 = contract.sha256
		if contract.serviceID != "" {
			providers[contract.serviceID] = append(providers[contract.serviceID], project)
		}
	}
	for _, project := range projects {
		if project == nil {
			continue
		}
		if from := generatedFromOf(project); from != nil {
			project.GeneratedClient = bindGeneratedClient(from, providers[from.Service])
		}
	}
}

// generatedFromOf returns the `generatedFrom` member a project declares, or nil.
func generatedFromOf(project *Project) *wsproto.GeneratedFrom {
	if project.Config == nil || project.Config.GeneratedFrom == nil {
		return nil
	}
	if project.Config.GeneratedFrom.Service == "" {
		return nil
	}
	return project.Config.GeneratedFrom
}

// bindGeneratedClient builds a client's binding from its `generatedFrom` and
// the projects whose committed contract declares its service. Exactly one
// provider resolves the binding; none or several leave it unresolved, with the
// fixed UnresolvedContractDigest, so the key does not depend on which of
// several ambiguous providers was read.
func bindGeneratedClient(from *wsproto.GeneratedFrom, providers []*Project) *GeneratedClientBinding {
	binding := &GeneratedClientBinding{
		ServiceID:      from.Service,
		Language:       from.Language,
		Options:        canonicalGenerationOptions(from),
		ContractSHA256: UnresolvedContractDigest,
	}
	if len(providers) == 1 {
		provider := providers[0]
		binding.ProviderID = provider.ID
		binding.ContractPath = provider.ContractPath
		binding.ContractSHA256 = provider.ContractSHA256
	}
	return binding
}

// canonicalGenerationOptions encodes the options that decide emitted bytes
// besides the contract: the Go package and client names, and the omitted
// operations, sorted and deduplicated so their order in putnami.json moves no
// key. Encoding a struct of strings and a string slice cannot fail.
func canonicalGenerationOptions(from *wsproto.GeneratedFrom) string {
	omitted := append([]string(nil), from.OmitOperations...)
	sort.Strings(omitted)
	omitted = slices.Compact(omitted)
	data, _ := json.Marshal(struct {
		PackageName    string   `json:"packageName,omitempty"`
		ClientName     string   `json:"clientName,omitempty"`
		OmitOperations []string `json:"omitOperations,omitempty"`
	}{from.PackageName, from.ClientName, omitted})
	return string(data)
}

// committedContract is a provider's committed contract: the service identity
// it declares, the file it was read from, and the digest of that file's bytes.
// The zero value is a project that provides no service.
type committedContract struct {
	serviceID string
	// path is workspace-relative, in slash form.
	path string
	// sha256 is the lowercase hex sha256 of the bytes read, with no
	// normalization: the same digest clientgen records as a client's
	// contractSha256.
	sha256 string
}

// readCommittedContract reads the provider identity a project's committed
// contract declares, and the file's path and digest, from one read of the
// file. It returns the zero value for a project that commits no contract and
// for one whose contract carries no first-party marker: neither provides a
// service a generated client can name.
func readCommittedContract(root string, project *Project) committedContract {
	rel := projectRelativePath(project, committedContractFile)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return committedContract{}
	}
	service, ok := clientcontract.DecodeContractService(data)
	if !ok {
		return committedContract{}
	}
	sum := sha256.Sum256(data)
	return committedContract{serviceID: service.ID, path: rel, sha256: hex.EncodeToString(sum[:])}
}

// projectRelativePath joins a project-relative slash path onto the project's
// workspace-relative directory.
func projectRelativePath(project *Project, name string) string {
	base := wsproto.CleanWorkspacePath(project.Path)
	if base == "" {
		return name
	}
	return base + "/" + name
}
