import { Post, postMetadata } from "../../../components/post";
import { Receipts } from "../../../components/receipts";
import { RELEASE } from "../../../data/facts";

const slug = "how-openmessage-added-google-messages";

export const metadata = postMetadata(slug);

export default function GoogleMessagesPost() {
  return (
    <Post
      slug={slug}
      lead="Google Messages was the first network OpenMessage connected. It carries SMS and RCS from an Android phone, and it set the pattern the other networks follow."
    >
      <p>
        This note describes version {RELEASE.version}. The April original overstated reliability
        and has been replaced.
      </p>

      <h2>Pairing</h2>
      <p>
        OpenMessage links to your phone the way Google Messages for web does. It uses a fork of
        the open-source mautrix-gmessages library, which Google does not support. You pair by
        scanning a QR code with the Google Messages app, or by giving OpenMessage a set of Google
        account cookies copied from your browser.
      </p>
      <Receipts ids={["gmessagesFork", "googleQrPairing", "googleCookiePairing"]} />

      <h2>The live event path</h2>
      <p>
        Once paired, the backend holds a session with Google and receives events as they happen.
        Each incoming message is written to the local database, and the app is told which
        conversation changed so the thread redraws.
      </p>
      <Receipts ids={["googleHandleMessage", "googleStoreMessage", "googleNotifyUi"]} />

      <h2>One table for every network</h2>
      <p>
        Google Messages rows go into the same <code>messages</code> table that WhatsApp and Signal
        use, tagged with the network they came from. Search, the sidebar and the MCP tools read
        that table, which is why one query or one tool call covers all three.
      </p>
      <Receipts ids={["messagesDb", "searchIndex", "toolRegistry"]} />

      <h2>What breaks</h2>
      <p>
        The cookies behind a Google account session rotate, and a session can expire after the
        backend has been offline for a while. Version {RELEASE.version} cannot renew it, so you
        pair again. Code on <code>main</code> refreshes the session from your local Chrome profile
        and reconnects with backoff; it has not shipped in a release yet.
      </p>
      <Receipts ids={["googleRefresh", "supervisorBackoff"]} />
      <p>
        On October 6, 2026, Google ingest on one install went silent for 38 hours while the
        status endpoint reported the connection as healthy. The fix on <code>main</code> compares
        each network&rsquo;s silence with its own recent traffic and flags the gap.
      </p>
      <Receipts ids={["googleStall", "silenceDefaults"]} />
    </Post>
  );
}
