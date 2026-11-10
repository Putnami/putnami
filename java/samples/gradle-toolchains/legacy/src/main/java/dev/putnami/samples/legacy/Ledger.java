package dev.putnami.samples.legacy;

import java.util.List;

/** The total of a ledger's entries, in cents. */
public final class Ledger {
    private Ledger() {}

    public static long total(List<Long> entries) {
        var total = 0L;
        for (var entry : entries) {
            total += entry;
        }
        return total;
    }
}
