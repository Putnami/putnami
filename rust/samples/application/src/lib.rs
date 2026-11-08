//! A checkout that applies a discount to a basket.

use rust_example_library::discount;

/// The total of the basket, in cents, after a discount of `percent` percent.
pub fn total(prices: &[u64], percent: u64) -> u64 {
    discount(prices.iter().sum(), percent)
}
