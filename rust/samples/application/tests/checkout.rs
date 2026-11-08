use rust_example_application::total;

#[test]
fn discounts_the_sum_of_the_basket() {
    assert_eq!(total(&[1000, 500], 10), 1350);
}
