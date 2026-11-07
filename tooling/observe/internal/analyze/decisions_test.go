package analyze

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

func TestAddedModifiedAndRenamedRecordsAreListed(t *testing.T) {
	spectest.Proves(t, "tooling/observe-decisions", "decisions-in-the-evidence", "touched-and-governing-decisions-are-listed")
	head := map[string]string{
		"docs/adr/0001-kept.md":      "# Kept\n\n* Status: accepted\n",
		"docs/adr/0002-edited.md":    "---\nstatus: deprecated\nscope: src/app/[slug]\n---\n# Edited\n",
		"docs/adr/0003-new.md":       "# New\n\n* Status: proposed\n\n## Scope\n\n- src/api\n",
		"docs/archive/0004-moved.md": "# Moved\n\n* Status: superseded\n",
		"docs/adr/0005-untouched.md": "# Untouched\n\n* Status: accepted\n\n## Scope\n\n- src/other\n",
		"src/app/[slug]/page.tsx":    "x",
		"src/api/handler.go":         "x",
	}
	root := t.TempDir()
	for name, content := range head {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{
		Root: root,
		ChangedFiles: []gitbase.ChangedFile{
			{Path: "docs/adr/0002-edited.md", Status: gitbase.StatusModified},
			{Path: "docs/adr/0003-new.md", Status: gitbase.StatusAdded},
			{Path: "docs/archive/0004-moved.md", Status: gitbase.StatusRenamed, PreviousPath: "docs/adr/0004-moved.md"},
			{Path: "src/api/handler.go", Status: gitbase.StatusModified},
			{Path: "src/app/[slug]/page.tsx", Status: gitbase.StatusAdded},
		},
		Show: func(side Revision, file string) ([]byte, bool, error) {
			if side != Head {
				t.Errorf("%s read at the base", file)
			}
			content, ok := head[file]
			return []byte(content), ok, nil
		},
		DecisionDirs: []string{"docs/adr", "../outside"},
	}
	var r Result
	r.decisionRecords(in)
	want := []evidence.Decision{
		{Path: "docs/adr/0002-edited.md", Title: "Edited", Status: evidence.DecisionDeprecated, Scope: []string{"src/app/[slug]"}, Governs: []string{"src/app/[slug]/page.tsx"}, Change: evidence.FileModified},
		{Path: "docs/adr/0003-new.md", Title: "New", Status: evidence.DecisionProposed, Scope: []string{"src/api"}, Governs: []string{"src/api/handler.go"}, Change: evidence.FileAdded},
		{Path: "docs/archive/0004-moved.md", Title: "Moved", Status: evidence.DecisionSuperseded, Change: evidence.FileRenamed},
	}
	if !reflect.DeepEqual(r.Decisions, want) {
		t.Errorf("decisions =\n%+v\nwant\n%+v", r.Decisions, want)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeDecisionUnread || r.Diagnostics[0].Severity != diag.Warning {
		t.Errorf("diagnostics = %+v, want one decision_unread for ../outside", r.Diagnostics)
	}
}
