package distributioncli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/client"
	ciproto "go.putnami.dev/protocol/ci"

	distributionapiclient "go.putnami.dev/cloud/clients/distribution-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The packages status: one check per package protocol, saying whether
// the linked workspace holds the namespace binding this checkout needs.
// Distribution checks the binding of a protocol before it mints a credential
// that publishes through it, so a needed binding that is missing fails the
// publication, not this command.
//
// OCI has a second supported model: a workspace publishes its images into a
// namespace another workspace owns, through exact package shares that owner
// granted it. A namespace has one owner, so that workspace cannot bind the
// namespace itself. One active OCI publisher share makes it ready.

// packageProtocols lists the binding protocols in the order the status shows
// them, with the name a user types.
var packageProtocols = []struct{ protocol, title string }{
	{"npm", "npm"}, {"gomod", "go"}, {"oci", "oci"}, {"put", "put"},
}

// PackageAnswer is what the checkout and distribution-api say about one
// protocol. Namespace is the namespace of the workspace's active binding,
// empty when the workspace binds none. Need says why this checkout needs a
// binding, empty when it does not. Expected is the namespace the fix
// activates, empty when the checkout does not name one.
//
// Shared counts, by namespace, the active publisher shares other workspaces
// granted this one. It is read only for oci, when the checkout needs it and
// the workspace binds none. SharesUnread says why those shares could not be
// read; it is empty when they were read or not needed.
type PackageAnswer struct {
	Protocol     string
	Namespace    string
	Need         string
	Expected     string
	Shared       map[string]int
	SharesUnread string
}

// PackagesStatus serves `putnami cloud packages status [<protocol>]`. args
// start after `status`. A session that cannot be read before the bindings are
// read keeps its exit 3 error; every other failed read is an unknown node.
func PackagesStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	positionals := clicore.Positionals(args)
	if len(positionals) > 1 {
		return clicore.NewError("cloud packages status takes at most one protocol", clicore.ExitUsage)
	}
	only := ""
	if len(positionals) == 1 {
		protocol, ok := packageProtocolByName(positionals[0])
		if !ok {
			return clicore.NewError("unknown package protocol "+positionals[0]+"; expected npm, go, oci or put", clicore.ExitUsage)
		}
		only = protocol
	}
	node, err := packagesStatusNode(params, workspaceRoot, env, ioctx, only)
	if err != nil {
		return err
	}
	return clicore.WriteStatus(params, ioctx, node)
}

// PackagesStatusNode is the packages status of the linked workspace, for
// every protocol. It is the same node `putnami cloud packages status` prints.
func PackagesStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	node, err := packagesStatusNode(params, workspaceRoot, env, ioctx, "")
	if err != nil {
		return clicore.UserSessionStatus("packages", "packages", err, env)
	}
	return node
}

func packagesStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, only string) (clicore.StatusNode, error) {
	id, title := "packages", "packages"
	if only != "" {
		title = packageProtocolTitle(only)
		id = "packages." + title
	}
	answers, err := packageNeeds(workspaceRoot)
	if err != nil {
		return clicore.UnknownStatus(id, title, err, ""), nil
	}
	source, err := openPackageSource(params, workspaceRoot, env, ioctx)
	var bound map[string]string
	if err == nil {
		bound, err = source.bindings()
	}
	if err != nil {
		if clicore.ExitCode(err) == clicore.ExitAuth {
			return clicore.StatusNode{}, err
		}
		return clicore.UnknownStatus(id, title, err, ""), nil
	}
	for index := range answers {
		answer := &answers[index]
		answer.Namespace = bound[answer.Protocol]
		if answer.Protocol != "oci" || answer.Need == "" || answer.Namespace != "" || (only != "" && only != "oci") {
			continue
		}
		// A failed read keeps the answer a binding alone gives, and says why
		// the shares that could clear it were not read.
		shared, err := source.receivedPublisherShares(answer.Protocol)
		if err != nil {
			answer.SharesUnread = clicore.UnknownStatus("", "", err, "").Detail
			continue
		}
		answer.Shared = shared
	}
	if only != "" {
		for _, answer := range answers {
			if answer.Protocol == only {
				return packageCheck(answer), nil
			}
		}
	}
	return PackagesStatusNodeFrom(answers), nil
}

