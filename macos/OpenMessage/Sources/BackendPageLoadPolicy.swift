import Foundation

/// Decides when the app's web view loads the backend's page.
///
/// The web view's URL never equals the backend address, so it can't tell
/// whether the page is stale. WebKit reports a page loaded from
/// `http://127.0.0.1:7007` as `http://127.0.0.1:7007/`, which `URL` equality
/// treats as a different URL. The page also writes the open conversation
/// into `?conversation=`, and a notification tap navigates it there
/// (`WebViewBridge`). Only a new backend address means a stale page. The web
/// view used to compare the two URLs on every SwiftUI update of the window,
/// so every update reloaded the page. On 2026-11-07 the menu bar alert
/// changed each time Signal dropped its connection for a few seconds and
/// again when it came back, and each change reloaded the page and discarded
/// what the user was typing.
struct BackendPageLoadPolicy {
    /// The backend address the page last loaded from.
    private(set) var loadedURL: URL?

    /// The request that loads `backendURL`, or nil when the page already
    /// came from that address.
    mutating func request(for backendURL: URL) -> URLRequest? {
        guard backendURL != loadedURL else { return nil }
        loadedURL = backendURL
        return URLRequest(url: backendURL)
    }

    /// Forgets the address whose page failed to load, so the next request
    /// for it loads again, and returns that address.
    mutating func loadFailed() -> URL? {
        defer { loadedURL = nil }
        return loadedURL
    }

    /// Whether a failed navigation means the page never arrived. A navigation
    /// that another navigation replaced (cancelled), or that the navigation
    /// policy stopped, left a page in place and needs no retry.
    static func shouldRetry(after error: Error) -> Bool {
        let error = error as NSError
        if error.domain == NSURLErrorDomain && error.code == NSURLErrorCancelled { return false }
        if error.domain == "WebKitErrorDomain" && error.code == frameLoadInterruptedByPolicyChange { return false }
        return true
    }

    /// WebKit's WebKitErrorFrameLoadInterruptedByPolicyChange.
    static let frameLoadInterruptedByPolicyChange = 102
}
