// swift-tools-version:5.9
import PackageDescription

// The noskill app (FR-I8, ADR-0017). Builds with only the Command Line Tools: ./build-app.sh
let package = Package(
    name: "NoskillApp",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(name: "NoskillApp", path: "Sources/NoskillApp"),
        .testTarget(name: "NoskillAppTests", dependencies: ["NoskillApp"], path: "Tests/NoskillAppTests"),
    ]
)