// packageNeeds decides, from what the checkout declares, which bindings it
// needs:
//
//	put        putnami.ci.json exists: CI publishes the workspace's release
//	           sets to put, under the namespace ChannelNamespace names
//	oci        putnami.ci.json declares an environment: the environment
//	           readiness check requires an OCI binding or an active OCI
//	           publisher share; the namespace is the one
//	           registries.oci.publish in putnami.workspace.json names, else
//	           ChannelNamespace
//	npm, go    putnami.ci.json declares distribution.registries.npm or
//	           distribution.registries.go; the checkout names no npm scope or
//	           module prefix, so the fix leaves the namespace to fill in
//
// A protocol the checkout does not need is ok without a binding.
func packageNeeds(workspaceRoot string) ([]PackageAnswer, error) {
	answers := make([]PackageAnswer, 0, len(packageProtocols))
	for _, entry := range packageProtocols {
		answers = append(answers, PackageAnswer{Protocol: entry.protocol})
	}
	document, found, err := readCIDocument(workspaceRoot)
	if err != nil || !found {
		return answers, err
	}
	namespace, err := ChannelNamespace(workspaceRoot)
	if err != nil {
		namespace = ""
	}
	for index := range answers {
		answer := &answers[index]
		switch answer.Protocol {
		case "put":
			answer.Need = ciproto.Filename + " publishes release sets"
			answer.Expected = namespace
		case "oci":
			if len(document.Envs) > 0 {
				answer.Need = ciproto.Filename + " declares environments"
				answer.Expected = clicore.FirstString(ociPublishNamespace(workspaceRoot), namespace)
			}
		default:
			ecosystem := packageProtocolTitle(answer.Protocol)
			if document.Distribution != nil {
				if _, declared := document.Distribution.Registries[ecosystem]; declared {
					answer.Need = ciproto.Filename + " declares distribution.registries." + ecosystem
				}
			}
		}
	}
	return answers, nil
}

