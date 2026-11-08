package dev.putnami.samples.pricing;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

import org.junit.jupiter.api.Test;

class DiscountTest {
    @Test
    void takesThePercentOff() {
        assertEquals(900, new Discount(10).apply(1000));
    }

    @Test
    void refusesMoreThanAll() {
        assertThrows(IllegalArgumentException.class, () -> new Discount(101));
    }
}
