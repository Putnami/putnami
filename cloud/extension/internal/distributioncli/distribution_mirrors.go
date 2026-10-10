package distributioncli

import (
	"fmt"
	"io"
	"os"
	"strings"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// mirrorCredentialInput is the reader an owner-supplied mirror credential
// arrives on. It is a package variable so tests can drive it; production always
// reads the process's standard input.
//
// There is deliberately no --token or --password flag: a credential passed as a
// flag lands in argv, shell history and any process listing, and this one is a
// publishing credential for a public registry.
var mirrorCredentialInput io.Reader = os.Stdin

// maxMirrorCredentialBytes bounds what the CLI will read from stdin. A registry
// token is small; the bound keeps a piped file from becoming a request body.
const maxMirrorCredentialBytes = 16 << 10

// mirrorEcosystemArchive names put platform archives. An archive target copies
// the files of one package to the assets of a GitHub Release.
const mirrorEcosystemArchive = "archive"

var mirrorEcosystems = map[string]bool{"npm": true, "oci": true, mirrorEcosystemArchive: true}

func distributionMirrorAdd(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ecosystem := clicore.ScalarStringValue(clicore.Param(params, "ecosystem"))
	id := clicore.ScalarStringValue(clicore.Param(params, "id"))
	destination := clicore.ScalarStringValue(clicore.Param(params, "destination"))
	username := clicore.ScalarStringValue(clicore.Param(params, "username"))
	pkg := clicore.ScalarStringValue(clicore.Param(params, "package"))
	if !mirrorEcosystems[ecosystem] || id == "" || id != strings.TrimSpace(id) ||
		destination == "" || destination != strings.TrimSpace(destination) {
		usage := "mirrors add requires --ecosystem npm|oci|archive, --id <id> and --destination <target>"
		// `put` is the registry, not an ecosystem: its platform archives are
		// release-set members of ecosystem `archive`.
		if ecosystem == "put" {
			usage += "; put platform archives are --ecosystem archive"
		}
		return clicore.NewError(usage, clicore.ExitUsage)
	}
	if username != "" && ecosystem != "oci" {
		return clicore.NewError(
			ecosystem+" mirror targets authenticate with a token only; drop --username and pipe the token on standard input", clicore.ExitUsage)
	}
	// A package is an exact <namespace>/<name>, so it is never trimmed into
	// shape; the provider decides whether the spelling is valid.
	if ecosystem == mirrorEcosystemArchive && (pkg == "" || pkg != strings.TrimSpace(pkg)) {
		return clicore.NewError(
			"archive mirror targets copy one package; pass --package <namespace>/<name>", clicore.ExitUsage)
	}
	if ecosystem != mirrorEcosystemArchive && pkg != "" {
		return clicore.NewError(
			"--package applies to archive mirror targets only; "+ecosystem+" targets mirror every selected member", clicore.ExitUsage)
	}
	aliases, err := mirrorAliases(params)
	if err != nil {
		return err
	}
	secret, err := readMirrorCredential()
	if err != nil {
		return err
	}
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	body := putserverclient.MirrorTargetCreateRequest{Id: &id, Ecosystem: &ecosystem, Destination: &destination}
	if len(aliases) > 0 {
		body.Aliases = &aliases
	}
	if pkg != "" {
		body.Package = &pkg
	}
	if username != "" {
		body.Username, body.Password = &username, &secret
	} else {
		body.Token = &secret
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	created, err := put.CreatePutReleaseSetsWorkspacesMirrors(withBearer(token), putserverclient.CreatePutReleaseSetsWorkspacesMirrorsInput{
		Path: putserverclient.CreatePutReleaseSetsWorkspacesMirrorsPath{Workspace: workspaceID},
		Body: body,
	})
	if err != nil {
		return clicore.RequestError(distributionMirrorsURL(registryURL, workspaceID), err)
	}
	response, err := clicore.ResponseMap(created)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx,
		fmt.Sprintf("Onboarded %s public mirror target %s for workspace %s.", ecosystem, id, workspaceID))
	return nil
}

func distributionMirrorsList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	endpoint := distributionMirrorsURL(registryURL, workspaceID)
	query := putserverclient.GetPutReleaseSetsWorkspacesMirrorsQuery{}
	if clicore.Truthy(clicore.Param(params, "include-revoked")) {
		includeRevoked := true
		query.IncludeRevoked = &includeRevoked
		endpoint += "?include_revoked=true"
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	listed, err := put.GetPutReleaseSetsWorkspacesMirrors(withBearer(token), putserverclient.GetPutReleaseSetsWorkspacesMirrorsInput{
		Path:  putserverclient.GetPutReleaseSetsWorkspacesMirrorsPath{Workspace: workspaceID},
		Query: query,
	})
	if err != nil {
		return clicore.RequestError(endpoint, err)
	}
	response, err := clicore.ResponseMap(listed)
	if err != nil {
		return err
	}
	return renderDistributionRows(params, ioctx, response, "targets", "No public mirror targets.", func(row map[string]any) string {
		state := "live"
		if clicore.ValueString(row, "revoked_at") != "" {
			state = "revoked"
		}
		line := fmt.Sprintf("%-4s %-24s %-40s %s",
			clicore.ValueString(row, "ecosystem"), clicore.ValueString(row, "id"),
			clicore.ValueString(row, "destination"), state)
		if pkg := clicore.ValueString(row, "package"); pkg != "" {
			line += " package=" + pkg
		}
		return line
	})
}

func distributionMirrorRemove(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return clicore.NewError("cloud packages mirrors remove requires <id>", clicore.ExitUsage)
	}
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	revoked, err := put.DeletePutReleaseSetsWorkspacesMirrors(withBearer(token), putserverclient.DeletePutReleaseSetsWorkspacesMirrorsInput{
		Path: putserverclient.DeletePutReleaseSetsWorkspacesMirrorsPath{Workspace: workspaceID, Id: id},
	})
	if err != nil {
		return clicore.RequestError(distributionMirrorsURL(registryURL, workspaceID)+"/"+clicore.URLPathEscape(id), err)
	}
	response, err := clicore.ResponseMap(revoked)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx,
		"Revoked public mirror target "+id+". Pending copies against it stop at their next attempt.")
	return nil
}

