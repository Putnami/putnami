package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// MissionsFormatVersion is the version of the missions document this package
// reads and writes. A document of another version is refused, never
// reinterpreted.
const MissionsFormatVersion = 1

const missionsFile = "missions.json"

// The owner's directories. The person who owns the store's missions writes
// one file per answer or message there; the provider only reads them.
const (
	// AnswersDir holds one answer per decision, named after the decision:
	// answers/D-1.json answers decision D-1.
	AnswersDir = "answers"
	// MessagesDir holds one message per file, under any name ending in .json.
	MessagesDir = "messages"
)

// MaxOwnerFileBytes bounds an owner file the store reads. A larger file is
// never read, so it is never taken.
const MaxOwnerFileBytes = 256 << 10

// Missions is the missions document: the missions, sessions, decisions and
// messages of one store directory. Ids only grow and are never reused.
type Missions struct {
	Version   int             `json:"version"`
	Next      MissionCounters `json:"next"`
	Missions  []Mission       `json:"missions"`
	Sessions  []Session       `json:"sessions"`
	Decisions []Decision      `json:"decisions"`
	Messages  []Message       `json:"messages"`
}

// MissionCounters allocate decision and message ids; they only grow.
type MissionCounters struct {
	Decision int `json:"decision"`
	Message  int `json:"message"`
}

// MissionKey names a mission: its repository and key, compared exactly.
type MissionKey struct {
	Repository string `json:"repository"`
	Key        string `json:"key"`
}

// Mission is what the store holds of a mission once a sync applied to it:
// its title and the plan the session that published last sent.
type Mission struct {
	MissionKey
	Title string            `json:"title,omitempty"`
	Plan  []collab.PlanStep `json:"plan"`
}

// Session is the last observation the store applied for one agent session.
type Session struct {
	Engine      string              `json:"engine"`
	ID          string              `json:"id"`
	Mission     MissionKey          `json:"mission"`
	State       collab.SessionState `json:"state"`
	ObservedAt  string              `json:"observedAt"`
	Event       string              `json:"event"`
	Model       string              `json:"model,omitempty"`
	Branch      string              `json:"branch,omitempty"`
	LastMessage string              `json:"lastMessage,omitempty"`
	Error       string              `json:"error,omitempty"`
	WaitUntil   string              `json:"waitUntil,omitempty"`
}

// Decision is one decision an agent asked the mission's owner.
type Decision struct {
	ID      string     `json:"id"`
	Mission MissionKey `json:"mission"`
	// IdempotencyKey and Digest are the ask that recorded the decision: its
	// key and a digest of its content.
	IdempotencyKey string                         `json:"idempotencyKey"`
	Digest         string                         `json:"digest"`
	Question       string                         `json:"question"`
	Context        string                         `json:"context,omitempty"`
	Options        []collab.DecisionOption        `json:"options"`
	Recommendation *collab.DecisionRecommendation `json:"recommendation,omitempty"`
	Blocks         string                         `json:"blocks,omitempty"`
	State          collab.DecisionState           `json:"state"`
	Answer         string                         `json:"answer,omitempty"`
	Option         string                         `json:"option,omitempty"`
	// Session is the session the decision is bound to, "<engine>/<id>": the
	// first one whose sync named it.
	Session    string `json:"session,omitempty"`
	AskedAt    string `json:"askedAt"`
	AnsweredAt string `json:"answeredAt,omitempty"`
}

// Message is one inbox message: one the owner wrote, or the answer to a
// decision.
type Message struct {
	ID      string           `json:"id"`
	Mission MissionKey       `json:"mission"`
	Kind    collab.InboxKind `json:"kind"`
	Text    string           `json:"text"`
	// Decision is the decision an answer message answers.
	Decision string `json:"decision,omitempty"`
	// Source is the owner file the message was taken from.
	Source      *MessageSource `json:"source,omitempty"`
	CreatedAt   string         `json:"createdAt"`
	DeliveredAt string         `json:"deliveredAt,omitempty"`
	Reply       string         `json:"reply,omitempty"`
	RepliedAt   string         `json:"repliedAt,omitempty"`
}

// MessageSource names the owner file a message was taken from: its name in
// MessagesDir and the SHA-256 of its content.
type MessageSource struct {
	File   string `json:"file"`
	Digest string `json:"digest"`
}

// OwnerFile is one file of an owner directory.
type OwnerFile struct {
	// Name is the file name, ending in .json.
	Name string
	Data []byte
}

func emptyMissions() *Missions {
	return &Missions{Version: MissionsFormatVersion, Missions: []Mission{}, Sessions: []Session{}, Decisions: []Decision{}, Messages: []Message{}}
}

// readMissions returns the missions document. A store whose missions were
// never written has none.
func (s *Store) readMissions() (*Missions, error) {
	path := filepath.Join(s.root, missionsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyMissions(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the local missions: %w", err)
	}
	missions := emptyMissions()
	if err := json.Unmarshal(data, missions); err != nil {
		return nil, fmt.Errorf("the local missions %s are not a valid missions document: %w", path, err)
	}
	if missions.Version != MissionsFormatVersion {
		return nil, fmt.Errorf("%w (missions format %d, this provider reads %d)", ErrUnsupportedFormat, missions.Version, MissionsFormatVersion)
	}
	return missions, nil
}

// UpdateMissions applies change to the missions document under the store's
// exclusive lock, the lock Update takes, and writes the result by atomic
// rename. When change returns an error nothing is written and that error is
// returned as is. A change that leaves the document as it was writes
// nothing.
func (s *Store) UpdateMissions(change func(*Missions) error) (*Missions, error) {
	var written *Missions
	err := s.locked(func() error {
		missions, err := s.readMissions()
		if err != nil {
			return &WriteError{Err: err}
		}
		before, err := json.Marshal(missions)
		if err != nil {
			return &WriteError{Err: fmt.Errorf("encode the local missions: %w", err)}
		}
		if err := change(missions); err != nil {
			return err
		}
		after, err := json.Marshal(missions)
		if err != nil {
			return &WriteError{Err: fmt.Errorf("encode the local missions: %w", err)}
		}
		if !bytes.Equal(after, before) {
			var encoded bytes.Buffer
			if err := json.Indent(&encoded, after, "", "  "); err != nil {
				return &WriteError{Err: fmt.Errorf("encode the local missions: %w", err)}
			}
			encoded.WriteByte('\n')
			if err := writeAtomically(s.root, missionsFile, encoded.Bytes()); err != nil {
				return err
			}
		}
		written = missions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// OwnerFiles returns the files of one owner directory in name order: the
// regular files whose name ends in .json and does not start with a dot, of
// at most MaxOwnerFileBytes. A directory that does not exist holds none. A
// file that disappears or cannot be read between the listing and the read is
// left out; it is read again on the next call.
func (s *Store) OwnerFiles(dir string) ([]OwnerFile, error) {
	path := filepath.Join(s.root, dir)
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the owner directory %s: %w", path, err)
	}
	var files []OwnerFile
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || !entry.Type().IsRegular() {
			continue
		}
		data, ok := readBounded(filepath.Join(path, name))
		if !ok {
			continue
		}
		files = append(files, OwnerFile{Name: name, Data: data})
	}
	return files, nil
}

// readBounded reads a file of at most MaxOwnerFileBytes.
func readBounded(path string) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxOwnerFileBytes+1))
	if err != nil || len(data) > MaxOwnerFileBytes {
		return nil, false
	}
	return data, true
}
