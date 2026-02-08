package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDisplayQROutputFormat(t *testing.T) {
	// Capture stdout to verify the output format that the Swift wrapper parses.
	// The Swift PairingView looks for lines containing "https://" to extract the QR URL.

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	testURL := "https://support.google.com/messages/?p=web_computer#?c=testdata"
	displayQR(testURL)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Must contain the URL line that Swift parses
	if !strings.Contains(output, "URL: "+testURL) {
		t.Errorf("output missing 'URL: <url>' line\ngot:\n%s", output)
	}

	// Must contain the URL itself (Swift looks for lines containing "https://")
	foundURL := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "https://") {
			foundURL = true
			break
		}
	}
	if !foundURL {
		t.Error("output contains no line with https:// — Swift wrapper won't find QR URL")
	}

	// Must contain QR art (block characters used by qrterminal)
	if !strings.Contains(output, "█") {
		t.Error("output missing QR code block characters")
	}
}
