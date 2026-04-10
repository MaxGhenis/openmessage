import Combine
import Contacts
import Foundation

@MainActor
final class ContactsManager: NSObject, ObservableObject {
    struct AvatarPayload: Codable {
        let data_url: String?
    }

    private let store = CNContactStore()
    private var phoneIndex: [String: String] = [:]
    private var nameIndex: [String: String] = [:]
    private var avatarIndex: [String: String] = [:]
    private var cacheLoaded = false
    private var accessRequestTask: Task<Bool, Never>?

    override init() {
        super.init()
        NotificationCenter.default.addObserver(
            self,
            selector: #selector(handleContactsChanged),
            name: .CNContactStoreDidChange,
            object: nil
        )
    }

    deinit {
        NotificationCenter.default.removeObserver(self)
    }

    func avatarDataURL(name: String, numbers: [String]) async -> String? {
        guard await ensureAccess() else {
            return nil
        }
        loadCacheIfNeeded()
        for number in numbers {
            for candidate in normalizedPhoneCandidates(number) {
                if let avatarID = phoneIndex[candidate], let avatar = avatarIndex[avatarID] {
                    return avatar
                }
            }
        }
        let normalizedName = normalizedLookupName(name)
        if !normalizedName.isEmpty,
           let avatarID = nameIndex[normalizedName],
           let avatar = avatarIndex[avatarID] {
            return avatar
        }
        return nil
    }

    @objc
    private func handleContactsChanged() {
        phoneIndex.removeAll()
        nameIndex.removeAll()
        avatarIndex.removeAll()
        cacheLoaded = false
    }

    private func ensureAccess() async -> Bool {
        switch CNContactStore.authorizationStatus(for: .contacts) {
        case .authorized:
            return true
        case .notDetermined:
            if let accessRequestTask {
                return await accessRequestTask.value
            }
            let task = Task<Bool, Never> { [store] in
                await withCheckedContinuation { continuation in
                    store.requestAccess(for: .contacts) { granted, _ in
                        continuation.resume(returning: granted)
                    }
                }
            }
            accessRequestTask = task
            let granted = await task.value
            accessRequestTask = nil
            return granted
        default:
            return false
        }
    }

    private func loadCacheIfNeeded() {
        guard !cacheLoaded else { return }

        let keys: [CNKeyDescriptor] = [
            CNContactIdentifierKey as CNKeyDescriptor,
            CNContactGivenNameKey as CNKeyDescriptor,
            CNContactFamilyNameKey as CNKeyDescriptor,
            CNContactMiddleNameKey as CNKeyDescriptor,
            CNContactNicknameKey as CNKeyDescriptor,
            CNContactPhoneNumbersKey as CNKeyDescriptor,
            CNContactThumbnailImageDataKey as CNKeyDescriptor,
            CNContactImageDataAvailableKey as CNKeyDescriptor,
        ]

        let request = CNContactFetchRequest(keysToFetch: keys)
        request.sortOrder = .userDefault

        var phoneLookup: [String: String] = [:]
        var nameLookup: [String: String] = [:]
        var avatarLookup: [String: String] = [:]

        do {
            try store.enumerateContacts(with: request) { contact, _ in
                guard
                    contact.imageDataAvailable,
                    let data = contact.thumbnailImageData,
                    !data.isEmpty
                else {
                    return
                }

                let avatarID = contact.identifier
                let dataURL = avatarLookup[avatarID] ?? Self.makeDataURL(for: data)
                avatarLookup[avatarID] = dataURL
                for phoneNumber in contact.phoneNumbers {
                    for candidate in self.normalizedPhoneCandidates(phoneNumber.value.stringValue) {
                        phoneLookup[candidate] = phoneLookup[candidate] ?? avatarID
                    }
                }

                let candidateNames = [
                    CNContactFormatter.string(from: contact, style: .fullName) ?? "",
                    contact.nickname,
                    "\(contact.givenName) \(contact.familyName)",
                ]
                for candidateName in candidateNames {
                    let normalizedName = self.normalizedLookupName(candidateName)
                    if normalizedName.isEmpty { continue }
                    nameLookup[normalizedName] = nameLookup[normalizedName] ?? avatarID
                }
            }
            phoneIndex = phoneLookup
            nameIndex = nameLookup
            avatarIndex = avatarLookup
            cacheLoaded = true
        } catch {
            phoneIndex.removeAll()
            nameIndex.removeAll()
            avatarIndex.removeAll()
            cacheLoaded = false
        }
    }

    private func normalizedPhoneCandidates(_ raw: String) -> Set<String> {
        let digits = raw.filter(\.isNumber)
        guard !digits.isEmpty else { return [] }

        var candidates: Set<String> = [digits]
        if digits.count > 10 {
            candidates.insert(String(digits.suffix(10)))
        }
        if digits.count == 11, digits.hasPrefix("1") {
            candidates.insert(String(digits.dropFirst()))
        }
        return candidates
    }

    private func normalizedLookupName(_ raw: String) -> String {
        raw
            .lowercased()
            .components(separatedBy: CharacterSet.alphanumerics.inverted)
            .filter { !$0.isEmpty }
            .joined(separator: " ")
    }

    private static func makeDataURL(for data: Data) -> String {
        let mimeType = mimeType(for: data)
        return "data:\(mimeType);base64,\(data.base64EncodedString())"
    }

    private static func mimeType(for data: Data) -> String {
        if data.starts(with: [0x89, 0x50, 0x4E, 0x47]) {
            return "image/png"
        }
        if data.starts(with: [0x47, 0x49, 0x46, 0x38]) {
            return "image/gif"
        }
        return "image/jpeg"
    }
}
