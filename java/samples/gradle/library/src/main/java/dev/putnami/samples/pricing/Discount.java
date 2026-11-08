package dev.putnami.samples.pricing;

/** A percentage taken off an amount in cents. */
public final class Discount {
    private final int percent;

    public Discount(int percent) {
        if (percent < 0 || percent > 100) {
            throw new IllegalArgumentException("a discount is between 0 and 100 percent");
        }
        this.percent = percent;
    }

    public long apply(long cents) {
        return cents - cents * percent / 100;
    }
}
