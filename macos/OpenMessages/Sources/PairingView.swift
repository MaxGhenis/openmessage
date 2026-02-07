import SwiftUI
import CoreImage.CIFilterBuiltins

struct PairingView: View {
    @ObservedObject var backend: BackendManager
    @State private var qrURL: String?
    @State private var statusText = "Preparing to pair..."
    @State private var isPairing = false
    @State private var pairingSucceeded = false

    var body: some View {
        VStack(spacing: 24) {
            Image(systemName: "message.fill")
                .font(.system(size: 56))
                .foregroundStyle(.blue)

            Text("Pair with Google Messages")
                .font(.title)
                .fontWeight(.medium)

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
                RoundedRectangle(cornerRadius: 12)
                    .fill(.quaternary)
                    .frame(width: 240, height: 240)
                    .overlay {
                        if isPairing {
                            ProgressView()
                                .scaleEffect(1.5)
                        } else {
                            Image(systemName: "qrcode")
                                .font(.system(size: 48))
                                .foregroundStyle(.tertiary)
                        }
                    }
            }

            Text(statusText)
                .font(.callout)
                .foregroundStyle(.secondary)

            if pairingSucceeded {
                Button("Continue") {
                    backend.start()
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
            } else if !isPairing {
                Button("Start pairing") {
                    startPairing()
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.large)
            }
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onAppear {
            startPairing()
        }
    }

    private func startPairing() {
        isPairing = true
        statusText = "Generating QR code..."

        Task {
            let events = await backend.startPairing()
            for await event in events {
                switch event {
                case .qrURL(let url):
                    qrURL = url
                    statusText = "Scan the QR code with your phone"
                case .log(let msg):
                    statusText = msg
                case .success:
                    pairingSucceeded = true
                    isPairing = false
                    statusText = "Paired successfully!"
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

        // Scale up for clarity
        let scale = 10.0
        let scaled = output.transformed(by: CGAffineTransform(scaleX: scale, y: scale))

        guard let cgImage = context.createCGImage(scaled, from: scaled.extent) else { return nil }
        return NSImage(cgImage: cgImage, size: NSSize(width: scaled.extent.width, height: scaled.extent.height))
    }
}
