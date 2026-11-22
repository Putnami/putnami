import XCTest
@testable import Calc

final class CalcTests: XCTestCase {
    func testAdd() {
        XCTAssertEqual(add(2, 3), 5)
    }

    func testSignOfNonNegativeInput() {
        XCTAssertEqual(sign(4), "positive")
    }

    func testAddWrongOnPurpose() {
        XCTAssertEqual(add(2, 2), 5, "two plus two should be five")
    }

    func testSkipped() throws {
        throw XCTSkip("not ready")
    }
}
