import AppKit
import SwiftUI
import WebKit

struct ContentView: View {
    @ObservedObject var backend: BackendManager
    @ObservedObject var notifications: NotificationManager
    @ObservedObject var contacts: ContactsManager

    var body: some View {
        ZStack {
            switch backend.state {
            case .stopped, .starting:
                LaunchView(backend: backend)
            case .needsPairing:
                PairingView(backend: backend)
            case .running:
                WebViewContainer(url: backend.baseURL, backend: backend, notifications: notifications, contacts: contacts)
            case .error(let message):
                ErrorView(message: message, backend: backend)
            }
        }
        .background(Color(nsColor: .windowBackgroundColor))
        .onAppear {
            backend.start()
            notifications.start()
        }
        .onChange(of: backend.state) { _, newState in
            switch newState {
            case .running:
                notifications.start()
            case .stopped, .needsPairing, .error:
                notifications.stop()
            case .starting:
                break
            }
        }
    }
}

// MARK: - Launch screen

struct LaunchView: View {
    @ObservedObject var backend: BackendManager

    var body: some View {
        VStack(spacing: 20) {
            ProgressView()
                .scaleEffect(1.5)
            Text("Starting OpenMessage...")
                .font(.title3)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// MARK: - Error screen

struct ErrorView: View {
    let message: String
    @ObservedObject var backend: BackendManager

    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 48))
                .foregroundStyle(.orange)
            Text("Something went wrong")
                .font(.title2)
            Text(message)
                .font(.body)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .padding(.horizontal, 40)
            Button("Try again") {
                backend.stop()
                backend.start()
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// MARK: - WebView wrapper

struct WebViewContainer: NSViewRepresentable {
    let url: URL
    @ObservedObject var backend: BackendManager
    @ObservedObject var notifications: NotificationManager
    @ObservedObject var contacts: ContactsManager

    func makeCoordinator() -> Coordinator {
        Coordinator(backend: backend, notifications: notifications, contacts: contacts)
    }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.preferences.setValue(true, forKey: "developerExtrasEnabled")
        config.userContentController.addUserScript(WKUserScript(
            source: Self.notificationBridgeScript,
            injectionTime: .atDocumentStart,
            forMainFrameOnly: true
        ))
        config.userContentController.add(context.coordinator, name: Coordinator.handlerName)

        let webView = WKWebView(frame: .zero, configuration: config)
        webView.setValue(false, forKey: "drawsBackground") // Transparent during load
        context.coordinator.webView = webView
        webView.navigationDelegate = context.coordinator
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {
        context.coordinator.backend = backend
        context.coordinator.notifications = notifications
        context.coordinator.contacts = contacts
        // Only reload if URL changed
        if webView.url != url {
            webView.load(URLRequest(url: url))
        }
    }

    private static let notificationBridgeScript = """
    (() => {
      if (window.OpenMessageNativeNotifications && window.OpenMessageNativeContacts && window.OpenMessageNativeApp) return;
      const pending = new Map();
      function request(type, extra = {}) {
        return new Promise((resolve, reject) => {
          const requestId = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
          pending.set(requestId, { resolve, reject });
          window.webkit.messageHandlers.openmessageNotifications.postMessage({ type, requestId, ...extra });
        });
      }
      window.__openMessageResolveNativeNotifications = function(requestId, payload) {
        const pendingRequest = pending.get(requestId);
        if (!pendingRequest) return;
        pending.delete(requestId);
        pendingRequest.resolve(payload);
      };
      window.__openMessageRejectNativeNotifications = function(requestId, message) {
        const pendingRequest = pending.get(requestId);
        if (!pendingRequest) return;
        pending.delete(requestId);
        pendingRequest.reject(new Error(message || 'Notification bridge request failed'));
      };
        window.OpenMessageNativeNotifications = {
        isNative: true,
        getState() {
          return request('getState');
        },
        setEnabled(enabled) {
          return request('setEnabled', { enabled: !!enabled });
        },
        openSettings() {
          return request('openSettings');
        },
      };
      window.OpenMessageNativeContacts = {
        isNative: true,
        getAvatar(name, numbers = []) {
          return request('getAvatar', {
            name: typeof name === 'string' ? name : '',
            numbers: Array.isArray(numbers) ? numbers : [],
          });
        },
      };
      window.OpenMessageNativeApp = {
        isNative: true,
        startGooglePairing() {
          return request('startGooglePairing');
        },
      };
    })();
    """

    final class Coordinator: NSObject, WKScriptMessageHandler, WKNavigationDelegate {
        static let handlerName = "openmessageNotifications"
        private static let openPlatformsScript = "if (typeof openWhatsAppOverlay === 'function') { openWhatsAppOverlay().catch(err => console.error('Failed to open platforms from native settings:', err)); }"

        var backend: BackendManager
        var notifications: NotificationManager
        var contacts: ContactsManager
        weak var webView: WKWebView?
        private var shouldOpenPlatformsAfterLoad = false

        init(backend: BackendManager, notifications: NotificationManager, contacts: ContactsManager) {
            self.backend = backend
            self.notifications = notifications
            self.contacts = contacts
            super.init()
            NotificationCenter.default.addObserver(
                self,
                selector: #selector(handleOpenPlatformsNotification),
                name: .openPlatformsRequested,
                object: nil
            )
        }

        deinit {
            NotificationCenter.default.removeObserver(self)
        }

        func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
            guard message.name == Self.handlerName else { return }
            guard let body = message.body as? [String: Any] else { return }
            let type = body["type"] as? String ?? ""
            let requestID = body["requestId"] as? String ?? ""

            Task { @MainActor in
                do {
                    switch type {
                    case "getState":
                        let state = await notifications.bridgeState()
                        resolve(requestID: requestID, payload: state)
                    case "setEnabled":
                        let enabled = (body["enabled"] as? NSNumber)?.boolValue ?? false
                        let state = await notifications.setEnabled(enabled)
                        resolve(requestID: requestID, payload: state)
                    case "openSettings":
                        notifications.openSystemSettings()
                        resolveEmpty(requestID: requestID)
                    case "startGooglePairing":
                        backend.beginGooglePairing()
                        resolveEmpty(requestID: requestID)
                    case "getAvatar":
                        let name = body["name"] as? String ?? ""
                        let numbers = (body["numbers"] as? [String]) ?? ((body["numbers"] as? [Any])?.compactMap { $0 as? String } ?? [])
                        let dataURL = await contacts.avatarDataURL(name: name, numbers: numbers)
                        resolve(requestID: requestID, payload: ContactsManager.AvatarPayload(data_url: dataURL))
                    default:
                        reject(requestID: requestID, message: "Unknown notification bridge request")
                    }
                }
            }
        }

        @MainActor
        func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction, decisionHandler: @escaping @MainActor @Sendable (WKNavigationActionPolicy) -> Void) {
            if navigationAction.navigationType == .linkActivated || navigationAction.targetFrame == nil,
               let targetURL = navigationAction.request.url {
                NSWorkspace.shared.open(targetURL)
                decisionHandler(.cancel)
                return
            }
            decisionHandler(.allow)
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            guard shouldOpenPlatformsAfterLoad else { return }
            shouldOpenPlatformsAfterLoad = false
            openPlatforms()
        }

        @objc private func handleOpenPlatformsNotification() {
            openPlatforms()
        }

        private func resolve<T: Encodable>(requestID: String, payload: T) {
            guard let webView else { return }
            let encoder = JSONEncoder()
            guard
                let requestData = try? encoder.encode(requestID),
                let requestJSON = String(data: requestData, encoding: .utf8),
                let payloadData = try? encoder.encode(payload),
                let payloadJSON = String(data: payloadData, encoding: .utf8)
            else {
                reject(requestID: requestID, message: "Failed to encode native bridge payload")
                return
            }
            webView.evaluateJavaScript("window.__openMessageResolveNativeNotifications(\(requestJSON), \(payloadJSON));")
        }

        private func reject(requestID: String, message: String) {
            guard let webView else { return }
            let encoder = JSONEncoder()
            guard
                let requestData = try? encoder.encode(requestID),
                let requestJSON = String(data: requestData, encoding: .utf8),
                let messageData = try? encoder.encode(message),
                let messageJSON = String(data: messageData, encoding: .utf8)
            else {
                return
            }
            webView.evaluateJavaScript("window.__openMessageRejectNativeNotifications(\(requestJSON), \(messageJSON));")
        }

        private func resolveEmpty(requestID: String) {
            guard let webView else { return }
            let encoder = JSONEncoder()
            guard
                let requestData = try? encoder.encode(requestID),
                let requestJSON = String(data: requestData, encoding: .utf8)
            else {
                return
            }
            webView.evaluateJavaScript("window.__openMessageResolveNativeNotifications(\(requestJSON), null);")
        }

        private func openPlatforms() {
            NSApp.activate(ignoringOtherApps: true)
            guard let webView else {
                shouldOpenPlatformsAfterLoad = true
                return
            }
            if webView.isLoading {
                shouldOpenPlatformsAfterLoad = true
                return
            }
            webView.evaluateJavaScript(Self.openPlatformsScript)
        }
    }
}
