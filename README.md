# OpenMessage

OpenMessage is a local-first messaging workspace for Google Messages, WhatsApp, and Signal. Use it from the native macOS app, the localhost web UI, or any MCP-compatible client.

Built on [mautrix/gmessages](https://github.com/mautrix/gmessages) (libgm) for the Google Messages protocol and [mcp-go](https://github.com/mark3labs/mcp-go) for the MCP server.

## What it does

- **Google Messages for Mac** — pair your Android phone and read/send SMS + RCS locally
- **Live WhatsApp support** — link WhatsApp as a live companion device on your machine
- **Live Signal support** — link Signal locally and keep its threads in the same inbox
- **One local inbox** — search, route-aware threads, favorites, media, reactions, drafts, scheduled sends, and grouped contacts
- **Rich compose** — text, media, GIFs, drag/drop attachments, link previews, replies, forwards, and browser-side queued sends while a route reconnects
- **macOS app + PWA web UI** — native wrapper with notifications and contact photos, plus an installable localhost UI
- **MCP-ready** — expose the same local inbox to Claude Code and other MCP clients over stdio, Streamable HTTP, or SSE

## Quick start

### Prerequisites

- **Go 1.22+** ([install](https://go.dev/dl/))
- **Google Messages** on your Android phone

### 1. Clone and build

```bash
git clone https://github.com/MaxGhenis/openmessage.git
cd openmessage
go build -o openmessage .
```

### 2. Pair with your phone

```bash
./openmessage pair
```

By default, a QR code appears in your terminal. On your phone, open **Google Messages > Settings > Device pairing > Pair a device** and scan it. The session saves to `~/.local/share/openmessage/session.json`.

If Google only offers account pairing, you can also pair with Google account cookies copied from browser devtools:

```bash
pbpaste | ./openmessage pair --google
```

The CLI accepts either a JSON cookie object or a full `curl` command for `messages.google.com/web/config`, then prompts you to confirm an emoji on your phone.

### 3. Start the server

```bash
./openmessage serve
```

This starts the **Web UI** at [http://127.0.0.1:7007](http://127.0.0.1:7007). A bare `serve` is web-only.

MCP transports require an explicit opt-in:

- `./openmessage serve --mcp-sse` starts MCP Streamable HTTP and SSE without the web UI.
- `./openmessage serve --web --mcp-sse` starts the web UI and both HTTP MCP endpoints.
- `./openmessage serve --mcp-stdio` starts MCP over stdio for pipe-based clients.

### 3a. Optional: link WhatsApp or Signal

After `serve` is running, open the local UI and link WhatsApp or Signal from the Connections surface. OpenMessage keeps those bridges local and syncs them into the same inbox as Google Messages.

### 3b. Safe demo mode for screenshots and recordings

```bash
./openmessage demo
```

or:

```bash
./openmessage serve --demo
```

Demo mode starts the same local UI on the normal port, but:
- uses a fresh temporary data directory instead of your real message store
- seeds fake SMS and WhatsApp conversations for screenshots and demos
- disables live Google Messages, WhatsApp, and local desktop import sync

This is the safest way to capture website screenshots, App Store assets, or demo recordings without real messages bleeding back in.

### 4. Connect to Claude Code

Add to `~/.mcp.json`:

```json
{
  "mcpServers": {
    "openmessage": {
      "command": "/path/to/openmessage",
      "args": ["serve", "--mcp-stdio"]
    }
  }
}
```

Restart Claude Code. The MCP tools appear automatically.

To let a client connect over HTTP, start OpenMessage with `--mcp-sse` (or `--web --mcp-sse` to keep the web UI), then point it at:

- Streamable HTTP: `http://127.0.0.1:7007/mcp`
- SSE: `http://127.0.0.1:7007/mcp/sse`

## Features

- **Read messages** — full conversation history, search, media, replies, reactions, and route-aware conversation previews
- **Send messages** — SMS/RCS plus live WhatsApp and Signal text, media, reactions, replies, forwards, and send-later scheduling
- **Queued compose** — browser-side pending sends keep text and attachment previews visible while a route reconnects, then flush with idempotency keys to avoid duplicate sends
- **GIF picker** — Klipy-backed GIF search, preview proxying, favorites, captions, and media sends from the web composer
- **Attachment workflow** — upload, paste, drag/drop, preview, fullscreen viewing, PDFs, audio/video, and generic file cards
- **Link previews** — local preview/image proxying for message URLs with SSRF-safe URL validation
- **Favorite threads** — star important conversations and jump to them from the favorites rail
- **Live WhatsApp sync** — pair a local WhatsApp companion device for inbound messages, typing indicators, read state, and media
- **Live Signal sync** — pair a local Signal linked device for inbound messages, media, reactions, and group threads
- **React to messages** — emoji reactions on any message
- **Image/media display** — inline images, video, audio, and fullscreen viewer
- **Contact workspace** — grouped people across routes, contact metadata, tags, reach-out cadence, and relationship summaries
- **Google repair flows** — detects expired Google cookies, stuck sessions, and dead credentials, then offers guided reconnect or re-pair flows
- **Desktop notifications** — native macOS notifications for fresh inbound messages
- **Web UI + macOS app** — real-time conversation view at localhost:7007 and a native wrapper
- **MCP tools** — conversation lookup, route-aware text/media/reaction sends, media download, import helpers, and story/viz tools
- **Local storage** — SQLite database, your data stays on your machine

## MCP tools

| Tool | Description |
|------|-------------|
| `get_messages` | Recent messages with filters (phone, date range, limit) |
| `get_conversation` | Messages in a specific conversation |
| `search_messages` | Full-text search across all messages |
| `send_message` | Send a direct text by platform. Defaults to SMS/RCS and also supports direct WhatsApp/Signal recipients |
| `send_to_conversation` | Send a text reply directly to an existing conversation ID |
| `send_media_to_conversation` | Send a local file attachment to an existing conversation ID |
| `send_group_message` | Send to a group conversation by conversation ID |
| `react_to_message` | Add, remove, or switch a reaction on an existing message |
| `set_message_transcript` | Save or update a transcript for an audio/media message |
| `list_conversations` | List recent conversations |
| `list_contacts` | List/search contacts |
| `resolve_contact_routes` | Resolve a person or phone number to the available SMS/WhatsApp/Signal routes |
| `get_status` | Google Messages, WhatsApp, and Signal connection status |
| `download_media` | Return the local `/api/media/<message-id>` URL and metadata for an attachment |
| `draft_message` | Save a draft for the local app to review/send later |
| `import_messages` | Import Google Chat, iMessage, WhatsApp export, or Signal Desktop history |
| `get_person_messages` | Load messages for conversations matching a person's name |
| `get_person_messages_range` | Load a bounded time range of messages matching a person's name |
| `conversation_stats` | Summarize conversation counts, cadence, and activity |
| `person_stats` | Summarize a person's cross-route activity |
| `generate_story` | Generate a narrative summary for one conversation |
| `generate_person_story` | Generate a narrative summary for a person across routes |
| `generate_viz` | Generate a local HTML visualization from a conversation |
| `render_story` | Render a story/viz HTML artifact with optional local photos |

## MCP examples

- List recent Signal threads: `list_conversations(source_platform="signal")`
- Search WhatsApp for a keyword: `search_messages(query="airbnb")`
- Send a direct Signal message: `send_message(platform="signal", recipient="+15551230000", message="On my way")`
- Send a text into a route-aware thread: `send_to_conversation(conversation_id="whatsapp:15551234567@s.whatsapp.net", message="On my way")`
- Send a photo from disk: `send_media_to_conversation(conversation_id="signal-group:abc123", file_path="/tmp/photo.jpg", caption="Here")`
- React to a message: `react_to_message(conversation_id="signal-group:abc123", message_id="signal:...", emoji="🔥")`
- Resolve a person's available routes before sending: `resolve_contact_routes(query="Taylor")`
- Review one person's cross-platform history: `get_person_messages(name="Taylor", limit=50)`
- Import Signal Desktop history: `import_messages(source="signal", path="$HOME/Library/Application Support/Signal", name="Your Name", address="+15551230000")`

## Web UI

The web UI runs at `http://localhost:7007` when the server is started. It provides:

- Conversation list with search and grouped multi-route contacts
- Message view with images, video, audio, reactions, and reply threads
- Favorites rail and thread-level shortcuts
- Route-aware compose, send, reply, forward, react, and send later
- GIF picker, emoji picker, media upload, drag/drop attachments, paste attachments, captions, and attachment previews
- Link previews and document/file cards
- Browser-side queued sends for temporarily disconnected routes
- Contact popovers, copied numbers, person-level summaries, tags, and reach-out cadence
- Google Messages + WhatsApp + Signal connection controls
- Google reconnect, cookie-expiry, and re-pair affordances
- Live typing indicators, read-state rendering, and notifications

## Native macOS app

The repo also includes a native Swift wrapper around the same local backend:

- embedded local OpenMessage backend
- native notifications
- contact photos
- the same Google Messages, WhatsApp, and Signal pairing/runtime model as the web UI

The macOS app target lives under `OpenMessage/`.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `OPENMESSAGES_DATA_DIR` | `~/.local/share/openmessage` | Data directory (DB + session) |
| `OPENMESSAGES_LOG_LEVEL` | `info` | Log level (debug/info/warn/error/trace) |
| `OPENMESSAGES_PORT` | `7007` | Web UI port |
| `OPENMESSAGES_HOST` | `127.0.0.1` | Host/interface to bind the local web server to |
| `OPENMESSAGES_MY_NAME` | system user name | Display name for outgoing imported iMessage/WhatsApp messages |
| `OPENMESSAGES_STARTUP_BACKFILL` | `auto` | Startup history sync mode: `auto`, `shallow`, `deep`, or `off` |
| `OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS` | `0` | Opt in to deep backfill's Phase C (contact-based orphan discovery). **Off by default** because it creates an empty SMS thread on your phone for each contact without prior message history. Enable with `1`/`true`/`yes`/`on` only if you understand the side effect. |
| `OPENMESSAGES_KLIPY_API_KEY` | unset | Optional Klipy API key for the web compose GIF picker. GIF search is unavailable until this is set. `KLIPY_API_KEY` is also accepted as a fallback. |
| `OPENMESSAGE_COOKIE_REFRESH_SCRIPT` | unset | Optional command run before reconnect when Google session cookies expire. If unset, OpenMessage backs off and prompts for manual re-pair instead of hammering Google's auth endpoint. |
| `OPENMESSAGES_MACOS_NOTIFICATIONS` | interactive macOS `serve` sessions only | Enable/disable native macOS notifications for fresh inbound live messages (`1`/`0`). Click-through opens the matching thread when `terminal-notifier` is available. The macOS app sets this to `0` and posts its own notifications (with tap-to-open and active-thread suppression) instead. |
| `OPENMESSAGES_SIGNAL_TMP_SWEEP` | enabled | Set to `0` to disable the cleanup of stale signal-cli temp directories (the app-owned run dirs plus `libsignal*` dirs older than 24h that pre-v0.2.10 builds leaked into the system temp dir — see #27). |
| `OPENMESSAGES_SIGNAL_CLI` | auto-detected `signal-cli` | Optional path to a specific `signal-cli` binary. |
| `OPENMESSAGES_GOOGLE_AVATAR_SYNC` | enabled | Set to `0` to disable Google contact avatar sync. The older singular `OPENMESSAGE_GOOGLE_AVATAR_SYNC` name is also accepted. |
| `OPENMESSAGES_EXPORT_DIR` | `~/Documents/OpenMessage` | Directory for `generate_viz` / `render_story` HTML outputs and photo inputs when using the default confined export mode. |
| `OPENMESSAGES_ALLOW_ANY_EXPORT_PATH` | unset (off) | Set to `1`/`true`/`yes`/`on` to allow viz/story tools to read photos from, or write HTML to, arbitrary local paths. |
| `OPENMESSAGE_TELEMETRY` | unset (off) | Set to `1` to send one anonymous heartbeat per launch (max one per 24h). Reports only: random install ID, version, OS/arch, and which platforms are paired (Google Messages / WhatsApp / Signal). No message content, no contact info, no IP-based identity. See `internal/telemetry/`. |

`OPENMESSAGES_HOST` defaults to localhost for safety. If you bind the server to another interface for LAN testing, keep it on a trusted network; local-control APIs and MCP endpoints are meant for same-origin/local clients.

### GIF picker setup

The web compose GIF picker uses Klipy search. To enable it:

1. Create a Klipy API key.
2. Start OpenMessage with the key in the environment:

   ```bash
   export OPENMESSAGES_KLIPY_API_KEY=<your key>
   ./openmessage serve
   ```

3. Optional: verify the local endpoint can reach Klipy:

   ```bash
   curl "http://127.0.0.1:7007/api/gifs/trending?limit=1"
   ```

4. Open the web UI and use the `GIF` button in a media-capable conversation. GIF sends use the same media path as other attachments, so they are only enabled for routes that currently support media.

OpenMessage does not include a bundled GIF provider key. Without `OPENMESSAGES_KLIPY_API_KEY`, GIF search endpoints return a setup error and the rest of messaging continues to work normally.

If you launch OpenMessage from the macOS app, launchd, systemd, or another supervisor, configure the environment there too; exporting the key in a shell only affects `serve` processes started from that shell.

## Architecture

- **libgm** handles the Google Messages protocol (pairing, encryption, long-polling)
- **whatsmeow** handles live WhatsApp pairing, sync, text/media send, receipts, typing, and avatars through a separate local session store
- **signal-cli** powers the local Signal linked-device bridge, message sync, media, and reactions
- **SQLite** (WAL mode, pure Go) stores messages, conversations, and contacts locally
- Real-time events from the phone are written to SQLite as they arrive
- The native macOS app and the localhost web UI run against the same local backend
- WhatsApp Desktop import remains as a fallback/repair path when the live bridge is not active
- Signal Desktop history can be imported into the same local store for backfill and repair workflows
- On first run, a deep backfill fetches full SMS/RCS history in the background; later runs do a lighter incremental sync by default
- Scheduled messages live in SQLite, are claimed atomically by the background scheduler, and retry when the target platform is temporarily disconnected
- The web composer stores pending browser-side sends in local storage/IndexedDB and attaches idempotency keys to prevent duplicate delivery during retry
- MCP tool handlers read from SQLite for queries and route sends through the same local runtime
- MCP HTTP transport supports both Streamable HTTP at `/mcp` and SSE at `/mcp/sse` when enabled with `--mcp-sse`; stdio is available with `--mcp-stdio`
- GIF and link-preview proxies validate remote URLs, cap downloads, and keep provider fetches inside the local backend
- Auth tokens auto-refresh and persist to `session.json`; expired Google cookies can be refreshed by an optional script or surfaced as a guided re-pair flow

## Development

```bash
go test ./...        # Run all tests
go build .           # Build binary
npm install          # Install Playwright test runner
npx playwright install chromium
npm run test:e2e     # Run browser-level web UI tests
./openmessage pair  # Pair with phone
./openmessage serve # Start server
./openmessage demo  # Start isolated fake-data demo mode
```

Before publishing a build or website update, run through [the release checklist](docs/release-checklist.md).

Debugging a live install (failing sends, re-pairing, the two-data-dir gotcha, signal-cli version)? See [docs/agent-runbook.md](docs/agent-runbook.md).

## License

MIT
