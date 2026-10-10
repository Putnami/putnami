package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

const (
	// ChangePlanVersion is the only ChangePlan wire version. A plan that
	// declares another version is refused.
	ChangePlanVersion = 1
	// ChangePlanDigestDomain is the first member of the payload a ChangePlan
	// digest covers. It separates this digest from every other SHA-256 value
	// Putnami computes: cache keys, task contracts, artifact digests, and the
	// putnami.ci.json digest.
	ChangePlanDigestDomain = "putnami/change-plan/v1"
)

// ChangePlan is the versioned, immutable CI admission plan `putnami
// change-plan` emits for one base..head commit range. It carries identities
// and declared scheduling metadata only: never source bytes, task arguments,
// working directories, environment values, output paths, or invocation
// locators. Cache is advisory: key presence is a point-in-time local-store
// observation, so Digest excludes it and a cache warm-up cannot change the
// plan identity.
type ChangePlan struct {
	// Version is the wire version, always ChangePlanVersion.
	Version int `json:"version"`
	// Generator is the implementation that built the plan.
	Generator ChangePlanGenerator `json:"generator"`
	// Repository is the credential-free identity of the planned repository.
	Repository ChangePlanRepository `json:"repository"`
	// BaseSHA is the full commit ID the range starts from, an ancestor of
	// HeadSHA.
	BaseSHA string `json:"baseSHA"`
	// HeadSHA is the full commit ID of the checked-out revision the plan
	// admits.
	HeadSHA string `json:"headSHA"`
	// ChangedFiles is the base..head commit diff, every path byte for byte,
	// sorted and without duplicates. A rename contributes both paths.
	ChangedFiles []string `json:"changedFiles"`
	// Impact is the project closure the changed files reach.
	Impact ChangePlanImpact `json:"impact"`
	// Tasks is every planned task of the quality gate over Impact, sorted by
	// identity key.
	Tasks []ChangePlanTask `json:"tasks"`
	// Cache is the advisory local cache summary, outside the digest.
	Cache ChangePlanCache `json:"cache"`
	// Digest is RecomputeChangePlanDigest of the plan: its identity.
	Digest string `json:"digest"`
}

// ChangePlanGenerator identifies the exact implementation that built a plan.
type ChangePlanGenerator struct {
	// Name is the generator's name, "putnami" for the CLI.
	Name string `json:"name"`
	// Version is the generator's exact version.
	Version string `json:"version"`
}

// ChangePlanRepository is a credential-free repository identity.
type ChangePlanRepository struct {
	// Remote is the Git remote the URL was read from, such as "origin".
	Remote string `json:"remote"`
	// URL is the remote's URL without user info, query, or fragment.
	URL string `json:"url"`
}

// ChangePlanImpact holds both the projects that own a changed path and the
// full impact closure. TransitiveDependents is always
// DerivedTransitiveDependents().
type ChangePlanImpact struct {
	// DirectProjects owns at least one changed path, sorted by ID.
	DirectProjects []ChangePlanProject `json:"directProjects"`
	// Projects is the full impact closure, sorted by ID.
	Projects []ChangePlanProject `json:"projects"`
	// TransitiveDependents is Projects minus DirectProjects, sorted by ID.
	TransitiveDependents []ChangePlanProject `json:"transitiveDependents"`
}

// DerivedTransitiveDependents returns Projects minus DirectProjects, in the
// order of Projects: the only value a valid plan's TransitiveDependents holds.
func (impact ChangePlanImpact) DerivedTransitiveDependents() []ChangePlanProject {
	direct := make(map[string]bool, len(impact.DirectProjects))
	for _, project := range impact.DirectProjects {
		direct[project.ID] = true
	}
	out := make([]ChangePlanProject, 0, len(impact.Projects))
	for _, project := range impact.Projects {
		if !direct[project.ID] {
			out = append(out, project)
		}
	}
	return out
}

