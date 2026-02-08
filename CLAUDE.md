# OpenMessages

Open-source Google Messages client for macOS with built-in MCP server.

## Architecture

```
├── cmd/              Go CLI commands (pair, serve)
├── internal/         Go backend (app, client, db, web, tools)
├── macos/            Swift macOS app wrapper
│   ├── OpenMessages/ Swift package (BackendManager, PairingView, etc.)
│   └── build.sh      Builds universal binary + .app + .dmg
├── site/             Static website (deployed to openmessages.ai)
└── vercel.json       Vercel config (root — NOT site/vercel.json)
```

## Vercel deployment (openmessages.ai)

**Config lives at root `vercel.json`**, not `site/vercel.json`. The root config sets `outputDirectory: "site"` and `cleanUrls: true`.

After pushing changes to `site/`:
```bash
cd /Users/maxghenis/openmessages && vercel --prod
```

**Always verify after deploy:**
```bash
curl -s -o /dev/null -w "%{http_code}" https://openmessages.ai
```

## Building the macOS app

```bash
./macos/build.sh
```

This builds: Go universal binary (arm64+amd64) → Swift app → .app bundle → .dmg

To install locally:
```bash
cp -R macos/build/OpenMessages.app /Applications/ && xattr -cr /Applications/OpenMessages.app
```

To update the GitHub release:
```bash
gh release upload v0.1.0 macos/build/OpenMessages.dmg --repo MaxGhenis/openmessages --clobber
```

## Testing

```bash
go test ./cmd/ -v      # Unit + integration tests
go test ./... -v       # All tests
```

## Key files

- `internal/app/app.go` — data dir resolution (`OPENMESSAGES_DATA_DIR` env var, defaults to `~/.local/share/openmessages`)
- `internal/client/events.go` — handles Google Messages protocol events
- `macos/OpenMessages/Sources/BackendManager.swift` — launches Go backend, manages app state
- `macos/OpenMessages/Sources/PairingView.swift` — QR code pairing UI
