import SwiftUI
import CoreImage.CIFilterBuiltins

private enum PairingMethod: String, CaseIterable, Identifiable {
    case qr = "QR Code"
    case google = "Google Account"

    var id: String { rawValue }
}

struct PairingView: View {
    @ObservedObject var backend: BackendManager
    @State private var method: PairingMethod = .qr
    @State private var qrURL: String?
    @State private var pairingEmoji: String?
    @State private var googleInput = ""
    @State private var statusText = "Choose a pairing method to connect Google Messages."
    @State private var isPairing = false
    @State private var pairingSucceeded = false
    @State private var pairingAttemptID = UUID()

    var body: some View {
        VStack(spacing: 24) {
            Image(systemName: "message.fill")
                .font(.system(size: 56))
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

            if method == .qr {
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

                Text("If Google only offers account pairing on your phone, switch to the Google Account tab instead.")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
                    .multilineTextAlignment(.center)
            } else {
                Text("Paste a Google cookie JSON object or a cURL command copied from browser devtools for messages.google.com. OpenMessage will use it to start account pairing, then you'll confirm on your phone.")
                    .font(.body)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .lineSpacing(4)

                if let pairingEmoji, !pairingEmoji.isEmpty {
                    VStack(spacing: 10) {
                        Text(pairingEmoji)
                            .font(.system(size: 88))
                        Text("Tap this emoji in Google Messages on your phone.")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .multilineTextAlignment(.center)
                    }
                    .frame(width: 240, height: 240)
                    .background(Color(nsColor: .quaternaryLabelColor).opacity(0.12))
                    .cornerRadius(20)
                } else {
                    placeholderCard(systemName: "person.crop.circle.badge.checkmark")
                }

                VStack(alignment: .leading, spacing: 8) {
                    Text("Cookies / cURL")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    TextEditor(text: $googleInput)
                        .font(.system(.body, design: .monospaced))
                        .frame(width: 420, height: 140)
                        .padding(10)
                        .background(Color(nsColor: .textBackgroundColor))
                        .overlay(
                            RoundedRectangle(cornerRadius: 12)
                                .stroke(Color.primary.opacity(0.08), lineWidth: 1)
                        )
                        .clipShape(RoundedRectangle(cornerRadius: 12))
                }
            }

            Text(statusText)
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)

            if !pairingSucceeded {
                Button(method == .qr ? "Start QR pairing" : "Start Google account pairing") {
                    startPairing()
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
                .disabled(isPairing || (method == .google && googleInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty))
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onChange(of: method) { _, newValue in
            qrURL = nil
            pairingEmoji = nil
            pairingSucceeded = false
            isPairing = false
            pairingAttemptID = UUID()
            statusText = newValue == .qr
                ? "Ready to generate a QR code."
                : "Paste your Google cookies or cURL command to start account pairing."
        }
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

    private func startPairing() {
        let attemptID = UUID()
        pairingAttemptID = attemptID
        isPairing = true
        pairingSucceeded = false
        qrURL = nil
        pairingEmoji = nil
        statusText = method == .qr ? "Generating QR code..." : "Starting Google account pairing..."

        Task {
            let events = method == .qr
                ? await backend.startPairing()
                : await backend.startGooglePairing(cookieInput: googleInput)
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
                    statusText = "Confirm the emoji on your phone."
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
