import Foundation

/// The backend refuses every `/api/` and `/mcp` request that lacks its control
/// token: 64 hex characters it keeps in `control.token` in its data directory
/// (mode 0600). Native calls send the token as a Bearer header. The web view
/// gets the `om_control` cookie by loading a single-use bootstrap link that
/// an authenticated call mints, so the cookie arrives through the backend's
/// own `Set-Cookie`.
enum LocalControlAuth {
    static let tokenFileName = "control.token"
    static let bootstrapLinkPath = "api/auth/bootstrap-link"

    /// Reads the token fresh on every call: the backend mints it on first
    /// start and replaces a corrupt one, so a cached copy can go stale.
    static func token(dataDirectory: String) -> String? {
        let path = (dataDirectory as NSString).appendingPathComponent(tokenFileName)
        guard let raw = try? String(contentsOfFile: path, encoding: .utf8) else { return nil }
        let token = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        return isWellFormed(token) ? token : nil
    }

    static func isWellFormed(_ token: String) -> Bool {
        token.utf8.count == 64 && token.unicodeScalars.allSatisfy { CharacterSet(charactersIn: "0123456789abcdefABCDEF").contains($0) }
    }

    /// Only a backend on this machine is ever sent the token.
    static func isLoopback(_ url: URL) -> Bool {
        guard url.scheme == "http" || url.scheme == "https", let host = url.host?.lowercased() else { return false }
        return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
    }

    /// Adds the Bearer header when `request` targets a loopback backend and
    /// the token is readable. Without one the backend answers 401.
    static func authorize(_ request: inout URLRequest, dataDirectory: String) {
        guard let url = request.url, isLoopback(url), let token = token(dataDirectory: dataDirectory) else { return }
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
    }

    static func request(_ url: URL, method: String = "GET", dataDirectory: String) -> URLRequest {
        var request = URLRequest(url: url)
        request.httpMethod = method
        authorize(&request, dataDirectory: dataDirectory)
        return request
    }

    /// The URL the web view should load: a freshly minted bootstrap link that
    /// sets the cookie and redirects to the UI, or the UI itself when minting
    /// fails, where the page explains how to authorize.
    static func authorizedUIURL(baseURL: URL, dataDirectory: String, session: URLSession = .shared) async -> URL {
        let mintURL = baseURL.appendingPathComponent(bootstrapLinkPath)
        guard isLoopback(mintURL) else { return baseURL }
        let request = request(mintURL, method: "POST", dataDirectory: dataDirectory)
        guard request.value(forHTTPHeaderField: "Authorization") != nil,
              let (data, response) = try? await session.data(for: request),
              let http = response as? HTTPURLResponse, http.statusCode == 200
        else { return baseURL }
        return bootstrapURL(fromMintResponse: data, baseURL: baseURL) ?? baseURL
    }

    /// Resolves the minted `path` against `baseURL`, refusing anything that
    /// would leave the backend's origin or isn't the bootstrap endpoint.
    static func bootstrapURL(fromMintResponse data: Data, baseURL: URL) -> URL? {
        struct Minted: Decodable { let path: String }
        guard let minted = try? JSONDecoder().decode(Minted.self, from: data),
              minted.path.hasPrefix("/auth/bootstrap?"),
              let url = URL(string: minted.path, relativeTo: baseURL)?.absoluteURL,
              url.scheme == baseURL.scheme, url.host == baseURL.host, url.port == baseURL.port
        else { return nil }
        return url
    }
}
