import SwiftUI
import CoreImage.CIFilterBuiltins

private enum PairingMethod: String, CaseIterable, Identifiable {
    case google = "Google Account"
    case qr = "QR Code"

    var id: String { rawValue }
}

/// Sub-steps within the Google Account method: sign in to Google (native web
/// view), confirm the emoji on the phone, or fall back to the advanced
/// cookie/cURL paste if the embedded sign-in is blocked.
private enum GoogleStep {
    case signIn
    case confirm
    case advanced
}

struct PairingView: View {
    @ObservedObject var backend: BackendManager
    @State private var method: PairingMethod = .google
    @State private var googleStep: GoogleStep = .signIn
    @State private var qrURL: String?
    @State private var pairingEmoji: String?
    @State private var googleInput = ""
    @State private var statusText = ""
    @State private var isPairing = false
    @State private var pairingSucceeded = false
    @State private var pairingAttemptID = UUID()
    @State private var pairingTask: Task<Void, Never>?

    var body: some View {
        VStack(spacing: 22) {
            Image(systemName: "message.fill")
                .font(.system(size: 52))
                .foregroundStyle(.blue)

            Text("Pair with Google Messages")
                .font(.title)
                .fontWeight(.medium)

            Picker("Pairing Method", selection: $method) {
                ForEach(PairingMethod.allCases) { item in
                    Text(item.rawValue).tag(item)
                }
            }
            .pickerStyle(.segmented)
            .frame(maxWidth: 420)
            .disabled(isPairing && qrURL == nil && pairingEmoji == nil)

            if pairingSucceeded {
                successCard
            } else if method == .qr {
                qrSection
            } else {
                googleSection
            }
        }
        .padding(36)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onAppear {
            // QR can auto-generate immediately so the user never sees a blank
            // placeholder. The Google method loads its own sign-in web view, so
            // it needs no kick here.
            if method == .qr && qrURL == nil && !isPairing && !pairingSucceeded {
                startPairing()
            }
        }
        .onChange(of: method) { _, newValue in
            resetPairingState()
            if newValue == .qr {
                statusText = "Generating QR code…"
                startPairing()
            } else {
                googleStep = .signIn
                statusText = ""
            }
        }
    }

    // MARK: - Google Account method

    @ViewBuilder
    private var googleSection: some View {
        switch googleStep {
        case .signIn:
            googleSignInStep
        case .confirm:
            googleConfirmStep
        case .advanced:
            googleAdvancedStep
        }
    }