func distributionMirrorRotate(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return clicore.NewError("cloud packages mirrors rotate-credential requires <id>", clicore.ExitUsage)
	}
	username := clicore.ScalarStringValue(clicore.Param(params, "username"))
	secret, err := readMirrorCredential()
	if err != nil {
		return err
	}
	workspaceID, token, registryURL, err := distributionGrantContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	body := putserverclient.MirrorCredentialRequest{}
	if username != "" {
		body.Username, body.Password = &username, &secret
	} else {
		body.Token = &secret
	}
	put, err := putOwnerClient(ioctx, registryURL)
	if err != nil {
		return err
	}
	rotated, err := put.UpdatePutReleaseSetsWorkspacesMirrorsCredential(withBearer(token), putserverclient.UpdatePutReleaseSetsWorkspacesMirrorsCredentialInput{
		Path: putserverclient.UpdatePutReleaseSetsWorkspacesMirrorsCredentialPath{Workspace: workspaceID, Id: id},
		Body: body,
	})
	if err != nil {
		return clicore.RequestError(distributionMirrorsURL(registryURL, workspaceID)+"/"+clicore.URLPathEscape(id)+"/credential", err)
	}
	response, err := clicore.ResponseMap(rotated)
	if err != nil {
		return err
	}
	clicore.WriteResult(response, params, ioctx,
		"Rotated the credential of public mirror target "+id+". Accepted copies keep executing.")
	return nil
}

// readMirrorCredential reads the token or password from standard input. An
// empty stdin is a usage error rather than an empty credential, so a forgotten
// pipe never onboards a target that cannot publish.
func readMirrorCredential() (string, error) {
	if mirrorCredentialInput == nil {
		return "", clicore.NewError("no standard input to read the mirror credential from", clicore.ExitUsage)
	}
	raw, err := io.ReadAll(io.LimitReader(mirrorCredentialInput, maxMirrorCredentialBytes+1))
	if err != nil {
		return "", clicore.NewError("read the mirror credential from standard input", clicore.ExitUsage)
	}
	if len(raw) > maxMirrorCredentialBytes {
		return "", clicore.NewError("the mirror credential on standard input is too large", clicore.ExitUsage)
	}
	secret := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(secret) == "" {
		return "", clicore.NewError(
			"pipe the registry token or password on standard input, for example `… mirrors add … < token.txt`",
			clicore.ExitUsage)
	}
	return secret, nil
}

// mirrorAliases accepts --alias repeated, or one comma-separated value. An
// alias is an exact alternate spelling of the destination, so it is never
// trimmed into shape: a value with surrounding whitespace is a typo, and
// accepting it would create a spelling nobody can reproduce in a release.
func mirrorAliases(params map[string]any) ([]string, error) {
	var raw []string
	switch value := clicore.Param(params, "alias", "aliases").(type) {
	case nil:
		return nil, nil
	case string:
		raw = strings.Split(value, ",")
	case []string:
		raw = value
	case []any:
		for _, item := range value {
			raw = append(raw, clicore.ScalarStringValue(item))
		}
	default:
		return nil, clicore.NewError("--alias takes one or more destination spellings", clicore.ExitUsage)
	}
	aliases := make([]string, 0, len(raw))
	for _, alias := range raw {
		if alias == "" || alias != strings.TrimSpace(alias) {
			return nil, clicore.NewError("--alias values must be exact destination spellings", clicore.ExitUsage)
		}
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

func distributionMirrorsURL(registryURL, workspaceID string) string {
	return putOwnerURL(registryURL, "/_/release-sets/workspaces/"+clicore.URLPathEscape(workspaceID)+"/mirrors")
}
