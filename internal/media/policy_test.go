package media

import "testing"

func TestServingDisposition(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	html := []byte("<html><body><script>alert(1)</script></body></html>")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	xml := []byte(`<?xml version="1.0"?><root/>`)

	tests := []struct {
		name     string
		declared string
		sniff    []byte
		want     Disposition
	}{
		{name: "PNG image", declared: "image/png", sniff: png, want: DispositionInline},
		{name: "audio", declared: "audio/mpeg", sniff: []byte("ID3\x04\x00\x00"), want: DispositionInline},
		{name: "video", declared: "video/mp4", sniff: []byte("\x00\x00\x00\x18ftypmp42"), want: DispositionInline},
		{name: "PDF", declared: "application/pdf", sniff: []byte("%PDF-1.7\n"), want: DispositionInline},
		{name: "plain text", declared: "text/plain", sniff: []byte("hello world\n"), want: DispositionInline},
		{name: "normalized passive MIME", declared: " IMAGE/PNG ; charset=binary ", sniff: png, want: DispositionInline},
		{name: "SVG", declared: "image/svg+xml", sniff: svg, want: DispositionForcedDownload},
		{name: "HTML", declared: "text/html", sniff: html, want: DispositionForcedDownload},
		{name: "XHTML", declared: "application/xhtml+xml", sniff: html, want: DispositionForcedDownload},
		{name: "text XML", declared: "text/xml", sniff: xml, want: DispositionForcedDownload},
		{name: "application XML", declared: "application/xml", sniff: xml, want: DispositionForcedDownload},
		{name: "unknown", declared: "application/x-unknown", sniff: []byte("opaque bytes"), want: DispositionForcedDownload},
		{name: "empty", declared: "", sniff: []byte("opaque bytes"), want: DispositionForcedDownload},
		{name: "PNG declared HTML sniffed spoof", declared: "image/png", sniff: html, want: DispositionForcedDownload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServingDisposition(tt.declared, tt.sniff); got != tt.want {
				t.Fatalf("ServingDisposition(%q) = %q, want %q", tt.declared, got, tt.want)
			}
		})
	}
}

func TestSanitizeMediaFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "photo.png", want: "photo.png"},
		{name: "slash path", in: "  ../../photo.png  ", want: "photo.png"},
		{name: "backslash path", in: `C:\temp\photo.png`, want: "photo.png"},
		{name: "controls and quote", in: "bad\x00\r\n\"name.png", want: "badname.png"},
		{name: "non ASCII", in: "caf\u00e9.png", want: "caf_.png"},
		{name: "empty after sanitizing", in: "\"\r\n", want: "attachment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeMediaFilename(tt.in); got != tt.want {
				t.Fatalf("sanitizeMediaFilename(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
