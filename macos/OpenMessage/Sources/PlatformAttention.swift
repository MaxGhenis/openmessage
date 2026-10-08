import Foundation

/// Reads the per-platform health the backend reports in `/api/status` and says
/// which paired platforms need the user's attention, and why. Pure so the menu
/// bar alert and the health notifications share one reading and can be tested
/// without a running backend.
struct PlatformAttention: Equatable {
    enum Reason: Equatable {
        /// Re-pair or reauth needed, a paired platform is disconnected, or its
        /// newest message trails the other platforms by days.
        case needsRepair
        /// Connected, but nothing has arrived for longer than the platform's
        /// own recent traffic explains (`freshness.<key>.stale_reason ==
        /// "silent"`). On 2026-10-06 the phone relayed nothing for 38 hours
        /// while Google Messages reported connected and phone-responding,
        /// apart from brief reconnects; a phone restart brought it back.
        /// `since` is the silence's last event (ms), which identifies the
        /// episode.
        case silent(hours: Int, since: Int64)
    }

    let key: String
    let name: String
    let reason: Reason

    static let platforms: [(key: String, name: String)] = [
        ("google", "Google Messages"),
        ("whatsapp", "WhatsApp"),
        ("signal", "Signal"),
    ]

    /// Platforms needing attention, in `platforms` order. Only platforms that
    /// report `paired`, or explicitly ask for pairing/reauth, are considered,
    /// so a platform the user never set up never nags.
    static func evaluate(status json: [String: Any]) -> [PlatformAttention] {
        let freshness = json["freshness"] as? [String: Any]
        return platforms.compactMap { platform in
            guard let p = json[platform.key] as? [String: Any] else { return nil }
            let paired = (p["paired"] as? Bool) ?? false
            let connected = (p["connected"] as? Bool) ?? false
            let needsPairing = (p["needs_pairing"] as? Bool) ?? false
            let needsReauth = (p["needs_reauth"] as? Bool) ?? false
            if needsPairing || needsReauth || (paired && !connected) {
                return PlatformAttention(key: platform.key, name: platform.name, reason: .needsRepair)
            }
            // `connected` can stay true while a bridge has silently stopped
            // delivering. Trust freshness: a paired platform flagged stale
            // needs attention even while it reports connected.
            guard paired,
                  let fresh = freshness?[platform.key] as? [String: Any],
                  (fresh["stale"] as? Bool) ?? false else {
                return nil
            }
            if (fresh["stale_reason"] as? String) == "silent" {
                let silence = fresh["silence"] as? [String: Any]
                let silentMS = (silence?["silent_ms"] as? NSNumber)?.doubleValue ?? 0
                let since = (silence?["last_event_ms"] as? NSNumber)?.int64Value ?? 0
                let hours = max(1, Int(silentMS / 3_600_000))
                return PlatformAttention(
                    key: platform.key, name: platform.name, reason: .silent(hours: hours, since: since)
                )
            }
            return PlatformAttention(key: platform.key, name: platform.name, reason: .needsRepair)
        }
    }

    static func evaluate(statusData data: Data) -> [PlatformAttention]? {
        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return nil }
        return evaluate(status: json)
    }

    /// One-line menu bar alert, or nil when every paired platform is healthy.
    static func alertText(for items: [PlatformAttention]) -> String? {
        var sentences: [String] = []
        let repair = items.filter { $0.reason == .needsRepair }.map(\.name)
        switch repair.count {
        case 0: break
        case 1: sentences.append("\(repair[0]) needs re-pairing — it has stopped syncing.")
        default: sentences.append("\(repair.joined(separator: ", ")) need re-pairing — they have stopped syncing.")
        }
        for item in items {
            if case let .silent(hours, _) = item.reason {
                sentences.append(silentSentence(item, hours: hours))
            }
        }
        return sentences.isEmpty ? nil : sentences.joined(separator: " ")
    }

    /// Body of the one-time notification for a platform that went silent.
    static func silentNotificationBody(_ item: PlatformAttention) -> String? {
        guard case let .silent(hours, _) = item.reason else { return nil }
        return silentSentence(item, hours: hours)
    }

    private static func silentSentence(_ item: PlatformAttention, hours: Int) -> String {
        let span = hours == 1 ? "1 hour" : "\(hours) hours"
        let base = "\(item.name) has synced nothing for \(span)."
        if item.key == "google" {
            // Google Messages relays everything through the phone.
            return base + " Check that your phone is on and online; restarting it can fix this."
        }
        return base
    }
}

/// Decides which silent platforms to notify about: once per silence episode,
/// identified by the episode's last event. A connection flap during the same
/// outage (silent, then "needs repair" while disconnected, then silent again)
/// or an app relaunch (with the persisted state) does not re-notify; a new
/// event followed by a new silence does.
struct SilenceNotificationLatch {
    /// Platform key → the `since` of the last episode notified. Persist it
    /// (see `NotificationManager`) so an app relaunch mid-outage stays quiet.
    private(set) var notifiedEpisode: [String: Int64]

    init(notifiedEpisode: [String: Int64] = [:]) {
        self.notifiedEpisode = notifiedEpisode
    }

    /// Restores the latch from a property-list value written by `storedValue`
    /// (UserDefaults). Anything unreadable starts empty.
    init(storedValue: Any?) {
        let stored = storedValue as? [String: Any] ?? [:]
        self.init(notifiedEpisode: stored.compactMapValues { ($0 as? NSNumber)?.int64Value })
    }

    /// The latch as a property-list value for UserDefaults.
    var storedValue: [String: NSNumber] {
        notifiedEpisode.mapValues { NSNumber(value: $0) }
    }

    /// Silent items not yet notified for their current episode. Marks them
    /// notified.
    mutating func newlySilent(_ items: [PlatformAttention]) -> [PlatformAttention] {
        items.filter { item in
            guard case let .silent(_, since) = item.reason else { return false }
            if notifiedEpisode[item.key] == since { return false }
            notifiedEpisode[item.key] = since
            return true
        }
    }
}
