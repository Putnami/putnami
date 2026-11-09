// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "application",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "App", targets: ["App"])
    ],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.0")
    ],
    targets: [
        .executableTarget(
            name: "App",
            dependencies: [.product(name: "Hummingbird", package: "hummingbird")]
        ),
        .testTarget(
            name: "AppTests",
            dependencies: [
                "App",
                .product(name: "HummingbirdTesting", package: "hummingbird"),
            ]
        ),
    ]
)
