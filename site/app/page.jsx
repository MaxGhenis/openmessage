import Image from "next/image";
import { IconBrandApple } from "@tabler/icons-react";

import { CopyCommand } from "../components/copy-command";
import { Receipts } from "../components/receipts";
import { SiteFooter } from "../components/site-footer";
import { SiteHeader } from "../components/site-header";
import { TwoReaders } from "../components/two-readers";
import { UpdatesForm } from "../components/updates-form";
import { DEMO_ARGS, DEMO_RESULT, DEMO_TOOL } from "../data/demo";
import {
  CLAUDE_MCP_COMMAND,
  DOWNLOAD_PATH,
  MAIN_CHECKED_ON,
  MAIN_STATS,
  MCP,
  MIN_MACOS,
  RELEASE,
  RELEASES_URL,
  REPO_URL,
  SENDING_TOOLS,
  SITE_URL,
  TOOL_COUNT,
  TOOL_GROUPS
} from "../data/facts";
import {
  commitsAheadOf,
  formatDate,
  formatMegabytes,
  getLatestRelease,
  getMainCommitTotal
} from "../lib/release";
import { clockTime, parseTranscript } from "../lib/transcript";
import appScreenshot from "../public/screens/app-demo-sms.png";

// Re-check GitHub for the latest release at most once an hour.
export const revalidate = 3600;

const ROUTE_LABELS = { sms: "SMS", whatsapp: "WhatsApp", signal: "Signal" };
const DEMO_PERSON = DEMO_ARGS.name;

function buildReaders() {
  const { lines, messages } = parseTranscript(DEMO_RESULT);
  const routes = Object.entries(ROUTE_LABELS).map(([platform, label]) => {
    const direct = messages.filter(
      (message) => message.platform === platform && message.conversationName === DEMO_PERSON
    );
    return {
      platform,
      label,
      conversationId: direct[0].conversationId,
      messages: direct.map((message) => ({
        body: message.body,
        mine: message.mine,
        time: clockTime(message.timestamp)
      }))
    };
  });
  return { lines, routes };
}

function DownloadButton({ source }) {
  return (
    <a className="btn btn--yours" href={`${DOWNLOAD_PATH}?source=${source}`}>
      <IconBrandApple stroke={1.7} aria-hidden="true" />
      Download for macOS
    </a>
  );
}

