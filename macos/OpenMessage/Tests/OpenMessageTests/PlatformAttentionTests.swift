import Foundation
import XCTest
@testable import OpenMessage

final class PlatformAttentionTests: XCTestCase {
    private func status(_ json: String) -> [String: Any] {
        let data = Data(json.utf8)
        return (try? JSONSerialization.jsonObject(with: data) as? [String: Any]) ?? [:]
    }

    // The 2026-10-06 shape: Google connected and phone-responding, newest
    // platform (behind_days 0), and silent 12.6 hours, past its baseline. The
    // alert points at the phone, not at re-pairing.
    func testSilentGoogleAsksForThePhoneNotARepair() {
        let json = status("""
        {
          "google": {"connected": true, "paired": true, "needs_pairing": false, "phone_responding": true},
          "whatsapp": {"connected": false, "paired": false},
          "signal": {"connected": false, "paired": false},
          "freshness": {
            "google": {"behind_days": 0, "stale": true, "stale_reason": "silent",
                       "silence": {"silent_ms": 45232898, "last_event_ms": 1791263467102, "stalled": true}},
            "whatsapp": {"behind_days": 46, "stale": true, "stale_reason": "behind"},
            "signal": {"behind_days": 48, "stale": true, "stale_reason": "behind"}
          }
        }
        """)
        let items = PlatformAttention.evaluate(status: json)
        XCTAssertEqual(
            items,
            [PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 12, since: 1_791_263_467_102))]
        )
        XCTAssertEqual(
            PlatformAttention.alertText(for: items),
            "Google Messages has synced nothing for 12 hours. Check that your phone is on and online; restarting it can fix this."
        )
        XCTAssertEqual(PlatformAttention.silentNotificationBody(items[0]), PlatformAttention.alertText(for: items))
    }

    func testBehindPlatformStillAsksForRepair() {
        let json = status("""
        {"google": {"connected": true, "paired": true},
         "freshness": {"google": {"stale": true, "stale_reason": "behind"}}}
        """)
        XCTAssertEqual(
            PlatformAttention.alertText(for: PlatformAttention.evaluate(status: json)),
            "Google Messages needs re-pairing — it has stopped syncing."
        )
    }

    // A backend from before stale_reason existed only says "stale".
    func testStaleWithoutReasonReadsAsRepair() {
        let json = status("""
        {"google": {"connected": true, "paired": true},
         "freshness": {"google": {"stale": true}}}
        """)
        XCTAssertEqual(PlatformAttention.evaluate(status: json).map(\.reason), [.needsRepair])
    }

    func testDisconnectedOrRepairFlagsNeedRepair() {
        let json = status("""
        {"google": {"connected": false, "paired": true},
         "whatsapp": {"connected": true, "paired": true},
         "signal": {"connected": false, "paired": false, "needs_reauth": true}}
        """)
        XCTAssertEqual(
            PlatformAttention.evaluate(status: json).map(\.key),
            ["google", "signal"]
        )
    }

    func testRepairAndSilenceCombine() {
        let json = status("""
        {"google": {"connected": true, "paired": true},
         "whatsapp": {"connected": false, "paired": true},
         "signal": {"connected": true, "paired": true},
         "freshness": {"signal": {"stale": true, "stale_reason": "silent", "silence": {"silent_ms": 1000}}}}
        """)
        XCTAssertEqual(
            PlatformAttention.alertText(for: PlatformAttention.evaluate(status: json)),
            "WhatsApp needs re-pairing — it has stopped syncing. Signal has synced nothing for 1 hour."
        )
    }

    func testHealthyOrUnpairedPlatformsStayQuiet() {
        let json = status("""
        {"google": {"connected": true, "paired": true},
         "whatsapp": {"connected": false, "paired": false},
         "freshness": {"google": {"stale": false, "stale_reason": ""},
                       "whatsapp": {"stale": true, "stale_reason": "silent", "silence": {"silent_ms": 99999999}}}}
        """)
        XCTAssertEqual(PlatformAttention.evaluate(status: json), [])
        XCTAssertNil(PlatformAttention.alertText(for: []))
        XCTAssertNil(PlatformAttention.evaluate(statusData: Data("not json".utf8)))
    }

    // One notification per silence episode. In the 10/6 outage Google flapped
    // between connected and disconnected many times while silent; each
    // reconnect must not notify again. (Review of PR #190.)
    func testSilenceLatchNotifiesOncePerEpisode() {
        let silent = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 7, since: 1_000))
        let later = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 9, since: 1_000))
        let flapping = PlatformAttention(key: "google", name: "Google Messages", reason: .needsRepair)
        let nextEpisode = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 7, since: 5_000))
        var latch = SilenceNotificationLatch()

        XCTAssertEqual(latch.newlySilent([silent]), [silent])
        XCTAssertEqual(latch.newlySilent([later]), [])
        XCTAssertEqual(latch.newlySilent([flapping]), [])
        XCTAssertEqual(latch.newlySilent([]), [])
        XCTAssertEqual(latch.newlySilent([later]), [])
        XCTAssertEqual(latch.newlySilent([nextEpisode]), [nextEpisode])
    }

    func testSilenceLatchTracksPlatformsSeparately() {
        let google = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 7, since: 1_000))
        let signal = PlatformAttention(key: "signal", name: "Signal", reason: .silent(hours: 20, since: 1_000))
        var latch = SilenceNotificationLatch()
        XCTAssertEqual(latch.newlySilent([google]), [google])
        XCTAssertEqual(latch.newlySilent([google, signal]), [signal])
    }

    // The latch state survives a relaunch: restored from what was persisted,
    // the same episode stays quiet. The 10/6 outage had three app relaunches.
    func testSilenceLatchRestoredStateStaysQuietForTheSameEpisode() {
        let silent = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 7, since: 1_000))
        var first = SilenceNotificationLatch()
        XCTAssertEqual(first.newlySilent([silent]), [silent])
        var relaunched = SilenceNotificationLatch(notifiedEpisode: first.notifiedEpisode)
        XCTAssertEqual(relaunched.newlySilent([silent]), [])
    }
}
