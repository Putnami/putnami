package agencycli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	agencyapiclient "go.putnami.dev/cloud/clients/agency-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// auditKeyDomain separates an attestation key from any other hash.
const auditKeyDomain = "putnami:audit-attest:v1"

// auditKey is the identity of one attestation.
type auditKey struct {
	Repo, Project, Group, InputHash, RubricHash string
}

// auditLookupLine is one JSONL lookup prefetch reads.
type auditLookupLine struct {
	Label      string `json:"label"`
	Repo       string `json:"repo"`
	Project    string `json:"project"`
	Group      string `json:"group"`
	InputHash  string `json:"inputHash"`
	RubricHash string `json:"rubricHash"`
}

func (l auditLookupLine) key() auditKey {
	return auditKey{Repo: l.Repo, Project: l.Project, Group: l.Group, InputHash: l.InputHash, RubricHash: l.RubricHash}
}

// auditPrefetchLine is one JSONL line prefetch prints.
type auditPrefetchLine struct {
	Label  string         `json:"label"`
	Key    string         `json:"key"`
	Found  bool           `json:"found"`
	Record map[string]any `json:"record,omitempty"`
}

// AuditAttest runs `putnami agency audit-attest get|set|prefetch`: the shared
// audit attestations of the linked workspace in Putnami Agency, which the
// audit skill reads and writes.
func AuditAttest(params map[string]any, args []string, root string, env map[string]string, ioctx clicore.IO, input io.Reader) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "get":
		return auditAttestGet(params, root, env, ioctx)
	case "set":
		return auditAttestSet(params, root, env, ioctx)
	case "prefetch":
		return auditAttestPrefetch(params, root, env, ioctx, input)
	case "", "help":
		commands := []map[string]string{
			{"command": "agency audit-attest get", "description": "read a shared audit attestation"},
			{"command": "agency audit-attest set", "description": "record a shared audit attestation"},
			{"command": "agency audit-attest prefetch", "description": "resolve JSONL lookups in one request"},
		}
		if clicore.StructuredOutput(params) {
			clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
			return nil
		}
		ioctx.Stdout("@putnami/cloud agency audit-attest commands:")
		for _, command := range commands {
			ioctx.Stdout(fmt.Sprintf("  putnami %-30s %s", command["command"], command["description"]))
		}
		return nil
	default:
		return clicore.NewError("unknown audit-attest command: "+sub+"; expected get, set or prefetch", clicore.ExitUsage)
	}
}

// auditSession opens the linked workspace and agency-api's generated client.
func auditSession(params map[string]any, root string, env map[string]string, ioctx clicore.IO) (*session, *agencyapiclient.AgencyClient, error) {
	s, err := openSession(params, root, env, ioctx, "Putnami Agency", nil)
	if err != nil {
		return nil, nil, err
	}
	api, err := clicore.NewServiceClient[agencyapiclient.AgencyClient](agencyapiclient.RegisterAgencyClient, s.workspace.ServiceBinding())
	if err != nil {
		return nil, nil, err
	}
	return s, api, nil
}

func auditAttestGet(params map[string]any, root string, env map[string]string, ioctx clicore.IO) error {
	key, err := auditKeyFromParams(params)
	if err != nil {
		return err
	}
	s, api, err := auditSession(params, root, env, ioctx)
	if err != nil {
		return err
	}
	answer, _, err := call(s, func(ctx context.Context) (*agencyapiclient.LookupResponse, error) {
		return api.CreateApiAgencyAuditAttestationsLookup(ctx, agencyapiclient.CreateApiAgencyAuditAttestationsLookupInput{
			Body: agencyapiclient.LookupBody{Lookups: &[]agencyapiclient.Lookup{lookupOf("", key)}},
		})
	})
	if err != nil {
		return err
	}
	result := agencyapiclient.Result{}
	if answer != nil && answer.Results != nil && len(*answer.Results) > 0 {
		result = (*answer.Results)[0]
	}
	id := deref(result.Key)
	if id == "" {
		id = auditKeyID(key)
	}
	record, hasRecord := result.Record.Value()
	found := result.Found != nil && *result.Found && hasRecord
	out := map[string]any{"found": found, "key": id}
	message := "No audit attestation found for the given key."
	if found {
		cli := auditRecord(record)
		out["record"] = cli
		message = fmt.Sprintf("Audit attestation for %s: head %s verified at %s (audit %s).",
			clicore.ShortDigest(id), cli["headSha"], cli["verifiedAt"], cli["auditVersion"])
	}
	clicore.WriteResult(out, params, ioctx, message)
	return nil
}

