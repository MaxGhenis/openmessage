// Every checkable fact on the site lives here, next to the line of code that
// proves it. tests/facts.test.js reads each cited file at the cited git ref and
// fails if the line no longer contains `expect`, so site copy cannot drift from
// the code without a red test.
//
// Two refs:
//   release — the tag the Download button served when these facts were checked.
//   main    — a pinned commit on main, for work that is merged but unreleased.
// When a release ships: set RELEASE to the new tag, move MAIN_COMMIT forward,
// re-run `bun run test`, fix whatever the tests flag, then update the copy.

export const REPO = "MaxGhenis/openmessage";
export const REPO_URL = `https://github.com/${REPO}`;
export const SITE_URL = "https://openmessage.ai";

export const RELEASE = {
  tag: "v0.2.9",
  version: "0.2.9",
  publishedAt: "2026-04-21T18:56:16Z",
  dmgName: "OpenMessage.dmg"
};

export const MAIN_COMMIT = "4208e081922b452c49cfba899c56a3ef072a8167";
export const MAIN_CHECKED_ON = "2026-10-08";

// Counted at MAIN_COMMIT by tests/facts.test.js.
export const MAIN_STATS = {
  commits: 254,
  commitsWithClaudeTrailer: 178,
  commitsSinceRelease: 116,
  // Commits reachable from the release tag. The live "commits ahead" number on
  // the homepage is main's current total minus this.
  commitsAtRelease: 138
};

export const MCP = {
  host: "127.0.0.1",
  port: 7007,
  ssePath: "/mcp/sse"
};
export const MCP_SSE_URL = `http://${MCP.host}:${MCP.port}${MCP.ssePath}`;
export const CLAUDE_MCP_COMMAND = `claude mcp add -s user --transport sse openmessage ${MCP_SSE_URL}`;

export const DOWNLOAD_PATH = "/download";
export const RELEASE_DMG_URL = `${REPO_URL}/releases/latest/download/${RELEASE.dmgName}`;
export const RELEASES_URL = `${REPO_URL}/releases`;

export const MIN_MACOS = 14;

// The 22 MCP tools registered in the release, grouped for reading. The test
// compares the flattened list with the mcp.NewTool(...) names at the tag.
export const TOOL_GROUPS = [
  {
    title: "Read",
    tools: [
      "list_conversations",
      "get_conversation",
      "get_messages",
      "get_person_messages",
      "get_person_messages_range",
      "search_messages",
      "list_contacts",
      "get_status"
    ]
  },
  {
    title: "Write",
    tools: [
      "draft_message",
      "send_message",
      "send_to_conversation",
      "send_group_message",
      "send_media_to_conversation",
      "react_to_message"
    ]
  },
  {
    title: "Files",
    tools: ["download_media", "import_messages"]
  },
  {
    title: "Analyze",
    tools: [
      "conversation_stats",
      "person_stats",
      "generate_story",
      "generate_person_story",
      "generate_viz",
      "render_story"
    ]
  }
];
// The tools that transmit to other people. The risks section names them.
export const SENDING_TOOLS = [
  "send_message",
  "send_to_conversation",
  "send_group_message",
  "send_media_to_conversation",
  "react_to_message"
];

export const TOOL_COUNT = TOOL_GROUPS.reduce((sum, group) => sum + group.tools.length, 0);

