package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// The contract edge: a generated client target and the provider whose contract
// it was generated from.
//
// Both ends are COMMITTED Putnami artifacts, never a built one. A generated
// target commits `client.putnami.json` at its root and a provider commits its
// OpenAPI contract at `schema/openapi.json`; whatever a build last wrote under
// `.gen` describes some other run's tree, so reading it would make the graph
// disagree between a cold clone and a warm checkout. Every project this
// resolves reads two committed files at most, and nothing is read at all in a
// workspace that commits no generated client.
//
// This is core reading a PROTOCOL artifact, not a language manifest: the two
// files are defined by `protocols/clientcontract` and decoded by it, the way
// `putnami.json` is defined by `protocols/workspace`. No language adapter can
// answer this edge, because a client and its provider are routinely written in
// different languages and neither module graph contains the other.

// committedContractFile is the provider's contract sidecar, project-relative.
const committedContractFile = "schema/openapi.json"

// resolveContractBindings fills each project's generated-client binding and
// contract service identity from the committed tree, with the path and digest
// of the contract that declares the identity.
//
// The provider scan is skipped entirely when no project commits a client
// manifest: with no client to place, a service identity answers no question,
// and a workspace of hundreds of providers pays nothing for a relation it does
// not use.
func resolveContractBindings(root string, projects []*Project) {
	clients := false
	for _, project := range projects {
		if project == nil {
			continue
		}
		project.GeneratedClient = readGeneratedClientBinding(root, project)
		clients = clients || project.GeneratedClient != nil
	}
	if !clients {
		return
	}
	for _, project := range projects {
		if project == nil {
			continue
		}
		contract := readCommittedContract(root, project)
		project.ContractServiceID = contract.serviceID
		project.ContractPath = contract.path
		project.ContractSHA256 = contract.sha256
	}
}

// readGeneratedClientBinding reads the generated client manifest committed at a
// project's own root. A manifest deeper in the tree belongs to another project
// — the one whose root holds it — and is that project's binding, not this
// one's.
func readGeneratedClientBinding(root string, project *Project) *GeneratedClientBinding {
	rel := projectRelativePath(project, clientcontract.GeneratedManifestFile)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // fixed name under an indexed project
	if err != nil {
		return nil
	}
	reference, ok := clientcontract.DecodeGeneratedClientReference(data)
	if !ok {
		return nil
	}
	return &GeneratedClientBinding{
		ServiceID:      reference.ServiceID,
		ContractSHA256: reference.ContractSHA256,
		Language:       string(reference.Language),
		ManifestPath:   rel,
	}
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
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // fixed name under an indexed project
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
	base := cleanWorkspacePath(project.Path)
	if base == "" {
		return name
	}
	return base + "/" + name
}
