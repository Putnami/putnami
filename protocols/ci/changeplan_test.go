package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// changePlanGoldenDigest is the v1 digest of fixtures/change-plan/valid/golden.json.
// It is a literal rather than a read of expectations.json, so regenerating the
// corpus after a change to the canonical form still fails here: every v1 plan
// already emitted keeps its digest.
const changePlanGoldenDigest = "sha256:79846d85a6bdc8ec28b7576ca9b01abcc3cd275be4cdfa5b4992a01280da1040"

// changePlanExpectations is fixtures/change-plan/expectations.json, the corpus
// index: the canonical bytes of a valid fixture, the digest of every valid
// fixture, and the refusal of every invalid one.
type changePlanExpectations struct {
	Canonical map[string]string `json:"canonical"`
	Valid     map[string]string `json:"valid"`
	Invalid   map[string]string `json:"invalid"`
}

func readChangePlanExpectations(t *testing.T) changePlanExpectations {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(readFixture(t, "change-plan", "expectations.json")))
	decoder.DisallowUnknownFields()
	var index changePlanExpectations
	if err := decoder.Decode(&index); err != nil {
		t.Fatalf("decode expectations.json: %v", err)
	}
	return index
}

// readChangePlanFixture decodes one fixture and requires the file to be the
// exact indented encoding of the decoded plan, which pins the wire member
// names, their order, and which members are omitted when empty.
func readChangePlanFixture(t *testing.T, kind, name string) ChangePlan {
	t.Helper()
	data := readFixture(t, filepath.Join("change-plan", kind), name)
	var plan ChangePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("encode %s/%s: %v", kind, name, err)
	}
	if !bytes.Equal(append(encoded, '\n'), data) {
		t.Fatalf("%s/%s is not the wire encoding of the plan it holds:\n--- file ---\n%s\n--- encoded ---\n%s",
			kind, name, data, encoded)
	}
	return plan
}

