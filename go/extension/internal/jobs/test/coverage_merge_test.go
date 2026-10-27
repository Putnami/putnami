package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

const coverageMergeRequirement = "coverage-profile-lists-each-block-once"

// These tests pin that the coverage profile a test run leaves lists each
// block once. `go test -coverpkg` writes the blocks of every instrumented
// package once per test binary, so a module with n test packages would leave
// a profile n times the size of its blocks, and the repeats carry no
// measurement the merged profile loses.

// repeatedBlocks returns the profile lines `go test -coverpkg` writes for
// two test binaries over the given packages: each package's two blocks,
// once per binary, the first binary hitting the first block only.
func repeatedBlocks(packages []string) []string {
	lines := make([]string, 0, 2*len(packages)*2)
	for binary := range 2 {
		for _, pkg := range packages {
			name := pkg[strings.LastIndexByte(pkg, '/')+1:]
			hit := "0"
			if binary == 0 {
				hit = "1"
			}
			lines = append(lines,
				pkg+"/"+name+".go:3.24,3.34 1 "+hit,
				pkg+"/"+name+".go:5.27,5.37 1 0",
			)
		}
	}
	return lines
}

// TestRunBatchMemberProfileListsEachBlockOnce pins the batch path under both
// coverage scopes: each member's coverage.out lists its blocks once, a block
// hit by one binary stays hit, and the reported coverage is that of the
// repeated profile.
func TestRunBatchMemberProfileListsEachBlockOnce(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", coverageMergeRequirement, "a-batch-members-profile-lists-each-block-once")
	t.Setenv(envDatabaseTestBindings, "")
	for _, scope := range []string{coverageScopeBatch, coverageScopeProject} {
		t.Run(scope, func(t *testing.T) {
			ctx := makeBatchTestContext(t, true)
			ctx.Params["coverage-scope"] = json.RawMessage(`"` + scope + `"`)
			mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
				packages := strings.Split(argumentAfter(t, args, "-coverpkg"), ",")
				profile := "mode: set\n" + strings.Join(repeatedBlocks(packages), "\n") + "\n"
				if err := os.WriteFile(argumentAfter(t, args, "-coverprofile"), []byte(profile), 0o644); err != nil {
					t.Fatal(err)
				}
				return []byte(passEvents(packages...)), nil
			})

			status, data, err := runBatch(ctx, nil)
			if err != nil || status != "OK" {
				t.Fatalf("runBatch status=%q err=%v", status, err)
			}
			for _, result := range data["batchResults"].([]batchProjectResult) {
				if result.Status != "OK" {
					t.Fatalf("project %s = %+v, want OK", result.ProjectID, result)
				}
				summary := coverageSummaryOf(t, result.Data)
				if summary["total"] != 2 || summary["covered"] != 1 {
					t.Errorf("project %s coverageSummary = %v, want 1 of 2 statements", result.ProjectID, summary)
				}
			}
			for _, name := range []string{"a", "b"} {
				assertFileBytes(t, filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", name, "test", "coverage.out"),
					"mode: set\n"+
						"example.com/"+name+"/"+name+".go:3.24,3.34 1 1\n"+
						"example.com/"+name+"/"+name+".go:5.27,5.37 1 0\n")
			}
		})
	}
}

// writeTwoTestPackageModule writes a module with two test packages, so
// `go test -coverpkg` over both writes each block twice. p's test calls P;
// q's test calls Q and P, so P runs once in each binary.
func writeTwoTestPackageModule(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{"p", "q"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/twotest\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "p", "p.go"),
		"package p\n\n// P is called by both test binaries.\nfunc P() int { return 1 }\n\n// PUnused has no caller.\nfunc PUnused() int { return 2 }\n")
	mustWrite(t, filepath.Join(dir, "p", "p_test.go"),
		"package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {\n\tif P() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
	mustWrite(t, filepath.Join(dir, "q", "q.go"),
		"package q\n\n// Q is called by q's test.\nfunc Q() int { return 1 }\n")
	mustWrite(t, filepath.Join(dir, "q", "q_test.go"),
		"package q\n\nimport (\n\t\"testing\"\n\n\t\"example.com/twotest/p\"\n)\n\n"+
			"func TestQ(t *testing.T) {\n\tif Q() != 1 || p.P() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
}

// TestRunSoloProfileListsEachBlockOnce pins the solo path against the real
// toolchain: the profile lists each of the module's three blocks once, the
// summary reports the same two of three statements, and in count mode the
// block both binaries ran counts both runs, as `go tool cover` adds them.
func TestRunSoloProfileListsEachBlockOnce(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", coverageMergeRequirement, "a-solo-profile-lists-each-block-once")
	t.Setenv(envDatabaseTestBindings, "")
	dir := t.TempDir()
	writeTwoTestPackageModule(t, dir)

	for _, mode := range []string{"", "count"} {
		t.Run("mode "+mode, func(t *testing.T) {
			params := pctx.Params{}
			if mode != "" {
				params["covermode"] = json.RawMessage(`"` + mode + `"`)
			}
			outDir := t.TempDir()
			status, data, err := Run(&pctx.Context{
				WorkspaceRoot: dir,
				OutputPath:    outDir,
				Project:       pctx.Project{Name: "twotest", FullPath: dir},
				Params:        params,
			}, jsonl.New(), nil)
			if err != nil || status != "OK" {
				t.Fatalf("Run status=%q err=%v", status, err)
			}
			profile, err := os.ReadFile(filepath.Join(outDir, "coverage.out"))
			if err != nil {
				t.Fatalf("Run wrote no profile: %v", err)
			}

			lines := strings.Split(strings.TrimSuffix(string(profile), "\n"), "\n")
			if len(lines) == 0 || !strings.HasPrefix(lines[0], "mode: ") {
				t.Fatalf("profile does not open with its mode line:\n%s", profile)
			}
			hits := map[string]string{}
			for _, line := range lines[1:] {
				fields := strings.Fields(line)
				if len(fields) != 3 {
					t.Fatalf("profile line %q is not a coverage block:\n%s", line, profile)
				}
				if _, seen := hits[fields[0]]; seen {
					t.Fatalf("profile lists %s more than once:\n%s", fields[0], profile)
				}
				hits[fields[0]] = fields[2]
			}
			if len(hits) != 3 {
				t.Fatalf("profile lists %d blocks, want the module's 3:\n%s", len(hits), profile)
			}

			summary := coverageSummaryOf(t, data)
			if summary["total"] != 3 || summary["covered"] != 2 {
				t.Errorf("coverageSummary = %v, want 2 of 3 statements", summary)
			}

			wantP := "1"
			if mode == "count" {
				wantP = "2"
			}
			for location, hit := range hits {
				if strings.HasPrefix(location, "example.com/twotest/p/p.go:4.") && hit != wantP {
					t.Errorf("P's block %s counts %s, want %s:\n%s", location, hit, wantP, profile)
				}
			}
		})
	}
}
