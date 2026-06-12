# OpenMessage — Code Map

A guide to where everything lives and how it fits together. OpenMessage is a
**local-first universal message inbox**: it pulls messages from several chat
platforms into one SQLite database, then exposes that inbox three ways — a
**localhost web UI**, an **MCP server** (for Claude/AI clients), and a **native
macOS app**.

```
                       ┌──────────────────── one Go binary (`serve`) ───────────────────┐
  Android phone ─────▶ │  libgm (Google Messages)  ┐                                     │
  WhatsApp      ─────▶ │  whatsmeow (live)          ├─▶  internal/db  (SQLite, the inbox)│
  Signal        ─────▶ │  signal-cli (live)         ┘            ▲                        │
  Imports       ─────▶ │  internal/importer (gchat/imessage/wa/signal)                    │
                       │                                          │ reads/writes          │
                       │   Three surfaces over the same store:    │                        │
                       │   • internal/web  (HTTP API + SSE + embedded static UI)           │
                       │   • internal/tools (22 MCP tools over stdio/SSE)                  │
                       │   • macOS Swift app (wraps the binary, shows the web UI)          │
                       └──────────────────────────────────────────────────────────────────┘
```

**Layering / dependency direction** (lower can't import higher):

```
cmd ─▶ internal/app ─▶ internal/client (libgm)      internal/web ─▶ internal/app, internal/story, internal/db
                    ─▶ internal/whatsapplive          internal/tools ─▶ internal/app, internal/story, internal/viz, internal/db
                    ─▶ internal/signallive            internal/story ─▶ internal/db
                    ─▶ internal/db                     internal/viz   ─▶ internal/story, internal/db
                    ─▶ internal/importer ─▶ internal/db
```

---

## 1. Entry point

### `main.go`
Parses `os.Args[1]` and dispatches to a `cmd.Run*` function. Subcommands:
`pair`, `serve`, `demo`, `send`, `send-group`, `import`, `debug-media`. Sets the
build-time `version` and the zerolog logger.

### `cmd/` — CLI commands
| File | Func(s) | Role |
|------|---------|------|
| `serve.go` | `RunServe`, `RunDemo`, `SetVersion`, `Version`, `LogLevel`, `startupBackfillMode` | **The main process.** Builds the `app.App`, connects Google/WhatsApp/Signal, starts the schedule-send loop (`a.StartScheduler`), wires the web HTTP handler (`web.APIHandlerWithOptions`) + SSE broker, registers all MCP tools (`tools.Register`), serves MCP over stdio (when launched by an MCP client) and SSE. Also kicks off startup backfill and the one-time Google contacts sync. |
| `pair.go` | `RunPair`, QR/Google-cookie helpers | Pairs Google Messages (QR or pasted account cookies). |
| `send.go` / `send_group.go` | `RunSend`, `RunSendGroup` | One-off SMS / MMS-group sends. `RunSendGroup` uses `app.GetOrCreateConversationForNumbers` (two-step RCS-group flow). |
| `import.go` | `RunImport`, `printResult`, `printContactsResult`, `hasFlag`, `flagValue` | Routes `import <source>` to the right importer (`gchat`, `gchat-conversation`, `imessage`, `whatsapp`, `signal`, `contacts`). |
| `debugmedia.go` | `RunDebugMedia` | Dumps media metadata for a conversation. |
| `e2e-server/main.go` | — | Test server for Playwright (`/_e2e/messages`, `/_e2e/drafts`, seeded fixture). |
| `genviz/main.go` | — | Standalone viz generator. |

---

## 2. `internal/app` — the coordinator

The `App` struct owns the DB store, the Google Messages client, the WhatsApp/
Signal bridges, and a set of `On*Change` callbacks that fan events out to the UI.

| File | Key functions | Role |
|------|---------------|------|
| `app.go` (34) | `New`, `LoadAndConnect`, `GetClient`, `Unpair`, `Close`, `DefaultDataDir`, `Status`, repair wiring | Bootstrap + lifecycle. On startup runs the repair chain `RepairLegacyArtifacts` → `RepairContentlessRecency` → `RepairTapbacks` → `RepairEmptyStubMessages` → `RepairLegacyMediaPlaceholders`. Holds `EnableContactDiscovery` (off by default — the flooder gate) and the `sendTextOverride`/`sendMediaOverride` test hooks the scheduler uses. |
| `backfill.go` (17) | `Backfill`, `DeepBackfill`/`deepBackfill`, `paginateFolder`, `deepBackfillConversation*`, `discoverFromContacts`, `reconcileRecentConversations`, `storeConversation`, `storeMessage` | History sync. Folder scan + per-conversation message paging; `discoverFromContacts` (gated off) is what used to flood the phone. `storeConversation` writes participants **with IDs** (for reaction-name resolution); `storeMessage` drops `IsEmptyStubMessage` rows. |
| `scheduler.go` (11) | `StartScheduler`, `processDueScheduledMessages`, `sendScheduledMessage`, `retryOrFail`, `SendTextToConversation`, `sendScheduledMedia`, `SendMediaToConversation`, `sendSMSMedia`, `sendSMSText`, `conversationPlatform`, `routeReady` | **Schedule-send engine.** Background goroutine: catch-up on startup + 20 s ticker. Atomically claims each due message (pending→sending, exactly-once), checks the route is connected, then sends text or media routed by platform (WhatsApp/Signal carry captions inline; SMS uploads via libgm and sends any caption as a follow-up text). Offline → revert to pending; ≥5 attempts → mark failed. |
| `contacts.go` (1) | `SyncGoogleContacts` | **Read-only** pull of the Google Messages contact list into the `contacts` table (never creates conversations). |
| `gm.go` (7) | `NewContactNumbers`, `NewGroupContactNumbers`, `GetOrCreateConversationForNumbers`, `ExtractSIMAndParticipant`, `BuildSendPayload`, `BuildSendMediaPayload` | Google Messages payload helpers. `GetOrCreateConversationForNumbers` does the two-step `CREATE_RCS` group-creation flow. |
| `gm_iface.go` (5) | `GMClient` interface + `realGMClient` | Abstraction over libgm for testing/backfill. |
| `whatsapp.go` (14) | `ensureWhatsApp`, `LoadAndConnectWhatsApp`, `StartWhatsAppConnect`, `SendWhatsAppText/Media/Reaction`, `WhatsAppStatus`, `WhatsAppQRCode`, `UnpairWhatsApp`, `LeaveWhatsAppGroup` | WhatsApp bridge lifecycle + send routing. |
| `signal.go` (12) | `ensureSignal`, `LoadAndConnectSignal`, `StartSignalConnect`, `SendSignalText/Media/Reaction`, `SignalStatus`, `SignalQRCode`, `UnpairSignal`, `ReplaySignalRecoveryQueue` | Signal bridge lifecycle + send routing. |

**Plays with:** `cmd/serve.go` constructs the App and wires its callbacks to the
SSE broker; `internal/web` and `internal/tools` call App methods; the App calls
`internal/client`, `whatsapplive`, `signallive`, and writes through `internal/db`.

---

## 3. Platform connectors

### `internal/client` — Google Messages (libgm)
| File | Key functions | Role |
|------|---------------|------|
| `client.go` (9) | `NewFromSession`, `NewForPairing`, `SessionData`, `ExtractMessageBody`, `ExtractMediaInfo`, `ExtractReactions`, `ExtractReplyToID`, `ExtractSenderInfo`, `MessageIsFromMe` | Wraps `*libgm.Client`. `ExtractReactions` returns `Reaction{Emoji,Count,Actors}` (actors = participant IDs, for reaction names). |
| `events.go` (10) | `Handle`, `handleMessage`, `handleConversation`, `storeConversation`, `handleTyping`, `handleClientReady`, `handleAuthRefresh` | The live event pump. `handleMessage`: build `db.Message` → **`ApplyTapback`** (convert `Loved "…"` to a reaction, skip storing if applied) → `UpsertMessage` → `AdvanceConversationRecency` (content-gated) → fire `On*Change`. |
| `session.go` (2) | `SaveSession`, `LoadSession` | Persists auth to `session.json`. |

### `internal/whatsapplive/client.go` (115 funcs)
The whatsmeow-based live WhatsApp bridge: `Bridge` with `New`, `Connect`,
`Link`, `Unpair`, `SendMessage`, reactions, typing, receipts, profile-photo
cache, group-leave repair. Reactions stored as `storedReaction{Emoji,Count,Actors}`
via `updateStoredReactions`.

### `internal/signallive/client.go` (109 funcs)
The signal-cli-based bridge: `Bridge` with `New`, `Connect`, `Link`, `Unpair`,
`SendMessage`, `ListContacts`, a receive loop with timeout/recovery, history-sync
detection, and the same `storedReaction` format.

---

## 4. `internal/db` — SQLite store (the inbox)

`Store` wraps `*sql.DB` (modernc pure-Go SQLite, WAL). Schema + migrations in
`db.go::migrate()`. Tables: `conversations`, `messages`, `messages_fts` (FTS5),
`contacts`, `unified_contacts`, `drafts`, `tabs`, `contact_meta`,
`scheduled_messages`.

| File | Key functions | Role |
|------|---------------|------|
| `db.go` (8) | `New`, `migrate`, `SeedDemo`, `Close` | Schema, ALTER migrations, demo seed. Structs `Conversation` (incl. `Tab`), `Message`, `Contact`, `UnifiedContact`, `Draft`. |
| `messages.go` (25) | `UpsertMessage`, `RecordOutgoingMessage`, `GetMessagesByConversation[s][Before/After/Range]`, `SearchMessages`, `GetMessageByID`, `DeleteMessageByID`, `syncMessageSearchIndex` | Message CRUD + FTS + paging. |
| `conversations.go` (22) | `UpsertConversation`, `GetConversation`, `ListConversations[ByPlatform]`, `BumpConversationTimestamp`, `MarkConversationRead`, `SetConversationNotificationMode`, `SetConversationTab`, `SetConversationsTab`, `MergeConversationIDs` | Conversation CRUD; `conversationColumns` is the canonical SELECT list. |
| `recency.go` (3) | `MessageHasContent`, `AdvanceConversationRecency`, `RepairContentlessRecency` | The "contentless stub must not float a conversation up" rule + startup repair. |
| `tapback.go` (7) | `ParseTapback`, `ApplyTapback`, `RepairTapbacks`, `findTapbackTarget`, `mergeReaction` | iMessage tapback (`Loved "…"`) → emoji reaction on the referenced message. |
| `stub.go` (2) | `IsEmptyStubMessage`, `RepairEmptyStubMessages` | Detects empty placeholder rows (no body/media/reactions + terminal status, not tombstone/pending) and batch-deletes them (recomputing recency). Used live by `events.go`/`backfill.go` and on startup. |
| `scheduled.go` (11) | `ValidateScheduleTime`, `CreateScheduledMessage`, `Get`/`List`/`GetDueScheduledMessages`, `GetScheduledMediaData`, `ClaimScheduledMessage`, `Mark…Sent`/`Failed`, `RevertScheduledMessageToPending`, `Cancel`/`DeleteScheduledMessage` | Backs schedule-send. `ScheduledMessage` carries an optional media blob; list/get/due never pull the blob (loaded on demand via `GetScheduledMediaData`). `ClaimScheduledMessage` is the atomic pending→sending claim for exactly-once delivery. |
| `tabs.go` (5) | `CreateTab`, `ListTabs`, `RenameTab`, `DeleteTab` | Custom inbox tabs (folders). |
| `people.go` (8) | `ListMessagedPeople`, `PersonByKey`, `PersonMessages`, `realMessageCountsByConversation`, `participantNumbers` | Contacts-CRM person aggregation (group 1:1s by normalized name; real message counts). |
| `contact_meta.go` (8) | `PersonKey`, `GetContactMeta[Map]`, `SetContactTags`, `SetContactReachOut`, `SetContactSummary` | Per-person CRM metadata (tags, reach-out cadence, cached summary). |
| `contacts.go` (7) | `UpsertContact`, `ListContacts`, `ListContactsFromConversations`, `UpsertUnifiedContact`, `ListUnifiedContacts` | Contact lookup tables. |
| `drafts.go` (4) | `UpsertDraft`, `ListDrafts`, `GetDraft`, `DeleteDraft` | AI/local drafts. |
| `repair.go` (3) | `RepairLegacyArtifacts` | Legacy bridge-bug cleanup (WhatsApp/Signal reaction placeholders, blank rows). |

**Plays with:** everything writes/reads through `Store`. `internal/story` and
`internal/web`'s people endpoints consume `people.go`/`contact_meta.go`.

---

## 5. `internal/importer` — historical imports

Common `Importer` interface + `ImportResult` (`importer.go`). Each generates
stable conversation IDs and dedups on `source_platform + source_id`.

| File | Type / entry | Source |
|------|--------------|--------|
| `gchat.go` | `GChat.Import`, `ImportGChatDirectory` | Google Chat Takeout |
| `imessage.go` | `IMessage.ImportFromDB` | `~/Library/Messages/chat.db` (Full Disk Access) |
| `whatsapp.go` | `WhatsApp.Import` | WhatsApp text export |
| `whatsapp_native.go` | `WhatsAppNative.ImportFromDB`, `RepairLegacyMediaPlaceholders` | WhatsApp Desktop SQLite |
| `signal_desktop.go` | `SignalDesktop.Import` | Encrypted Signal Desktop DB |
| `contacts_macos.go` | `MacOSContacts.ImportFromAddressBook` | macOS AddressBook abcddb (names+numbers+emails) |

---

## 6. `internal/web` — HTTP API + SSE + embedded UI

| File | Key functions | Role |
|------|---------------|------|
| `api.go` (14, ~2,400 lines) | `APIHandlerWithOptions(store, cli, logger, mcpHandler, APIOptions)` + ~90 inline handlers | The whole HTTP API. `APIOptions` is a big struct of callbacks wired in `serve.go` (status, send, QR, backfill, `SyncGoogleContacts`, …). Endpoints below. Helpers: `normalizePhoneNumber`, `localContactID`, `buildPersonPayload`, `scheduledID`, `writeJSON`, `httpError`. Serves embedded UI via `//go:embed static/*`. |
| `events.go` (16) | SSE `EventBroker`: `PublishMessages`, `PublishConversations`, `PublishStatus`, `PublishTyping` | Browser opens `/api/events`; server pushes invalidation events; the UI re-fetches/patches. |
| `linkpreview.go` (17) | URL metadata fetch (blocks private hosts) | Link preview cards. |

**Endpoint groups** (all in `api.go`): conversations (`/api/conversations`,
`/api/conversations/{id}/{messages,notification-mode,tab}`, `/api/conversations/move`),
tabs (`/api/tabs`, `/api/tabs/{id}`), messages (`/api/search`, `/api/send`,
`/api/send-media`, `/api/react`, `/api/mark-read`, `/api/new-conversation`),
**schedule-send** (`/api/schedule` GET-list+POST-text, `/api/schedule-media`
POST-multipart, `/api/schedule/{id}` DELETE-cancel), contacts (`/api/contacts`
GET+POST, `/api/contacts/sync`), **people/CRM** (`/api/people`,
`/api/people/{key}/{tags,reach-out,summary}`), drafts, media (`/api/media/{id}`),
stats/story (`/api/stats/{id}`, `/api/story/{id}`), backfill, status, and the
`{google,whatsapp,signal}/{connect,qr,unpair,status}` platform controls.

### `internal/web/static/index.html` — the frontend (~10,150 lines, vanilla JS)
Single embedded file: inline CSS (dark + light theme) and one IIFE `<script>`.
No framework/build step. Functional areas:

- **Conversation list / sidebar:** `loadConversations`, `renderConversations`,
  `createConversationRow`, `createContactRow`, `buildConversationRenderItems`
  (contact clustering), platform filters, **tabs** (`renderTabs`, `setCurrentTab`,
  `loadTabs`), **multi-select** (`enterSelectionMode`, `moveSelectedToTab`).
- **Thread view:** `selectConversation`, `loadMessages`, **`renderLoadedMessages`**
  (keyed-reconcile rendering — reuses unchanged nodes by `data-rk`+`data-rs` to
  avoid re-render flashes), `messageRenderSignature`, `threadSignature`,
  reactions (`reactionTooltip`, `reactionActorName`), media (`handleMediaLoad`,
  `resolveCompletedMedia` — both exposed on `window` for inline handlers).
- **Compose / send:** `sendMessage`, `sendDraft`, attachments
  (`setAttachment`/`clearAttachment`, `autoResize` gates send + schedule buttons),
  **new message** (`openNewMsg`, `updateNewMessageRoutes`,
  `addNewMsgRecipient`/group recipients, `updateNewMsgButtonLabel`,
  `saveLocalContact`).
- **Schedule send:** `openScheduleMenu` (Tomorrow 9 AM / 5 PM / custom),
  `nextDayAtHour`, `scheduleSend` (JSON for text, multipart for an attached
  file), `loadScheduledMessages`/`renderScheduledStrip` (pending strip above the
  composer, 📎 for media), `cancelScheduled`, `formatScheduledWhen`,
  `show`/`hideScheduleCustom`.
- **Contacts CRM view:** `openContacts`, `loadPeople`, `sortPeople`,
  `renderContactsList`, `selectPerson`, `renderPersonDetail` (tags, tickler,
  summary, **open-conversation**), `wirePersonDetail`.
- **Realtime:** `startEventStream` (SSE), `scheduleThreadRefresh`,
  `scheduleConversationsRefresh`.
- **Infra:** `fetchJSON`/`postJSON`, `escapeHtml`, `avatarHTML`,
  `hydrateNativeAvatars` (native-app contact photos bridge), context menus.

---

## 7. `internal/tools` — MCP server (22 tools)

`tools.go::Register(server, app)` registers every tool. Each `*.go` defines a
`xTool()` (schema) + `xHandler(app)` (logic), reading/writing through `app.Store`
or routing sends through the App.

Tools: `get_messages`, `get_conversation`, `search_messages`, `send_message`,
`send_to_conversation`, `send_media_to_conversation`, `send_group_message`,
`react_to_message`, `list_conversations`, `list_contacts`, `get_status`,
`download_media`, `draft_message`, `import_messages`, `get_person_messages`,
`get_person_messages_range`, `conversation_stats`, `generate_story`,
`person_stats`, `generate_person_story`, `generate_viz`, `render_story`.

`story.go` holds the shared `collectPersonMessages` / `findPersonConversations`
/ `deduplicateMessages` used by the story/viz tools.

---

## 8. `internal/story` + `internal/viz` — analytics & visualization

- **`story/stats.go`** — `ComputeStats(messages, tz) *Stats`: volume-over-time,
  hour×day heatmap, top phrases (per-sender), sender split, response times,
  longest gap.
- **`story/generate.go`** — `Generate(messages, GenerateConfig) *Story`:
  narrative chapters; uses the Claude API if `APIKey` set, else local generation.
- **`story/summary.go`** — `RelationshipSummary(messages, name, tz)` (local CRM
  summary) + `FilterRealMessages`/`isSystemMessage` (drops RCS banners → "No
  communication").
- **`viz/`** — `RenderHTML` orchestrator (`render.go`), `VizConfig` + theming
  (`config.go`), inline Go template (`template.go`), photo date-sort/intersperse
  (`photos.go`). Produces a self-contained HTML relationship visualization.

---

## 9. Support: `notify`, `telemetry`

- **`internal/notify/macos.go`** — `MacOSNotifier` via `terminal-notifier`, with
  per-conversation mute/mention filtering and seen-dedup.
- **`internal/telemetry/telemetry.go`** — opt-in (off by default) anonymous
  heartbeat (install ID, version, OS, paired platforms — no content).

---

## 10. macOS app (`OpenMessage/` + `macos/`)

Swift wrapper around the same Go binary (duplicated under both dirs):

| File | Role |
|------|------|
| `OpenMessageApp.swift` | App entry; owns `BackendManager`, `NotificationManager`, `ContactsManager`. |
| `BackendManager.swift` | Launches the Go binary as a subprocess, health-checks `:7007`, manages state. |
| `ContentView.swift` | Routes launch/pairing/error/ready; ready state is a `WKWebView` at the local web UI; bridges `window.OpenMessageNative{Notifications,Contacts,App}`. |
| `ContactsManager.swift` | Reads macOS Contacts (`CNContactStore`) **for avatar photos only** (permission-gated, in-memory, never persisted/uploaded). |
| `NotificationManager.swift` | Bridges SSE events → macOS notification center. |
| `PairingView.swift` / `MenuBarView.swift` | Pairing UI / menu-bar item. |

`macos/build.sh` → universal Go binary → `.app` → `.dmg`.

---

## 11. Cross-cutting data flows

**Receive a message (live, Google Messages):**
`libgm event` → `client/events.go::handleMessage` → (tapback? → `db.ApplyTapback`,
then skip if it was applied) → (drop if `db.IsEmptyStubMessage`) →
`db.UpsertMessage` → `db.AdvanceConversationRecency` → App `On*Change`
callbacks → `web/events.go` SSE publish → browser `startEventStream` →
`scheduleThreadRefresh`/`scheduleConversationsRefresh` → `loadMessages` →
`renderLoadedMessages` (keyed reconcile, no flash).

**Send a message (web UI):**
`sendMessage` (optimistic node) → `POST /api/send` → `api.go` routes by platform
(`BuildSendPayload`+libgm, or App's WhatsApp/Signal senders) →
`db.RecordOutgoingMessage` → SSE echo → reconcile render.

**Start a new (group) conversation:**
new-message UI → `POST /api/new-conversation` → `normalizePhoneNumber` →
`app.GetOrCreateConversationForNumbers` (two-step `CREATE_RCS`) →
`db.UpsertConversation` → SSE.

**Schedule a message (text or media):**
compose UI → `openScheduleMenu` → `scheduleSend` → `POST /api/schedule` (JSON)
or `/api/schedule-media` (multipart, blob persisted) → `db.CreateScheduledMessage`
→ pending strip via `/api/schedule` GET. Later: `scheduler.go` ticker →
`GetDueScheduledMessages` → `ClaimScheduledMessage` (atomic) → `routeReady`? →
`SendTextToConversation`/`SendMediaToConversation` (platform-routed) →
`MarkScheduledMessageSent` (or retry/fail) → SSE. Catch-up on startup covers
messages whose time passed while the app was closed.

**Contacts CRM:**
`SyncGoogleContacts` (or contact imports) fill `contacts`; `GET /api/people`
aggregates via `db.ListMessagedPeople`; detail uses `db.PersonMessages` +
`story.RelationshipSummary` + `db.GetContactMeta`; tags/cadence persist to
`contact_meta`.

**Story / viz:**
MCP tool or HTTP → `collectPersonMessages` → `story.ComputeStats` /
`story.Generate` → `viz.RenderHTML` → self-contained HTML.
