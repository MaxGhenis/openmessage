package importer

import (
	"testing"
)

// makeAttributedBody builds a minimal streamtyped-style attributedBody blob
// with the given text using a single-byte length (text < 128 bytes).
func makeAttributedBody(text string) []byte {
	blob := []byte("streamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84NSMutableAttributedString\x00")
	blob = append(blob, []byte("\x84\x84NSString\x01\x94\x84\x01+")...)
	blob = append(blob, byte(len(text)))
	blob = append(blob, []byte(text)...)
	return blob
}

func TestDecodeAttributedBody(t *testing.T) {
	t.Run("short text", func(t *testing.T) {
		got := decodeAttributedBody(makeAttributedBody("Hello there"))
		if got != "Hello there" {
			t.Errorf("got %q, want %q", got, "Hello there")
		}
	})

	t.Run("unicode text", func(t *testing.T) {
		want := "héllo 👋 wörld"
		got := decodeAttributedBody(makeAttributedBody(want))
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("two-byte length (>127 bytes)", func(t *testing.T) {
		text := ""
		for i := 0; i < 200; i++ {
			text += "x"
		}
		blob := []byte("\x84\x84NSString\x01\x94\x84\x01+\x81")
		blob = append(blob, byte(len(text)&0xff), byte(len(text)>>8))
		blob = append(blob, []byte(text)...)
		if got := decodeAttributedBody(blob); got != text {
			t.Errorf("two-byte length: got %d chars, want %d", len(got), len(text))
		}
	})

	t.Run("no NSString marker returns empty", func(t *testing.T) {
		if got := decodeAttributedBody([]byte("garbage data with no marker")); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("length past end returns empty (no panic)", func(t *testing.T) {
		blob := []byte("\x84\x84NSString\x01\x94\x84\x01+\x7f") // claims 127 bytes but none follow
		if got := decodeAttributedBody(blob); got != "" {
			t.Errorf("expected empty for truncated blob, got %q", got)
		}
	})

	t.Run("empty blob returns empty", func(t *testing.T) {
		if got := decodeAttributedBody(nil); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}
