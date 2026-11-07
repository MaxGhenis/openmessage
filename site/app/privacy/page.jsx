import { Receipts } from "../../components/receipts";
import { SiteFooter } from "../../components/site-footer";
import { SiteHeader } from "../../components/site-header";
import { RELEASE, REPO_URL } from "../../data/facts";

export const metadata = {
  title: "Privacy",
  description:
    "Everything the OpenMessage app and this site send anywhere, and what stays on your Mac.",
  alternates: { canonical: "/privacy" }
};

const UPDATED = "October 8, 2026";

export default function PrivacyPage() {
  return (
    <>
      <SiteHeader />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>What OpenMessage does with your data</h1>
          <p>
            OpenMessage has no accounts and no server in the path of your messages. This page lists
            everything the app and this site send anywhere, as of version {RELEASE.version}.
          </p>
          <p className="dateline">Updated {UPDATED}</p>
        </header>

        <div className="prose">
          <h2>What stays on your Mac</h2>
          <p>
            Your messages, contacts, drafts and search index live in a SQLite database,{" "}
            <code>messages.db</code>, in <code>~/Library/Application Support/OpenMessage</code>.
            The login for each network you pair sits in its own file in the same folder. The
            database is not encrypted; it is protected by your macOS account.
          </p>
          <p>
            To delete your messages and logins from this Mac, quit the app and remove that folder.
            Your messages remain with Google, WhatsApp and Signal.
          </p>
          <Receipts ids={["dataFolder", "messagesDb", "sessionFiles"]} />

          <h2>What the app sends, and where</h2>
          <div className="table-scroll">
            <table className="ledger">
              <thead>
                <tr>
                  <th scope="col">Goes to</th>
                  <th scope="col">What</th>
                  <th scope="col">When</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <th scope="row">Google, WhatsApp, Signal</th>
                  <td data-label="What">
                    The messages you send, and the traffic needed to stay linked and receive
                  </td>
                  <td data-label="When">While that network is paired</td>
                </tr>
                <tr>
                  <th scope="row">Sites linked in your threads</th>
                  <td data-label="What">
                    A request from your Mac for the page&rsquo;s title and preview image. Private
                    and local addresses are refused
                  </td>
                  <td data-label="When">When a thread shows a link preview</td>
                </tr>
                <tr>
                  <th scope="row">Google Fonts</th>
                  <td data-label="What">A request for the app&rsquo;s typeface</td>
                  <td data-label="When">When the app window loads</td>
                </tr>
                <tr>
                  <th scope="row">Anthropic</th>
                  <td data-label="What">The conversation you asked a story about</td>
                  <td data-label="When">
                    Only when you give a story tool an Anthropic API key
                  </td>
                </tr>
                <tr>
                  <th scope="row">jsDelivr and Google Fonts</th>
                  <td data-label="What">Requests for a chart library and fonts</td>
                  <td data-label="When">
                    When you open a relationship page made by <code>generate_viz</code>
                  </td>
                </tr>
                <tr>
                  <th scope="row">The model behind your agent</th>
                  <td data-label="What">Whatever your agent reads through the MCP tools</td>
                  <td data-label="When">When you connect an agent and it calls a tool</td>
                </tr>
                <tr>
                  <th scope="row">openmessage.ai</th>
                  <td data-label="What">
                    A random install ID, the version, operating system, CPU type and which
                    networks are paired. No message data
                  </td>
                  <td data-label="When">
                    Only if OpenMessage is started with <code>OPENMESSAGE_TELEMETRY=1</code>. The
                    Mac app does not set it
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <Receipts
            ids={["linkPreviewGuard", "uiFonts", "storyApiKey", "telemetryOptIn", "telemetryEndpoint"]}
          />
          <p>
            The app can show contact photos from macOS Contacts if you grant access. They are read
            on your Mac and sent nowhere. Notifications are generated on your Mac.
          </p>

          <h2>What an install report keeps</h2>
          <p>
            If you turn on the report above, this site stores one record per install per day: the
            install ID, version, operating system, CPU type, which networks are paired, and the
            time received. It stores no IP address and no message data. The install ID is random
            but stays the same from day to day, so the records are pseudonymous. Nothing deletes
            them automatically.
          </p>

          <h2>What this site collects</h2>
          <ul>
            <li>
              No cookies and no analytics scripts. Your light or dark theme choice is saved in your
              own browser.
            </li>
            <li>
              The site is hosted on Vercel, which handles each request, including your IP address,
              in order to serve it.
            </li>
            <li>
              The download button redirects to GitHub. The site records nothing about it; GitHub
              counts downloads per release.
            </li>
            <li>
              If you leave an email address for release news, it is encrypted with AES-256-GCM
              before it is stored, and it is used only for OpenMessage release news. To have it
              removed, write to the address below.
            </li>
          </ul>

          <h2>The networks&rsquo; own policies</h2>
          <p>
            OpenMessage connects to accounts you already have with Google, WhatsApp and Signal.
            Their privacy policies govern what those services do with your messages.
          </p>

          <h2>Changes</h2>
          <p>
            When this page changes, the date at the top changes with it. The{" "}
            <a href={REPO_URL} rel="noopener">
              source code
            </a>{" "}
            is the reference for what the app does.
          </p>

          <h2>Contact</h2>
          <p>
            Write to <a href="mailto:max@maxghenis.com">max@maxghenis.com</a> or open an issue on{" "}
            <a href={`${REPO_URL}/issues`} rel="noopener">
              GitHub
            </a>
            .
          </p>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
