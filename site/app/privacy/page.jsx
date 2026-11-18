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

const UPDATED = "November 18, 2026";

export default function PrivacyPage() {
  return (
    <>
      <SiteHeader />
      <main id="main" className="wrap">
        <header className="page-head">
          <h1>What OpenMessage does with your data</h1>
          <p>
            OpenMessage has no accounts and no server in the path of your messages.
          </p>
          <p className="dateline">Updated {UPDATED}</p>
        </header>

        <div className="prose">
          <h2>What stays on your Mac</h2>
          <p>
            Your messages, contacts, drafts and search index live in a SQLite database,{" "}
            <code>messages.db</code>, in <code>~/Library/Application Support/OpenMessage</code>.
            The login for each network you pair sits in its own file in the same folder. OpenMessage
            does not encrypt the database; your macOS account&rsquo;s file permissions protect it.
          </p>
          <p>
            To delete your messages and logins from this Mac, quit the app and remove that folder.
            Your messages remain with Google, WhatsApp and Signal.
          </p>
          <Receipts ids={["dataFolder", "messagesDb", "sessionFiles"]} />

          <h2>What the app sends, and where</h2>
          <p>Version {RELEASE.version} of the app sends data to these places and no others.</p>
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
                    The messages you send, and the traffic that keeps the link up and brings in
                    new messages
                  </td>
                  <td data-label="When">While you have that network paired</td>
                </tr>
                <tr>
                  <th scope="row">Sites linked in your threads</th>
                  <td data-label="What">
                    A request from your Mac for the page&rsquo;s title and preview image.
                    OpenMessage refuses private and local addresses
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
                    When you open a relationship page that <code>generate_viz</code> made
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
                    networks you have paired. No message data
                  </td>
                  <td data-label="When">
                    Only if you start OpenMessage with <code>OPENMESSAGE_TELEMETRY=1</code>. The
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
            The app can show contact photos from macOS Contacts if you grant access. It reads them
            on your Mac and sends them nowhere. It also creates notifications on your Mac.
          </p>

          <h2>What an install report keeps</h2>
          <p>
            If you start OpenMessage with <code>OPENMESSAGE_TELEMETRY=1</code>, this site stores
            one record per install per day: the install ID, version, operating system, CPU type,
            which networks you have paired, and the time it arrived. It stores no IP address and
            no message data. The app picks the install ID at random and keeps it from day to day,
            so the records link an install&rsquo;s days together without naming anyone. Nothing
            deletes them automatically.
          </p>

          <h2>What this site collects</h2>
          <ul>
            <li>
              The site sets no cookies and runs no analytics scripts. Your browser saves your light
              or dark theme choice.
            </li>
            <li>
              Vercel hosts the site and handles each request, including your IP address, to serve
              it.
            </li>
            <li>
              To limit abuse, the server counts sign-ups and install reports per IP address in
              memory. It never writes the counts to storage, and they disappear when the server
              instance restarts.
            </li>
            <li>
              The download button redirects to GitHub. The site records nothing about it; GitHub
              counts downloads per release.
            </li>
            <li>
              If you leave an email address for release news, the site encrypts it with AES-256-GCM
              before storing it, and I use it only for OpenMessage release news. To have it
              removed, email me at <a href="mailto:max@maxghenis.com">max@maxghenis.com</a>.
            </li>
          </ul>

          <h2>The networks&rsquo; own policies</h2>
          <p>
            OpenMessage connects to accounts you already have with Google, WhatsApp and Signal,
            and each of them handles your messages under its own privacy policy.
          </p>

          <h2>Changes</h2>
          <p>
            I update the date whenever I change this policy, and the{" "}
            <a href={REPO_URL} rel="noopener">
              source code
            </a>{" "}
            shows what the app does in detail.
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
