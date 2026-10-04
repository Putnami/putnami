package sessionreporter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// Resolver prepares the provider launch of one capability.
type Resolver func(Capability) Resolve

// Failure is one capability's start or delivery failure. It never carries
// provider output, only core-owned reason strings.
type Failure struct {
	Capability Capability
	Err        error
}

// Runs is every capability one session started, each an independent subscriber
// of the session event stream.
type Runs struct {
	runs []*Run
}

// Selected returns the capabilities the context's captured selectors name, in
// Capabilities order.
func Selected(ctx context.Context) []Capability {
	var selected []Capability
	for _, capability := range Capabilities() {
		if capability.Provider(ctx) != "" {
			selected = append(selected, capability)
		}
	}
	return selected
}

// StartSelected starts every selected capability as its own subscriber of
// events, one after another so that two capabilities served by one extension
// never prepare its runtime concurrently. A capability that cannot start is
// reported in the failures and never prevents another from starting; one that
// reached its checkpoint still records its evidence at Finish. Each run adopts
// what holders started for its capability before repository code; holders
// may be nil.
func StartSelected(ctx context.Context, events *sessionstream.Log, sessionID string, resolve Resolver, holders *Holders) (*Runs, []Failure) {
	runs := &Runs{}
	var failures []Failure
	for _, capability := range Selected(ctx) {
		run, err := startRun(ctx, capability, events, sessionID, resolve(capability), holders)
		if err != nil {
			failures = append(failures, Failure{capability, err})
		}
		if run != nil {
			runs.runs = append(runs.runs, run)
		}
	}
	return runs, failures
}

// Finish drains every run at once, each under its own finalization limits, and
// records each run's evidence. It returns the runs whose delivery is
// incomplete, in start order. One run's failure never changes another run's
// delivery or evidence.
func (rs *Runs) Finish() []Failure {
	if rs == nil {
		return nil
	}
	errs := make([]error, len(rs.runs))
	var wg sync.WaitGroup
	for i, run := range rs.runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = run.Finish()
		}()
	}
	wg.Wait()
	var failures []Failure
	for i, err := range errs {
		if err != nil {
			failures = append(failures, Failure{rs.runs[i].capability, err})
		}
	}
	return failures
}

// ReplaySelected replays every selected capability whose recorded evidence is
// not delivered, each from its own acknowledged cursor, with the caller's fresh
// credentials. Each capability's provider setup is bounded on its own, and its
// delivery drains under the finalization limits of a run (Run.Finish). A
// canceled ctx stops every replay at once. A delivered capability is left
// alone, so its subscribers.json entry is not rewritten. It requires a real
// finalized session, since executor loss never invents a terminal document.
// Providers are prepared one after another, then every replay drains at once.
// It fails when no capability is selected, and joins the failures of the
// capabilities it replayed, each naming its capability.
//
// A replay runs no repository code. On a hosted run, it first starts the
// reporters it replays as holders (Holders.Start) and hands report each one
// that does not hold the run credential.
func ReplaySelected(ctx context.Context, dir, sessionID string, resolve Resolver, report func(Failure)) error {
	selected := Selected(ctx)
	if len(selected) == 0 {
		envs := make([]string, 0, len(Capabilities()))
		for _, capability := range Capabilities() {
			envs = append(envs, capability.SelectorEnv)
		}
		return fmt.Errorf("%s must select a reporter extension", strings.Join(envs, " or "))
	}
	if err := validateFinalizedSession(dir, sessionID); err != nil {
		return err
	}
	delivered := map[string]bool{}
	if evidence, err := sessionstream.ReadEvidence(dir); err == nil && evidence.SessionID == sessionID {
		for _, entry := range evidence.Subscribers {
			delivered[entry.Name] = entry.Evidence == protocolcli.SubscriberEvidenceDelivered
		}
	}
	var replayed []Capability
	for _, capability := range selected {
		if !delivered[capability.Name] {
			replayed = append(replayed, capability)
		}
	}
	holders := &Holders{}
	defer holders.Close()
	for _, failure := range holders.Start(ctx, replayed, resolve) {
		report(failure)
	}
	errs := make([]error, len(selected))
	finishes := make([]func() error, len(selected))
	for i, capability := range selected {
		if !delivered[capability.Name] {
			finishes[i], errs[i] = startReplay(ctx, capability, dir, sessionID, resolve(capability), holders)
		}
	}
	var wg sync.WaitGroup
	for i, finish := range finishes {
		if finish == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = finish()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// startReplay starts one capability's replay. It returns the function that
// drains it under the finalization limits, or the error that kept it from
// starting. A canceled ctx stops the replay at once.
func startReplay(ctx context.Context, capability Capability, dir, sessionID string, resolve Resolve, holders *Holders) (func() error, error) {
	events, err := sessionstream.Open(dir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("%s: session has no readable event stream", capability.Label)
	}
	r, err := startRun(ctx, capability, events, sessionID, resolve, holders)
	if err != nil {
		if r != nil {
			_ = r.recordEvidence()
		}
		return nil, err
	}
	stop := context.AfterFunc(ctx, r.cancel)
	return func() error {
		defer stop()
		if err := r.Finish(); err != nil {
			return fmt.Errorf("%s: %w", capability.Label, err)
		}
		return nil
	}, nil
}
