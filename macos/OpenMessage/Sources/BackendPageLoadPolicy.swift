import Foundation

/// Decides when the app's web view loads the backend's page.
///
/// The page keeps its own place in its URL. It writes the open conversation
/// into `?conversation=`, and a notification tap navigates it there
/// (`WebViewBridge`). So the web view's URL differs from the backend's
/// whenever a conversation is open, and it can't tell whether the page is
/// stale. Only a new backend address can. On 2026-11-07 the web view compared
/// the two on every SwiftUI update of the window. Each time Signal dropped
/// its connection for a few seconds, the menu bar alert changed, the window
/// updated, and the page reloaded twice, discarding what the user was typing.
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
}
