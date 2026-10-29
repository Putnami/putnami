package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// TestTheSharedContractScenarios runs the scenarios every tasks and proposals
// provider passes against a fresh local store.
func TestTheSharedContractScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-scenarios", "the-local-provider-passes-the-proposal-scenarios")
	spectest.Proves(t, "collaboration/provider-contract", "task-scenarios", "the-local-provider-passes-the-task-scenarios")
	newTarget := func(t *testing.T) providertest.Target {
		clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
		return providertest.Target{
			Handlers:      (&Provider{Now: func() time.Time { return clock }}).Handlers(),
			WorkspaceRoot: t.TempDir(),
			Base:          "main",
			Heads:         [2]string{"topic-a", "topic-b"},
		}
	}
	providertest.RunTasks(t, newTarget)
	providertest.RunProposals(t, newTarget)
}

// TestTheMissionsScenarios runs the scenarios every missions provider passes
// against a fresh local store on disk. The target posts a message and answers
// a decision the way the store's owner does: by writing a file to the store
// directory. The status scenario must run, not skip.
func TestTheMissionsScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-provider-semantics", "the-local-provider-passes-the-missions-scenarios")
	spectest.Proves(t, "collaboration/provider-contract", "missions-status", "the-local-provider-passes-the-missions-status-scenario")
	statusRan := false
	providertest.RunMissions(t, func(t *testing.T) providertest.Target {
		if strings.HasSuffix(t.Name(), "status_lists_the_sessions_of_a_mission,_newest_first") {
			t.Cleanup(func() { statusRan = !t.Skipped() })
		}
		clock := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
		p := &Provider{Now: func() time.Time { return clock }}
		workspace := t.TempDir()
		s := store.Open(filepath.Join(workspace, filepath.FromSlash(DefaultRoot)))
		posted := 0
		return providertest.Target{
			Handlers:      p.Handlers(),
			WorkspaceRoot: workspace,
			Post: func(t *testing.T, mission collab.MissionName, kind collab.InboxKind, text string) string {
				posted++
				name := "post-" + strconv.Itoa(posted) + ".json"
				writeOwnerFile(t, s, store.MessagesDir, name, ownerMessage{Mission: keyOf(mission), Kind: kind, Text: text})
				return takenMessage(t, p, s, name)
			},
			Answer: func(t *testing.T, decision, option, text string) {
				writeOwnerFile(t, s, store.AnswersDir, decision+".json", ownerAnswer{Option: option, Text: text})
			},
		}
	})
	if !statusRan {
		t.Error("the missions status scenario did not run against the local provider")
	}
}

// writeOwnerFile writes one owner file the way the README tells a person to.
func writeOwnerFile(t *testing.T, s *store.Store, dir, name string, content any) {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, s, dir, name, data)
}

func writeRaw(t *testing.T, s *store.Store, dir, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.Root(), dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// takenMessage takes the owner files now, as the next missions call would,
// and returns the id of the message the named file became.
func takenMessage(t *testing.T, p *Provider, s *store.Store, name string) string {
	t.Helper()
	id := ""
	_, err := s.UpdateMissions(func(m *store.Missions) error {
		if err := p.takeOwnerFiles(s, m); err != nil {
			return err
		}
		for _, message := range m.Messages {
			if message.Source != nil && message.Source.File == name {
				id = message.ID
			}
		}
		return nil
	})
	if err != nil || id == "" {
		t.Fatalf("the store did not take the message file %s: %v", name, err)
	}
	return id
}
