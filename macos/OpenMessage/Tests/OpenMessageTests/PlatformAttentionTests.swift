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

    // The latch round-trips through UserDefaults, the way NotificationManager
    // persists it, and an unreadable stored value starts empty.
    func testSilenceLatchRoundTripsThroughUserDefaults() throws {
        let suite = "PlatformAttentionTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }

        let silent = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 7, since: 1_791_263_467_102))
        var latch = SilenceNotificationLatch()
        XCTAssertEqual(latch.newlySilent([silent]), [silent])
        defaults.set(latch.storedValue, forKey: "silenceNotifiedEpisodes")

        var restored = SilenceNotificationLatch(storedValue: defaults.object(forKey: "silenceNotifiedEpisodes"))
        XCTAssertEqual(restored.notifiedEpisode, ["google": 1_791_263_467_102])
        XCTAssertEqual(restored.newlySilent([silent]), [])

        XCTAssertEqual(SilenceNotificationLatch(storedValue: "garbage").notifiedEpisode, [:])
        XCTAssertEqual(SilenceNotificationLatch(storedValue: nil).notifiedEpisode, [:])
    }

    // The 2026-10-04 shape: Google connected, newest platform, not stale, and
    // RCS still flowing, but no incoming SMS for 25 hours.
    func testStoppedSMSAsksForAPhoneRestart() {
        let json = status("""
        {"google": {"connected": true, "paired": true, "phone_responding": true},
         "freshness": {"google": {"behind_days": 0, "stale": false,
                                  "sms_path": {"stalled": true, "reason": "sms_silent_rcs_flowing",
                                               "silent_ms": 90000000, "last_sms_ms": 1791058607000}},
                       "sms_path_stalled": true}}
        """)
        let items = PlatformAttention.evaluate(status: json)
        XCTAssertEqual(items, [PlatformAttention(
            key: "google", name: "Google Messages", reason: .smsStopped(hours: 25, since: 1791058607000)
        )])
        XCTAssertEqual(
            PlatformAttention.alertText(for: items),
            "Your phone has received no SMS for 25 hours, longer than usual, while RCS still arrives, so texts and codes from non-RCS senders may not be getting through. Try restarting the phone."
        )
        XCTAssertEqual(PlatformAttention.smsStoppedNotificationBody(items[0]), PlatformAttention.alertText(for: items))
        XCTAssertNil(PlatformAttention.silentNotificationBody(items[0]))
    }

    func testFlowingOrUnjudgedSMSPathNeedsNothing() {
        for path in [#"{"stalled": false, "reason": "sms_recent"}"#,
                     #"{"stalled": false, "reason": "within_usual_pace", "silent_ms": 100000000}"#,
                     #"{"stalled": false, "reason": "rcs_quiet", "silent_ms": 200000000}"#,
                     #"{"evaluated": false, "stalled": false, "reason": "thin_baseline"}"#] {
            let json = status("""
            {"google": {"connected": true, "paired": true},
             "freshness": {"google": {"stale": false, "sms_path": \(path)}}}
            """)
            XCTAssertEqual(PlatformAttention.evaluate(status: json), [], path)
        }
    }

    // A stale Google entry keeps its own reason: a re-pair or a silent relay
    // explains missing SMS better than the SMS path does.
    func testStaleGoogleOutranksTheSMSPath() {
        let json = status("""
        {"google": {"connected": true, "paired": true},
         "freshness": {"google": {"stale": true, "stale_reason": "silent",
                                  "silence": {"silent_ms": 45000000, "last_event_ms": 7},
                                  "sms_path": {"stalled": true, "silent_ms": 90000000, "last_sms_ms": 5}}}}
        """)
        XCTAssertEqual(PlatformAttention.evaluate(status: json).map(\.reason), [.silent(hours: 12, since: 7)])
    }

    func testUnpairedGoogleIgnoresTheSMSPath() {
        let json = status("""
        {"google": {"connected": false, "paired": false},
         "freshness": {"google": {"stale": false, "sms_path": {"stalled": true, "silent_ms": 90000000}}}}
        """)
        XCTAssertEqual(PlatformAttention.evaluate(status: json), [])
    }

    // One notification per SMS outage, kept apart from the relay-silence
    // latch: Google can be SMS-stopped and then silent within one outage.
    func testSMSLatchNotifiesOncePerEpisodeAndSeparatelyFromSilence() {
        let stopped = PlatformAttention(key: "google", name: "Google Messages", reason: .smsStopped(hours: 25, since: 100))
        let later = PlatformAttention(key: "google", name: "Google Messages", reason: .smsStopped(hours: 49, since: 100))
        let next = PlatformAttention(key: "google", name: "Google Messages", reason: .smsStopped(hours: 24, since: 900))
        let silent = PlatformAttention(key: "google", name: "Google Messages", reason: .silent(hours: 13, since: 500))
        var smsLatch = SilenceNotificationLatch()
        var silenceLatch = SilenceNotificationLatch()
        XCTAssertEqual(smsLatch.newlyStoppedSMS([stopped]), [stopped])
        XCTAssertEqual(smsLatch.newlyStoppedSMS([later]), [])
        XCTAssertEqual(silenceLatch.newlySilent([silent]), [silent])
        XCTAssertEqual(smsLatch.newlyStoppedSMS([silent]), [])
        XCTAssertEqual(silenceLatch.newlySilent([stopped]), [])
        XCTAssertEqual(smsLatch.newlyStoppedSMS([later]), [], "a relay silence in between does not re-arm the SMS episode")
        XCTAssertEqual(smsLatch.newlyStoppedSMS([next]), [next])
    }
}
