package dev.putnami.samples.shop;

import static org.junit.jupiter.api.Assertions.assertEquals;

import dev.putnami.samples.pricing.Discount;
import org.junit.jupiter.api.Test;

class CheckoutTest {
    @Test
    void discountsTheSumOfTheLines() {
        assertEquals("13.50", new Checkout(new Discount(10)).total(1000, 500));
    }
}
