import Testing
@testable import Calc

@Test func addition() {
    #expect(add(2, 3) == 5)
}

@Suite struct SignTests {
    @Suite struct NonNegative {
        @Test func positive() {
            #expect(sign(4) == "positive")
        }
    }
}

@Test func addWrongOnPurpose() {
    #expect(add(2, 2) == 5, "two plus two should be five")
}

@Test(.disabled("not ready")) func skipped() {}
