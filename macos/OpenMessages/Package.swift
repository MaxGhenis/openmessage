// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "OpenMessages",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "OpenMessages",
            path: "Sources",
            exclude: ["Info.plist"],
            resources: [
                .copy("Assets.xcassets"),
            ]
        ),
    ]
)