    private var googleSignInStep: some View {
        VStack(spacing: 14) {
            Text("Sign in with the Google account that has Messages set up on your phone.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 460)

            GoogleSignInView(
                onCookiesReady: { header in
                    beginGoogleConfirmation(cookieHeader: header)
                },
                onLoadError: { message in
                    statusText = "Couldn't load Google sign-in: \(message)"
                }
            )
            .frame(width: 460, height: 460)
            .background(Color(nsColor: .textBackgroundColor))
            .clipShape(RoundedRectangle(cornerRadius: 12))
            .overlay(
                RoundedRectangle(cornerRadius: 12)
                    .stroke(Color.primary.opacity(0.08), lineWidth: 1)
            )

            if !statusText.isEmpty {
                Text(statusText)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }

            HStack(spacing: 6) {
                Image(systemName: "lock.fill")
                    .font(.caption2)
                Text("Sign-in happens locally on this Mac. Only the pairing session is saved.")
            }
            .font(.caption)
            .foregroundStyle(.tertiary)

            Button("Trouble signing in? Use the advanced method") {
                statusText = ""
                googleStep = .advanced
            }
            .buttonStyle(.link)
            .font(.caption)
        }
    }

    private var googleConfirmStep: some View {
        VStack(spacing: 16) {
            if let pairingEmoji, !pairingEmoji.isEmpty {
                confirmCard {
                    Text(pairingEmoji)
                        .font(.system(size: 96))
                    Text("In Google Messages on your phone, tap this emoji to finish pairing.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: 320)
                }
            } else if isPairing {
                confirmCard {
                    ProgressView()
                        .scaleEffect(1.4)
                    Text(statusText.isEmpty ? "Connecting to Google…" : statusText)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                }
            } else {
                confirmCard {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .font(.system(size: 40))
                        .foregroundStyle(.orange)
                    Text(statusText.isEmpty ? "Pairing didn't complete." : statusText)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: 320)
                }
            }

            if pairingEmoji != nil && !statusText.isEmpty {
                Text(statusText)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }

            Button(pairingEmoji == nil && !isPairing ? "Try again" : "Start over") {
                resetPairingState()
                googleStep = .signIn
            }
            .buttonStyle(.link)
            .font(.caption)
        }
    }

    private func confirmCard<Content: View>(@ViewBuilder _ content: () -> Content) -> some View {
        VStack(spacing: 12) {
            content()
        }
        .frame(width: 300, height: 260)
        .background(Color(nsColor: .quaternaryLabelColor).opacity(0.12))
        .cornerRadius(20)
    }

    private var googleAdvancedStep: some View {
        VStack(spacing: 14) {
            VStack(spacing: 6) {
                Text("Advanced: paste Google cookies")
                    .font(.headline)
                Text("Use this only if the embedded sign-in doesn't work.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text("1. Open messages.google.com/web in Chrome and sign in\n2. Open DevTools (⌥⌘I) → Network tab\n3. Right-click any request to messages.google.com → Copy → Copy as cURL\n4. Paste below")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
                    .multilineTextAlignment(.leading)
                    .lineSpacing(3)
            }

            TextEditor(text: $googleInput)
                .font(.system(.body, design: .monospaced))
                .frame(width: 460, height: 130)
                .padding(10)
                .background(Color(nsColor: .textBackgroundColor))
                .overlay(
                    RoundedRectangle(cornerRadius: 12)
                        .stroke(Color.primary.opacity(0.08), lineWidth: 1)
                )
                .clipShape(RoundedRectangle(cornerRadius: 12))

            if !statusText.isEmpty {
                Text(statusText)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }

            HStack(spacing: 12) {
                Button("Back to sign-in") {
                    statusText = ""
                    googleStep = .signIn
                }
                .buttonStyle(.bordered)
                .controlSize(.large)

                Button("Start pairing") {
                    googleStep = .confirm
                    startPairing(googleCookieOverride: googleInput)
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
                .disabled(isPairing || googleInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
    }

    // MARK: - QR method

    private var qrSection: some View {
        VStack(spacing: 16) {
            Text("Open Google Messages on your phone, go to\nSettings > Device pairing, and scan this QR code.")
                .font(.body)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .lineSpacing(4)

            if let qrURL, let image = generateQRCode(from: qrURL) {
                Image(nsImage: image)
                    .interpolation(.none)
                    .resizable()
                    .scaledToFit()
                    .frame(width: 240, height: 240)
                    .background(.white)
                    .cornerRadius(12)
                    .shadow(color: .black.opacity(0.1), radius: 10)
            } else {
                placeholderCard(systemName: "qrcode")
            }

            VStack(spacing: 4) {
                Text("⚠︎ Google is phasing out QR pairing.")
                    .font(.caption)
                    .foregroundStyle(.orange)
                Text("If your phone no longer shows a QR scanner under Device pairing, use the Google Account tab instead — that's the method Google is keeping.")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 420)
            }

            if !statusText.isEmpty {
                Text(statusText)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }
        }
    }

    // MARK: - Shared

    private var successCard: some View {
        VStack(spacing: 14) {
            Image(systemName: "checkmark.circle.fill")
                .font(.system(size: 64))
                .foregroundStyle(.green)
            Text("Paired successfully!")
                .font(.title2)
                .fontWeight(.medium)
            Text("Loading your messages…")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .frame(width: 300, height: 240)
    }

    private func placeholderCard(systemName: String) -> some View {
        RoundedRectangle(cornerRadius: 16)
            .fill(.quaternary)
            .frame(width: 240, height: 240)
            .overlay {
                if isPairing {
                    ProgressView()
                        .scaleEffect(1.5)
                } else {
                    Image(systemName: systemName)
                        .font(.system(size: 48))
                        .foregroundStyle(.tertiary)
                }
            }
    }

    // MARK: - Pairing engine

    private func resetPairingState() {
        // Cancel any in-flight pairing consumer so the subprocess is torn down
        // (the AsyncStream's onTermination terminates the `pair` process).
        pairingTask?.cancel()
        pairingTask = nil
        qrURL = nil
        pairingEmoji = nil
        pairingSucceeded = false
        isPairing = false
        pairingAttemptID = UUID()
        statusText = ""
    }

    /// Kick off Gaia (emoji) pairing using cookies harvested from the embedded
    /// Google sign-in, then show the emoji confirmation step.
    private func beginGoogleConfirmation(cookieHeader: String) {
        guard method == .google, !pairingSucceeded else { return }
        googleStep = .confirm
        statusText = "Connecting to Google…"
        startPairing(googleCookieOverride: cookieHeader)
    }

    private func startPairing(googleCookieOverride: String? = nil) {
        let attemptID = UUID()
        pairingAttemptID = attemptID
        isPairing = true
        pairingSucceeded = false
        qrURL = nil
        pairingEmoji = nil
        statusText = method == .qr ? "Generating QR code…" : "Connecting to Google…"

        let cookieInput = googleCookieOverride ?? googleInput

        pairingTask?.cancel()
        pairingTask = Task {
            let events = method == .qr
                ? await backend.startPairing()
                : await backend.startGooglePairing(cookieInput: cookieInput)
            for await event in events {
                guard pairingAttemptID == attemptID else { continue }
                switch event {
                case .qrURL(let url):
                    qrURL = url
                    isPairing = false
                    statusText = "Scan the QR code with your phone."
                case .emoji(let emoji):
                    pairingEmoji = emoji
                    isPairing = false
                    statusText = "Tap the emoji on your phone to confirm."
                case .log(let msg):
                    statusText = msg
                case .success:
                    pairingSucceeded = true
                    isPairing = false
                    statusText = "Paired successfully!"
                    backend.start()
                case .failed(let msg):
                    isPairing = false
                    statusText = "Pairing failed: \(msg)"
                }
            }
        }
    }

    private func generateQRCode(from string: String) -> NSImage? {
        let context = CIContext()
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(string.utf8)
        filter.correctionLevel = "M"

        guard let output = filter.outputImage else { return nil }

        let scale = 10.0
        let scaled = output.transformed(by: CGAffineTransform(scaleX: scale, y: scale))

        guard let cgImage = context.createCGImage(scaled, from: scaled.extent) else { return nil }
        return NSImage(cgImage: cgImage, size: NSSize(width: scaled.extent.width, height: scaled.extent.height))
    }
}
