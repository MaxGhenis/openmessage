# OpenMessage

OpenMessage is a local-first messaging workspace for Google Messages, WhatsApp, and Signal. Use it from the native macOS app, the localhost web UI, or any MCP-compatible client.

Built on [mautrix/gmessages](https://github.com/mautrix/gmessages) (libgm) for the Google Messages protocol and [mcp-go](https://github.com/mark3labs/mcp-go) for the MCP server.

## What it does

- **Google Messages for Mac** — pair your Android phone and read/send SMS + RCS locally
- **Live WhatsApp support** — link WhatsApp as a live companion device on your machine
- **Live Signal support** — link Signal locally and keep its threads in the same inbox
- **One local inbox** — search, route-aware threads, media, reactions, drafts, and grouped contacts
- **macOS app + web UI** — native wrapper with notifications and contact photos, plus a localhost UI
- **MCP-ready** — expose the same local inbox to Claude Code and other MCP clients

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

This starts both:
- **Web UI** at [http://127.0.0.1:7007](http://127.0.0.1:7007)
- **MCP SSE endpoint** at `http://127.0.0.1:7007/mcp/sse`

When `serve` is launched by an MCP client over pipes, it also serves MCP on stdio automatically.

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
      "args": ["serve"]
    }
  }
}
```

Restart Claude Code. The MCP tools appear automatically.

## Features

- **Read messages** — full conversation history, search, media, replies, reactions
- **Send messages** — SMS/RCS plus live WhatsApp and Signal text, media, and reactions
- **Live WhatsApp sync** — pair a local WhatsApp companion device for inbound messages, typing indicators, read state, and media
- **Live Signal sync** — pair a local Signal linked device for inbound messages, media, reactions, and group threads
- **React to messages** — emoji reactions on any message
- **Image/media display** — inline images, video, audio, and fullscreen viewer
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
| `react_to_message` | Add, remove, or switch a reaction on an existing message |
| `list_conversations` | List recent conversations |
| `list_contacts` | List/search contacts |
| `get_status` | Google Messages, WhatsApp, and Signal connection status |
| `download_media` | Download an attachment from a message to a local temp file |
| `draft_message` | Save a draft for the local app to review/send later |
| `import_messages` | Import Google Chat, iMessage, WhatsApp export, or Signal Desktop history |

## MCP examples

- List recent Signal threads: `list_conversations(source_platform="signal")`
- Search WhatsApp for a keyword: `search_messages(query="airbnb")`
- Send a direct Signal message: `send_message(platform="signal", recipient="+15551230000", message="On my way")`
- Send a text into a route-aware thread: `send_to_conversation(conversation_id="whatsapp:15551234567@s.whatsapp.net", message="On my way")`
- Send a photo from disk: `send_media_to_conversation(conversation_id="signal-group:abc123", file_path="/tmp/photo.jpg", caption="Here")`
- React to a message: `react_to_message(conversation_id="signal-group:abc123", message_id="signal:...", emoji="🔥")`
- Import Signal Desktop history: `import_messages(source="signal", path="$HOME/Library/Application Support/Signal", name="Your Name", address="+15551230000")`

## Web UI

The web UI runs at `http://localhost:7007` when the server is started. It provides:

- Conversation list with search and grouped multi-route contacts
- Message view with images, video, audio, reactions, and reply threads
- Route-aware compose and send
- Google Messages + WhatsApp + Signal connection controls
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
| `OPENMESSAGES_MACOS_NOTIFICATIONS` | interactive macOS `serve` sessions only | Enable/disable native macOS notifications for fresh inbound live messages (`1`/`0`). Click-through opens the matching thread when `terminal-notifier` is available. The macOS app sets this to `0` and posts its own notifications (with tap-to-open and active-thread suppression) instead. |
| `OPENMESSAGES_SIGNAL_TMP_SWEEP` | enabled | Set to `0` to disable the cleanup of stale signal-cli temp directories (the app-owned run dirs plus `libsignal*` dirs older than 24h that pre-v0.2.10 builds leaked into the system temp dir — see #27). |
| `OPENMESSAGES_EXPORT_DIR` | `~/Documents/OpenMessage` | Directory for `generate_viz` / `render_story` HTML outputs and photo inputs when using the default confined export mode. |
| `OPENMESSAGES_ALLOW_ANY_EXPORT_PATH` | unset (off) | Set to `1`/`true`/`yes`/`on` to allow viz/story tools to read photos from, or write HTML to, arbitrary local paths. |
| `OPENMESSAGE_TELEMETRY` | unset (off) | Set to `1` to send one anonymous heartbeat per launch (max one per 24h). Reports only: random install ID, version, OS/arch, and which platforms are paired (Google Messages / WhatsApp / Signal). No message content, no contact info, no IP-based identity. See `internal/telemetry/`. |

### GIF picker setup

The web compose GIF picker uses Klipy search. To enable it:

1. Create a Klipy API key.
2. Start OpenMessage with `OPENMESSAGES_KLIPY_API_KEY=<your key>`.
3. Open the web UI and use the `GIF` button in a media-capable conversation.

OpenMessage does not include a bundled GIF provider key. Without `OPENMESSAGES_KLIPY_API_KEY`, GIF search endpoints return a setup error and the rest of messaging continues to work normally.

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
- MCP tool handlers read from SQLite for queries and route sends through the same local runtime
- Auth tokens auto-refresh and persist to `session.json`

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
