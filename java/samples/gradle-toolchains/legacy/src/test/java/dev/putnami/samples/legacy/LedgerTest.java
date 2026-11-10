package dev.putnami.samples.legacy;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.util.List;
import org.junit.jupiter.api.Test;

class LedgerTest {
    @Test
    void addsTheEntries() {
        assertEquals(1350, Ledger.total(List.of(1000L, 500L, -150L)));
    }

    @Test
    void runsOnTheJava11Toolchain() {
        assertEquals("11", System.getProperty("java.specification.version"));
    }
}
