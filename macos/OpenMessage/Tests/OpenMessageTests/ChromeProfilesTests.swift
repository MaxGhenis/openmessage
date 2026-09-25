import XCTest
@testable import OpenMessage

final class ChromeProfilesTests: XCTestCase {
    func testParseListsSignedInProfilesFirst() {
        let json = """
        {"profile":{"info_cache":{
            "Default":{"name":"Person 1"},
            "Profile 3":{"user_name":"k@example.com","name":"Kamil"},
            "Profile 14":{"user_name":"a@example.com","gaia_name":"A"},
            "Profile 25":{"name":"SEOtools"}
        }}}
        """
        let profiles = ChromeProfiles.parse(localStateJSON: Data(json.utf8))
        XCTAssertEqual(profiles.map(\.directory), ["Profile 14", "Profile 3", "Default", "Profile 25"])
        XCTAssertEqual(profiles[1].label, "k@example.com  (Profile 3)")
        XCTAssertEqual(profiles[2].label, "Person 1  (Default)")
        XCTAssertEqual(profiles[3].label, "SEOtools  (Profile 25)")
    }

    func testParseToleratesMissingOrMalformedState() {
        XCTAssertEqual(ChromeProfiles.parse(localStateJSON: Data("not json".utf8)), [])
        XCTAssertEqual(ChromeProfiles.parse(localStateJSON: Data("{}".utf8)), [])
    }
}
