import SwiftUI
import WebKit

/// Embedded Google sign-in that makes "Google Account" pairing feel native:
/// the user signs in to Google exactly as they would in a browser, and as
/// soon as a valid session exists we harvest the Google cookies and hand them
/// back so the Gaia (emoji) pairing can run — no DevTools, no copy-as-cURL.
///
/// Privacy: this uses a non-persistent data store, so the Google session lives
/// only for the duration of sign-in and nothing is written to disk by the
/// web view. The only thing that persists is the pairing session the Go
/// backend saves after the emoji is confirmed on the phone.
struct GoogleSignInView: NSViewRepresentable {
    /// Called once, on the main actor, with a ready-to-send Cookie header
    /// (e.g. "SAPISID=…; __Secure-3PSID=…; …") as soon as the user is signed
    /// in (detected by the presence of the SAPISID auth cookie that libgm's
    /// Gaia pairing requires).
    var onCookiesReady: @MainActor (String) -> Void
    /// Called if the embedded sign-in page fails to load at all.
    var onLoadError: (@MainActor (String) -> Void)?

    // WKWebView's default user agent omits the "Version/… Safari/…" tokens,
    // which Google's sign-in occasionally treats as a non-browser ("this
    // browser or app may not be secure"). Presenting a complete desktop Safari
    // UA — the same engine WKWebView already runs — avoids that rejection.
    private static let safariUserAgent =
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15"

    // Force a Google account sign-in, then continue to Messages for web. The
    // account cookies (incl. SAPISID) are set during the accounts.google.com
    // step, so they're available even before the continue redirect completes.
    private static let signInURL = URL(string:
        "https://accounts.google.com/ServiceLogin?continue=https%3A%2F%2Fmessages.google.com%2Fweb%2F")!

    func makeCoordinator() -> Coordinator {
        Coordinator(onCookiesReady: onCookiesReady, onLoadError: onLoadError)
    }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        // Fresh, disk-free session each time we pair.
        config.websiteDataStore = .nonPersistent()
        let webView = WKWebView(frame: .zero, configuration: config)
        webView.customUserAgent = Self.safariUserAgent
        webView.navigationDelegate = context.coordinator
        context.coordinator.webView = webView
        webView.load(URLRequest(url: Self.signInURL))
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {}

    final class Coordinator: NSObject, WKNavigationDelegate {
        private let onCookiesReady: @MainActor (String) -> Void
        private let onLoadError: (@MainActor (String) -> Void)?
        weak var webView: WKWebView?
        private var harvested = false

        init(onCookiesReady: @escaping @MainActor (String) -> Void,
             onLoadError: (@MainActor (String) -> Void)?) {
            self.onCookiesReady = onCookiesReady
            self.onLoadError = onLoadError
        }

        // Checked after every server response (Set-Cookie already applied to
        // the store) and again when each page finishes — the sign-in redirect
        // chain triggers both, so SAPISID is caught the moment it appears.
        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationResponse: WKNavigationResponse,
                     decisionHandler: @escaping @MainActor @Sendable (WKNavigationResponsePolicy) -> Void) {
            checkCookies()
            decisionHandler(.allow)
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            checkCookies()
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            reportError(error)
        }

        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
            reportError(error)
        }

        private func reportError(_ error: Error) {
            let nsError = error as NSError
            // Ignore benign cancellations (a redirect superseding a load).
            if nsError.domain == NSURLErrorDomain && nsError.code == NSURLErrorCancelled { return }
            let message = error.localizedDescription
            Task { @MainActor in self.onLoadError?(message) }
        }

        private func checkCookies() {
            guard !harvested, let webView else { return }
            webView.configuration.websiteDataStore.httpCookieStore.getAllCookies { cookies in
                let google = cookies.filter { $0.domain.contains("google.com") }
                // SAPISID is exactly what libgm needs for the SAPISIDHASH auth
                // header, so its presence means we have a usable session.
                guard google.contains(where: { $0.name == "SAPISID" }) else { return }
                var byName: [String: String] = [:]
                for cookie in google { byName[cookie.name] = cookie.value }
                let header = byName.map { "\($0.key)=\($0.value)" }.joined(separator: "; ")
                self.deliver(header)
            }
        }

        private func deliver(_ header: String) {
            Task { @MainActor in
                guard !self.harvested else { return }
                self.harvested = true
                self.onCookiesReady(header)
            }
        }
    }
}
