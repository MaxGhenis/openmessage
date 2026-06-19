package web

import "testing"

func TestNormalizePhoneNumber(t *testing.T) {
	cases := map[string]string{
		"15629249581":      "+15629249581", // 11 digits, no plus (the bug)
		"+13106194153":     "+13106194153", // already E.164
		"+1 (310) 619-4153": "+13106194153", // formatted with plus
		"(310) 555-1234":   "+13105551234", // 10-digit US, formatted
		"3105551234":       "+13105551234", // bare 10-digit US
		"57527":            "57527",         // short code — left alone
		"":                 "",
		"   ":              "",
	}
	for in, want := range cases {
		if got := normalizePhoneNumber(in); got != want {
			t.Errorf("normalizePhoneNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
