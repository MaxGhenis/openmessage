import Foundation

/// One Google Chrome profile directory and the account signed into it.
///
/// The Google cookie self-heal (internal/googlecookies) reads cookies from a
/// single Chrome profile. Multi-profile installs keep the account that owns
/// Messages in some "Profile N" directory, so the app lets the user pick which
/// one and forwards it as `OPENMESSAGE_CHROME_PROFILE` (see
/// `BackendLaunchConfiguration.application(chromeProfile:)`).
struct ChromeProfile: Identifiable, Equatable, Sendable {
    /// Directory name under Chrome's user-data dir, e.g. "Default" or "Profile 3".
    let directory: String
    /// Signed-in account email, when Chrome recorded one.
    let account: String
    /// Chrome's display name for the profile.
    let name: String

    var id: String { directory }

    /// Menu label: account when signed in, else the profile name and directory.
    var label: String {
        if !account.isEmpty { return "\(account)  (\(directory))" }
        if !name.isEmpty, name != directory { return "\(name)  (\(directory))" }
        return directory
    }
}

enum ChromeProfiles {
    static var userDataDirectory: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/Google/Chrome", isDirectory: true)
    }

    /// Lists the profiles recorded in Chrome's `Local State`, sorted with
    /// signed-in accounts first. Returns [] when Chrome is not installed.
    static func installed() -> [ChromeProfile] {
        let localState = userDataDirectory.appendingPathComponent("Local State")
        guard let data = try? Data(contentsOf: localState) else { return [] }
        return parse(localStateJSON: data)
    }

    /// Parses `Local State` (`profile.info_cache`) into profiles. Pure, so tests
    /// can feed fixtures without touching the real Chrome install.
    static func parse(localStateJSON data: Data) -> [ChromeProfile] {
        guard
            let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
            let profile = root["profile"] as? [String: Any],
            let cache = profile["info_cache"] as? [String: Any]
        else { return [] }
        var profiles: [ChromeProfile] = []
        for (directory, raw) in cache {
            let info = raw as? [String: Any] ?? [:]
            let account = (info["user_name"] as? String ?? "").trimmingCharacters(in: .whitespaces)
            let name = (info["name"] as? String ?? info["gaia_name"] as? String ?? "").trimmingCharacters(in: .whitespaces)
            profiles.append(ChromeProfile(directory: directory, account: account, name: name))
        }
        return profiles.sorted { a, b in
            if a.account.isEmpty != b.account.isEmpty { return !a.account.isEmpty }
            if a.account != b.account { return a.account.localizedCaseInsensitiveCompare(b.account) == .orderedAscending }
            return a.directory.localizedStandardCompare(b.directory) == .orderedAscending
        }
    }
}
