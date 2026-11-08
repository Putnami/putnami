package dev.putnami.samples.shop;

import dev.putnami.samples.pricing.Discount;

/** The total of a cart, with its discount, as text. */
public final class Checkout {
    private final Discount discount;

    public Checkout(Discount discount) {
        this.discount = discount;
    }

    public String total(long... cents) {
        long sum = 0;
        for (long line : cents) {
            sum += line;
        }
        long total = discount.apply(sum);
        return String.format("%d.%02d", total / 100, total % 100);
    }
}