// ociPublishNamespace is the path after the host of registries.oci.publish in
// putnami.workspace.json, the namespace deploy publishes images under. It is
// empty when the manifest names no host and namespace.
func ociPublishNamespace(workspaceRoot string) string {
	path := clicore.ManifestPath(workspaceRoot)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var manifest struct {
		Registries struct {
			OCI struct {
				Publish string `json:"publish"`
			} `json:"oci"`
		} `json:"registries"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	_, namespace, found := strings.Cut(strings.Trim(strings.TrimSpace(manifest.Registries.OCI.Publish), "/"), "/")
	if !found {
		return ""
	}
	return strings.Trim(namespace, "/")
}

// packageSource is the linked workspace and the distribution-api client the
// packages status reads it through, with the signed-in user's bearer.
type packageSource struct {
	workspaceID string
	controlURL  string
	ctx         context.Context
	api         *distributionapiclient.DistributionClient
}

func openPackageSource(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (packageSource, error) {
	workspaceID, token, controlURL, err := distributionAdminContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return packageSource{}, err
	}
	api, err := distributionAPIClient(ioctx, controlURL)
	if err != nil {
		return packageSource{}, err
	}
	return packageSource{
		workspaceID: workspaceID, controlURL: controlURL, api: api,
		ctx: client.WithForwardedUserToken(commandContext(ioctx), token),
	}, nil
}

// bindings reads the linked workspace's active bindings: one call. It returns
// the namespace bound for each protocol.
func (s packageSource) bindings() (map[string]string, error) {
	listed, err := s.api.GetV1WorkspacesDistributionBindings(s.ctx, distributionapiclient.GetV1WorkspacesDistributionBindingsInput{
		Path: distributionapiclient.GetV1WorkspacesDistributionBindingsPath{Workspace: s.workspaceID},
	})
	if err != nil {
		return nil, clicore.RequestError(distributionBindingsURL(s.controlURL, s.workspaceID), err)
	}
	bound := map[string]string{}
	if listed != nil && listed.Bindings != nil {
		for _, binding := range *listed.Bindings {
			bound[clicore.Deref(binding.Protocol)] = clicore.Deref(binding.Namespace)
		}
	}
	return bound, nil
}

// receivedPublisherShares reads the package shares other workspaces granted
// the linked workspace: one call. It counts, by namespace, the active
// publisher shares of protocol. A revoked share or a reader share does not
// count.
func (s packageSource) receivedPublisherShares(protocol string) (map[string]int, error) {
	listed, err := s.api.GetV1WorkspacesDistributionPackageSharesReceived(s.ctx, distributionapiclient.GetV1WorkspacesDistributionPackageSharesReceivedInput{
		Path: distributionapiclient.GetV1WorkspacesDistributionPackageSharesReceivedPath{Workspace: s.workspaceID},
	})
	if err != nil {
		return nil, clicore.RequestError(distributionReceivedSharesURL(s.controlURL, s.workspaceID), err)
	}
	shared := map[string]int{}
	if listed == nil || listed.Shares == nil {
		return shared, nil
	}
	for _, share := range *listed.Shares {
		if clicore.Deref(share.Protocol) != protocol ||
			clicore.Deref(share.Permission) != "publisher" ||
			clicore.Deref(share.GranteeWorkspaceId) != s.workspaceID ||
			clicore.Deref(share.RevokedOrder) != 0 {
			continue
		}
		if _, revoked := share.RevokedAt.Value(); revoked {
			continue
		}
		shared[clicore.Deref(share.Namespace)]++
	}
	return shared, nil
}

func distributionReceivedSharesURL(controlURL, workspaceID string) string {
	return controlURL + "/v1/workspaces/" + clicore.URLPathEscape(workspaceID) + "/distribution/package-shares/received"
}

// PackagesStatusNodeFrom folds one answer per protocol. The node takes the
// worst protocol.
//
//	bound                                      ok
//	needed, not bound, active publisher share  ok
//	needed and not bound                       degraded
//	not needed, not bound                      ok
func PackagesStatusNodeFrom(answers []PackageAnswer) clicore.StatusNode {
	node := clicore.StatusNode{ID: "packages", Title: "packages"}
	bound := 0
	type group struct {
		phrase string
		names  []string
	}
	var groups []*group
	for _, answer := range answers {
		child := packageCheck(answer)
		node.Children = append(node.Children, child)
		if answer.Namespace != "" {
			bound++
		}
		phrase := packagePhrase(answer)
		var found *group
		for _, g := range groups {
			if g.phrase == phrase {
				found = g
			}
		}
		if found == nil {
			found = &group{phrase: phrase}
			groups = append(groups, found)
		}
		found.names = append(found.names, child.Title)
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, strings.Join(g.names, ", ")+" "+g.phrase)
	}
	node.Detail = strings.Join(parts, "; ")
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("bindings", "bindings active", bound, clicore.MetricUsage),
	}
	return node
}

func packageCheck(answer PackageAnswer) clicore.StatusNode {
	title := packageProtocolTitle(answer.Protocol)
	node := clicore.StatusNode{ID: "packages." + title, Title: title, State: clicore.StatusOK}
	switch {
	case answer.Namespace != "":
		node.Detail = "namespace " + answer.Namespace
	case answer.Need != "" && len(answer.Shared) > 0:
		node.Detail = "not bound; publishes into " + sharedNamespacesPhrase(answer.Shared)
	case answer.Need != "":
		node.State = clicore.StatusDegraded
		node.Detail = "not bound; " + answer.Need
		if answer.SharesUnread != "" {
			node.Detail += "; package shares not read: " + answer.SharesUnread
		}
		namespace := clicore.FirstString(answer.Expected, "<namespace>")
		node.Fix = "putnami cloud packages namespaces activate --protocol " + answer.Protocol +
			" --namespace " + namespace + " --idempotency-key " + namespace + "-" + answer.Protocol
	default:
		node.Detail = "not needed"
	}
	return node
}

// sharedNamespacesPhrase names each namespace and its share count, in
// namespace order: "namespace putnami through 38 package shares".
func sharedNamespacesPhrase(shared map[string]int) string {
	namespaces := make([]string, 0, len(shared))
	for namespace := range shared {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	parts := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		noun := "package shares"
		if shared[namespace] == 1 {
			noun = "package share"
		}
		parts = append(parts, fmt.Sprintf("namespace %s through %d %s", namespace, shared[namespace], noun))
	}
	return strings.Join(parts, ", ")
}

func packagePhrase(answer PackageAnswer) string {
	switch {
	case answer.Namespace != "":
		return "bound"
	case answer.Need != "" && len(answer.Shared) > 0:
		return "shared"
	case answer.Need != "":
		return "not bound"
	default:
		return "not needed"
	}
}

// packageProtocolByName reads a protocol as a user types it: go or gomod,
// npm, oci, put.
func packageProtocolByName(name string) (string, bool) {
	for _, entry := range packageProtocols {
		if name == entry.protocol || name == entry.title {
			return entry.protocol, true
		}
	}
	return "", false
}

func packageProtocolTitle(protocol string) string {
	for _, entry := range packageProtocols {
		if entry.protocol == protocol {
			return entry.title
		}
	}
	return protocol
}
