//! Price reductions.

/// Takes `percent` percent off `cents`, rounding down.
pub fn discount(cents: u64, percent: u64) -> u64 {
    cents - cents * percent.min(100) / 100
}
