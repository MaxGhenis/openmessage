import AppKit
import Foundation
import WebKit

/// Lets non-view components (notifications, Settings, the menu bar) talk to
/// the app's single WKWebView: open a conversation on banner tap, read which
/// conversation is on screen for foreground suppression, and open the
/// Platforms overlay. Both directions ride on contracts the web UI already
/// maintains — it keeps the active conversation synced into `?conversation=`
/// and restores it from the same query parameter on load, and it exports
/// `window.openWhatsAppOverlay` for the platforms flow.
@MainActor
final class WebViewBridge {
    static let shared = WebViewBridge()

    weak var webView: WKWebView?

    /// Set when the Platforms overlay was requested before the web UI could
    /// take it (main window closed so no web view exists yet, backend still
    /// on the pairing screen, or the page mid-load); consumed on the next
    /// successful page load. A plain NotificationCenter post can't cover
    /// those cases — there is no observer alive to hear it.
    private var pendingOpenPlatforms = false

    /// The web UI exports this as a real window global (the app script is an
    /// IIFE, so un-exported functions are invisible to evaluateJavaScript).
    private static let openPlatformsScript = """
    if (typeof window.openWhatsAppOverlay === 'function') { \
    window.openWhatsAppOverlay().catch(err => console.error('Failed to open platforms from native UI:', err)); \
    } else { console.error('Native bridge: window.openWhatsAppOverlay is not exported'); }
    """

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

    /// Opens the Platforms overlay in the web UI, bringing the main window
    /// forward. Safe to call while the window is closed or the page is still
    /// loading — the request is remembered and fires after the next load.
    /// Callers that can create the main window (a scene with
    /// `@Environment(\.openWindow)`) should call `openWindow(id: "main")`
    /// first so a closed window is materialized.
    func requestOpenPlatforms() {
        NSApp.activate(ignoringOtherApps: true)
        guard let webView else {
            pendingOpenPlatforms = true
            return
        }
        webView.window?.makeKeyAndOrderFront(nil)
        guard !webView.isLoading else {
            pendingOpenPlatforms = true
            return
        }
        webView.evaluateJavaScript(Self.openPlatformsScript)
    }

    /// Called by the web view's navigation delegate after each successful
    /// load; delivers a platforms request that arrived while no interactive
    /// page existed.
    func webViewDidFinishLoad() {
        guard pendingOpenPlatforms else { return }
        pendingOpenPlatforms = false
        webView?.window?.makeKeyAndOrderFront(nil)
        webView?.evaluateJavaScript(Self.openPlatformsScript)
    }
}
