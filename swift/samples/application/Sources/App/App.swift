import Foundation
import Hummingbird

/// The routes of the sample: GET / answers a greeting, HEAD / its status, and
/// GET /robots.txt the file the target ships as a resource.
func buildRouter() -> Router<BasicRequestContext> {
    let router = Router()
    router.get("/robots.txt") { _, _ -> String in
        guard let url = Bundle.module.url(forResource: "robots", withExtension: "txt", subdirectory: "Public") else {
            throw HTTPError(.notFound)
        }
        return try String(contentsOf: url, encoding: .utf8)
    }
    router.get("/") { _, _ in
        "Hello from Swift\n"
    }
    router.head("/") { _, _ in
        HTTPResponse.Status.ok
    }
    return router
}

/// Serves the routes on 0.0.0.0 and the port PORT names, 8080 without one.
@main
struct App {
    static func main() async throws {
        let port = ProcessInfo.processInfo.environment["PORT"].flatMap(Int.init) ?? 8080
        let app = Application(
            router: buildRouter(),
            configuration: .init(address: .hostname("0.0.0.0", port: port))
        )
        try await app.runService()
    }
}