export const RECEIPTS = {
  // --- release (what the Download button serves) ---
  mcpPort: { ref: "release", path: "cmd/serve.go", line: 67, expect: 'port = "7007"' },
  mcpHost: { ref: "release", path: "cmd/serve.go", line: 71, expect: 'host = "127.0.0.1"' },
  mcpMount: { ref: "release", path: "cmd/serve.go", line: 334, expect: 'WithStaticBasePath("/mcp")' },
  toolRegistry: { ref: "release", path: "internal/tools/tools.go", line: 15, expect: "func Register" },
  untrustedPreamble: {
    ref: "release",
    path: "internal/tools/tools.go",
    line: 63,
    expect: "const messagePreamble"
  },
  draftTool: {
    ref: "release",
    path: "internal/tools/draft_message.go",
    line: 17,
    expect: "The user can review and send it from the app."
  },
  sendResult: {
    ref: "release",
    path: "internal/tools/send_message.go",
    line: 117,
    expect: "SendMessageResponse_SUCCESS"
  },
  personRoutes: {
    ref: "release",
    path: "internal/web/static/index.html",
    line: 4993,
    expect: "function linkedRoutesForConversation"
  },
  searchIndex: { ref: "release", path: "internal/db/db.go", line: 345, expect: "USING fts5" },
  gmessagesLibrary: { ref: "release", path: "go.mod", line: 9, expect: "go.mau.fi/mautrix-gmessages" },
  whatsmeowLibrary: { ref: "release", path: "go.mod", line: 10, expect: "go.mau.fi/whatsmeow" },
  signalCli: {
    ref: "release",
    path: "internal/signallive/client.go",
    line: 335,
    expect: 'signalCLILookPath("signal-cli")'
  },
  imessageImport: {
    ref: "release",
    path: "internal/importer/imessage.go",
    line: 20,
    expect: "Requires Full Disk Access"
  },
  importCommands: { ref: "release", path: "main.go", line: 31, expect: "import gchat" },
  dataFolder: {
    ref: "release",
    path: "macos/OpenMessage/Sources/BackendManager.swift",
    line: 55,
    expect: "Library/Application Support/OpenMessage"
  },
  messagesDb: { ref: "release", path: "internal/app/app.go", line: 189, expect: '"messages.db"' },
  sessionFiles: { ref: "release", path: "internal/app/app.go", line: 253, expect: '"whatsapp-session.db"' },
  telemetryOptIn: { ref: "release", path: "cmd/serve.go", line: 404, expect: 'os.Getenv("OPENMESSAGE_TELEMETRY") == "1"' },
  telemetryEndpoint: {
    ref: "release",
    path: "internal/telemetry/telemetry.go",
    line: 29,
    expect: "https://openmessage.ai/api/heartbeat"
  },
  uiFonts: {
    ref: "release",
    path: "internal/web/static/index.html",
    line: 10,
    expect: "fonts.googleapis.com/css2?family=Public+Sans"
  },
  linkPreviewGuard: {
    ref: "release",
    path: "internal/web/linkpreview.go",
    line: 255,
    expect: "func isPrivatePreviewIP"
  },
  storyApiKey: { ref: "release", path: "internal/tools/story.go", line: 119, expect: 'strArg(args, "api_key")' },
  demoMode: { ref: "release", path: "internal/app/app.go", line: 158, expect: "func DemoMode" },
  demoSeed: { ref: "release", path: "internal/db/db.go", line: 94, expect: "SeedDemo" },
  minMacos: { ref: "release", path: "macos/OpenMessage/Package.swift", line: 6, expect: ".macOS(.v14)" },
  sqliteDriver: { ref: "release", path: "go.mod", line: 12, expect: "modernc.org/sqlite" },

  // Cited by the engineering notes under /blog.
  gmessagesFork: {
    ref: "release",
    path: "go.mod",
    line: 50,
    expect: "replace go.mau.fi/mautrix-gmessages => github.com/MaxGhenis/gmessages"
  },
  googleQrPairing: { ref: "release", path: "cmd/pair.go", line: 59, expect: "func runQRPairing" },
  googleCookiePairing: {
    ref: "release",
    path: "cmd/pair.go",
    line: 148,
    expect: "Paste a Google cookie JSON object or a cURL command copied from browser devtools"
  },
  googleHandleMessage: {
    ref: "release",
    path: "internal/client/events.go",
    line: 85,
    expect: "func (h *EventHandler) handleMessage"
  },
  googleStoreMessage: {
    ref: "release",
    path: "internal/client/events.go",
    line: 119,
    expect: "h.Store.UpsertMessage(dbMsg)"
  },
  googleNotifyUi: {
    ref: "release",
    path: "internal/client/events.go",
    line: 150,
    expect: "h.OnMessagesChange(dbMsg.ConversationID)"
  },
  whatsappQrPairing: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 306,
    expect: "cli.GetQRChannel"
  },
  whatsappSessionStore: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 203,
    expect: "sqlstore.New"
  },
  whatsappReceipts: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 1046,
    expect: "case *waevents.Receipt"
  },
  whatsappTyping: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 1048,
    expect: "case *waevents.ChatPresence"
  },
  whatsappHistory: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 1052,
    expect: "case *waevents.HistorySync"
  },
  whatsappEncReaction: {
    ref: "release",
    path: "internal/whatsapplive/client.go",
    line: 1147,
    expect: "Skipping EncReactionMessage (decryption not yet implemented)"
  },

  // --- main (merged, not yet in a release) ---
  supervisorStart: { ref: "main", path: "cmd/serve.go", line: 245, expect: "if !isDemo && transports" },
  supervisorBackoff: { ref: "main", path: "cmd/google_supervisor.go", line: 36, expect: "MinBackoff:         5 * time.Second" },
  silenceDefaults: {
    ref: "main",
    path: "internal/freshness/silence.go",
    line: 67,
    expect: "ExpectedActiveHoursLimit: 6"
  },
  statusBehind: { ref: "main", path: "cmd/status.go", line: 64, expect: "behind >= 3" },
  googleStall: {
    ref: "main",
    path: "internal/freshness/silence.go",
    line: 8,
    expect: "silent at 01:11 for 38 hours"
  },
  googleRefresh: {
    ref: "main",
    path: "internal/googlecookies/googlecookies.go",
    line: 1,
    expect: "Package googlecookies refreshes the Google account cookies"
  },
  outboxFlag: { ref: "main", path: "cmd/v2stack.go", line: 56, expect: "OPENMESSAGES_V2_SEND" },
  outboxUncertain: {
    ref: "main",
    path: "internal/storage/sqlite/outbox.go",
    line: 31,
    expect: 'OutboxUncertain     OutboxState = "uncertain"'
  },
  outboxLease: {
    ref: "main",
    path: "internal/storage/sqlite/outbox.go",
    line: 1172,
    expect: "state IN ('queued', 'not_dispatched')"
  }
};

export const BLOG_POSTS = [
  {
    slug: "how-openmessage-added-google-messages",
    title: "How OpenMessage added Google Messages",
    description: "Pairing, the live event path and the shared message table behind SMS and RCS in OpenMessage.",
    published: "2026-04-05",
    revised: "2026-10-08"
  },
  {
    slug: "how-openmessage-added-whatsapp",
    title: "How OpenMessage added live WhatsApp support",
    description: "The linked-device session, what it syncs, and what it leaves out.",
    published: "2026-04-05",
    revised: "2026-10-08"
  }
];

export function receiptRef(receipt) {
  return receipt.ref === "release" ? RELEASE.tag : MAIN_COMMIT;
}

export function receiptUrl(receipt) {
  return `${REPO_URL}/blob/${receiptRef(receipt)}/${receipt.path}#L${receipt.line}`;
}

export function receiptLabel(receipt) {
  return `${receipt.path}:${receipt.line}`;
}
