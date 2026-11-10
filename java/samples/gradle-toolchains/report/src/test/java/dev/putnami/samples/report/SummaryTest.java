package dev.putnami.samples.report;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.util.List;
import org.junit.jupiter.api.Test;

class SummaryTest {
    @Test
    void labelsTheTotal() {
        assertEquals("3 entries, 13.50 EUR", Summary.of(List.of(1000L, 500L, -150L)).label());
    }

    @Test
    void runsOnTheJava17Toolchain() {
        assertEquals("17", System.getProperty("java.specification.version"));
    }
}
