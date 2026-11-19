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
  TOOL_GROUPS,
  UNWARNED_QUOTING_TOOLS,
  WARNED_TOOLS
} from "../data/facts";
import { formatDate, getLatestRelease, releaseCopy } from "../lib/release";
import { clockTime, parseTranscript } from "../lib/transcript";
import appScreenshot from "../public/screens/app-demo-sms.png";

// Re-check GitHub for the latest release at most every five minutes.
export const revalidate = 300;

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

function DownloadButton() {
  return (
    <a className="btn btn--yours" href={DOWNLOAD_PATH}>
      <IconBrandApple stroke={1.7} aria-hidden="true" />
      Download for macOS
    </a>
  );
}

export default async function HomePage() {
  const release = await getLatestRelease();
  const { lines, routes } = buildReaders();
  // The copy describes RELEASE and names it inline; releaseCopy decides what
  // the page says about the release GitHub reports (see lib/release.js).
  const copy = releaseCopy(release);
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
    ...copy.jsonLd,
    downloadUrl: `${SITE_URL}${DOWNLOAD_PATH}`,
    offers: { "@type": "Offer", price: "0", priceCurrency: "USD" },
    author: { "@type": "Person", name: "Max Ghenis", url: "https://maxghenis.com" },
    sameAs: [REPO_URL]
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
              OpenMessage connects to Google Messages on your Android phone (SMS and RCS), WhatsApp
              and Signal, and keeps your messages in one SQLite file on your Mac. A Mac app and a
              local MCP server both read that file.
            </p>
            <div className="hero__cta">
              <DownloadButton />
              <a href="#setup">Set it up in three steps</a>
            </div>
            <p className="hero__release">
              {copy.hero && `${copy.hero} `}
              OpenMessage is free, needs no sign-up and runs on macOS {MIN_MACOS} or later.
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
                OpenMessage includes these demo conversations, and none of these people exist. The
                &ldquo;Your agent, over MCP&rdquo; pane shows the text <code>{DEMO_TOOL}</code>{" "}
                returned in version {RELEASE.version}, and the other pane lays out the direct
                messages in that text as a thread, one tab per network.
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
                While you have the Mac app open, an MCP server runs on your Mac at{" "}
                <code>
                  {MCP.host}:{MCP.port}
                </code>
                , and any MCP client that speaks SSE can connect to it. For Claude Code:
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
                  <h3>One call spans a person&rsquo;s networks</h3>
                  <p>
                    <code>get_person_messages</code> takes a name and looks through your 1,000 most
                    recent conversations, including groups, for ones whose name or participant list
                    contains it. By default it returns the oldest 50 messages across those
                    conversations, whatever the network. <code>person_stats</code> summarizes that
                    person&rsquo;s one-to-one history.
                  </p>
                  <Receipts ids={["personMatch", "personOldest", "personStatsDirect"]} />
                </div>
                <div className="fact">
                  <h3>Drafts wait for you</h3>
                  <p>
                    <code>draft_message</code> leaves a draft in the app for you to review and send
                    yourself.
                  </p>
                  <Receipts ids={["draftTool"]} />
                </div>
                <div className="fact">
                  <h3>{WARNED_TOOLS.length} tools warn the model about message text</h3>
                  <p>
                    Before the message text in results from {toolNames(WARNED_TOOLS)}, OpenMessage
                    puts a warning that tells the model to ignore instructions inside message
                    bodies. {toolNames(UNWARNED_QUOTING_TOOLS)} quote message text without that
                    warning.
                  </p>
                  <Receipts ids={["untrustedPreamble", "storyQuotes", "statsPhrases"]} />
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
                The Mac app writes messages from all three networks to one file,{" "}
                <code>messages.db</code>, in <code>~/Library/Application Support/OpenMessage</code>,
                and tags each row with the network it came from. A command-line build uses{" "}
                <code>~/.local/share/openmessage</code> instead.
              </p>
              <figure>
                <pre className="command command__code">
                  <code>{`$ sqlite3 messages.db "SELECT source_platform, COUNT(*) FROM messages GROUP BY 1 ORDER BY 2 DESC;"
sms|31
whatsapp|12
signal|11`}</code>
                </pre>
                <figcaption className="readers__caption">
                  I ran this on the demo data in version {RELEASE.version}.
                </figcaption>
              </figure>
              <Receipts
                ids={["dataFolder", "messagesDb", "sourcePlatformColumn", "sqliteDriver", "cliDataDir"]}
              />

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
                    The command line and the <code>import_messages</code> tool load older history
                    from four sources: iMessage&rsquo;s <code>chat.db</code> on your Mac, a Google
                    Chat Takeout, a WhatsApp chat export and Signal Desktop. You can read imported
                    iMessage and Google Chat threads but not reply to them, because OpenMessage has
                    no live connection to either.
                  </p>
                  <Receipts ids={["imessageImport", "importCommands", "importSources"]} />
                </div>
                <div className="fact">
                  <h3>OpenMessage keeps logins beside the database</h3>
                  <p>
                    In the same folder, <code>session.json</code> holds the Google Messages login,{" "}
                    <code>whatsapp-session.db</code> the WhatsApp login and a{" "}
                    <code>signal-cli</code> folder the Signal login.
                  </p>
                  <Receipts ids={["googleSessionFile", "sessionFiles", "signalConfigDir"]} />
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
                <Receipts ids={["sidebarMerge", "personRoutes"]} />
              </div>
            </div>
            <figure className="shot">
              <Image
                className="shot__img"
                src={appScreenshot}
                alt="OpenMessage's web interface. The sidebar highlights Jordan Rivera, above a second Jordan Rivera row that holds the Signal thread. The thread shows four SMS messages under tabs for SMS and WhatsApp."
                sizes="(max-width: 72rem) 100vw, 67rem"
                placeholder="empty"
              />
              <figcaption>
                <code>openmessage demo</code> serves version {RELEASE.version}&rsquo;s interface,
                which the Mac app shows in its window. The demo gives Jordan Rivera&rsquo;s Signal
                thread an account ID in place of a phone number, so it gets a row of its own.
                <Receipts ids={["appWebView", "demoSignalId"]} />
              </figcaption>
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
                    OpenMessage reaches the three networks through open-source software that Google,
                    WhatsApp and Signal do not support: the mautrix-gmessages and whatsmeow
                    libraries and the signal-cli program.{" "}
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
                    OpenMessage does not ask you to confirm before these tools send to other
                    people: {toolNames(SENDING_TOOLS)}. Keep your MCP client&rsquo;s permission
                    prompt on for them, or have your agent use <code>draft_message</code>.
                  </p>
                  <p>
                    <code>send_message</code> reports &ldquo;Message sent&rdquo; when the network
                    accepts the message, and its result says nothing about delivery or reading. For
                    SMS, <code>send_to_conversation</code> and <code>send_group_message</code> report
                    success without checking Google&rsquo;s answer. <code>send_message</code> and{" "}
                    <code>send_to_conversation</code> can also report an error after the network
                    has taken the message, when OpenMessage fails to save its own copy. An agent
                    that retries on that error sends the message twice.
                  </p>
                  <Receipts
                    ids={["sendResult", "sendUnchecked", "groupSendUnchecked", "sendPersistError"]}
                  />
                </div>
                <div className="fact">
                  <h3>What your agent reads leaves your Mac</h3>
                  <p>
                    Text that a tool returns goes to the model behind your agent, so anyone who can
                    text you can put instructions in front of that model. The warning some tools add
                    tells the model to ignore them, and nothing makes it comply.
                  </p>
                </div>
                <div className="fact">
                  <h3>Anything on your Mac can call the server</h3>
                  <p>
                    The local server listens on <code>{MCP.host}</code> with no password, so any
                    program on your Mac, under any account, can call it. OpenMessage does not
                    encrypt the database, so software running under your macOS account can also read
                    the file.
                  </p>
                  <Receipts ids={["mcpHost", "plainOpen"]} />
                </div>
                <div className="fact">
                  <h3>Connections break when the networks change</h3>
                  <p>
                    A protocol change at Google, WhatsApp or Signal can stop a connection until a
                    fix ships. For Google Messages and WhatsApp, the fix comes in a new OpenMessage
                    release, which you install yourself. For Signal, it may come in a{" "}
                    <code>signal-cli</code> update instead. Version {RELEASE.version} cannot renew
                    an expired Google Messages login, so you pair again when it lapses.
                  </p>
                  <Receipts ids={["signalCli"]} />
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
                {copy.lead}
              </p>
              {copy.showNewer && (
                <p>
                  Version {release.version} shipped after version {RELEASE.version}, so some details
                  here may have changed, including the agent setup command. Its{" "}
                  <a href={RELEASES_URL} rel="noopener">
                    release notes
                  </a>{" "}
                  list what changed.
                </p>
              )}
              {copy.showMainList && (
                <>
                  <p>
                    These changes landed on <code>main</code> after version {RELEASE.version}:
                  </p>
                  <div className="facts">
                    <div className="fact">
                      <h3>Reconnects with backoff</h3>
                      <p>
                        A supervisor for each network retries a dropped connection after a random
                        wait, under a ceiling that doubles from 5 seconds to 5 minutes. The Google
                        Messages supervisor can also refresh an expired login from your local Chrome
                        profile.
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
                      <h3>Silence detection</h3>
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
                        If you run <code>openmessage serve</code> from a command-line build with{" "}
                        <code>OPENMESSAGES_V2_SEND=1</code>, OpenMessage writes a send to disk
                        before it hands the message to the network. When it cannot tell whether a
                        send went out, it marks the send <code>uncertain</code> and never retries it
                        on its own.
                      </p>
                      <Receipts ids={["outboxFlag", "outboxUncertain", "outboxLease"]} />
                    </div>
                  </div>
                </>
              )}

              <div className="table-scroll">
                <table className="ledger">
                  <caption className="visually-hidden">
                    Where OpenMessage runs and which networks it reaches
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Platform or network</th>
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
                      <DownloadButton />
                    </p>
                  </div>
                </li>
                <li>
                  <div>
                    <h3>Pair a network</h3>
                    <p>
                      For Signal, first install <code>signal-cli</code> with{" "}
                      <code>brew install signal-cli</code>. For WhatsApp and Signal, the app shows a
                      QR code; scan it with WhatsApp or Signal on your phone, the way you would link
                      a desktop client.
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
                        "appCookiePairing",
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
                To look before you pair anything, quit the app and run{" "}
                <code>openmessage demo</code> from a command-line build, then open{" "}
                <code>
                  {MCP.host}:{MCP.port}
                </code>{" "}
                in a browser to see the demo conversations. The demo connects to none of the three
                networks.
              </p>
              <Receipts ids={["demoMode", "demoOffline"]} />
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
