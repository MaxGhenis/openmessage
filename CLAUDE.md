# OpenMessage

Local-first universal message database with built-in MCP server. Ingests messages from SMS/RCS (Google Messages), Google Chat, iMessage, and WhatsApp.

## Architecture

```
├── cmd/              Go CLI commands (pair, serve, send, import)
├── internal/
│   ├── app/          Bootstrap, data dir, backfill
│   ├── client/       libgm Google Messages protocol
│   ├── db/           SQLite store (conversations, messages, contacts, unified_contacts, drafts)
│   ├── importer/     Multi-platform import adapters (gchat, imessage, whatsapp)
│   ├── story/        Stats computation + narrative story generation
│   ├── tools/        MCP tools (16 tools)
│   ├── viz/          Relationship visualization renderer (self-contained HTML)
│   └── web/          HTTP API + embedded React UI
├── macos/            Swift macOS app wrapper
│   ├── OpenMessage/  Swift package (BackendManager, PairingView, etc.)
│   └── build.sh      Builds universal binary + .app + .dmg
├── site/             Static website (deployed to openmessage.ai)
└── vercel.json       Vercel config (root — NOT site/vercel.json)
```

## Multi-platform import

```bash
openmessage import gchat /path/to/Takeout/Google\ Chat/Groups/ --email you@gmail.com
openmessage import gchat-conversation /path/to/messages.json --email you@gmail.com
openmessage import imessage                     # reads ~/Library/Messages/chat.db (needs Full Disk Access)
openmessage import whatsapp /path/to/chat.txt --name "Your Name"
```

### MCP tools

16 tools registered:
- `get_messages`, `get_conversation`, `search_messages` — cross-platform by default
- `list_conversations` — optional `source_platform` filter (sms, gchat, imessage, whatsapp)
- `get_person_messages` — all messages with a person across all platforms
- `import_messages` — import from any supported source
- `conversation_stats` — volume, heatmap, phrases, response times, gaps (single conversation)
- `generate_story` — narrative chapters with optional Claude API enhancement (single conversation)
- `person_stats` — cross-platform stats for all 1:1 messages with a person (merges + deduplicates)
- `generate_person_story` — cross-platform narrative story for a person (merges + deduplicates)
- `generate_viz` — self-contained HTML visualization combining data dashboards + narrative (see below)
- `send_message`, `draft_message`, `download_media`, `list_contacts`, `get_status`

### HTTP API

- `GET /api/stats/{conversation_id}` — conversation statistics JSON
- `GET /api/story/{conversation_id}?style=intimate&api_key=...` — generated story JSON
- `GET /api/conversations?limit=50` — list all conversations (all platforms)
- `GET /api/search?q=...` — search across all platforms

### Schema

Messages and conversations have `source_platform` (sms/gchat/imessage/whatsapp/signal/telegram) and messages have `source_id` for dedup. Unified contacts table maps people across platforms.

## Vercel deployment (openmessage.ai)

**CRITICAL: Always deploy from the repo root**, not from `~` or any other directory. The `.vercel/project.json` links to the correct project/scope.

**Config lives at root `vercel.json`**, not `site/vercel.json`. The root config sets `outputDirectory: "site"` and `cleanUrls: true`. A `.vercelignore` excludes Go/Swift build artifacts.

**Scope: `max-ghenis-projects`** (personal account, NOT PolicyEngine).

Deploy:
```bash
cd /Users/maxghenis/openmessages && vercel --prod
```

**Always verify after deploy:**
```bash
curl -s -o /dev/null -w "%{http_code}" https://openmessage.ai
```

**Domains:** `openmessage.ai` (primary) and `openmessages.ai` (alias), both on Cloudflare DNS → 76.76.21.21.

## Building the macOS app

```bash
./macos/build.sh
```

This builds: Go universal binary (arm64+amd64) → Swift app → .app bundle → .dmg

To install locally:
```bash
cp -R macos/build/OpenMessage.app /Applications/ && xattr -cr /Applications/OpenMessage.app
```

To update the GitHub release:
```bash
gh release upload v0.1.0 macos/build/OpenMessage.dmg --repo MaxGhenis/openmessage --clobber
```

## Testing

```bash
go test ./cmd/ -v      # Unit + integration tests
go test ./... -v       # All tests
```

## Relationship visualization (`generate_viz`)

Generates a self-contained HTML file combining data dashboards with narrative chapters. Output is deployable to Vercel or viewable locally.

**Sections**: password gate, hero, timeline nav, narrative chapters (early/middle/late), monthly volume chart (Chart.js), sender split donut, response times, hour-of-week heatmap, phrase cloud (colored by sender ratio), longest gap callout, photo gallery, interludes, closing.

**Key parameters**: `name` (person to search), `output_path`, `timezone` (default ET), `password`, `api_key` (for Claude-generated narrative), colors (`primary_color`, `secondary_color`, etc.).

**Architecture**:
- `internal/viz/config.go` — `VizConfig` struct, section ordering, color theming
- `internal/viz/render.go` — `RenderHTML()` orchestrator, Chart.js data building
- `internal/viz/template.go` — Go html/template with all CSS/JS inline (except CDN fonts + Chart.js)
- `internal/viz/photos.go` — `FindMediaMessages()` for photo gallery
- `internal/tools/viz.go` — MCP tool handler

**Stats engine extensions** (`internal/story/stats.go`):
- `PhraseCount.BySender` — per-sender phrase counts for colored word cloud
- `ComputeStats(messages, tz)` — timezone parameter for TZ-shifted heatmap

## Key files

- `internal/app/app.go` — data dir resolution (`OPENMESSAGES_DATA_DIR` env var, defaults to `~/.local/share/openmessage`)
- `internal/db/db.go` — schema, structs, migration
- `internal/importer/` — gchat.go, imessage.go, whatsapp.go
- `internal/story/stats.go` — conversation statistics computation (with timezone + per-sender phrases)
- `internal/story/generate.go` — narrative story generation (local or Claude API)
- `internal/viz/` — relationship visualization renderer (config, template, render, photos)
- `internal/client/events.go` — handles Google Messages protocol events
- `macos/OpenMessage/Sources/BackendManager.swift` — launches Go backend, manages app state