func auditAttestSet(params map[string]any, root string, env map[string]string, ioctx clicore.IO) error {
	key, err := auditKeyFromParams(params)
	if err != nil {
		return err
	}
	headSHA := clicore.StringParam(params, "head-sha", "headSha")
	auditVersion := clicore.StringParam(params, "audit-version", "auditVersion")
	var missing []string
	if headSHA == "" {
		missing = append(missing, "--head-sha")
	}
	if auditVersion == "" {
		missing = append(missing, "--audit-version")
	}
	if len(missing) > 0 {
		return clicore.NewError("agency audit-attest set requires "+strings.Join(missing, ", "), clicore.ExitUsage)
	}
	body := agencyapiclient.PutBody{Repo: &key.Repo, Project: &key.Project, Group: &key.Group, InputHash: &key.InputHash,
		RubricHash: &key.RubricHash, HeadSha: &headSHA, AuditVersion: &auditVersion}
	if verifiedAt := clicore.StringParam(params, "verified-at", "verifiedAt"); verifiedAt != "" {
		if _, err := time.Parse(time.RFC3339, verifiedAt); err != nil {
			return clicore.NewError("--verified-at is an RFC 3339 time", clicore.ExitUsage)
		}
		body.VerifiedAt = &verifiedAt
	}
	s, api, err := auditSession(params, root, env, ioctx)
	if err != nil {
		return err
	}
	answer, _, err := call(s, func(ctx context.Context) (*agencyapiclient.UpdateApiAgencyAuditAttestationsResult, error) {
		return api.UpdateApiAgencyAuditAttestations(ctx, agencyapiclient.UpdateApiAgencyAuditAttestationsInput{Body: body})
	})
	if err != nil {
		return err
	}
	if answer == nil || answer.Body == nil {
		return clicore.NewError("Putnami Agency answered no attestation", clicore.ExitAPI)
	}
	out := map[string]any{"recorded": answer.Body.Recorded != nil && *answer.Body.Recorded, "key": deref(answer.Body.Key), "record": map[string]any{}}
	if record, ok := answer.Body.Record.Value(); ok {
		out["record"] = auditRecord(record)
	}
	clicore.WriteResult(out, params, ioctx, fmt.Sprintf("Recorded audit attestation for %s (head %s, audit %s).",
		clicore.ShortDigest(deref(answer.Body.Key)), headSHA, auditVersion))
	return nil
}

func auditAttestPrefetch(params map[string]any, root string, env map[string]string, ioctx clicore.IO, input io.Reader) error {
	lines, err := readAuditLookups(params, input)
	if err != nil || len(lines) == 0 {
		return err
	}
	lookups := make([]agencyapiclient.Lookup, 0, len(lines))
	for _, line := range lines {
		lookups = append(lookups, lookupOf(line.Label, line.key()))
	}
	s, api, err := auditSession(params, root, env, ioctx)
	if err != nil {
		return err
	}
	answer, _, err := call(s, func(ctx context.Context) (*agencyapiclient.LookupResponse, error) {
		return api.CreateApiAgencyAuditAttestationsLookup(ctx, agencyapiclient.CreateApiAgencyAuditAttestationsLookupInput{
			Body: agencyapiclient.LookupBody{Lookups: &lookups},
		})
	})
	if err != nil {
		return err
	}
	if answer == nil || answer.Results == nil {
		return clicore.NewError("Putnami Agency answered no lookup result", clicore.ExitAPI)
	}
	for _, result := range *answer.Results {
		out := auditPrefetchLine{Label: deref(result.Label), Key: deref(result.Key), Found: result.Found != nil && *result.Found}
		if record, ok := result.Record.Value(); out.Found && ok {
			out.Record = auditRecord(record)
		}
		line, err := json.Marshal(out)
		if err != nil {
			return clicore.NewError("audit-attest prefetch: encode the result: "+err.Error(), clicore.ExitAPI)
		}
		ioctx.Stdout(string(line))
	}
	return nil
}

