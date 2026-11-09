import Hummingbird
import HummingbirdTesting
import XCTest

@testable import App

final class AppTests: XCTestCase {
    func testRootAnswersAGreeting() async throws {
        let app = Application(router: buildRouter())
        try await app.test(.router) { client in
            try await client.execute(uri: "/", method: .get) { response in
                XCTAssertEqual(response.status, .ok)
                XCTAssertEqual(String(buffer: response.body), "Hello from Swift\n")
            }
        }
    }
}
