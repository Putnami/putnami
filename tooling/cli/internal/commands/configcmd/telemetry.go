package configcmd

import (
	"encoding/json"
	"os"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/telemetry"
)

// TelemetryOn enables telemetry collection.
func TelemetryOn() error {
	if err := telemetry.Enable(); err != nil {
		return err
	}
	iox.Fprintln(os.Stdout, "  Telemetry enabled.")
	iox.Fprintln(os.Stdout, "  Minimal anonymous usage data is recorded after the first-run notice.")
	iox.Fprintln(os.Stdout, "  It includes command names, counts, durations, and a rotating random ID;")
	iox.Fprintln(os.Stdout, "  never code, paths, or personal details.")
	iox.Fprintln(os.Stdout, "  Inspect them with 'putnami telemetry show'.")
	iox.Fprintln(os.Stdout, "  Details and your rights: https://putnami.dev/docs/concepts/cli-telemetry")
	return nil
}

// TelemetryOff disables telemetry collection and removes buffered data.
func TelemetryOff() error {
	if err := telemetry.Disable(); err != nil {
		return err
	}
	iox.Fprintln(os.Stdout, "  Telemetry disabled. Buffered data removed.")
	return nil
}

// TelemetryStatus shows the current telemetry status.
func TelemetryStatus() error {
	cfg := telemetry.Status()
	state := telemetry.EffectiveState()
	iox.Fprintln(os.Stdout)
	if state.Enabled {
		iox.Fprintln(os.Stdout, "  Telemetry: enabled")
	} else {
		iox.Fprintln(os.Stdout, "  Telemetry: disabled")
	}
	iox.Fprintf(os.Stdout, "  Effective rule: %s\n", state.Decision)
	if cfg.DeviceIDMonth != "" {
		iox.Fprintf(os.Stdout, "  Device ID month: %s\n", cfg.DeviceIDMonth)
	} else {
		iox.Fprintln(os.Stdout, "  Device ID month: not created yet")
	}
	if cfg.NoticeShownAt != "" {
		iox.Fprintf(os.Stdout, "  Notice shown: %s\n", cfg.NoticeShownAt)
	}
	iox.Fprintln(os.Stdout)
	return nil
}

// TelemetryShow displays buffered telemetry events.
func TelemetryShow(outputFormat string) error {
	events, err := telemetry.Show()
	if err != nil {
		return err
	}

	if len(events) == 0 {
		iox.Fprintln(os.Stdout, "  No buffered telemetry events.")
		return nil
	}

	if outputFormat == "jsonl" {
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			iox.Fprintln(os.Stdout, string(data))
		}
		return nil
	}

	iox.Fprintf(os.Stdout, "\n  Buffered events: %d\n\n", len(events))
	for _, ev := range events {
		iox.Fprintf(os.Stdout, "  %s  %s\n", ev.Timestamp, ev.Name)
		if ev.Data != nil {
			for k, v := range ev.Data {
				iox.Fprintf(os.Stdout, "    %s: %v\n", k, v)
			}
		}
	}
	iox.Fprintln(os.Stdout)
	return nil
}
