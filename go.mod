module github.com/maxghenis/openmessage

go 1.26.0

toolchain go1.26.10

require (
	github.com/google/uuid v1.6.0
	github.com/mark3labs/mcp-go v0.47.1
	github.com/mdp/qrterminal/v3 v3.2.1
	github.com/rs/zerolog v1.35.1
	go.mau.fi/libsignal v0.2.2
	go.mau.fi/mautrix-gmessages v0.2601.0
	go.mau.fi/util v0.11.0
	go.mau.fi/whatsmeow v0.0.0-20261029193418-2fc844fe1693
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	golang.org/x/sync v0.24.0
	golang.org/x/term v0.47.0
	google.golang.org/protobuf v1.36.12
	modernc.org/sqlite v1.61.0
	rsc.io/qr v0.2.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/beeper/argo-go v1.1.2 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/petermattis/goid v0.0.0-20260918085751-abfca077860b // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/vektah/gqlparser/v2 v2.5.64 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.79.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace go.mau.fi/mautrix-gmessages => github.com/MaxGhenis/gmessages v0.2602.1-0.20261021163109-6495a585293d
