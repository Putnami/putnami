package dev.putnami.samples.report;

import dev.putnami.samples.legacy.Ledger;
import java.util.List;

/** A ledger's summary: its entry count and total, in cents. */
public record Summary(int entries, long total) {
    public static Summary of(List<Long> entries) {
        return new Summary(entries.size(), Ledger.total(entries));
    }

    public String label() {
        return """
            %d entries, %d.%02d EUR""".formatted(entries, total / 100, total % 100);
    }
}