export default async function HomePage() {
  const [release, mainTotal] = await Promise.all([getLatestRelease(), getMainCommitTotal()]);
  const { lines, routes } = buildReaders();
  const releaseDate = formatDate(release.publishedAt);
  const size = formatMegabytes(release.dmgBytes);
  // The claims below were checked against RELEASE.tag. If GitHub reports a
  // newer release, say so instead of describing it from memory.
  const describesLatest = release.tag === RELEASE.tag;
  // Live when GitHub answers; otherwise the count checked at MAIN_COMMIT.
  const commitsAhead = describesLatest
    ? (commitsAheadOf(mainTotal, MAIN_STATS.commitsAtRelease) ?? MAIN_STATS.commitsSinceRelease)
    : null;
  const toolNames = (names) =>
    names.map((name, index) => (
      <span key={name}>
        {index > 0 && (index === names.length - 1 ? " and " : ", ")}
        <code>{name}</code>
      </span>
    ));

  const jsonLd = {
    "@context": "https://schema.org",
    "@type": "SoftwareApplication",
    name: "OpenMessage",
    url: SITE_URL,
    applicationCategory: "CommunicationApplication",
    operatingSystem: `macOS ${MIN_MACOS} or later`,
    softwareVersion: release.version,
    datePublished: release.publishedAt.slice(0, 10),
    downloadUrl: `${SITE_URL}${DOWNLOAD_PATH}`,
    offers: { "@type": "Offer", price: "0", priceCurrency: "USD" },
    author: { "@type": "Person", name: "Max Ghenis", url: "https://maxghenis.com" },
    codeRepository: REPO_URL
  };

  return (
    <>
      <SiteHeader />
      <main id="main">
        <section className="hero">
          <div className="wrap">
            <h1 className="hero__title">
              Your texts, WhatsApp and Signal in one SQLite database your agent can query
            </h1>
            <p className="hero__sub">
              OpenMessage connects to Google Messages (SMS and RCS), WhatsApp and Signal, and keeps
              your messages in SQLite on your Mac. A desktop app and a local MCP server read the
              same file.
            </p>
            <div className="hero__cta">
              <DownloadButton source="hero" />
              <a href="#setup">Set it up in three steps</a>
            </div>
            <p className="hero__release">
              Version {release.version}, released {releaseDate}
              {size ? `. ${size}` : ""}. macOS {MIN_MACOS} or later. Free, with no account.
            </p>

            <div className="readers">
              <TwoReaders
                person={DEMO_PERSON}
                call={`${DEMO_TOOL} ${JSON.stringify(DEMO_ARGS)}`}
                routes={routes}
                lines={lines}
                initialRoute="whatsapp"
              />
              <p className="readers__caption">
                Demo conversations that ship with the app; none of these people exist. The right
                pane shows the text <code>{DEMO_TOOL}</code> returned in version {RELEASE.version},
                and the left pane lays out the same messages as a thread, one tab per network.
              </p>
              <Receipts ids={["demoSeed", "untrustedPreamble"]} />
            </div>
          </div>
        </section>

        <section className="section" id="agents" aria-labelledby="agents-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="agents-title">{TOOL_COUNT} tools for your agent</h2>
            </div>
            <div className="section__body">
              <p className="lead">
                While the app is open, an MCP server runs on your Mac at{" "}
                <code>
                  {MCP.host}:{MCP.port}
                </code>
                . Any MCP client that speaks SSE can connect. For Claude Code:
              </p>
              <CopyCommand command={CLAUDE_MCP_COMMAND} label="the Claude Code command" />
              <Receipts ids={["mcpPort", "mcpMount", "toolRegistry"]} />

              <div className="tools">
                {TOOL_GROUPS.map((group) => (
                  <div key={group.title}>
                    <h3>{group.title}</h3>
                    <ul>
                      {group.tools.map((tool) => (
                        <li key={tool}>{tool}</li>
                      ))}
                    </ul>
                  </div>
                ))}
              </div>

              <div className="facts">
                <div className="fact">
                  <h3>One call covers a person on every network</h3>
                  <p>
                    <code>get_person_messages</code> takes a name and returns that person&rsquo;s
                    SMS, WhatsApp and Signal messages in one result.{" "}
                    <code>person_stats</code> summarizes the same history.
                  </p>
                </div>
                <div className="fact">
                  <h3>Drafts wait for you</h3>
                  <p>
                    <code>draft_message</code> leaves a draft in the app. You review it and send it
                    yourself.
                  </p>
                  <Receipts ids={["draftTool"]} />
                </div>
                <div className="fact">
                  <h3>Results flag message text as untrusted</h3>
                  <p>
                    Results that contain message text open with a warning that tells the model to
                    ignore instructions inside message bodies.
                  </p>
                  <Receipts ids={["untrustedPreamble"]} />
                </div>
              </div>
            </div>
          </div>
        </section>

        <section className="section" id="database" aria-labelledby="database-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="database-title">A database you can open</h2>
            </div>
            <div className="section__body">
              <p className="lead">
                Messages from all three networks go into one file, <code>messages.db</code>, in{" "}
                <code>~/Library/Application Support/OpenMessage</code>. Each row records the network
                it came from.
              </p>
              <figure>
                <pre className="command command__code" tabIndex={0}>
                  <code>{`$ sqlite3 messages.db "SELECT source_platform, COUNT(*) FROM messages GROUP BY 1 ORDER BY 2 DESC;"
sms|31
whatsapp|12
signal|11`}</code>
                </pre>
                <figcaption className="readers__caption">
                  I ran this on the demo data in version {RELEASE.version}.
                </figcaption>
              </figure>
              <Receipts ids={["dataFolder", "messagesDb", "sqliteDriver"]} />

              <div className="facts">
                <div className="fact">
                  <h3>Search works offline</h3>
                  <p>
                    OpenMessage indexes message text with SQLite&rsquo;s FTS5, so search runs on
                    your Mac with no network.
                  </p>
                  <Receipts ids={["searchIndex"]} />
                </div>
                <div className="fact">
                  <h3>Import older history</h3>
                  <p>
                    The command line and the <code>import_messages</code> tool load iMessage history
                    from <code>chat.db</code> on your Mac, a Google Chat Takeout and a WhatsApp
                    export. You can read imported iMessage and Google Chat threads but not reply
                    to them, because OpenMessage has no live connection to either.
                  </p>
                  <Receipts ids={["imessageImport", "importCommands"]} />
                </div>
                <div className="fact">
                  <h3>Pairing sessions sit beside it</h3>
                  <p>Each network&rsquo;s login lives in its own file in the same folder.</p>
                  <Receipts ids={["sessionFiles"]} />
                </div>
              </div>
            </div>
          </div>
        </section>

        <section className="section" id="app" aria-labelledby="app-title">
          <div className="wrap">
            <div className="section__grid">
              <div className="section__head">
                <h2 id="app-title">The Mac app reads the same file</h2>
              </div>
              <div className="section__body">
                <p className="lead">
                  You read and reply on all three networks from one window, and can send
                  attachments and reactions.
                </p>
                <p>
                  When a person&rsquo;s name and number match on more than one network, the sidebar
                  shows one row for them, with a tab per network in the thread.
                </p>
                <Receipts ids={["personRoutes"]} />
              </div>
            </div>
            <figure className="shot">
              <Image
                className="shot__img"
                src={appScreenshot}
                alt="OpenMessage window with Jordan Rivera selected in the sidebar of conversations. The thread shows four SMS messages, with tabs for SMS, WhatsApp and Signal above them."
                sizes="(max-width: 72rem) 100vw, 67rem"
                placeholder="empty"
              />
              <figcaption>The app shows its built-in demo data.</figcaption>
            </figure>
          </div>
        </section>

        <section className="section" id="risks" aria-labelledby="risks-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="risks-title">What can go wrong</h2>
            </div>
            <div className="section__body">
              <div className="facts">
                <div className="fact">
                  <h3>Unofficial clients</h3>
                  <p>
                    OpenMessage reaches the three networks through open-source libraries that Google,
                    WhatsApp and Signal do not support: mautrix-gmessages, whatsmeow and signal-cli.{" "}
                    <a href="https://www.whatsapp.com/legal/terms-of-service" rel="noopener">
                      WhatsApp&rsquo;s terms
                    </a>{" "}
                    forbid reverse engineering its service and let it suspend an account at any
                    time. Pair accounts you could stand to lose.
                  </p>
                  <Receipts ids={["gmessagesLibrary", "whatsmeowLibrary", "signalCli"]} />
                </div>
                <div className="fact">
                  <h3>Your agent can send as you</h3>
                  <p>
                    These tools send to other people: {toolNames(SENDING_TOOLS)}. OpenMessage
                    does not ask you to confirm them. Keep your MCP client&rsquo;s
                    permission prompt on for those tools, or have your agent use{" "}
                    <code>draft_message</code>.
                  </p>
                  <p>
                    A send tool reports &ldquo;Message sent&rdquo; when the network accepts the
                    message, and its result says nothing about delivery or reading.
                  </p>
                  <Receipts ids={["sendResult"]} />
                </div>
                <div className="fact">
                  <h3>What your agent reads leaves your Mac</h3>
                  <p>
                    Text that a tool returns goes to the model running your agent, and anyone can
                    text you instructions aimed at that model. The warning OpenMessage puts on each
                    result lowers that risk and does not remove it.
                  </p>
                </div>
                <div className="fact">
                  <h3>Anything running as you can reach it</h3>
                  <p>
                    The local server listens on <code>{MCP.host}</code> with no password, and
                    OpenMessage does not encrypt the database. Other software running under your
                    macOS account can call the server and read the file.
                  </p>
                  <Receipts ids={["mcpHost", "sqliteDriver"]} />
                </div>
                <div className="fact">
                  <h3>Connections break when the networks change</h3>
                  <p>
                    A protocol change at Google, WhatsApp or Signal can stop a connection until a
                    new version ships, and the app does not update itself. Version{" "}
                    {RELEASE.version} cannot renew an expired Google Messages session, so you pair
                    again when one lapses.
                  </p>
                </div>
              </div>
            </div>
          </div>
        </section>

        <section className="section" id="status" aria-labelledby="status-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="status-title">What ships today</h2>
            </div>
            <div className="section__body">
              <p className="lead">
                The download button serves version {release.version}, released {releaseDate}.
                {commitsAhead !== null && commitsAhead > 0 && (
                  <>
                    {" "}
                    Since then, {commitsAhead} commits have landed on <code>main</code>.
                  </>
                )}
              </p>
              {!describesLatest && (
                <p>
                  Version {release.version} shipped after version {RELEASE.version}, so some details
                  here may have changed, including the agent setup command. Its{" "}
                  <a href={RELEASES_URL} rel="noopener">
                    release notes
                  </a>{" "}
                  list what changed.
                </p>
              )}
              {describesLatest && (
                <>
                  <p>These changes sit on <code>main</code>, waiting for the next release:</p>
                  <div className="facts">
                    <div className="fact">
                      <h3>Connections that reconnect themselves</h3>
                      <p>
                        A supervisor for each network retries a dropped connection after a random
                        wait, under a ceiling that doubles from 5 seconds to 5 minutes. It can also
                        refresh an expired Google session from your local Chrome profile.
                      </p>
                      <Receipts
                        ids={[
                          "supervisorBackoff",
                          "whatsappBackoff",
                          "signalBackoff",
                          "supervisorJitter",
                          "googleRefresh"
                        ]}
                      />
                    </div>
                    <div className="fact">
                      <h3>Stall detection</h3>
                      <p>
                        The app compares each network&rsquo;s silence with its own recent traffic
                        and flags one that has gone quiet for longer than that history explains.{" "}
                        <code>openmessage status</code> shows how fresh each network&rsquo;s data
                        is and marks a silent one.
                      </p>
                      <Receipts ids={["silenceDefaults", "statusSilence"]} />
                    </div>
                    <div className="fact">
                      <h3>A send outbox, behind a flag</h3>
                      <p>
                        With <code>OPENMESSAGES_V2_SEND=1</code>, OpenMessage saves each send
                        locally before it tries it.
                        It parks a send whose outcome is unknown as <code>uncertain</code> and never
                        retries it automatically.
                      </p>
                      <Receipts ids={["outboxFlag", "outboxUncertain", "outboxLease"]} />
                    </div>
                  </div>
                </>
              )}

              <div className="table-scroll">
                <table className="ledger">
                  <caption className="visually-hidden">Where OpenMessage runs</caption>
                  <thead>
                    <tr>
                      <th scope="col">Where</th>
                      <th scope="col">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr>
                      <th scope="row">Mac app</th>
                      <td data-label="Status">
                        macOS {MIN_MACOS} or later, Apple silicon and Intel
                      </td>
                    </tr>
                    <tr>
                      <th scope="row">Command line</th>
                      <td data-label="Status">macOS and Linux builds</td>
                    </tr>
                    <tr>
                      <th scope="row">SMS and RCS</th>
                      <td data-label="Status">Through Google Messages on an Android phone</td>
                    </tr>
                    <tr>
                      <th scope="row">iMessage</th>
                      <td data-label="Status">Import only</td>
                    </tr>
                    <tr>
                      <th scope="row">Windows, iOS, Android</th>
                      <td data-label="Status">No app</td>
                    </tr>
                  </tbody>
                </table>
              </div>

              <Receipts ids={["minMacos", "imessageImport"]} />

              <p>
                Claude co-wrote {MAIN_STATS.commitsWithClaudeTrailer} of the {MAIN_STATS.commits}{" "}
                commits on <code>main</code>, as of {formatDate(`${MAIN_CHECKED_ON}T00:00:00Z`)}.
              </p>
            </div>
          </div>
        </section>

        <section className="section" id="setup" aria-labelledby="setup-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="setup-title">Set it up</h2>
            </div>
            <div className="section__body">
              <ol className="steps">
                <li>
                  <div>
                    <h3>Download the app</h3>
                    <p>
                      Open the disk image and drag OpenMessage to your Applications folder. You
                      need macOS {MIN_MACOS} or later.
                    </p>
                    <p>
                      <DownloadButton source="setup" />
                    </p>
                  </div>
                </li>
                <li>
                  <div>
                    <h3>Pair a network</h3>
                    <p>
                      For WhatsApp and Signal, the app shows a QR code. Scan it from the app on
                      your phone, the way you would link a desktop client. For Signal, install{" "}
                      <code>signal-cli</code> first: <code>brew install signal-cli</code>.
                    </p>
                    <p>
                      For Google Messages, use the Google Account tab, since{" "}
                      <a
                        href="https://support.google.com/messages/answer/9077245?co=GENIE.Platform%3DDesktop&hl=en"
                        rel="noopener"
                      >
                        Google no longer offers QR pairing
                      </a>{" "}
                      in the US. Copy a cookie object or a cURL request for{" "}
                      <code>messages.google.com</code> from your browser&rsquo;s developer tools and
                      paste it there. Then tap the matching emoji in Google Messages on your Android
                      phone.
                    </p>
                    <Receipts
                      ids={[
                        "whatsappQrPairing",
                        "signalQrPairing",
                        "signalCli",
                        "googleAccountTab",
                        "googleCookiePairing",
                        "googleEmojiConfirm"
                      ]}
                    />
                  </div>
                </li>
                <li>
                  <div>
                    <h3>Connect your agent</h3>
                    <p>Add the server to Claude Code, then ask about your messages.</p>
                    <CopyCommand command={CLAUDE_MCP_COMMAND} label="the Claude Code command" />
                  </div>
                </li>
              </ol>
              <p>
                To look before you pair anything, run <code>openmessage demo</code> from a
                command-line build. It starts the app on the demo conversations with every network
                disconnected.
              </p>
              <Receipts ids={["demoMode"]} />
            </div>
          </div>
        </section>

        <section className="section" id="updates" aria-labelledby="updates-title">
          <div className="wrap section__grid">
            <div className="section__head">
              <h2 id="updates-title">Hear about releases</h2>
            </div>
            <div className="section__body">
              <p>
                The app does not update itself, so leave an address to get an email when a release
                ships, or watch{" "}
                <a href={RELEASES_URL} rel="noopener">
                  releases on GitHub
                </a>
                .
              </p>
              <UpdatesForm />
              <p className="muted">
                The site encrypts your address before storing it, and I use it only for
                OpenMessage release news. See the{" "}
                <a href="/privacy">privacy page</a>.
              </p>
            </div>
          </div>
        </section>
      </main>
      <SiteFooter />
      <script
        type="application/ld+json"
        // "<" is escaped so no value can close the script element.
        dangerouslySetInnerHTML={{ __html: JSON.stringify(jsonLd).replace(/</g, "\\u003c") }}
      />
    </>
  );
}
