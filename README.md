# OpenMessage

OpenMessage is a local-first messaging workspace for Google Messages and WhatsApp. Use it from the native macOS app, the localhost web UI, or any MCP-compatible client.

Built on [mautrix/gmessages](https://github.com/mautrix/gmessages) (libgm) for the Google Messages protocol and [mcp-go](https://github.com/mark3labs/mcp-go) for the MCP server.

## What it does

- **Google Messages for Mac** — pair your Android phone and read/send SMS + RCS locally
- **Live WhatsApp support** — link WhatsApp as a live companion device on your machine
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

### 3a. Optional: link WhatsApp

After `serve` is running, open the local UI and link WhatsApp from the Connections surface. OpenMessage keeps that bridge local and syncs it into the same inbox as Google Messages.

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
- **Send messages** — SMS/RCS plus live WhatsApp text, media, and voice notes
- **Live WhatsApp sync** — pair a local WhatsApp companion device for inbound messages, typing indicators, read state, and media
- **React to messages** — emoji reactions on any message
- **Image/media display** — inline images, video, audio, and fullscreen viewer
- **Desktop notifications** — native macOS notifications for fresh inbound messages
- **Web UI + macOS app** — real-time conversation view at localhost:7007 and a native wrapper
- **MCP tools** — conversation lookup, direct replies, route-aware sends, media download, import helpers, and story/viz tools
- **Local storage** — SQLite database, your data stays on your machine

## MCP tools

| Tool | Description |
|------|-------------|
| `get_messages` | Recent messages with filters (phone, date range, limit) |
| `get_conversation` | Messages in a specific conversation |
| `search_messages` | Full-text search across all messages |
| `send_message` | Send SMS/RCS to a phone number |
| `send_to_conversation` | Send a text reply directly to an existing conversation ID |
| `list_conversations` | List recent conversations |
| `list_contacts` | List/search contacts |
| `get_status` | Connection status and paired phone info |

## Web UI

The web UI runs at `http://localhost:7007` when the server is started. It provides:

- Conversation list with search and grouped multi-route contacts
- Message view with images, video, audio, reactions, and reply threads
- Route-aware compose and send
- Google Messages + WhatsApp connection controls
- Live typing indicators, read-state rendering, and notifications

## Native macOS app

The repo also includes a native Swift wrapper around the same local backend:

- embedded local OpenMessage backend
- native notifications
- contact photos
- the same Google Messages and WhatsApp pairing/runtime model as the web UI

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
| `OPENMESSAGES_MACOS_NOTIFICATIONS` | interactive macOS `serve` sessions only | Enable/disable native macOS notifications for fresh inbound live messages (`1`/`0`). Click-through opens the matching thread when `terminal-notifier` is available. |

## Architecture

- **libgm** handles the Google Messages protocol (pairing, encryption, long-polling)
- **whatsmeow** handles live WhatsApp pairing, sync, text/media send, receipts, typing, and avatars through a separate local session store
- **SQLite** (WAL mode, pure Go) stores messages, conversations, and contacts locally
- Real-time events from the phone are written to SQLite as they arrive
- The native macOS app and the localhost web UI run against the same local backend
- WhatsApp Desktop import remains as a fallback/repair path when the live bridge is not active
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

## License

MIT