// ChangePlanProject is the stable project projection a plan carries.
type ChangePlanProject struct {
	// ID is the workspace project ID, such as "/tooling/cli".
	ID string `json:"id"`
	// Name is the project's declared name.
	Name string `json:"name"`
	// Path is the project directory relative to the workspace root, with
	// forward slashes.
	Path string `json:"path"`
}

// ChangePlanTask is one planned task projected onto admission metadata.
// Identity is the canonical protocol task identity, and DeadlineMS is the
// resolved hard subprocess deadline.
type ChangePlanTask struct {
	// Identity is the task's typed identity; Identity.Key orders Tasks.
	Identity protocolcli.TaskIdentity `json:"identity"`
	// DeadlineMS is the hard subprocess deadline in milliseconds, at least 1.
	DeadlineMS int `json:"deadlineMs"`
	// ResourceClass is the task's declared scheduling metadata.
	ResourceClass ChangePlanResourceClass `json:"resourceClass"`
	// DependsOn names, by identity key, the tasks whose results this task
	// consumes, sorted.
	DependsOn []string `json:"dependsOn,omitempty"`
	// SerializeAfter names, by identity key, the tasks this task must not
	// overlap, sorted.
	SerializeAfter []string `json:"serializeAfter,omitempty"`
	// ContractDigest is the digest of the task's declared contract, when the
	// task declares one.
	ContractDigest string `json:"contractDigest,omitempty"`
}

// ChangePlanResourceClass is the resolved scheduling metadata a task declares.
// CPUWeight is always materialized, 1 by default, so an absent declaration
// cannot make two equivalent plans serialize differently.
type ChangePlanResourceClass struct {
	// Heavy reports whether the task is scheduled as heavy.
	Heavy bool `json:"heavy"`
	// CPUWeight is the task's relative CPU demand, greater than 0.
	CPUWeight float64 `json:"cpuWeight"`
	// Reads is the resources the task reads, sorted by scope then ID.
	Reads []ChangePlanResource `json:"reads,omitempty"`
	// Writes is the resources the task writes, sorted by scope then ID.
	Writes []ChangePlanResource `json:"writes,omitempty"`
}

// ChangePlanResource names a declared read or write resource by its effective
// scope.
type ChangePlanResource struct {
	// ID is the resource name, such as "sources".
	ID string `json:"id"`
	// Scope is the resource's effective scope, such as "project".
	Scope string `json:"scope"`
}

// ChangePlanCache is the advisory local cache summary. Its entries are not
// digest-bound and never justify skipping a planned task.
type ChangePlanCache struct {
	// Status is "available", "disabled", or "unavailable".
	Status string `json:"status"`
	// Reason names why an "unavailable" summary has no entries. An
	// "available" summary has none.
	Reason string `json:"reason,omitempty"`
	// Entries is one cache hint per cacheable task, sorted by task key, and
	// empty unless Status is "available".
	Entries []ChangePlanCacheEntry `json:"entries,omitempty"`
}

// ChangePlanCacheEntry records a task's cache key and whether the local store
// held an interpretable entry at that key when the plan was built.
type ChangePlanCacheEntry struct {
	// TaskKey is the task's identity key.
	TaskKey string `json:"taskKey"`
	// Key is the task's cache key.
	Key string `json:"key"`
	// Present reports whether the local store held the entry.
	Present bool `json:"present"`
}

// changePlanDigestPayload is the digest input: the domain, then every plan
// member except Cache and Digest, in wire order.
type changePlanDigestPayload struct {
	// Domain is always ChangePlanDigestDomain.
	Domain string `json:"domain"`
	// Version is ChangePlan.Version.
	Version int `json:"version"`
	// Generator is ChangePlan.Generator.
	Generator ChangePlanGenerator `json:"generator"`
	// Repository is ChangePlan.Repository.
	Repository ChangePlanRepository `json:"repository"`
	// BaseSHA is ChangePlan.BaseSHA.
	BaseSHA string `json:"baseSHA"`
	// HeadSHA is ChangePlan.HeadSHA.
	HeadSHA string `json:"headSHA"`
	// ChangedFiles is ChangePlan.ChangedFiles.
	ChangedFiles []string `json:"changedFiles"`
	// Impact is ChangePlan.Impact.
	Impact ChangePlanImpact `json:"impact"`
	// Tasks is ChangePlan.Tasks.
	Tasks []ChangePlanTask `json:"tasks"`
}

