package cmd

import "testing"

func TestParseGoogleCookiesInputJSON(t *testing.T) {
	cookies, err := parseGoogleCookiesInput(`{"SID":"sid-value","SAPISID":"sap-value"}`)
	if err != nil {
		t.Fatalf("parseGoogleCookiesInput(): %v", err)
	}
	if cookies["SID"] != "sid-value" || cookies["SAPISID"] != "sap-value" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}

func TestParseGoogleCookiesInputCurl(t *testing.T) {
	curl := `curl 'https://messages.google.com/web/config' -H 'Cookie: SID=sid-value; SAPISID=sap-value'`
	cookies, err := parseGoogleCookiesInput(curl)
	if err != nil {
		t.Fatalf("parseGoogleCookiesInput(): %v", err)
	}
	if cookies["SID"] != "sid-value" || cookies["SAPISID"] != "sap-value" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}

func TestParseGoogleCookiesInputCurlCookieOption(t *testing.T) {
	cookies, err := parseGoogleCookiesInput(`curl 'https://messages.google.com/web/config' -b 'SID=test-only; SAPISID=also-test-only'`)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["SID"] != "test-only" || cookies["SAPISID"] != "also-test-only" {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
}