func TestChangePlanCorpusMatchesExpectations(t *testing.T) {
	index := readChangePlanExpectations(t)
	for kind, listed := range map[string]map[string]string{"valid": index.Valid, "invalid": index.Invalid} {
		entries, err := os.ReadDir(filepath.Join("fixtures", "change-plan", kind))
		if err != nil {
			t.Fatal(err)
		}
		var onDisk, names []string
		for _, entry := range entries {
			onDisk = append(onDisk, entry.Name())
		}
		for name := range listed {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(onDisk) == 0 || !slices.Equal(onDisk, names) {
			t.Fatalf("fixtures/change-plan/%s holds %v but expectations.json lists %v", kind, onDisk, names)
		}
	}
}

func TestChangePlanValidCorpus(t *testing.T) {
	index := readChangePlanExpectations(t)
	for name, digest := range index.Valid {
		t.Run(name, func(t *testing.T) {
			plan := readChangePlanFixture(t, "valid", name)
			if err := ValidateChangePlan(plan); err != nil {
				t.Fatalf("ValidateChangePlan: %v", err)
			}
			recomputed, err := RecomputeChangePlanDigest(plan)
			if err != nil {
				t.Fatalf("RecomputeChangePlanDigest: %v", err)
			}
			if recomputed != digest || plan.Digest != digest {
				t.Fatalf("digest: recomputed %s, fixture %s, expected %s", recomputed, plan.Digest, digest)
			}
		})
	}
	for name, canonical := range index.Canonical {
		got, err := ChangePlanCanonicalBytes(readChangePlanFixture(t, "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != canonical {
			t.Fatalf("canonical bytes of %s:\n got %s\nwant %s", name, got, canonical)
		}
	}
}

func TestChangePlanInvalidCorpus(t *testing.T) {
	index := readChangePlanExpectations(t)
	for name, refusal := range index.Invalid {
		t.Run(name, func(t *testing.T) {
			err := ValidateChangePlan(readChangePlanFixture(t, "invalid", name))
			if err == nil || err.Error() != refusal {
				t.Fatalf("ValidateChangePlan = %v, want %q", err, refusal)
			}
		})
	}
}

func TestChangePlanV1IdentityIsUnchanged(t *testing.T) {
	if ChangePlanVersion != 1 || ChangePlanDigestDomain != "putnami/change-plan/v1" {
		t.Fatalf("version %d, domain %q: a new canonical form needs a new version", ChangePlanVersion, ChangePlanDigestDomain)
	}
	index := readChangePlanExpectations(t)
	if index.Valid["golden.json"] != changePlanGoldenDigest {
		t.Fatalf("expectations.json digest for golden.json = %s, want the v1 digest %s",
			index.Valid["golden.json"], changePlanGoldenDigest)
	}
	golden := readChangePlanFixture(t, "valid", "golden.json")
	if digest, err := RecomputeChangePlanDigest(golden); err != nil || digest != changePlanGoldenDigest {
		t.Fatalf("golden digest = %s (%v), want %s", digest, err, changePlanGoldenDigest)
	}
	// Cache is advisory and excluded from the digest.
	disabled := readChangePlanFixture(t, "valid", "golden-cache-disabled.json")
	if index.Valid["golden-cache-disabled.json"] != changePlanGoldenDigest || disabled.Digest != changePlanGoldenDigest {
		t.Fatalf("a different cache summary changed the digest: %s", disabled.Digest)
	}
}

// TestValidateChangePlanRefusals holds each refusal to its own defect: every
// case changes one member of the golden plan and re-stamps the digest of the
// result, so a refusal cannot come from a digest mismatch.
func TestValidateChangePlanRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ChangePlan)
		want   string
	}{
		{"generator", func(p *ChangePlan) { p.Generator.Version = "" }, "change plan generator is incomplete"},
		{"repository", func(p *ChangePlan) { p.Repository.Remote = "" }, "change plan repository identity is incomplete or contains credentials"},
		{"credentials", func(p *ChangePlan) { p.Repository.URL = "https://token@example.test/org/repo.git" },
			"change plan repository identity is incomplete or contains credentials"},
		{"non-hex base", func(p *ChangePlan) { p.BaseSHA = strings.Repeat("g", 40) }, "change plan revisions must be full lowercase commit IDs"},
		{"empty path", func(p *ChangePlan) { p.ChangedFiles = []string{""} }, "change plan changedFiles is not canonical"},
		{"unsorted direct projects", func(p *ChangePlan) {
			p.Impact.DirectProjects = []ChangePlanProject{p.Impact.Projects[2], p.Impact.Projects[0]}
		}, "change plan project lists are not canonical"},
		{"transitive dependents", func(p *ChangePlan) { p.Impact.TransitiveDependents = p.Impact.TransitiveDependents[:1] },
			"change plan transitive dependents do not match impact closure"},
		{"deadline", func(p *ChangePlan) { p.Tasks[0].DeadlineMS = 0 }, "change plan tasks are not canonical"},
		{"cpu weight", func(p *ChangePlan) { p.Tasks[0].ResourceClass.CPUWeight = 0 }, "change plan tasks are not canonical"},
		{"unsorted edges", func(p *ChangePlan) { slices.Reverse(p.Tasks[1].DependsOn) }, "change plan tasks are not canonical"},
		{"unsorted resources", func(p *ChangePlan) { slices.Reverse(p.Tasks[1].ResourceClass.Writes) }, "change plan tasks are not canonical"},
		{"resource without scope", func(p *ChangePlan) { p.Tasks[0].ResourceClass.Reads[0].Scope = "" }, "change plan tasks are not canonical"},
		{"available with reason", func(p *ChangePlan) { p.Cache.Reason = "ambient-input" }, "available cache summary has a reason"},
		{"disabled with entries", func(p *ChangePlan) { p.Cache.Status = "disabled" }, "disabled cache summary has entries"},
		{"unavailable without reason", func(p *ChangePlan) { p.Cache = ChangePlanCache{Status: "unavailable"} },
			"unavailable cache summary has no reason"},
		{"unknown cache status", func(p *ChangePlan) { p.Cache.Status = "warm" }, `unknown cache summary status "warm"`},
		{"unsorted cache entries", func(p *ChangePlan) { slices.Reverse(p.Cache.Entries) }, "cache entries are not canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := readChangePlanFixture(t, "valid", "golden.json")
			tc.mutate(&plan)
			canonical, err := ChangePlanCanonicalBytes(plan)
			if err != nil {
				t.Fatal(err)
			}
			plan.Digest = digestOfBytes(canonical)
			if err := ValidateChangePlan(plan); err == nil || err.Error() != tc.want {
				t.Fatalf("ValidateChangePlan = %v, want %q", err, tc.want)
			}
			if _, err := RecomputeChangePlanDigest(plan); err == nil || err.Error() != tc.want {
				t.Fatalf("RecomputeChangePlanDigest = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateChangePlanRefusesMalformedDigest(t *testing.T) {
	for _, digest := range []string{"", changePlanGoldenDigest[len("sha256:"):], "sha256:" + strings.Repeat("z", 64), changePlanGoldenDigest + "00"} {
		plan := readChangePlanFixture(t, "valid", "golden.json")
		plan.Digest = digest
		if err := ValidateChangePlan(plan); err == nil || err.Error() != "change plan digest is malformed" {
			t.Fatalf("digest %q: ValidateChangePlan = %v, want a malformed-digest refusal", digest, err)
		}
	}
}

func TestDerivedTransitiveDependentsKeepsClosureOrder(t *testing.T) {
	impact := ChangePlanImpact{
		DirectProjects: []ChangePlanProject{{ID: "/b", Name: "b"}},
		Projects:       []ChangePlanProject{{ID: "/a", Name: "a"}, {ID: "/b", Name: "b"}, {ID: "/c", Name: "c"}},
	}
	got := impact.DerivedTransitiveDependents()
	if len(got) != 2 || got[0].ID != "/a" || got[1].ID != "/c" {
		t.Fatalf("DerivedTransitiveDependents = %#v, want /a then /c", got)
	}
	if empty := (ChangePlanImpact{}).DerivedTransitiveDependents(); empty == nil || len(empty) != 0 {
		t.Fatalf("an empty closure derives %#v, want an empty non-nil list so it encodes as []", empty)
	}
}

func digestOfBytes(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
