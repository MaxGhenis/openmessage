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
        /// Google Messages is connected and not stale, and RCS keeps arriving,
        /// but no incoming SMS for longer than this phone's usual gaps
        /// (`freshness.google.sms_path.stalled`). From 2026-10-03 to 10-07 a
        /// Pixel's IMS stack lost its SMS layer for four days while every
        /// other check stayed green; a phone restart fixed it. `since` is the
        /// last incoming SMS (ms), which identifies the episode.
        case smsStopped(hours: Int, since: Int64)
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
            let fresh = freshness?[platform.key] as? [String: Any]
            let stale = (fresh?["stale"] as? Bool) ?? false
            // Only Google publishes an SMS path, and only an otherwise healthy
            // entry reads it: a stale platform's own reason explains more.
            if paired, !stale,
               let smsPath = fresh?["sms_path"] as? [String: Any],
               (smsPath["stalled"] as? Bool) ?? false {
                let silentMS = (smsPath["silent_ms"] as? NSNumber)?.doubleValue ?? 0
                let since = (smsPath["last_sms_ms"] as? NSNumber)?.int64Value ?? 0
                let hours = max(1, Int(silentMS / 3_600_000))
                return PlatformAttention(
                    key: platform.key, name: platform.name, reason: .smsStopped(hours: hours, since: since)
                )
            }
            // `connected` can stay true while a bridge has silently stopped
            // delivering. Trust freshness: a paired platform flagged stale
            // needs attention even while it reports connected.
            guard paired, let fresh, stale else {
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
            switch item.reason {
            case let .silent(hours, _):
                sentences.append(silentSentence(item, hours: hours))
            case let .smsStopped(hours, _):
                sentences.append(smsStoppedSentence(hours: hours))
            case .needsRepair:
                break
            }
        }
        return sentences.isEmpty ? nil : sentences.joined(separator: " ")
    }

    /// Body of the one-time notification for a platform that went silent.
    static func silentNotificationBody(_ item: PlatformAttention) -> String? {
        guard case let .silent(hours, _) = item.reason else { return nil }
        return silentSentence(item, hours: hours)
    }

    /// Body of the one-time notification for a phone that stopped receiving SMS.
    static func smsStoppedNotificationBody(_ item: PlatformAttention) -> String? {
        guard case let .smsStopped(hours, _) = item.reason else { return nil }
        return smsStoppedSentence(hours: hours)
    }

    private static func smsStoppedSentence(hours: Int) -> String {
        let span = hours == 1 ? "1 hour" : "\(hours) hours"
        return "Your phone has received no SMS for \(span), longer than usual, while RCS still arrives, "
            + "so texts and codes from non-RCS senders may not be getting through. Try restarting the phone."
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

    /// Silent items not yet notified for their current episode. Marks them
    /// notified.
    mutating func newlySilent(_ items: [PlatformAttention]) -> [PlatformAttention] {
        newlyNotified(items) { reason in
            if case let .silent(_, since) = reason { return since }
            return nil
        }
    }

    /// Stopped-SMS items not yet notified for their current episode. Keep a
    /// separate latch for these: episodes are keyed by platform, and Google
    /// can be silent and SMS-stopped in turn.
    mutating func newlyStoppedSMS(_ items: [PlatformAttention]) -> [PlatformAttention] {
        newlyNotified(items) { reason in
            if case let .smsStopped(_, since) = reason { return since }
            return nil
        }
    }

    private mutating func newlyNotified(
        _ items: [PlatformAttention],
        episode: (PlatformAttention.Reason) -> Int64?
    ) -> [PlatformAttention] {
        items.filter { item in
            guard let since = episode(item.reason) else { return false }
            if notifiedEpisode[item.key] == since { return false }
            notifiedEpisode[item.key] = since
            return true
        }
    }
}
