import Foundation
import XCTest
@testable import OpenMessage

final class LocalControlAuthTests: XCTestCase {
    private let token = String(repeating: "ab", count: 32)

    private func dataDirectory(holding contents: String?) throws -> String {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("openmessage-auth-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        addTeardownBlock { try? FileManager.default.removeItem(at: directory) }
        if let contents {
            try contents.write(to: directory.appendingPathComponent(LocalControlAuth.tokenFileName), atomically: true, encoding: .utf8)
        }
        return directory.path
    }

    func testTokenIsReadTrimmedAndValidated() throws {
        XCTAssertEqual(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: token + "\n")), token)
        XCTAssertNil(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: nil)))
        XCTAssertNil(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: "")))
        XCTAssertNil(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: "not-hex")))
        XCTAssertNil(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: String(repeating: "g", count: 64))))
        XCTAssertNil(LocalControlAuth.token(dataDirectory: try dataDirectory(holding: token + "00")))
    }

    func testAuthorizeOnlyAddsTheTokenForALoopbackBackend() throws {
        let directory = try dataDirectory(holding: token)
        for url in ["http://127.0.0.1:7007/api/status", "http://localhost:7007/api/status", "http://[::1]:7007/api/status"] {
            let request = LocalControlAuth.request(URL(string: url)!, dataDirectory: directory)
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer \(token)", url)
        }
        for url in ["http://example.com:7007/api/status", "http://192.168.1.10:7007/api/status", "file:///tmp/x"] {
            let request = LocalControlAuth.request(URL(string: url)!, dataDirectory: directory)
            XCTAssertNil(request.value(forHTTPHeaderField: "Authorization"), url)
        }
        let withoutToken = LocalControlAuth.request(URL(string: "http://127.0.0.1:7007/api/status")!, dataDirectory: try dataDirectory(holding: nil))
        XCTAssertNil(withoutToken.value(forHTTPHeaderField: "Authorization"))
        let post = LocalControlAuth.request(URL(string: "http://127.0.0.1:7007/api/unpair")!, method: "POST", dataDirectory: directory)
        XCTAssertEqual(post.httpMethod, "POST")
    }

    func testMintedPathMustStayOnTheBackendsBootstrapEndpoint() {
        let base = URL(string: "http://127.0.0.1:7007")!
        func resolve(_ path: String) -> URL? {
            LocalControlAuth.bootstrapURL(fromMintResponse: Data(#"{"path":"\#(path)"}"#.utf8), baseURL: base)
        }
        XCTAssertEqual(resolve("/auth/bootstrap?t=abc")?.absoluteString, "http://127.0.0.1:7007/auth/bootstrap?t=abc")
        XCTAssertNil(resolve("//evil.example/auth/bootstrap?t=abc"))
        XCTAssertNil(resolve("http://evil.example/auth/bootstrap?t=abc"))
        XCTAssertNil(resolve("/api/status"))
        XCTAssertNil(resolve("/auth/bootstrapx?t=abc"))
        XCTAssertNil(LocalControlAuth.bootstrapURL(fromMintResponse: Data("not json".utf8), baseURL: base))
    }

    func testAuthorizedUIURLFallsBackToTheUIWithoutAToken() async throws {
        let base = URL(string: "http://127.0.0.1:1")!
        let url = await LocalControlAuth.authorizedUIURL(baseURL: base, dataDirectory: try dataDirectory(holding: nil))
        XCTAssertEqual(url, base)
    }
}
