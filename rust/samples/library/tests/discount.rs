use library::discount;

#[test]
fn takes_the_percentage_off() {
    assert_eq!(discount(1000, 25), 750);
}

#[test]
fn never_goes_below_zero() {
    assert_eq!(discount(1000, 150), 0);
}
