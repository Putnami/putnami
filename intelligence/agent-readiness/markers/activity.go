package markers

import (
	"path"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
)

// Activity is what the window's commits did in one area, or in the whole
// repository.
type Activity struct {
	// Commits counts the commits that changed the area.
	Commits int
	// Authors holds the lowercase emails of their authors. It never leaves
	// the machine.
	Authors map[string]bool
	// Agents holds the agent tools those commits credit, in a Co-Authored-By
	// trailer or as their author. It never leaves the machine.
	Agents map[string]bool
	// CoAuthors holds the lowercase emails of the people Co-Authored-By
	// trailers credit. It never leaves the machine.
	CoAuthors map[string]bool
	// AgentCommits counts the commits that credit an agent, and
	// AgentReverted those of them a later commit of the window reverted.
	AgentCommits  int
	AgentReverted int
	// CrossArea counts the commits that also changed another area.
	CrossArea int
	// AreaCommits counts the commits that changed an area other than the
	// root. Only the repository total is set.
	AreaCommits int
	// CodeCommits counts the commits that changed code other than tests;
	// CodeWithTest counts those that also changed a test.
	CodeCommits  int
	CodeWithTest int
	// LastCode is the time of the newest commit that changed code, zero when
	// none did.
	LastCode        int64
	FileChanges     int
	DeclaredChanges int
}

// Measure counts recent commit activity for the repository and its areas.
func Measure(layout areas.Layout, commits []history.Commit, present func(path string) bool) (Activity, []Activity) {
	repo := Activity{Authors: map[string]bool{}, Agents: map[string]bool{}}
	perArea := make([]Activity, len(layout.Areas))
	for i := range perArea {
		perArea[i].Authors = map[string]bool{}
		perArea[i].Agents = map[string]bool{}
	}
	rootOnly := len(layout.Areas) == 1
	isRoot := func(area int) bool { return !rootOnly && layout.Areas[area].Path == areas.RootPath }
	supporting := func(area int) bool {
		return layout.Areas[area].Role == contract.AreaTests || layout.Areas[area].Role == contract.AreaDocs
	}
	for _, commit := range commits {
		repo.Commits++
		repo.Authors[commit.Author] = true
		repo.credit(commit)
		touched := map[int]bool{}
		code := map[int]bool{}
		tests := map[int]bool{}
		// mirrored holds the directories whose code the commit's test trees
		// test: "." for a root-level tree such as spec/.
		mirrored := map[string]bool{}
		for _, file := range commit.Files {
			area := layout.Assign(file.Path)
			if area < 0 {
				continue
			}
			touched[area] = true
			if inventory.Language(file.Path) != "" && !hidden(file.Path) && (present == nil || present(file.Path)) {
				repo.FileChanges++
				if layout.Areas[area].Source == contract.AreaFromManifest {
					repo.DeclaredChanges++
				}
			}
			switch {
			case IsTest(file.Path):
				tests[area] = true
				if tree, ok := areas.TestTree(file.Path); ok {
					mirrored[path.Dir(tree)] = true
				}
			case inventory.Language(file.Path) != "":
				code[area] = true
			}
		}
		for area := range code {
			for dir := range mirrored {
				if dir == "." || layout.Areas[area].Path == dir || strings.HasPrefix(layout.Areas[area].Path, dir+"/") {
					tests[area] = true
				}
			}
		}
		// The root area holds shared configuration, and a test tree or a
		// documentation site supports the code beside it: touching either
		// alongside a code area is not a cross-area change.
		owned, codeAreas := 0, 0
		for area := range touched {
			if isRoot(area) {
				continue
			}
			owned++
			if !supporting(area) {
				codeAreas++
			}
		}
		for area := range touched {
			if isRoot(area) && owned > 0 {
				continue
			}
			activity := &perArea[area]
			activity.Commits++
			activity.Authors[commit.Author] = true
			activity.credit(commit)
			switch {
			case isRoot(area):
			case supporting(area) && owned > 1:
				activity.CrossArea++
			case !supporting(area) && codeAreas > 1:
				activity.CrossArea++
			}
			if code[area] {
				activity.CodeCommits++
				if tests[area] {
					activity.CodeWithTest++
				}
				activity.LastCode = max(activity.LastCode, commit.Time)
			}
		}
		if owned > 0 {
			repo.AreaCommits++
		}
		if codeAreas > 1 {
			repo.CrossArea++
		}
		if len(code) > 0 {
			repo.CodeCommits++
			if len(tests) > 0 {
				repo.CodeWithTest++
			}
			repo.LastCode = max(repo.LastCode, commit.Time)
		}
	}
	return repo, perArea
}

// credit records the people and the agents a commit's trailers credit.
func (a *Activity) credit(commit history.Commit) {
	for _, person := range commit.CoAuthors {
		if a.CoAuthors == nil {
			a.CoAuthors = map[string]bool{}
		}
		a.CoAuthors[person] = true
	}
	if len(commit.Agents) == 0 {
		return
	}
	if a.Agents == nil {
		a.Agents = map[string]bool{}
	}
	a.AgentCommits++
	if commit.Reverted {
		a.AgentReverted++
	}
	for _, agent := range commit.Agents {
		a.Agents[agent] = true
	}
}

// Contributors counts who changed the area, people and agents: each person
// who authored a commit or is credited in a Co-Authored-By trailer, and each
// agent credited in a trailer or as author. A person who runs an agent under
// their own name, without a trailer, counts once.
func (a Activity) Contributors() int {
	people := len(a.CoAuthors)
	for author := range a.Authors {
		if history.Agent(author) == "" && !a.CoAuthors[author] {
			people++
		}
	}
	return people + len(a.Agents)
}

// IsTest reports whether a path holds tests, by the naming conventions of
// the common test runners. Only source files count: a snapshot, a fixture
// in YAML or a tsconfig.spec.json sits next to tests without being one, and
// nothing under a dot directory such as .gitlab or .vscode is a test.
func IsTest(file string) bool {
	if inventory.Language(file) == "" {
		return false
	}
	dirs := strings.Split(path.Dir(file), "/")
	for _, segment := range dirs {
		if strings.HasPrefix(segment, ".") && segment != "." {
			return false
		}
	}
	base := path.Base(file)
	switch {
	case strings.HasSuffix(base, "_test.go"), strings.HasSuffix(base, "_test.py"), strings.HasSuffix(base, "_spec.rb"),
		strings.HasSuffix(base, ".tftest.hcl"), strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.Contains(base, ".test."), strings.Contains(base, ".spec."),
		strings.HasSuffix(base, "Test.java"), strings.HasSuffix(base, "Tests.java"), strings.HasSuffix(base, "Test.kt"):
		return true
	}
	_, inTree := areas.TestTree(file)
	return inTree
}

// hidden reports whether a path lies under a directory whose name starts
// with a dot.
func hidden(file string) bool {
	for _, segment := range strings.Split(path.Dir(file), "/") {
		if strings.HasPrefix(segment, ".") && segment != "." {
			return true
		}
	}
	return false
}
