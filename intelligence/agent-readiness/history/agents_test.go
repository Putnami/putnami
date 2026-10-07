package history

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/gittest"
)

// TestAgentReadsTheAddressOnly pins that only the address names an agent:
// the exact addresses agent tools write, and the GitHub noreply slugs of
// their accounts. A name alone never does, because Claude is also a person,
// and the slugs a person can hold as a login, claude and cursor, need the
// [bot] suffix.
func TestAgentReadsTheAddressOnly(t *testing.T) {
	for _, tc := range []struct{ identity, want string }{
		{"noreply@anthropic.com", "claude"},
		{"Claude <noreply@anthropic.com>", "claude"},
		{"Claude Opus 5.5 (1M context) <noreply@anthropic.com>", "claude"},
		{"  Cursor Agent <cursoragent@cursor.com>  ", "cursor"},
		{"Aider <noreply@aider.chat>", "aider"},
		{"codex@openai.com", "codex"},
		{"NOREPLY@ANTHROPIC.COM", "claude"},
		{"198982749+Copilot@users.noreply.github.com", "copilot"},
		{"Copilot <198982749+Copilot@users.noreply.github.com>", "copilot"},
		{"copilot-swe-agent[bot]@users.noreply.github.com", "copilot"},
		{"claude[bot] <209825114+claude[bot]@users.noreply.github.com>", "claude"},
		{"cursor[bot]@users.noreply.github.com", "cursor"},
		{"209825114+claude@users.noreply.github.com", ""},
		{"Cursor <cursor@users.noreply.github.com>", ""},
		{"41898282+google-labs-jules[bot]@users.noreply.github.com", "jules"},
		{"Claude Smith <claude@example.com>", ""},
		{"Claude", ""},
		{"12345+alice@users.noreply.github.com", ""},
		{"alice@acme.example", ""},
		{"", ""},
	} {
		if got := Agent(tc.identity); got != tc.want {
			t.Errorf("Agent(%q) = %q, want %q", tc.identity, got, tc.want)
		}
	}
}

// TestCoAuthorKeepsPeopleOnly pins which Co-Authored-By trailers credit a
// person: an address that names no agent and no bot.
func TestCoAuthorKeepsPeopleOnly(t *testing.T) {
	for _, tc := range []struct{ identity, want string }{
		{"Bob <Bob@Acme.Example>", "bob@acme.example"},
		{"12345+alice@users.noreply.github.com", "12345+alice@users.noreply.github.com"},
		{"Claude <noreply@anthropic.com>", ""},
		{"dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>", ""},
		{"  ", ""},
	} {
		if got := coAuthor(tc.identity); got != tc.want {
			t.Errorf("coAuthor(%q) = %q, want %q", tc.identity, got, tc.want)
		}
	}
}

// TestReadRevertReadsHashesAndSubjects pins the two ways a revert names what
// it undoes: git's "This reverts commit <hash>" line, and the quoted subject
// a forge writes, with or without its squash suffix.
func TestReadRevertReadsHashesAndSubjects(t *testing.T) {
	var revert Revert
	for _, line := range []string{
		"This reverts commit 0123456789abcdef.",
		`Revert "feat: login"`,
		`  Revert "fix: typo" (#12)  `,
		`Revert "feat: signup (#11)" (#13)`,
		"feat: unrelated",
		"This reverts commit xyz, which is no id.",
		`Revert "unterminated`,
		`Reverted "feat: search"`,
	} {
		revert.ReadRevert(line)
	}
	if !slices.Equal(revert.Hashes, []string{"0123456789abcdef"}) {
		t.Errorf("hashes = %q, want the one id", revert.Hashes)
	}
	if want := []string{"feat: login", "fix: typo", "feat: signup (#11)"}; !slices.Equal(revert.Subjects, want) {
		t.Errorf("subjects = %q, want %q", revert.Subjects, want)
	}
}

