import { readFile } from "node:fs/promises";
import path from "node:path";

import { ImageResponse } from "next/og";

// The card shown when the site is shared. Built once at build time from the
// same two typefaces as the page; the font files come from @fontsource, so
// nothing is fetched over the network.
export const alt = "OpenMessage: your texts, WhatsApp and Signal in one SQLite database your agent can query";
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

function font(pkg, file) {
  return readFile(path.join(process.cwd(), "node_modules", "@fontsource", pkg, "files", file));
}

export default async function OpengraphImage() {
  const [bold, regular, mono] = await Promise.all([
    font("atkinson-hyperlegible-next", "atkinson-hyperlegible-next-latin-700-normal.woff"),
    font("atkinson-hyperlegible-next", "atkinson-hyperlegible-next-latin-400-normal.woff"),
    font("atkinson-hyperlegible-mono", "atkinson-hyperlegible-mono-latin-400-normal.woff")
  ]);

  return new ImageResponse(
    (
      <div
        style={{
          width: "100%",
          height: "100%",
          display: "flex",
          flexDirection: "column",
          justifyContent: "space-between",
          padding: "64px 72px",
          background: "#09111c",
          color: "#edf2fb",
          fontFamily: "Atkinson"
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 18, fontSize: 34, fontWeight: 700 }}>
          <svg width="52" height="52" viewBox="0 0 32 32">
            <rect width="32" height="32" rx="7" fill="#0d1728" />
            <path
              d="M7 9a3 3 0 0 1 3-3h12a3 3 0 0 1 3 3v10a3 3 0 0 1-3 3h-3.5l-4 4v-4H10a3 3 0 0 1-3-3V9z"
              fill="#6f82ff"
            />
            <circle cx="12" cy="14" r="1.5" fill="#f3f6fc" />
            <circle cx="16" cy="14" r="1.5" fill="#f3f6fc" />
            <circle cx="20" cy="14" r="1.5" fill="#f3f6fc" />
          </svg>
          OpenMessage
        </div>

        <div style={{ display: "flex", fontSize: 68, fontWeight: 700, lineHeight: 1.06, letterSpacing: -2, maxWidth: 980 }}>
          Your texts, WhatsApp and Signal in one SQLite database your agent can query
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          <div style={{ display: "flex", fontFamily: "Atkinson Mono", fontSize: 24, color: "#93a6ff" }}>
            {"get_person_messages {\"name\": \"Jordan Rivera\"}"}
          </div>
          <div style={{ display: "flex", fontSize: 26, color: "#9aa8c0" }}>
            A Mac app and a local MCP server on the same file. Free, with no account.
          </div>
        </div>
      </div>
    ),
    {
      ...size,
      fonts: [
        { name: "Atkinson", data: bold, weight: 700, style: "normal" },
        { name: "Atkinson", data: regular, weight: 400, style: "normal" },
        { name: "Atkinson Mono", data: mono, weight: 400, style: "normal" }
      ]
    }
  );
}
