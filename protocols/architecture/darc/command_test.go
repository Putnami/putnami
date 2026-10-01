package darc

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
)

// usageRecord is one command payload with no meaning of its own.
type usageRecord struct {
	Command string
}

// commandContract is the shape the protocol requires of a command import: one
// transport, and no projection machinery. It is the CLI's own usage-telemetry
// contract, whose justification says a refused or unreachable ingest never fails
// a run — which is exactly the obligation Emit exists to keep.
func commandContract(mutate ...func(*archproto.Import)) archproto.Import {
	contract := archproto.Import{
		ID:      "cli.usage-telemetry.v1",
		Version: 1,
		From:    archproto.ExportReference{Domain: "observability", Export: "observability.usage-ingest.v1"},
		As:      "cli.usage-telemetry",
		Mode:    archproto.ModeCommand,
		Status:  archproto.StatusActive,
		Facts:   []string{"usage_ingest"},
		Transport: &archproto.Transport{
			Kind: archproto.TransportEvent, Contract: "putnami.cli-usage.otlp-logs.v1",
			Availability: archproto.StatusActive,
		},
		Justification: "After the one-time notice, the CLI fire-and-forgets anonymous usage records to the ingest.",
	}
	for _, apply := range mutate {
		apply(&contract)
	}
	return contract
}

// TestCommandRefusesAContractThatIsNotLive is the refusal that keeps a target
// design from quietly becoming traffic: a planned import, or one carried by a
// planned transport, cannot be sent over.
func TestCommandRefusesAContractThatIsNotLive(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-command-refuses-a-contract-that-is-not-live")
	silent := func(context.Context, usageRecord) error { return nil }

	planned := commandContract(func(i *archproto.Import) {
		i.Status = archproto.StatusPlanned
	})
	if _, err := NewCommand(planned, silent); !errors.Is(err, ErrNotActive) {
		t.Errorf("err = %v, want ErrNotActive for a planned import", err)
	}

	// A legacy import is live enough for the protocol and not for this: the
	// component enforces the contract's own lifecycle, and only `active` means
	// "this is how it works today".
	legacy := commandContract(func(i *archproto.Import) {
		i.Status = archproto.StatusLegacy
	})
	if _, err := NewCommand(legacy, silent); !errors.Is(err, ErrNotActive) {
		t.Errorf("err = %v, want ErrNotActive for a legacy import", err)
	}

	if _, err := NewCommand[usageRecord](commandContract(), nil); err == nil {
		t.Error("a command with no carrier was accepted")
	}
	if _, err := NewCommand(commandContract(func(i *archproto.Import) {
		i.Mode = archproto.ModeQuery
	}), silent); err == nil {
		t.Error("a query contract was accepted by the command component")
	}
}

// TestCommandSendSurfacesTheCarrierVerdict pins the acknowledged shape: the
// caller's own outcome depends on the request being accepted, so the error
// reaches it.
func TestCommandSendSurfacesTheCarrierVerdict(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"an-acknowledged-send-surfaces-the-carrier-verdict")
	refused := errors.New("ingest refused")
	command, err := NewCommand(commandContract(), func(context.Context, usageRecord) error { return refused })
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Send(context.Background(), usageRecord{Command: "build"}); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the carrier's own error", err)
	}
	if attempted, failed := command.Stats(); attempted != 1 || failed != 1 {
		t.Errorf("stats = (%d, %d), want one attempt and one failure", attempted, failed)
	}
}

// TestCommandEmitNeverFailsTheCaller is the fire-and-forget obligation, stated
// as a test: the delivery failure reaches the observer and the caller carries on.
// "A refused or unreachable ingest never fails a run" is a sentence in a
// manifest until something makes it true.
func TestCommandEmitNeverFailsTheCaller(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "runtime-enforcement",
		"a-fire-and-forget-send-never-fails-the-caller")
	unreachable := errors.New("ingest unreachable")
	var observed []error
	command, err := NewCommand(commandContract(),
		func(context.Context, usageRecord) error { return unreachable },
		WithFailureObserver[usageRecord](func(err error) { observed = append(observed, err) }))
	if err != nil {
		t.Fatal(err)
	}

	command.Emit(context.Background(), usageRecord{Command: "build"})

	if len(observed) != 1 || !errors.Is(observed[0], unreachable) {
		t.Fatalf("observed = %v, want the delivery failure", observed)
	}
	if attempted, failed := command.Stats(); attempted != 1 || failed != 1 {
		t.Errorf("stats = (%d, %d), want one attempt and one failure", attempted, failed)
	}
}

// TestCommandEmitIsSilentWithoutAnObserver pins that a fire-and-forget send
// without an observer is silent — which is what fire-and-forget means, and why
// the counters exist so "we sent nothing" and "everything failed" do not look
// alike.
func TestCommandEmitIsSilentWithoutAnObserver(t *testing.T) {
	command, err := NewCommand(commandContract(),
		func(context.Context, usageRecord) error { return errors.New("gone") })
	if err != nil {
		t.Fatal(err)
	}
	command.Emit(context.Background(), usageRecord{Command: "test"})
	if attempted, failed := command.Stats(); attempted != 1 || failed != 1 {
		t.Errorf("stats = (%d, %d), want the failure counted even with nobody watching", attempted, failed)
	}
}

// TestCommandHonoursContextCancellation pins that a canceled caller does not
// reach the carrier at all, on either shape.
func TestCommandHonoursContextCancellation(t *testing.T) {
	sent := 0
	command, err := NewCommand(commandContract(), func(context.Context, usageRecord) error {
		sent++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := command.Send(ctx, usageRecord{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Send err = %v, want context.Canceled", err)
	}
	command.Emit(ctx, usageRecord{})
	if sent != 0 {
		t.Errorf("the carrier ran %d time(s) under a canceled context", sent)
	}
	if _, failed := command.Stats(); failed != 1 {
		t.Errorf("failed = %d, want the canceled emit counted as a failure", failed)
	}
	if got := command.Contract().ID; got != "cli.usage-telemetry.v1" {
		t.Errorf("contract = %q, want the declaration the component enforces", got)
	}
}