// readAuditLookups reads the JSONL lookups of --file, else of input. A line
// that is not a whole lookup refuses the batch before anything is sent.
func readAuditLookups(params map[string]any, input io.Reader) ([]auditLookupLine, error) {
	reader := input
	if path := clicore.StringParam(params, "file"); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, clicore.NewError("agency audit-attest prefetch: open --file "+path+": "+err.Error(), clicore.ExitUsage)
		}
		defer file.Close()
		reader = file
	}
	if reader == nil {
		return nil, clicore.NewError("agency audit-attest prefetch reads JSONL lookups from standard input or --file", clicore.ExitUsage)
	}
	var lookups []auditLookupLine
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for number := 1; scanner.Scan(); number++ {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var lookup auditLookupLine
		if err := json.Unmarshal([]byte(raw), &lookup); err != nil {
			return nil, clicore.NewError(fmt.Sprintf("agency audit-attest prefetch: line %d is not valid JSON: %s", number, err), clicore.ExitUsage)
		}
		if missing := missingAuditKey(lookup.key()); len(missing) > 0 {
			return nil, clicore.NewError(fmt.Sprintf("agency audit-attest prefetch: line %d is missing %s", number, strings.Join(missing, ", ")), clicore.ExitUsage)
		}
		lookups = append(lookups, lookup)
	}
	if err := scanner.Err(); err != nil {
		return nil, clicore.NewError("agency audit-attest prefetch: read the lookups: "+err.Error(), clicore.ExitUsage)
	}
	return lookups, nil
}

func auditKeyFromParams(params map[string]any) (auditKey, error) {
	key := auditKey{
		Repo: clicore.StringParam(params, "repo", "repository"), Project: clicore.StringParam(params, "project", "projectShort"),
		Group: clicore.StringParam(params, "group"), InputHash: clicore.StringParam(params, "input-hash", "inputHash"),
		RubricHash: clicore.StringParam(params, "rubric-hash", "rubricHash"),
	}
	if missing := missingAuditKey(key); len(missing) > 0 {
		flags := make([]string, len(missing))
		for i, name := range missing {
			flags[i] = "--" + strings.ReplaceAll(name, "Hash", "-hash")
		}
		return auditKey{}, clicore.NewError("agency audit-attest requires "+strings.Join(flags, ", "), clicore.ExitUsage)
	}
	return key, nil
}

// missingAuditKey names the members of key that are blank, in key order.
func missingAuditKey(key auditKey) []string {
	var missing []string
	for _, member := range []struct{ name, value string }{
		{"repo", key.Repo}, {"project", key.Project}, {"group", key.Group}, {"inputHash", key.InputHash}, {"rubricHash", key.RubricHash},
	} {
		if strings.TrimSpace(member.value) == "" {
			missing = append(missing, member.name)
		}
	}
	return missing
}

func lookupOf(label string, key auditKey) agencyapiclient.Lookup {
	lookup := agencyapiclient.Lookup{Repo: &key.Repo, Project: &key.Project, Group: &key.Group, InputHash: &key.InputHash, RubricHash: &key.RubricHash}
	if label != "" {
		lookup.Label = &label
	}
	return lookup
}

func auditRecord(record agencyapiclient.AttestationRecord) map[string]any {
	out := map[string]any{"headSha": deref(record.HeadSha), "auditVersion": deref(record.AuditVersion)}
	if record.VerifiedAt != nil {
		out["verifiedAt"] = record.VerifiedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// auditKeyID is the key agency-api derives for an attestation, which get
// reports when agency-api answers none.
func auditKeyID(key auditKey) string {
	canonical, _ := json.Marshal([]string{auditKeyDomain, key.Repo, key.Project, key.Group, key.InputHash, key.RubricHash})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
