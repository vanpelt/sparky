// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Reframe",
    platforms: [.macOS(.v15)],
    products: [
        .executable(name: "Reframe", targets: ["ReframeApp"]),
        .executable(name: "reframe-cli", targets: ["ReframeCLI"]),
        .library(name: "ReframeKit", targets: ["ReframeKit"]),
    ],
    targets: [
        .target(name: "ReframeKit"),
        .executableTarget(name: "ReframeApp", dependencies: ["ReframeKit"]),
        .executableTarget(name: "ReframeCLI", dependencies: ["ReframeKit"]),
    ],
    swiftLanguageModes: [.v5]
)
