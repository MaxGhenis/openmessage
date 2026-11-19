import { Receipts } from "../../components/receipts";
import { SiteFooter } from "../../components/site-footer";
import { SiteHeader } from "../../components/site-header";
import { RELEASE, REPO_URL } from "../../data/facts";
import { pageMetadata } from "../../lib/metadata";

export const metadata = pageMetadata({
  title: "Privacy",
  description:
    "Everything the OpenMessage app and this site send anywhere, and what stays on your Mac.",
  path: "/privacy"
});

const UPDATED = "November 19, 2026";

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
            OpenMessage keeps your messages, contacts, drafts and search index in a SQLite
            database, <code>messages.db</code>, in{" "}
            <code>~/Library/Application Support/OpenMessage</code>. It keeps each network&rsquo;s
            login in the same folder, outside the database. OpenMessage encrypts neither the
            database nor the logins.
          </p>
          <Receipts
            ids={[
              "dataFolder",
              "messagesDb",
              "googleSessionFile",
              "sessionFiles",
              "signalConfigDir",
              "plainOpen",
              "googleSessionJson",
              "whatsappSessionStore"
            ]}
          />

          <h2>Deleting your data from this Mac</h2>
          <p>To delete your messages and logins from this Mac, quit the app and remove:</p>
          <ul>
            <li>
              <code>~/Library/Application Support/OpenMessage</code>, which holds the database
              and logins.
            </li>
            <li>
              <code>~/Library/Caches/com.openmessage.app</code>, where the app&rsquo;s window
              caches the photos and other attachments it has shown you.
            </li>
            <li>
              <code>~/.local/share/openmessage</code>, if you have run a command-line build. That
              build keeps its data there, and the app copies from that folder without emptying it.
            </li>
            <li>Any files your agent saved elsewhere.</li>
          </ul>
          <p>
            Then clear the notifications that show message text from Notification Center. The app
            posts some itself, and its server posts others through{" "}
            <code>terminal-notifier</code> or AppleScript.
          </p>
          <p>
            None of this touches the messages on your phone or any other device you have linked.
            It also leaves OpenMessage linked to your accounts: unlink it in WhatsApp, Signal and
            Google Messages on your phone, where Signal lists it as &ldquo;OpenMessage&rdquo;.
          </p>
          <Receipts
            ids={[
              "appBundleId",
              "mediaCache",
              "cliDataDir",
              "legacyCopy",
              "notificationBody",
              "appServerNotifications",
              "serverNotifier",
              "signalDeviceName"
            ]}
          />

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
                    The messages you send, the traffic that keeps the link up and brings in new
                    messages, and an online status your WhatsApp contacts may see
                  </td>
                  <td data-label="When">While you have that network paired</td>
                </tr>
                <tr>
                  <th scope="row">Sites your threads link to</th>
                  <td data-label="What">
                    Requests from your Mac for each linked page, any page it redirects to, and the
                    preview image the page names, which another site may host. OpenMessage skips
                    links whose own address is private or local
                  </td>
                  <td data-label="When">When you open a thread that contains a link</td>
                </tr>
                <tr>
                  <th scope="row">Google Fonts</th>
                  <td data-label="What">A request for the app&rsquo;s typeface</td>
                  <td data-label="When">When the app window loads</td>
                </tr>
                <tr>
                  <th scope="row">Anthropic</th>
                  <td data-label="What">
                    Up to 200 messages with their times and senders, plus the message count, date
                    range and participants. They come from the conversation you named, or from
                    the one-to-one conversations that match the name you gave
                  </td>
                  <td data-label="When">
                    When you give <code>generate_story</code>, <code>generate_person_story</code> or{" "}
                    <code>generate_viz</code> an Anthropic API key
                  </td>
                </tr>
                <tr>
                  <th scope="row">jsDelivr and Google Fonts</th>
                  <td data-label="What">Requests for a chart library and fonts</td>
                  <td data-label="When">
                    When you open a page that <code>generate_viz</code> or <code>render_story</code>{" "}
                    made
                  </td>
                </tr>
                <tr>
                  <th scope="row">The model behind your agent</th>
                  <td data-label="What">
                    Whatever the MCP tools return to your agent, which passes it to its model
                  </td>
                  <td data-label="When">When you connect an agent and it calls a tool</td>
                </tr>
                <tr>
                  <th scope="row">openmessage.ai</th>
                  <td data-label="What">
                    A random install ID, the version, operating system, CPU type and which
                    networks you have paired. No message data
                  </td>
                  <td data-label="When">
                    When you start OpenMessage with <code>OPENMESSAGE_TELEMETRY=1</code>. The Mac
                    app does not set it
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <Receipts
            ids={[
              "whatsappPresence",
              "linkPreviewGuard",
              "previewImage",
              "uiFonts",
              "storyApiKey",
              "storySample",
              "storyPrompt",
              "vizTemplate",
              "telemetryOptIn",
              "appEnv",
              "telemetryEndpoint"
            ]}
          />
          <p>
            If you grant access to macOS Contacts, the app shows contact photos, which it reads on
            your Mac and sends nowhere.
          </p>

          <h2>What this site keeps from install reports</h2>
          <p>
            If you start OpenMessage with <code>OPENMESSAGE_TELEMETRY=1</code>, this site stores
            one record per install per day: the install ID, version, operating system, CPU type,
            which networks you have paired, and the time it arrived. It stores no IP address and
            no message data. The app picks the install ID at random and keeps it from day to day,
            so the records link an install&rsquo;s days together without naming anyone. The site
            keeps them until I delete them.
          </p>

          <h2>What this site collects</h2>
          <ul>
            <li>
              The site sets no cookies, runs no analytics scripts and saves only your light or dark
              theme choice, in your browser&rsquo;s local storage.
            </li>
            <li>
              Vercel hosts the site, so each request you make, including your IP address, passes
              through Vercel.
            </li>
            <li>
              To limit abuse, the server counts sign-ups and install reports per IP address in
              memory. It never writes the counts to storage, and they disappear when the server
              instance restarts.
            </li>
            <li>
              The download button redirects to GitHub, which counts downloads per release. The
              site records nothing about the click.
            </li>
            <li>
              If you leave an email address, the site encrypts it with AES-256-GCM before storing
              it, and I use it only for OpenMessage release news. Email me at{" "}
              <a href="mailto:max@maxghenis.com">max@maxghenis.com</a> and I&rsquo;ll delete it.
            </li>
          </ul>

          <h2>The networks&rsquo; own policies</h2>
          <p>
            OpenMessage connects to accounts you already have with Google, WhatsApp and Signal,
            and each company handles your messages under its own privacy policy.
          </p>

          <h2>Changes</h2>
          <p>I update the date whenever I change this policy.</p>

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