// ChangePlanCanonicalBytes returns the exact bytes a ChangePlan digest covers:
// Go's encoding/json encoding of ChangePlanDigestDomain followed by every plan
// member except Cache and Digest, in wire order. It neither validates nor
// reorders, so a consumer can recompute the digest without replicating the
// struct layout.
func ChangePlanCanonicalBytes(plan ChangePlan) ([]byte, error) {
	return json.Marshal(changePlanDigestPayload{
		Domain:       ChangePlanDigestDomain,
		Version:      plan.Version,
		Generator:    plan.Generator,
		Repository:   plan.Repository,
		BaseSHA:      plan.BaseSHA,
		HeadSHA:      plan.HeadSHA,
		ChangedFiles: plan.ChangedFiles,
		Impact:       plan.Impact,
		Tasks:        plan.Tasks,
	})
}

// RecomputeChangePlanDigest returns "sha256:" followed by the lowercase hex
// SHA-256 of ChangePlanCanonicalBytes. It refuses a plan whose members are not
// canonical, so two spellings of one plan never produce two digests. It does
// not read plan.Digest.
func RecomputeChangePlanDigest(plan ChangePlan) (string, error) {
	if err := validateChangePlan(plan, false); err != nil {
		return "", err
	}
	bytes, err := ChangePlanCanonicalBytes(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateChangePlan is the check a consumer runs before it uses a plan for
// admission. It refuses an unknown version, an incomplete generator or
// repository, a repository URL that carries credentials, a revision that is
// not a full lowercase commit ID, a list that is unsorted or holds a
// duplicate, transitive dependents that differ from the derived ones, an
// inconsistent cache summary, and a digest that is malformed or does not match
// the canonical payload.
func ValidateChangePlan(plan ChangePlan) error {
	if err := validateChangePlan(plan, true); err != nil {
		return err
	}
	want, err := RecomputeChangePlanDigest(plan)
	if err != nil {
		return err
	}
	if plan.Digest != want {
		return fmt.Errorf("change plan digest does not match canonical payload")
	}
	return nil
}

func validateChangePlan(plan ChangePlan, requireDigest bool) error {
	const noun = "change plan"
	if plan.Version != ChangePlanVersion {
		return fmt.Errorf("unsupported change plan version %d", plan.Version)
	}
	if err := validatePlanGenerator(noun, plan.Generator); err != nil {
		return err
	}
	if plan.Repository.Remote == "" || plan.Repository.URL == "" || strings.Contains(plan.Repository.URL, "@") {
		return fmt.Errorf("change plan repository identity is incomplete or contains credentials")
	}
	body := planBody{BaseSHA: plan.BaseSHA, HeadSHA: plan.HeadSHA, ChangedFiles: plan.ChangedFiles,
		Impact: plan.Impact, Tasks: plan.Tasks, Cache: plan.Cache}
	if err := body.validate(noun); err != nil {
		return err
	}
	if requireDigest && !validChangePlanDigest(plan.Digest) {
		return fmt.Errorf("change plan digest is malformed")
	}
	return nil
}

// validatePlanGenerator refuses a generator without a name or a version. noun
// names the document in the refusal.
func validatePlanGenerator(noun string, generator ChangePlanGenerator) error {
	if generator.Name == "" || generator.Version == "" {
		return fmt.Errorf("%s generator is incomplete", noun)
	}
	return nil
}

// planBody is the members a ChangePlan and an ImpactPlan share after their
// identity members: the revision range, the files it changes, the closure they
// reach, the tasks planned over it, and the advisory cache summary.
type planBody struct {
	BaseSHA      string
	HeadSHA      string
	ChangedFiles []string
	Impact       ChangePlanImpact
	Tasks        []ChangePlanTask
	Cache        ChangePlanCache
}

// validate refuses a revision that is not a full lowercase commit ID, a list
// that is unsorted or holds a duplicate, transitive dependents that differ
// from the derived ones, and an inconsistent cache summary, in that order.
// noun names the document in the refusal.
func (body planBody) validate(noun string) error {
	if !fullCommitID(body.BaseSHA) || !fullCommitID(body.HeadSHA) {
		return fmt.Errorf("%s revisions must be full lowercase commit IDs", noun)
	}
	if !canonicalStringSlice(body.ChangedFiles) {
		return fmt.Errorf("%s changedFiles is not canonical", noun)
	}
	if !canonicalProjectSlice(body.Impact.DirectProjects) || !canonicalProjectSlice(body.Impact.Projects) ||
		!canonicalProjectSlice(body.Impact.TransitiveDependents) {
		return fmt.Errorf("%s project lists are not canonical", noun)
	}
	if !equalChangePlanProjects(body.Impact.TransitiveDependents, body.Impact.DerivedTransitiveDependents()) {
		return fmt.Errorf("%s transitive dependents do not match impact closure", noun)
	}
	if !canonicalTaskSlice(body.Tasks) {
		return fmt.Errorf("%s tasks are not canonical", noun)
	}
	return validateChangePlanCache(body.Cache)
}

// canonicalStringSlice reports whether values are non-empty, strictly
// ascending, and therefore free of duplicates.
func canonicalStringSlice(values []string) bool {
	for i, value := range values {
		if value == "" || (i > 0 && values[i-1] >= value) {
			return false
		}
	}
	return true
}

func canonicalProjectSlice(projects []ChangePlanProject) bool {
	for i, project := range projects {
		if project.ID == "" || project.Name == "" || (i > 0 && projects[i-1].ID >= project.ID) {
			return false
		}
	}
	return true
}

func canonicalTaskSlice(tasks []ChangePlanTask) bool {
	for i, task := range tasks {
		if task.Identity.Key == "" || task.DeadlineMS < 1 || task.ResourceClass.CPUWeight <= 0 ||
			!canonicalStringSlice(task.DependsOn) || !canonicalStringSlice(task.SerializeAfter) ||
			!canonicalResourceSlice(task.ResourceClass.Reads) || !canonicalResourceSlice(task.ResourceClass.Writes) ||
			(i > 0 && tasks[i-1].Identity.Key >= task.Identity.Key) {
			return false
		}
	}
	return true
}

// canonicalResourceSlice reports whether resources are complete and strictly
// ascending by scope, then by ID.
func canonicalResourceSlice(resources []ChangePlanResource) bool {
	for i, resource := range resources {
		if resource.ID == "" || resource.Scope == "" {
			return false
		}
		if i > 0 {
			previous := resources[i-1]
			if previous.Scope > resource.Scope || (previous.Scope == resource.Scope && previous.ID >= resource.ID) {
				return false
			}
		}
	}
	return true
}

func validateChangePlanCache(cache ChangePlanCache) error {
	switch cache.Status {
	case "available":
		if cache.Reason != "" {
			return fmt.Errorf("available cache summary has a reason")
		}
	case "disabled", "unavailable":
		if len(cache.Entries) > 0 {
			return fmt.Errorf("%s cache summary has entries", cache.Status)
		}
		if cache.Status == "unavailable" && cache.Reason == "" {
			return fmt.Errorf("unavailable cache summary has no reason")
		}
	default:
		return fmt.Errorf("unknown cache summary status %q", cache.Status)
	}
	for i, entry := range cache.Entries {
		if entry.TaskKey == "" || entry.Key == "" || (i > 0 && cache.Entries[i-1].TaskKey >= entry.TaskKey) {
			return fmt.Errorf("cache entries are not canonical")
		}
	}
	return nil
}

// fullCommitID reports whether value is a full SHA-1 or SHA-256 Git object ID
// in its one canonical spelling: 40 or 64 lowercase hexadecimal characters.
func fullCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validChangePlanDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func equalChangePlanProjects(left, right []ChangePlanProject) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