// TestMarkRevertedPrefersIdsOverSubjects pins MarkReverted's rule: a revert
// undoes the commits whose id it names, and the branch commits of a merge it
// names. Only when no id names a commit of the window does it fall back to
// its quoted subject, and then it undoes the newest commit with that subject
// that is not newer than itself.
func TestMarkRevertedPrefersIdsOverSubjects(t *testing.T) {
	window := func() []Commit {
		return []Commit{
			{Hash: "c4aaaaaa", Subject: "chore: bump", Time: 40},
			{Hash: "c3aaaaaa", Subject: "chore: bump", Time: 30},
			{Hash: "c2aaaaaa", Subject: "chore: bump", Time: 20},
			{Hash: "c1aaaaaa", Subject: "feat: login", Time: 10},
			{Hash: "b2aaaaaa", Subject: "feat: branch two", Time: 8},
			{Hash: "b1aaaaaa", Subject: "feat: branch one", Time: 6},
		}
	}
	branches := map[string][]string{"m1aaaaaa": {"b2aaaaaa", "b1aaaaaa"}, "0ldaaaaa": {"0ldaaaaa"}}
	branch := func(hash string) []string { return branches[hash] }
	for _, tc := range []struct {
		name   string
		revert Revert
		want   []string
	}{
		{"an id wins over the subject", Revert{Time: 50, Hashes: []string{"c1aaaa"}, Subjects: []string{"chore: bump"}}, []string{"c1aaaaaa"}},
		{"an unknown id falls back to the newest older subject", Revert{Time: 35, Hashes: []string{"ffffffff"}, Subjects: []string{"chore: bump"}}, []string{"c3aaaaaa"}},
		{"a subject alone", Revert{Time: 25, Subjects: []string{"chore: bump"}}, []string{"c2aaaaaa"}},
		{"a subject newer than every revert", Revert{Time: 15, Subjects: []string{"chore: bump"}}, nil},
		{"a merge brings its branch", Revert{Time: 50, Hashes: []string{"m1aaaaaa"}}, []string{"b2aaaaaa", "b1aaaaaa"}},
		{"a known id outside the window keeps the subject out", Revert{Time: 35, Hashes: []string{"0ldaaaaa"}, Subjects: []string{"chore: bump"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commits := window()
			MarkReverted(commits, []Revert{tc.revert}, branch)
			var got []string
			for _, commit := range commits {
				if commit.Reverted {
					got = append(got, commit.Hash)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("reverted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadCreditsAgentsAndReverts pins what Read keeps of each commit: the
// subject, the agents a Co-Authored-By trailer in any case or the author
// address credits, each once, the people the other trailers credit, and
// whether a later commit reverted it: by id, by the id of the merge that
// brought it in, or, when a revert names no id, by its subject.
func TestReadCreditsAgentsAndReverts(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(hours int) time.Time { return now.Add(time.Duration(-hours) * time.Hour) }
	r := gittest.New(t)
	r.Write("a.txt", "one\n")
	hashes := map[string]string{}
	hashes["init"] = r.Commit("init", "dev@acme.example", at(90))
	r.Write("login.txt", "login\n")
	hashes["login"] = r.Commit("feat: login\n\nCo-authored-by: Claude <noreply@anthropic.com>", "dev@acme.example", at(80))
	r.Write("signup.txt", "signup\n")
	hashes["signup"] = r.Commit("feat: signup\n\n"+
		"co-authored-by: Claude <noreply@anthropic.com>\n"+
		"Co-Authored-By: Claude Opus <noreply@anthropic.com>\n"+
		"Co-authored-by: Bob <Bob@acme.example>\n"+
		"Co-authored-by: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>",
		"dev@acme.example", at(75))
	r.Write("search.txt", "search\n")
	hashes["search"] = r.Commit("feat: search\n\nCo-authored-by: Claude <noreply@anthropic.com>", "cursoragent@cursor.com", at(70))
	r.Write("profile.txt", "profile\n")
	hashes["profile"] = r.Commit("feat: profile\n\nCo-authored-by: Alice <alice@acme.example>", "198982749+Copilot@users.noreply.github.com", at(65))
	r.Write("person.txt", "person\n")
	hashes["person"] = r.Commit("feat: person", "claude@example.com", at(60))
	r.Write("bump.txt", "1\n")
	hashes["bump"] = r.Commit("chore: bump", "dev@acme.example", at(55))
	r.Write("login.txt", "")
	hashes["revert login"] = r.Commit("Revert \"feat: login\"\n\nThis reverts commit "+hashes["login"]+".", "dev@acme.example", at(50))
	r.Write("bump.txt", "")
	hashes["revert bump"] = r.Commit("Revert \"chore: bump\" (#9)", "dev@acme.example", at(45))
	r.Write("bump.txt", "1\n")
	hashes["bump again"] = r.Commit("chore: bump", "dev@acme.example", at(40))

	r.Git("checkout", "-q", "-b", "feature")
	r.Write("f1.txt", "1\n")
	hashes["branch one"] = r.Commit("feat: branch one", "dev@acme.example", at(35))
	r.Write("f2.txt", "2\n")
	hashes["branch two"] = r.Commit("feat: branch two", "dev@acme.example", at(34))
	r.Git("checkout", "-q", "main")
	merge := r.Merge("feature", "Merge pull request #7 from acme/feature", "dev@acme.example", at(30))
	r.Git("rm", "-q", "f1.txt", "f2.txt")
	hashes["revert merge"] = r.Commit("Revert \"Merge pull request #7 from acme/feature\"\n\nThis reverts commit "+merge+", reversing\nchanges made to "+hashes["bump again"]+".",
		"dev@acme.example", at(25))

	r.Git("checkout", "-q", "-b", "revert-signup")
	r.Write("signup.txt", "")
	hashes["drop signup"] = r.Commit("drop signup", "dev@acme.example", at(15))
	r.Git("checkout", "-q", "main")
	r.Merge("revert-signup", "Merge branch 'revert-signup' into 'main'\n\nRevert \"feat: signup\"", "dev@acme.example", at(10))

	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Read(context.Background(), repo, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]Commit{}
	for _, commit := range h.Commits {
		commits[commit.Hash] = commit
	}
	if len(commits) != len(hashes) {
		t.Fatalf("read %d commits, want %d", len(commits), len(hashes))
	}
	for _, tc := range []struct {
		name      string
		agents    []string
		coAuthors []string
		reverted  bool
	}{
		{"init", nil, nil, false},
		{"login", []string{"claude"}, nil, true},
		{"signup", []string{"claude"}, []string{"bob@acme.example"}, true},
		{"search", []string{"cursor", "claude"}, nil, false},
		{"profile", []string{"copilot"}, []string{"alice@acme.example"}, false},
		{"person", nil, nil, false},
		{"bump", nil, nil, true},
		{"revert login", nil, nil, false},
		{"revert bump", nil, nil, false},
		{"bump again", nil, nil, false},
		{"branch one", nil, nil, true},
		{"branch two", nil, nil, true},
		{"revert merge", nil, nil, false},
		{"drop signup", nil, nil, false},
	} {
		commit, ok := commits[hashes[tc.name]]
		if !ok {
			t.Fatalf("no commit %q in %+v", tc.name, h.Commits)
		}
		if !slices.Equal(commit.Agents, tc.agents) || !slices.Equal(commit.CoAuthors, tc.coAuthors) || commit.Reverted != tc.reverted {
			t.Errorf("%s = agents %v, co-authors %v, reverted %v; want %v, %v, %v",
				tc.name, commit.Agents, commit.CoAuthors, commit.Reverted, tc.agents, tc.coAuthors, tc.reverted)
		}
	}
	if subject := commits[hashes["login"]].Subject; subject != "feat: login" {
		t.Fatalf("subject = %q, want feat: login", subject)
	}
}
