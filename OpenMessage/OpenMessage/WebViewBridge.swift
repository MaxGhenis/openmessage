import Foundation
import WebKit

/// Lets non-view components (notifications) talk to the app's single
/// WKWebView: open a conversation on banner tap, and read which
/// conversation is on screen for foreground suppression. Both directions
/// ride on contracts the web UI already maintains: it keeps the active
/// conversation synced into `?conversation=` and restores it from the same
/// query parameter on load, so no extra JS globals are needed.
@MainActor
final class WebViewBridge {
    static let shared = WebViewBridge()

    weak var webView: WKWebView?

    /// Navigates the UI to a conversation via the existing deep-link path.
    func openConversation(_ conversationID: String) {
        guard let webView, !conversationID.isEmpty else { return }
        guard let data = try? JSONEncoder().encode(conversationID),
              let literal = String(data: data, encoding: .utf8) else { return }
        let js = "window.location.href = '/?conversation=' + encodeURIComponent(\(literal))"
        webView.evaluateJavaScript(js, completionHandler: nil)
    }

    /// The conversation currently open in the UI, or nil if none/unknown.
    func activeConversationID() async -> String? {
        guard let webView else { return nil }
        let js = "new URLSearchParams(window.location.search).get('conversation') || ''"
        let result = try? await webView.evaluateJavaScript(js)
        guard let id = result as? String, !id.isEmpty else { return nil }
        return id
    }
}
