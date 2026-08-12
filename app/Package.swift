// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "OApp",
    platforms: [.macOS(.v15)],
    targets: [
        .executableTarget(
            name: "OApp",
            path: "Sources/OApp",
            linkerSettings: [.linkedLibrary("sqlite3")]
        ),
        .testTarget(
            name: "OAppTests",
            dependencies: ["OApp"],
            path: "Tests/OAppTests"
        ),
    ]
)
