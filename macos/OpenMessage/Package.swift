// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "OpenMessage",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "OpenMessage",
            path: "Sources",
            exclude: ["Info.plist"],
            resources: [
                .copy("Assets.xcassets"),
            ]
        ),
        .testTarget(
            name: "OpenMessageTests",
            dependencies: [.target(name: "OpenMessage")],
            path: "Tests/OpenMessageTests"
        ),
    ]
)
