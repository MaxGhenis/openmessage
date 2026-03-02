package tools

import "testing"

func TestJoinNames(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"A [sms]"}, "A [sms]"},
		{[]string{"A [sms]", "B [imessage]"}, "A [sms], B [imessage]"},
		{[]string{"A", "B", "C", "D", "E"}, "A, B, C, +2 more"},
	}
	for _, tt := range tests {
		got := joinNames(tt.names)
		if got != tt.want {
			t.Errorf("joinNames(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}
